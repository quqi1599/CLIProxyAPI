package handlers

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

const (
	defaultAdmissionCapacity        = 128
	defaultAdmissionQueueSize       = 64
	defaultAdmissionMaxWait         = 30 * time.Second
	defaultAdmissionRetryAfter      = 1
	defaultAdmissionSaturationGrace = 5 * time.Second
	lightAdmissionWeight            = 4
	heavyAdmissionAging             = 100 * time.Millisecond
	ingressAdmissionGinKey          = "cliproxy_ingress_admission"
	imagesEditIngressBodyBytes      = 128 << 20
	downstreamWebsocketBodyBytes    = 64 << 20
)

var (
	errAdmissionQueueFull   = errors.New("request admission queue is full; retry later")
	errAdmissionWaitTimeout = errors.New("request admission wait timed out; retry later")
)

// AdmissionWaitDurationBuckets contains fixed, low-cardinality queue-wait counters.
type AdmissionWaitDurationBuckets struct {
	LessThan10Milliseconds  uint64 `json:"lt_10ms"`
	LessThan100Milliseconds uint64 `json:"lt_100ms"`
	LessThanOneSecond       uint64 `json:"lt_1s"`
	LessThanTenSeconds      uint64 `json:"lt_10s"`
	TenSecondsOrMore        uint64 `json:"gte_10s"`
}

// AdmissionRejectCounters contains fixed, low-cardinality rejection counters.
type AdmissionRejectCounters struct {
	QueueFull   uint64 `json:"queue_full"`
	WaitTimeout uint64 `json:"wait_timeout"`
}

// AdmissionSnapshot is an immutable in-memory view of the global admission pool.
type AdmissionSnapshot struct {
	Enabled             bool                         `json:"enabled"`
	ActiveWeight        int                          `json:"active_weight"`
	QueueDepth          int                          `json:"queue_depth"`
	WaitDurationBuckets AdmissionWaitDurationBuckets `json:"wait_duration_buckets"`
	Rejects             AdmissionRejectCounters      `json:"rejects"`
}

type admissionHeldContextKey struct{}

type admissionLease struct {
	mu         sync.Mutex
	controller *admissionController
	references int
	weight     int
	root       *admissionLeaseReference
}

// admissionLeaseReference makes nested paths share capacity while sibling paths add capacity.
type admissionLeaseReference struct {
	lease       *admissionLease
	parent      *admissionLeaseReference
	active      bool
	weight      int
	childWeight int
	aggregate   int
}

type admissionController struct {
	mu              sync.Mutex
	enabled         bool
	capacity        int
	maxQueue        int
	maxWait         time.Duration
	saturationGrace time.Duration
	activeWeight    int
	activeWeights   map[int]int
	waiters         [2][]*admissionWaiter
	preferHeavy     bool
	saturatedSince  time.Time
	waitBuckets     AdmissionWaitDurationBuckets
	rejects         AdmissionRejectCounters
	now             func() time.Time
}

type admissionWaiter struct {
	ready           chan struct{}
	requestedWeight int
	weight          int
	admitted        bool
	enqueuedAt      time.Time
	deadline        time.Time
}

func newAdmissionController(capacity, maxQueue int, saturationGrace time.Duration) *admissionController {
	if capacity <= 0 {
		capacity = defaultAdmissionCapacity
	}
	if maxQueue <= 0 {
		maxQueue = defaultAdmissionQueueSize
	}
	if saturationGrace < 0 {
		saturationGrace = 0
	}
	return &admissionController{
		enabled:         true,
		capacity:        capacity,
		maxQueue:        maxQueue,
		maxWait:         defaultAdmissionMaxWait,
		saturationGrace: saturationGrace,
		activeWeights:   make(map[int]int),
		now:             time.Now,
	}
}

func admissionControllerFromConfig(cfg *config.SDKConfig) *admissionController {
	if cfg == nil || !cfg.RequestGuards.GlobalAdmission.Enabled {
		return nil
	}
	settings := cfg.RequestGuards.GlobalAdmission
	grace := defaultAdmissionSaturationGrace
	if settings.SaturationGraceSeconds > 0 {
		grace = time.Duration(settings.SaturationGraceSeconds) * time.Second
	}
	controller := newAdmissionController(settings.Capacity, settings.MaxQueue, grace)
	if settings.MaxWaitSeconds > 0 {
		controller.maxWait = time.Duration(settings.MaxWaitSeconds) * time.Second
	}
	return controller
}

func (h *BaseAPIHandler) updateAdmissionController(cfg *config.SDKConfig) {
	if h == nil {
		return
	}
	h.admissionUpdateMu.Lock()
	defer h.admissionUpdateMu.Unlock()

	next := admissionControllerFromConfig(cfg)
	current := h.admission.Load()
	if current == nil {
		h.admission.Store(next)
		return
	}
	if next == nil {
		current.updateSettings(false, 0, 0, 0, 0)
		return
	}
	current.updateSettings(true, next.capacity, next.maxQueue, next.maxWait, next.saturationGrace)
}

func newAdmissionLease(controller *admissionController, weight int) *admissionLease {
	lease := &admissionLease{
		controller: controller,
		references: 1,
		weight:     weight,
	}
	lease.root = &admissionLeaseReference{lease: lease, active: true, weight: weight, aggregate: weight}
	return lease
}

func (r *admissionLeaseReference) retain(ctx context.Context, controller *admissionController, weight int) (*admissionLeaseReference, func(), bool, error) {
	if r == nil || r.lease == nil || controller == nil {
		return nil, nil, false, nil
	}
	l := r.lease
	l.mu.Lock()
	if l.references == 0 || l.controller != controller || !r.active {
		l.mu.Unlock()
		return nil, nil, false, nil
	}
	normalizedWeight := controller.normalizedWeight(weight)
	child := &admissionLeaseReference{
		lease:     l,
		parent:    r,
		active:    true,
		weight:    normalizedWeight,
		aggregate: normalizedWeight,
	}
	r.adjustChildWeight(normalizedWeight)
	l.references++
	addedWeight, err := controller.acquireUpgrade(ctx, l.weight, l.root.aggregate)
	if err != nil {
		r.adjustChildWeight(-normalizedWeight)
		l.references--
		l.mu.Unlock()
		return nil, nil, true, err
	}
	l.weight += addedWeight
	l.mu.Unlock()
	return child, child.releaseFunc(), true, nil
}

func (r *admissionLeaseReference) upgrade(ctx context.Context, weight int) error {
	if r == nil || r.lease == nil {
		return nil
	}
	l := r.lease
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.references == 0 || !r.active || l.controller == nil {
		return nil
	}
	normalizedWeight := l.controller.normalizedWeight(weight)
	if normalizedWeight == r.weight {
		return nil
	}
	previousWeight := r.weight
	previousAggregate := r.aggregate
	r.weight = normalizedWeight
	r.aggregate = max(r.weight, r.childWeight)
	delta := r.aggregate - previousAggregate
	if r.parent != nil {
		r.parent.adjustChildWeight(delta)
	}
	addedWeight, err := l.controller.acquireUpgrade(ctx, l.weight, l.root.aggregate)
	if err != nil {
		if r.parent != nil {
			r.parent.adjustChildWeight(-delta)
		}
		r.weight = previousWeight
		r.aggregate = previousAggregate
		return err
	}
	l.weight += addedWeight
	return nil
}

func (r *admissionLeaseReference) adjustChildWeight(delta int) {
	for current := r; current != nil && delta != 0; current = current.parent {
		previous := current.aggregate
		current.childWeight += delta
		current.aggregate = current.childWeight
		if current.active && current.weight > current.aggregate {
			current.aggregate = current.weight
		}
		delta = current.aggregate - previous
	}
}

func (r *admissionLeaseReference) releaseFunc() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			r.releaseReference()
		})
	}
}

func (r *admissionLeaseReference) releaseReference() {
	if r == nil || r.lease == nil {
		return
	}
	l := r.lease
	heldWeight := 0
	remainingWeight := 0
	controller := l.controller
	l.mu.Lock()
	if r.active && l.references > 0 {
		previous := r.aggregate
		r.active = false
		r.aggregate = r.childWeight
		if r.parent != nil {
			r.parent.adjustChildWeight(r.aggregate - previous)
		}
		l.references--
		if l.references == 0 {
			heldWeight = l.weight
			l.weight = 0
		} else if l.root != nil && l.root.aggregate < l.weight {
			heldWeight = l.weight
			l.weight = l.root.aggregate
			remainingWeight = l.weight
		}
	}
	l.mu.Unlock()
	if heldWeight > 0 && controller != nil {
		controller.resize(heldWeight, remainingWeight)
	}
}

func complexityAdmissionWeight(vector complexityVector) int {
	weight := 1
	weight += ceilAdmissionUnits(int(vector.DecodedBytes), 2<<20)
	weight += ceilAdmissionUnits(int(vector.ToolOutputBytes), 1<<20)
	weight += ceilAdmissionUnits(int(vector.InlineImageBytes), 4<<20)
	weight += ceilAdmissionUnits(vector.MessageCount, 256)
	return weight
}

// IngressAdmissionMiddleware reserves weighted capacity before request parsing
// and compatibility transforms allocate copies of the request body.
func (h *BaseAPIHandler) IngressAdmissionMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c == nil {
			return
		}
		if h == nil || c.Request == nil || admissionReferenceFromGin(c) != nil {
			c.Next()
			return
		}
		if payloadBodyLimitPolicyFromContext(c) == nil {
			injectPayloadBodyLimitPolicy(c, h.payloadBodyLimit.Load())
		}
		controller := h.admission.Load()
		if controller == nil {
			c.Next()
			return
		}
		vector, limit, applies := ingressAdmissionEstimate(c.Request)
		if !applies {
			c.Next()
			return
		}
		rejectLimit, decision := effectiveKnownContentLengthLimit(c, limit)
		if c.Request.ContentLength > rejectLimit {
			recordKnownContentLengthBodyLimit(c, decision, c.Request.ContentLength)
			WriteRequestBodyError(c, NewRequestBodyLimitError(rejectLimit, false))
			c.Abort()
			return
		}
		_, admittedWeight, admitted, err := controller.acquireTracked(c.Request.Context(), ingressAdmissionWeight(c.Request, vector, decision))
		if err != nil {
			writeAdmissionHTTPError(c, err)
			c.Abort()
			return
		}
		if !admitted {
			c.Next()
			return
		}
		lease := newAdmissionLease(controller, admittedWeight)
		c.Set(ingressAdmissionGinKey, lease.root)
		defer func() {
			c.Set(ingressAdmissionGinKey, (*admissionLeaseReference)(nil))
			lease.root.releaseReference()
		}()
		c.Next()
	}
}

// PreAuthIngressAdmissionMiddleware takes a non-queueing capacity lease before
// request logging or body-reading authentication plugins can allocate payload
// copies. Route-level admission reuses and upgrades this lease after auth.
func (h *BaseAPIHandler) PreAuthIngressAdmissionMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c == nil || h == nil || c.Request == nil {
			c.Next()
			return
		}
		if !isPublicAPIPath(c.Request.URL.Path) {
			c.Next()
			return
		}
		injectPayloadBodyLimitPolicy(c, h.payloadBodyLimit.Load())
		if admissionReferenceFromGin(c) != nil {
			c.Next()
			return
		}
		controller := h.admission.Load()
		if controller == nil {
			c.Next()
			return
		}
		vector, limit, applies := ingressAdmissionEstimate(c.Request)
		if !applies {
			c.Next()
			return
		}
		rejectLimit, decision := effectiveKnownContentLengthLimit(c, limit)
		if rejectLimit > 0 && c.Request.ContentLength > rejectLimit {
			recordKnownContentLengthBodyLimit(c, decision, c.Request.ContentLength)
			WriteRequestBodyError(c, NewRequestBodyLimitError(rejectLimit, false))
			c.Abort()
			return
		}
		_, admittedWeight, admitted, err := controller.acquireImmediate(c.Request.Context(), ingressAdmissionWeight(c.Request, vector, decision))
		if err != nil {
			writeAdmissionHTTPError(c, err)
			c.Abort()
			return
		}
		if !admitted {
			c.Next()
			return
		}
		lease := newAdmissionLease(controller, admittedWeight)
		c.Set(ingressAdmissionGinKey, lease.root)
		defer func() {
			c.Set(ingressAdmissionGinKey, (*admissionLeaseReference)(nil))
			lease.root.releaseReference()
		}()
		c.Next()
	}
}

func isPublicAPIPath(path string) bool {
	return strings.HasPrefix(path, "/v1/") || strings.HasPrefix(path, "/openai/v1/") || strings.HasPrefix(path, "/backend-api/codex/") || strings.HasPrefix(path, "/v1beta/")
}

func ingressAdmissionEstimate(request *http.Request) (complexityVector, int64, bool) {
	if request == nil {
		return complexityVector{}, 0, false
	}
	if request.Body == nil || request.Body == http.NoBody {
		return complexityVector{}, 0, false
	}
	switch request.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
	default:
		return complexityVector{}, 0, false
	}
	limit := maxDecodedRequestBodyBytes
	inlineImages := false
	if request.URL != nil && request.URL.Path == "/v1/images/edits" && strings.HasPrefix(strings.ToLower(strings.TrimSpace(request.Header.Get("Content-Type"))), "multipart/form-data") {
		limit = imagesEditIngressBodyBytes
		inlineImages = true
	}
	estimatedBytes := request.ContentLength
	if estimatedBytes <= 0 || !identityContentEncoding(request.Header.Get("Content-Encoding")) {
		estimatedBytes = limit
	}
	vector := complexityVector{WireBytes: max(request.ContentLength, 0), DecodedBytes: estimatedBytes}
	if inlineImages {
		vector.InlineImageBytes = estimatedBytes
	}
	return vector, limit, true
}

func ingressAdmissionWeight(request *http.Request, vector complexityVector, decision payloadBodyLimitDecision) int {
	if request != nil && (request.ContentLength <= 0 || !identityContentEncoding(request.Header.Get("Content-Encoding"))) {
		vector.DecodedBytes = decision.maxDecodedBytes
		if vector.InlineImageBytes > 0 {
			vector.InlineImageBytes = decision.maxDecodedBytes
		}
	}
	return complexityAdmissionWeight(vector)
}

func admissionReferenceFromGin(c *gin.Context) *admissionLeaseReference {
	if c == nil {
		return nil
	}
	value, exists := c.Get(ingressAdmissionGinKey)
	if !exists {
		return nil
	}
	reference, _ := value.(*admissionLeaseReference)
	return reference
}

func admissionReferenceFromContext(ctx context.Context) *admissionLeaseReference {
	if ctx == nil {
		return nil
	}
	if reference, _ := ctx.Value(admissionHeldContextKey{}).(*admissionLeaseReference); reference != nil {
		return reference
	}
	ginContext, _ := ctx.Value("gin").(*gin.Context)
	return admissionReferenceFromGin(ginContext)
}

func upgradeIngressAdmission(c *gin.Context, vector complexityVector) error {
	reference := admissionReferenceFromGin(c)
	if reference == nil {
		return nil
	}
	ctx := context.Background()
	if c != nil && c.Request != nil {
		ctx = c.Request.Context()
	}
	return reference.upgrade(ctx, complexityAdmissionWeight(vector))
}

func (h *BaseAPIHandler) inspectAndAcquireAdmission(ctx context.Context, rawJSON []byte, options *modelExecutionOptions) (context.Context, func(), error) {
	requestBodyLimit := maxDecodedRequestBodyBytes
	bodyKind := payloadBodyKindJSON
	if coreexecutor.DownstreamWebsocket(ctx) {
		requestBodyLimit = downstreamWebsocketBodyBytes
		bodyKind = payloadBodyKindWebsocket
	}
	if ctx != nil {
		if ginContext, _ := ctx.Value("gin").(*gin.Context); ginContext != nil {
			decision := payloadBodyLimitDecisionForContext(ginContext, bodyKind, requestBodyLimit, requestBodyLimit)
			requestBodyLimit = decision.maxDecodedBytes
		}
	}
	if _, err := enforceRequestBodyLimit(rawJSON, requestBodyLimit, true); err != nil {
		return ctx, nil, err
	}
	dimensions := complexityDimensions{}
	if options != nil {
		dimensions = options.complexityDimensions
	}
	var vector complexityVector
	var valid, cached bool
	if options == nil || !options.InternalSource {
		vector, valid, cached = requestComplexityFromContext(ctx)
	}
	if !cached {
		vector, valid = inspectRequestComplexityWithDimensions(rawJSON, dimensions)
	}
	vector.applyDimensions(dimensions)
	if ctx == nil {
		ctx = context.Background()
	}
	if h != nil {
		ctx = h.withAmplificationGuardMode(ctx)
	}
	if !internalpayload.HasTransformReport(ctx) {
		ctx = internalpayload.WithTransformReportBytes(ctx, vector.WireBytes, vector.DecodedBytes)
		addTransformReportLogObserver(ctx)
	}
	releaseReport := internalpayload.RetainTransformReport(ctx)
	finish := func(release func()) func() {
		if release == nil {
			release = func() {}
		}
		return func() {
			release()
			releaseReport()
		}
	}
	if options != nil {
		options.complexity = &vector
		options.complexityValid = valid
	}
	if h == nil {
		allowAdmissionKeepAlive(ctx)
		return ctx, finish(nil), nil
	}
	controller := h.admission.Load()
	if controller == nil {
		allowAdmissionKeepAlive(ctx)
		return ctx, finish(nil), nil
	}
	weight := complexityAdmissionWeight(vector)
	if held := admissionReferenceFromContext(ctx); held != nil {
		if reference, release, retained, err := held.retain(ctx, controller, weight); retained {
			if err != nil {
				releaseReport()
				return ctx, nil, err
			}
			allowAdmissionKeepAlive(ctx)
			return context.WithValue(ctx, admissionHeldContextKey{}, reference), finish(release), nil
		}
	}
	_, admittedWeight, admitted, err := controller.acquireTracked(ctx, weight)
	if err != nil {
		releaseReport()
		return ctx, nil, err
	}
	if !admitted {
		allowAdmissionKeepAlive(ctx)
		return ctx, finish(nil), nil
	}
	lease := newAdmissionLease(controller, admittedWeight)
	allowAdmissionKeepAlive(ctx)
	return context.WithValue(ctx, admissionHeldContextKey{}, lease.root), finish(lease.root.releaseFunc()), nil
}

func setExecutionRequestShapeMetadata(meta map[string]any, rawJSON []byte, options modelExecutionOptions) {
	if options.complexity != nil {
		if options.complexityValid {
			options.complexity.applyDimensions(refineComplexityDimensions(options.complexityDimensions, requestPathMetadata(meta)))
			setRequestShapeAndToolMetadataFromComplexity(meta, *options.complexity)
		}
		return
	}
	setRequestShapeAndToolMetadata(meta, rawJSON)
}

// AdmissionReady reports whether the shared execution pool has avoided sustained saturation.
func (h *BaseAPIHandler) AdmissionReady() bool {
	if h == nil {
		return true
	}
	controller := h.admission.Load()
	return controller == nil || controller.ready()
}

// AdmissionSnapshot returns a low-cardinality snapshot of the shared execution pool.
func (h *BaseAPIHandler) AdmissionSnapshot() AdmissionSnapshot {
	if h == nil {
		return AdmissionSnapshot{}
	}
	controller := h.admission.Load()
	if controller == nil {
		return AdmissionSnapshot{}
	}
	return controller.metricsSnapshot()
}

func admissionErrorMessage(err error) *interfaces.ErrorMessage {
	status := statusFromError(err)
	if status <= 0 {
		status = http.StatusServiceUnavailable
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusRequestTimeout
		}
	}
	message := &interfaces.ErrorMessage{StatusCode: status, Error: err}
	if errors.Is(err, errAdmissionQueueFull) || errors.Is(err, errAdmissionWaitTimeout) {
		message.Addon = http.Header{"Retry-After": {strconv.Itoa(defaultAdmissionRetryAfter)}}
	}
	return message
}

// IsAdmissionError reports failures produced while reserving request capacity.
func IsAdmissionError(err error) bool {
	return errors.Is(err, errAdmissionQueueFull) || errors.Is(err, errAdmissionWaitTimeout) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func writeAdmissionHTTPError(c *gin.Context, err error) {
	if c == nil {
		return
	}
	message := admissionErrorMessage(err)
	for key, values := range message.Addon {
		for _, value := range values {
			c.Writer.Header().Add(key, value)
		}
	}
	code := "admission_rejected"
	if message.StatusCode == http.StatusRequestTimeout {
		code = "request_canceled"
	}
	c.JSON(message.StatusCode, ErrorResponse{Error: ErrorDetail{
		Message: "Server is busy; retry later",
		Type:    "server_error",
		Code:    code,
	}})
}

func ceilAdmissionUnits(value, unit int) int {
	if value <= 0 || unit <= 0 {
		return 0
	}
	return 1 + (value-1)/unit
}

func (c *admissionController) acquire(ctx context.Context, weight int) (func(), error) {
	release, _, _, err := c.acquireTracked(ctx, weight)
	return release, err
}

func (c *admissionController) acquireUpgrade(ctx context.Context, heldWeight, requestedWeight int) (int, error) {
	if c == nil {
		return 0, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	c.mu.Lock()
	if !c.enabled {
		c.mu.Unlock()
		return 0, nil
	}
	if requestedWeight == heldWeight {
		c.mu.Unlock()
		return 0, nil
	}
	if requestedWeight > c.capacity {
		c.observeRejectLocked(errAdmissionQueueFull)
		c.mu.Unlock()
		return 0, errAdmissionQueueFull
	}
	weight := requestedWeight - heldWeight
	if weight > 0 && c.activeWeight+weight > c.capacity {
		c.observeRejectLocked(errAdmissionQueueFull)
		c.mu.Unlock()
		return 0, errAdmissionQueueFull
	}
	c.resizeAssignedLocked(heldWeight, requestedWeight)
	if weight < 0 {
		c.dispatchLocked()
	}
	c.updateSaturationLocked()
	c.mu.Unlock()
	return weight, nil
}

func (c *admissionController) acquireImmediate(ctx context.Context, requestedWeight int) (func(), int, bool, error) {
	if c == nil {
		return func() {}, 0, false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, false, err
	}
	c.mu.Lock()
	if !c.enabled {
		c.mu.Unlock()
		return func() {}, 0, false, nil
	}
	weight := c.normalizeWeightLocked(requestedWeight)
	if c.queueDepthLocked() > 0 || c.activeWeight+weight > c.capacity {
		c.observeRejectLocked(errAdmissionQueueFull)
		c.updateSaturationLocked()
		c.mu.Unlock()
		return nil, 0, false, errAdmissionQueueFull
	}
	c.assignWeightLocked(weight)
	c.updateSaturationLocked()
	c.mu.Unlock()
	return c.releaseFunc(weight), weight, true, nil
}

func (c *admissionController) acquireTracked(ctx context.Context, requestedWeight int) (func(), int, bool, error) {
	if c == nil {
		return func() {}, 0, false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, false, err
	}
	c.mu.Lock()
	if !c.enabled {
		c.mu.Unlock()
		return func() {}, 0, false, nil
	}
	weight := c.normalizeWeightLocked(requestedWeight)
	if c.queueDepthLocked() == 0 && c.activeWeight+weight <= c.capacity {
		c.assignWeightLocked(weight)
		c.updateSaturationLocked()
		c.mu.Unlock()
		return c.releaseFunc(weight), weight, true, nil
	}
	if c.queueDepthLocked() >= c.maxQueue {
		c.updateSaturationLocked()
		c.observeRejectLocked(errAdmissionQueueFull)
		c.mu.Unlock()
		return nil, 0, false, errAdmissionQueueFull
	}

	waiter := &admissionWaiter{
		ready:           make(chan struct{}),
		requestedWeight: requestedWeight,
		weight:          weight,
		enqueuedAt:      time.Now(),
	}
	waiter.deadline = waiter.enqueuedAt.Add(c.maxWait)
	bucket := admissionBucket(weight)
	c.waiters[bucket] = append(c.waiters[bucket], waiter)
	c.dispatchLocked()
	c.updateSaturationLocked()
	c.mu.Unlock()

	return c.waitForAdmission(ctx, waiter)
}

func (c *admissionController) waitForAdmission(ctx context.Context, waiter *admissionWaiter) (func(), int, bool, error) {
	remaining := time.Until(waiter.deadline)
	if remaining < 0 {
		remaining = 0
	}
	timer := time.NewTimer(remaining)
	select {
	case <-waiter.ready:
		stopAdmissionTimer(timer)
		c.observeWait(time.Since(waiter.enqueuedAt))
		if err := ctx.Err(); err != nil {
			if waiter.admitted {
				c.release(waiter.weight)
			}
			return nil, 0, false, err
		}
		if !waiter.admitted {
			return func() {}, 0, false, nil
		}
		return c.releaseFunc(waiter.weight), waiter.weight, true, nil
	case <-ctx.Done():
		stopAdmissionTimer(timer)
		c.mu.Lock()
		c.observeWaitLocked(time.Since(waiter.enqueuedAt))
		c.cancelWaiterLocked(waiter)
		c.mu.Unlock()
		return nil, 0, false, ctx.Err()
	case <-timer.C:
		c.mu.Lock()
		if err := ctx.Err(); err != nil {
			c.observeWaitLocked(time.Since(waiter.enqueuedAt))
			c.cancelWaiterLocked(waiter)
			c.mu.Unlock()
			return nil, 0, false, err
		}
		c.observeWaitLocked(time.Since(waiter.enqueuedAt))
		if c.removeWaiterLocked(waiter) {
			c.observeRejectLocked(errAdmissionWaitTimeout)
			c.dispatchLocked()
			c.updateSaturationLocked()
			c.mu.Unlock()
			return nil, 0, false, errAdmissionWaitTimeout
		}
		if waiter.admitted {
			c.releaseAssignedLocked(waiter.weight)
			c.observeRejectLocked(errAdmissionWaitTimeout)
			c.mu.Unlock()
			return nil, 0, false, errAdmissionWaitTimeout
		}
		c.mu.Unlock()
		return func() {}, 0, false, nil
	}
}

func (c *admissionController) cancelWaiterLocked(waiter *admissionWaiter) {
	if !c.removeWaiterLocked(waiter) {
		if waiter.admitted {
			c.releaseAssignedLocked(waiter.weight)
		}
		return
	}
	c.dispatchLocked()
	c.updateSaturationLocked()
}

func stopAdmissionTimer(timer *time.Timer) {
	if timer == nil || timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}

func (c *admissionController) normalizeWeightLocked(weight int) int {
	if weight <= 0 {
		return 1
	}
	if weight > c.capacity {
		return c.capacity
	}
	return weight
}

func (c *admissionController) normalizedWeight(weight int) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.normalizeWeightLocked(weight)
}

func (c *admissionController) updateSettings(enabled bool, capacity, maxQueue int, maxWait, saturationGrace time.Duration) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.enabled = enabled
	if !enabled {
		c.releaseWaitersLocked()
		c.updateSaturationLocked()
		return
	}
	next := newAdmissionController(capacity, maxQueue, saturationGrace)
	if maxWait > 0 {
		next.maxWait = maxWait
	}
	c.capacity = next.capacity
	c.maxQueue = next.maxQueue
	c.maxWait = next.maxWait
	c.saturationGrace = next.saturationGrace
	c.reweightWaitersLocked()
	c.dispatchLocked()
	c.updateSaturationLocked()
}

func admissionBucket(weight int) int {
	if weight <= lightAdmissionWeight {
		return 0
	}
	return 1
}

func (c *admissionController) releaseFunc(weight int) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			c.release(weight)
		})
	}
}

func (c *admissionController) release(weight int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.releaseAssignedLocked(weight)
	c.mu.Unlock()
}

func (c *admissionController) resize(heldWeight, requestedWeight int) {
	if c == nil || heldWeight == requestedWeight {
		return
	}
	c.mu.Lock()
	c.resizeAssignedLocked(heldWeight, requestedWeight)
	c.dispatchLocked()
	c.updateSaturationLocked()
	c.mu.Unlock()
}

func (c *admissionController) releaseAssignedLocked(weight int) {
	c.resizeAssignedLocked(weight, 0)
	c.dispatchLocked()
	c.updateSaturationLocked()
}

func (c *admissionController) resizeAssignedLocked(heldWeight, requestedWeight int) {
	if heldWeight == requestedWeight {
		return
	}
	c.activeWeight += requestedWeight - heldWeight
	if count := c.activeWeights[heldWeight]; count > 1 {
		c.activeWeights[heldWeight] = count - 1
	} else {
		delete(c.activeWeights, heldWeight)
	}
	if requestedWeight > 0 {
		if c.activeWeights == nil {
			c.activeWeights = make(map[int]int)
		}
		c.activeWeights[requestedWeight]++
	}
	if c.activeWeight < 0 {
		c.activeWeight = 0
	}
}

func (c *admissionController) dispatchLocked() {
	if !c.enabled {
		return
	}
	for c.activeWeight < c.capacity {
		available := c.capacity - c.activeWeight
		light := c.firstFittingLocked(0, available)
		heavy := c.firstFittingLocked(1, available)
		if light == nil && heavy == nil {
			return
		}
		heavyHead := c.firstWaiterLocked(1)
		now := time.Now()
		heavyAged := heavyHead != nil && now.Sub(heavyHead.enqueuedAt) >= heavyAdmissionAging

		bucket := 0
		switch {
		case heavy != nil && (c.preferHeavy || light == nil):
			bucket = 1
		case light != nil:
			if heavyHead != nil && heavy == nil && heavyAged && !c.lightFitsAgedHeavyCreditLocked(heavyHead.weight, light.weight) {
				c.preferHeavy = true
				return
			}
		default:
			return
		}

		waiter := c.waiters[bucket][0]
		c.waiters[bucket][0] = nil
		c.waiters[bucket] = c.waiters[bucket][1:]
		c.assignWeightLocked(waiter.weight)
		waiter.admitted = true
		if bucket == 1 {
			c.preferHeavy = false
		} else if len(c.waiters[1]) > 0 {
			c.preferHeavy = true
		}
		close(waiter.ready)
	}
}

func (c *admissionController) assignWeightLocked(weight int) {
	if c.activeWeights == nil {
		c.activeWeights = make(map[int]int)
	}
	c.activeWeight += weight
	c.activeWeights[weight]++
}

// lightFitsAgedHeavyCreditLocked admits only light weight that cannot delay the
// aged heavy beyond the largest single active lease that already blocks it.
func (c *admissionController) lightFitsAgedHeavyCreditLocked(heavyWeight, lightWeight int) bool {
	targetActiveWeight := c.capacity - heavyWeight
	if targetActiveWeight < 0 {
		targetActiveWeight = 0
	}
	largestBlocker := 0
	for weight, count := range c.activeWeights {
		if count > 0 && weight > targetActiveWeight && weight > largestBlocker {
			largestBlocker = weight
		}
	}
	nonBlockingWeight := c.activeWeight - largestBlocker
	return nonBlockingWeight+lightWeight <= targetActiveWeight
}

func (c *admissionController) firstWaiterLocked(bucket int) *admissionWaiter {
	if len(c.waiters[bucket]) == 0 {
		return nil
	}
	return c.waiters[bucket][0]
}

func (c *admissionController) firstFittingLocked(bucket, available int) *admissionWaiter {
	waiter := c.firstWaiterLocked(bucket)
	if waiter == nil {
		return nil
	}
	if waiter.weight > available {
		return nil
	}
	return waiter
}

func (c *admissionController) removeWaiterLocked(waiter *admissionWaiter) bool {
	for bucket := range c.waiters {
		for i, queued := range c.waiters[bucket] {
			if queued != waiter {
				continue
			}
			copy(c.waiters[bucket][i:], c.waiters[bucket][i+1:])
			c.waiters[bucket][len(c.waiters[bucket])-1] = nil
			c.waiters[bucket] = c.waiters[bucket][:len(c.waiters[bucket])-1]
			return true
		}
	}
	return false
}

func (c *admissionController) releaseWaitersLocked() {
	for bucket := range c.waiters {
		for _, waiter := range c.waiters[bucket] {
			close(waiter.ready)
		}
		c.waiters[bucket] = nil
	}
	c.preferHeavy = false
}

func (c *admissionController) reweightWaitersLocked() {
	queued := c.waiters
	c.waiters = [2][]*admissionWaiter{}
	c.preferHeavy = false
	for bucket := range queued {
		for _, waiter := range queued[bucket] {
			waiter.weight = c.normalizeWeightLocked(waiter.requestedWeight)
			nextBucket := admissionBucket(waiter.weight)
			c.waiters[nextBucket] = append(c.waiters[nextBucket], waiter)
		}
	}
}

func (c *admissionController) queueDepthLocked() int {
	return len(c.waiters[0]) + len(c.waiters[1])
}

func (c *admissionController) observeWait(wait time.Duration) {
	c.mu.Lock()
	c.observeWaitLocked(wait)
	c.mu.Unlock()
}

func (c *admissionController) observeWaitLocked(wait time.Duration) {
	switch {
	case wait < 10*time.Millisecond:
		c.waitBuckets.LessThan10Milliseconds++
	case wait < 100*time.Millisecond:
		c.waitBuckets.LessThan100Milliseconds++
	case wait < time.Second:
		c.waitBuckets.LessThanOneSecond++
	case wait < 10*time.Second:
		c.waitBuckets.LessThanTenSeconds++
	default:
		c.waitBuckets.TenSecondsOrMore++
	}
}

func (c *admissionController) observeRejectLocked(cause error) {
	switch {
	case errors.Is(cause, errAdmissionQueueFull):
		c.rejects.QueueFull++
	case errors.Is(cause, errAdmissionWaitTimeout):
		c.rejects.WaitTimeout++
	}
}

func (c *admissionController) updateSaturationLocked() {
	if !c.enabled {
		c.saturatedSince = time.Time{}
		return
	}
	saturated := c.activeWeight >= c.capacity || c.queueDepthLocked() > 0
	if saturated {
		if c.saturatedSince.IsZero() {
			c.saturatedSince = c.currentTimeLocked()
		}
		return
	}
	c.saturatedSince = time.Time{}
}

func (c *admissionController) currentTimeLocked() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *admissionController) ready() bool {
	if c == nil {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.enabled {
		return true
	}
	if c.saturatedSince.IsZero() {
		return true
	}
	return c.currentTimeLocked().Sub(c.saturatedSince) < c.saturationGrace
}

func (c *admissionController) snapshot() (activeWeight, queueDepth int) {
	snapshot := c.metricsSnapshot()
	return snapshot.ActiveWeight, snapshot.QueueDepth
}

func (c *admissionController) metricsSnapshot() AdmissionSnapshot {
	if c == nil {
		return AdmissionSnapshot{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return AdmissionSnapshot{
		Enabled:             c.enabled,
		ActiveWeight:        c.activeWeight,
		QueueDepth:          c.queueDepthLocked(),
		WaitDurationBuckets: c.waitBuckets,
		Rejects:             c.rejects,
	}
}
