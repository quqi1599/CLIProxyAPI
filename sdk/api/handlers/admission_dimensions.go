package handlers

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

const (
	ingressBodyAdmissionGinKey    = "cliproxy_body_admission"
	bodyAdmissionControllerGinKey = "cliproxy_body_admission_controller"
	largeAdmissionBodyBytes       = 16 << 20
	defaultBodyCapacityBytes      = 512 << 20
	defaultTransformCapacity      = 16
)

// AdmissionDimensionSnapshot keeps each resource's capacity and wait telemetry separate.
type AdmissionDimensionSnapshot struct {
	Enabled             bool                         `json:"enabled"`
	Capacity            int                          `json:"capacity"`
	Active              int                          `json:"active"`
	QueueDepth          int                          `json:"queue_depth"`
	MaxQueue            int                          `json:"max_queue"`
	MaxWaitMilliseconds int64                        `json:"max_wait_milliseconds"`
	WaitDurationBuckets AdmissionWaitDurationBuckets `json:"wait_duration_buckets"`
	Rejects             AdmissionRejectCounters      `json:"rejects"`
}

func dimensionSnapshot(c *admissionController) AdmissionDimensionSnapshot {
	if c == nil {
		return AdmissionDimensionSnapshot{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return AdmissionDimensionSnapshot{Enabled: c.enabled, Capacity: c.capacity, Active: c.activeWeight, QueueDepth: c.queueDepthLocked(), MaxQueue: c.maxQueue, MaxWaitMilliseconds: c.maxWait.Milliseconds(), WaitDurationBuckets: c.waitBuckets, Rejects: c.rejects}
}

func readControllerFromConfig(cfg *config.SDKConfig) *admissionController {
	c := admissionControllerFromConfig(cfg)
	if c != nil && cfg.RequestGuards.GlobalAdmission.ReadCapacity > 0 {
		c.capacity = cfg.RequestGuards.GlobalAdmission.ReadCapacity
	}
	return c
}

func (h *BaseAPIHandler) updateDimensionControllers(cfg *config.SDKConfig) {
	body, transform := admissionControllerFromConfig(cfg), admissionControllerFromConfig(cfg)
	if body != nil {
		settings := cfg.RequestGuards.GlobalAdmission
		body.capacity = defaultBodyCapacityBytes
		if settings.BodyCapacityBytes > 0 {
			body.capacity = int(min(settings.BodyCapacityBytes, int64(^uint(0)>>1)))
		}
		body.strictCapacity = true
		transform.capacity = defaultTransformCapacity
		if settings.TransformCapacity > 0 {
			transform.capacity = settings.TransformCapacity
		}
		if settings.TransformMaxQueue > 0 {
			transform.maxQueue = settings.TransformMaxQueue
		}
		transform.maxWait = time.Second
		if settings.TransformMaxWaitMilliseconds > 0 {
			transform.maxWait = time.Duration(settings.TransformMaxWaitMilliseconds) * time.Millisecond
		}
	}
	for _, item := range []struct {
		current, next *admissionController
		body          bool
	}{
		{h.bodyAdmission.Load(), body, true}, {h.transformAdmission.Load(), transform, false},
	} {
		if item.current == nil {
			if item.body {
				h.bodyAdmission.Store(item.next)
			} else {
				h.transformAdmission.Store(item.next)
			}
		} else if item.next == nil {
			item.current.updateSettings(false, 0, 0, 0, 0)
		} else {
			item.current.updateSettings(true, item.next.capacity, item.next.maxQueue, item.next.maxWait, item.next.saturationGrace)
		}
	}
}

// Large requests consume at least four slots to reduce their concurrency.
func executionAdmissionWeight(vector complexityVector) int {
	if vector.DecodedBytes > largeAdmissionBodyBytes {
		return 4 * ceilAdmissionUnits(int(vector.DecodedBytes), 32<<20)
	}
	return 1
}

func ingressBodyAdmissionWeight(request *http.Request, vector complexityVector) int {
	if request.ContentLength <= 0 || !identityContentEncoding(request.Header.Get("Content-Encoding")) {
		return 0
	}
	return int(max(vector.DecodedBytes, 1))
}

func bodyAdmissionReferenceFromGin(c *gin.Context) *admissionLeaseReference {
	if c == nil {
		return nil
	}
	value, _ := c.Get(ingressBodyAdmissionGinKey)
	ref, _ := value.(*admissionLeaseReference)
	return ref
}

type bodyAdmissionContextKey struct{}

func bodyReferenceFromContext(ctx context.Context) *admissionLeaseReference {
	if ctx == nil {
		return nil
	}
	if ref, _ := ctx.Value(bodyAdmissionContextKey{}).(*admissionLeaseReference); ref != nil {
		return ref
	}
	c, _ := ctx.Value("gin").(*gin.Context)
	return bodyAdmissionReferenceFromGin(c)
}

// Unknown reads have a separate conservative read lease. Transfer to measured
// retained bytes before inspection; never wait while retaining unaccounted input.
func reserveMeasuredBody(c *gin.Context, vector complexityVector) error {
	if c == nil || c.Request == nil {
		return nil
	}
	value, _ := c.Get(bodyAdmissionControllerGinKey)
	controller, _ := value.(*admissionController)
	if controller == nil {
		return nil
	}
	weight := int(max(vector.DecodedBytes, 1))
	if ref := bodyAdmissionReferenceFromGin(c); ref != nil {
		return ref.upgrade(c.Request.Context(), weight)
	}
	_, weight, admitted, err := controller.acquireImmediate(c.Request.Context(), weight)
	if err != nil {
		return err
	}
	if admitted {
		c.Set(ingressBodyAdmissionGinKey, newAdmissionLease(controller, weight).root)
	}
	return nil
}

func releaseIngressBodyAdmission(c *gin.Context) {
	if ref := bodyAdmissionReferenceFromGin(c); ref != nil {
		c.Set(ingressBodyAdmissionGinKey, (*admissionLeaseReference)(nil))
		ref.releaseReference()
	}
}

func (h *BaseAPIHandler) acquireExecutionBody(ctx context.Context, size int64) (context.Context, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	noop := func() {}
	if h == nil {
		return ctx, noop, nil
	}
	controller := h.bodyAdmission.Load()
	if controller == nil {
		return ctx, noop, nil
	}
	weight := int(max(size, 1))
	if ref := bodyReferenceFromContext(ctx); ref != nil {
		child, release, retained, err := ref.retain(ctx, controller, weight)
		if retained {
			return context.WithValue(ctx, bodyAdmissionContextKey{}, child), release, err
		}
	}
	_, weight, admitted, err := controller.acquireImmediate(ctx, weight)
	if err != nil {
		return ctx, nil, err
	}
	if !admitted {
		return ctx, noop, nil
	}
	ref := newAdmissionLease(controller, weight).root
	return context.WithValue(ctx, bodyAdmissionContextKey{}, ref), ref.releaseFunc(), nil
}

// Queue time is a shared cumulative budget, never a context/network deadline.
type admissionQueueBudgetKey struct{}
type admissionQueueBudget struct {
	mu   sync.Mutex
	left time.Duration
}

func (b *admissionQueueBudget) remaining() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return max(b.left, 0)
}
func (b *admissionQueueBudget) consume(d time.Duration) {
	b.mu.Lock()
	b.left = max(b.left-d, 0)
	b.mu.Unlock()
}
func admissionQueueBudgetFromContext(ctx context.Context) *admissionQueueBudget {
	b, _ := ctx.Value(admissionQueueBudgetKey{}).(*admissionQueueBudget)
	if b == nil {
		// GetContextWithCancel preserves the caller parent rather than every
		// request value. Keep ingress and executor queue accounting shared.
		if c, _ := ctx.Value("gin").(*gin.Context); c != nil && c.Request != nil {
			b, _ = c.Request.Context().Value(admissionQueueBudgetKey{}).(*admissionQueueBudget)
		}
	}
	return b
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// Fixed size buckets keep feedback bounded and free of tenant/model identifiers.
// Elapsed preparation time is a cost proxy; it is not process CPU time.
type transformCostHistory struct {
	mu      sync.Mutex
	elapsed [3]time.Duration
}

func transformSizeBucket(size int64) int {
	if size > largeAdmissionBodyBytes {
		return 2
	}
	if size > 1<<20 {
		return 1
	}
	return 0
}
func (h *BaseAPIHandler) withTransformAdmission(ctx context.Context) context.Context {
	if h == nil {
		return ctx
	}
	if controller := h.admission.Load(); controller != nil && admissionQueueBudgetFromContext(ctx) == nil {
		controller.mu.Lock()
		wait := controller.maxWait
		controller.mu.Unlock()
		ctx = context.WithValue(ctx, admissionQueueBudgetKey{}, &admissionQueueBudget{left: wait})
	}
	return internalpayload.WithTransformAdmission(ctx, h)
}

// AcquireTransform implements payload.TransformAdmission without coupling the
// executor to handlers. Release precedes upstream IO; feedback excludes queue time.
func (h *BaseAPIHandler) AcquireTransform(ctx context.Context, size int64) (func(time.Duration), error) {
	controller := h.transformAdmission.Load()
	if controller == nil {
		return func(time.Duration) {}, nil
	}
	bucket := transformSizeBucket(size)
	weight := [3]int{1, 2, 8}[bucket]
	h.transformCosts.mu.Lock()
	slow := h.transformCosts.elapsed[bucket] > time.Second
	h.transformCosts.mu.Unlock()
	if slow {
		weight *= 2
	}
	release, err := controller.acquire(ctx, weight)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, &failurecontract.Failure{Kind: failurecontract.ProviderUnavailable, Scope: failurecontract.ScopeRequest, HTTPStatus: http.StatusServiceUnavailable, SemanticCode: "transform_admission_rejected", ProviderCode: "transform_admission_rejected", PublicMessage: "Server is busy preparing requests; retry later", Cause: err}
	}
	var once sync.Once
	return func(elapsed time.Duration) {
		once.Do(func() {
			release()
			h.transformCosts.mu.Lock()
			old := h.transformCosts.elapsed[bucket]
			if old == 0 {
				h.transformCosts.elapsed[bucket] = elapsed
			} else {
				h.transformCosts.elapsed[bucket] = old - old/4 + elapsed/4
			}
			h.transformCosts.mu.Unlock()
		})
	}, nil
}
