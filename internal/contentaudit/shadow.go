package contentaudit

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const shadowWorkerCount = 4

// Selection is stable for the same scoped task and policy; it is not a content
// decision. Quotas still apply to critical candidates that bypass sampling.
func sampleShadowReview(state *runtimeState, request ModelReviewRequest) bool {
	rate := *state.cfg.ModelReview.ShadowSampleRate
	if rate <= 0 {
		return false
	}
	if rate >= 1 || request.Severity == "critical" {
		return true
	}
	mac := hmac.New(sha256.New, state.evidenceKeyFingerprint[:])
	var size [8]byte
	for _, value := range []string{"shadow-sample-v1", request.TenantScope, request.PolicyVersion, request.RuleID, request.Text, request.ReferenceText} {
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		_, _ = mac.Write(size[:])
		_, _ = mac.Write([]byte(value))
	}
	value := binary.BigEndian.Uint64(mac.Sum(nil)[:8]) >> 11
	return float64(value)/float64(uint64(1)<<53) < rate
}

// ShadowReviewStatus contains only bounded operational counters, never prompts.
type ShadowReviewStatus struct {
	Queued          int    `json:"queued"`
	QueuedBytes     int64  `json:"queued_bytes"`
	Active          int    `json:"active"`
	Submitted       uint64 `json:"submitted"`
	Completed       uint64 `json:"completed"`
	ValidDecisions  uint64 `json:"valid_decisions"`
	Recovered       uint64 `json:"recovered_interrupted"`
	Skipped         uint64 `json:"skipped"`
	PersistenceLost uint64 `json:"persistence_lost"`
}

type shadowReviewJob struct {
	eventID string
	state   *runtimeState
	request ModelReviewRequest
	queued  time.Time
	bytes   int64
}

// A single pool belongs to the service rather than to each hot configuration.
type shadowReviewQueue struct {
	service  *Service
	mu       sync.Mutex
	jobs     chan shadowReviewJob
	started  bool
	closed   bool
	stats    ShadowReviewStatus
	wg       sync.WaitGroup
	waitOnce sync.Once
	done     chan struct{}
}

func newShadowReviewQueue(service *Service) *shadowReviewQueue {
	return &shadowReviewQueue{service: service, jobs: make(chan shadowReviewJob, 1024), done: make(chan struct{})}
}

func (q *shadowReviewQueue) status() ShadowReviewStatus {
	if q == nil {
		return ShadowReviewStatus{}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.stats
}

// submit never waits for queue capacity, a model, or a database operation.
func (q *shadowReviewQueue) submit(job shadowReviewJob) string {
	if q == nil {
		return "shadow_shutdown"
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.service.ctx.Err() != nil {
		q.stats.Skipped++
		return "shadow_shutdown"
	}
	cfg := job.state.cfg.ModelReview
	job.bytes = int64(len(job.request.Text) + len(job.request.ReferenceText))
	if job.bytes > cfg.ShadowQueueBytes {
		q.stats.Skipped++
		return "shadow_oversize"
	}
	if q.stats.Queued >= cfg.ShadowQueueSize || q.stats.QueuedBytes+job.bytes > cfg.ShadowQueueBytes {
		q.stats.Skipped++
		return "shadow_queue_full"
	}
	// Clone only admitted text so slices cannot retain an entire request body.
	job.request.Text = strings.Clone(job.request.Text)
	job.request.ReferenceText = strings.Clone(job.request.ReferenceText)
	job.queued = time.Now()
	if !q.started {
		q.started = true
		for range shadowWorkerCount {
			q.wg.Add(1)
			go q.worker()
		}
	}
	q.jobs <- job // The queue count and channel capacity are checked under mu.
	q.stats.Submitted++
	q.stats.Queued++
	q.stats.QueuedBytes += job.bytes
	return ""
}

func (q *shadowReviewQueue) worker() {
	defer q.wg.Done()
	for job := range q.jobs {
		q.mu.Lock()
		q.stats.Queued--
		q.stats.QueuedBytes -= job.bytes
		q.stats.Active++
		q.mu.Unlock()
		outcome := modelReviewOutcome{ModelReviewResult: ModelReviewResult{Decision: ModelReviewUncertain}, Model: job.state.cfg.ModelReview.Model}
		current := q.service.state.Load()
		switch {
		case q.service.ctx.Err() != nil:
			outcome.Fallback = "shadow_shutdown"
		case time.Since(job.queued) > time.Duration(job.state.cfg.ModelReview.ShadowMaxAgeSeconds)*time.Second:
			outcome.Fallback = "shadow_expired"
		case current == nil || current.modelReview != job.state.modelReview || current.matcher == nil || current.matcher.Version() != job.request.PolicyVersion || !current.cfg.Enabled:
			outcome.Fallback = "shadow_stale_config"
		default:
			outcome = job.state.modelReview.review(q.service.ctx, job.request)
		}
		if outcome.StageLatenciesMS == nil {
			outcome.StageLatenciesMS = make(map[string]int64)
		}
		outcome.StageLatenciesMS["shadow_queue"] = max(0, time.Since(job.queued).Milliseconds()-outcome.Latency.Milliseconds())
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := job.state.store.UpdateShadowReview(ctx, job.eventID, job.request.PolicyVersion, outcome)
		cancel()
		q.mu.Lock()
		q.stats.Active--
		if strings.HasPrefix(outcome.Fallback, "shadow_") {
			q.stats.Skipped++
		} else {
			q.stats.Completed++
			if outcome.Fallback == "" && (outcome.Decision == ModelReviewAllow || outcome.Decision == ModelReviewBlock) {
				q.stats.ValidDecisions++
			}
		}
		if err != nil {
			q.stats.PersistenceLost++
		}
		q.mu.Unlock()
		if err != nil {
			log.Warn("content audit shadow result persistence failed")
		}
	}
}

func (q *shadowReviewQueue) stop(ctx context.Context) error {
	q.mu.Lock()
	if !q.closed {
		q.closed = true
		close(q.jobs)
	}
	q.mu.Unlock()
	// Drain queued work in bounded batches without making shutdown wait for models.
	pending := make(map[*Store][]string)
	for job := range q.jobs {
		pending[job.state.store] = append(pending[job.state.store], job.eventID)
		q.mu.Lock()
		q.stats.Queued--
		q.stats.QueuedBytes -= job.bytes
		q.stats.Skipped++
		q.mu.Unlock()
	}
	for store, ids := range pending {
		if err := store.InterruptShadowReviews(ctx, ids, "shadow_shutdown"); err != nil {
			q.mu.Lock()
			q.stats.PersistenceLost += uint64(len(ids))
			q.mu.Unlock()
		}
	}
	q.waitOnce.Do(func() { go func() { q.wg.Wait(); close(q.done) }() })
	select {
	case <-q.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Shutdown ends background work after HTTP handlers have drained. The store
// remains caller-owned so old snapshots are not closed while still in use.
func (s *Service) Shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.policyMu.Lock()
	s.cancel()
	s.policyMu.Unlock()
	s.backgroundWaitOnce.Do(func() { go func() { s.background.Wait(); close(s.backgroundDone) }() })
	if err := s.shadow.stop(ctx); err != nil {
		return err
	}
	select {
	case <-s.backgroundDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
