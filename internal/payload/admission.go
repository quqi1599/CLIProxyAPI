package payload

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/routemetrics"
)

// TransformAdmission admits synchronous preparation work independently of the
// lifetime of an upstream request. The callback receives wall time excluding queueing.
type TransformAdmission interface {
	AcquireTransform(context.Context, int64) (func(time.Duration), error)
}
type transformAdmissionKey struct{}
type transformScopeKey struct{}
type transformScopeState struct{ active atomic.Bool }

func WithTransformAdmission(ctx context.Context, gate TransformAdmission) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, transformAdmissionKey{}, gate)
}

// BeginTransformScope must be deferred at a synchronous preparation boundary.
// Nested helpers reuse the scope; sibling calls using the original context acquire
// separately. Do not pass a scoped context to asynchronous work or upstream IO.
func BeginTransformScope(ctx context.Context, size int64) (context.Context, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if state, _ := ctx.Value(transformScopeKey{}).(*transformScopeState); state != nil && state.active.Load() {
		return ctx, func() {}, nil
	}
	gate, _ := ctx.Value(transformAdmissionKey{}).(TransformAdmission)
	release := func(time.Duration) {}
	if gate != nil {
		var err error
		release, err = gate.AcquireTransform(ctx, size)
		if err != nil {
			return ctx, nil, err
		}
	}
	started := time.Now()
	var once sync.Once
	state := &transformScopeState{}
	state.active.Store(true)
	return context.WithValue(ctx, transformScopeKey{}, state), func() {
		once.Do(func() {
			elapsed := time.Since(started)
			state.active.Store(false)
			observePreparationDuration(size, elapsed)
			routemetrics.Observe(ctx, "transform", elapsed)
			release(elapsed)
		})
	}, nil
}

// PreparationMetrics includes full outer scopes, even failed/unfinalized work.
// Unlike stage sums this never double counts nested translation and policy work.
type PreparationMetrics struct {
	Scopes             uint64                   `json:"scopes"`
	OverOneSecond      uint64                   `json:"over_one_second"`
	LargeOverOneSecond uint64                   `json:"large_over_one_second"`
	Nanoseconds        uint64                   `json:"nanoseconds"`
	DurationBuckets    TransformDurationBuckets `json:"duration_buckets"`
}

var preparationMetrics struct {
	sync.Mutex
	value PreparationMetrics
}

func observePreparationDuration(size int64, elapsed time.Duration) {
	preparationMetrics.Lock()
	defer preparationMetrics.Unlock()
	v := &preparationMetrics.value
	v.Scopes++
	v.Nanoseconds += uint64(max(elapsed, 0))
	if elapsed > time.Second {
		v.OverOneSecond++
		if size > 16<<20 {
			v.LargeOverOneSecond++
		}
	}
	observeTransformDurationBucket(&v.DurationBuckets, elapsed)
}
func currentPreparationMetrics() PreparationMetrics {
	preparationMetrics.Lock()
	defer preparationMetrics.Unlock()
	return preparationMetrics.value
}
