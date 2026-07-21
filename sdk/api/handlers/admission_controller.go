package handlers

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

const (
	defaultAdmissionCapacity        = 128
	defaultAdmissionQueueSize       = 64
	defaultAdmissionSaturationGrace = 5 * time.Second
	lightAdmissionWeight            = 4
	heavyAdmissionAging             = 100 * time.Millisecond
)

var errAdmissionQueueFull = errors.New("request admission queue is full; retry later")

type admissionHeldContextKey struct{}

type admissionLease struct {
	mu          sync.Mutex
	controller  *admissionController
	references  int
	weight      int
	releasePool []func()
}

type admissionController struct {
	mu              sync.Mutex
	enabled         bool
	capacity        int
	maxQueue        int
	saturationGrace time.Duration
	activeWeight    int
	activeWeights   map[int]int
	waiters         [2][]*admissionWaiter
	preferHeavy     bool
	saturatedSince  time.Time
}

type admissionWaiter struct {
	ready           chan struct{}
	requestedWeight int
	weight          int
	admitted        bool
	enqueuedAt      time.Time
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
		saturationGrace: saturationGrace,
		activeWeights:   make(map[int]int),
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
	return newAdmissionController(settings.Capacity, settings.MaxQueue, grace)
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
		current.updateSettings(false, 0, 0, 0)
		return
	}
	current.updateSettings(true, next.capacity, next.maxQueue, next.saturationGrace)
}

func newAdmissionLease(controller *admissionController, weight int, releasePool func()) *admissionLease {
	return &admissionLease{
		controller:  controller,
		references:  1,
		weight:      weight,
		releasePool: []func(){releasePool},
	}
}

func (l *admissionLease) retain(ctx context.Context, controller *admissionController, weight int) (func(), bool, error) {
	if l == nil || controller == nil {
		return nil, false, nil
	}
	l.mu.Lock()
	if l.references == 0 || l.controller != controller {
		l.mu.Unlock()
		return nil, false, nil
	}
	l.references++
	releasePool, addedWeight, err := controller.acquireUpgrade(ctx, l.weight, weight)
	if err != nil {
		l.references--
		l.mu.Unlock()
		return nil, true, err
	}
	l.weight += addedWeight
	if addedWeight > 0 {
		l.releasePool = append(l.releasePool, releasePool)
	}
	l.mu.Unlock()
	return l.releaseFunc(), true, nil
}

func (l *admissionLease) releaseFunc() func() {
	var once sync.Once
	return func() {
		once.Do(l.releaseReference)
	}
}

func (l *admissionLease) releaseReference() {
	if l == nil {
		return
	}
	var releasePool []func()
	l.mu.Lock()
	if l.references > 0 {
		l.references--
		if l.references == 0 {
			releasePool = l.releasePool
			l.releasePool = nil
		}
	}
	l.mu.Unlock()
	for i := len(releasePool) - 1; i >= 0; i-- {
		releasePool[i]()
	}
}

func complexityAdmissionWeight(vector complexityVector) int {
	weight := 1
	weight += ceilAdmissionUnits(vector.BodyBytes, 2<<20)
	weight += ceilAdmissionUnits(vector.MessageCount, 256)
	weight += ceilAdmissionUnits(vector.ContentPartCount, 512)
	toolItems := vector.DeclaredToolCount
	if vector.InteractionCount > toolItems {
		toolItems = vector.InteractionCount
	}
	weight += ceilAdmissionUnits(toolItems, 64)
	return weight
}

func (h *BaseAPIHandler) inspectAndAcquireAdmission(ctx context.Context, rawJSON []byte, options *modelExecutionOptions) (context.Context, func(), error) {
	vector, valid := inspectRequestComplexity(rawJSON)
	if options != nil {
		options.complexity = &vector
		options.complexityValid = valid
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if h == nil {
		return ctx, func() {}, nil
	}
	controller := h.admission.Load()
	if controller == nil {
		return ctx, func() {}, nil
	}
	weight := complexityAdmissionWeight(vector)
	if held, _ := ctx.Value(admissionHeldContextKey{}).(*admissionLease); held != nil {
		if release, retained, err := held.retain(ctx, controller, weight); retained {
			return ctx, release, err
		}
	}
	releasePool, admittedWeight, admitted, err := controller.acquireTracked(ctx, weight)
	if err != nil {
		return ctx, nil, err
	}
	if !admitted {
		return ctx, func() {}, nil
	}
	lease := newAdmissionLease(controller, admittedWeight, releasePool)
	return context.WithValue(ctx, admissionHeldContextKey{}, lease), lease.releaseFunc(), nil
}

func setExecutionRequestShapeMetadata(meta map[string]any, rawJSON []byte, options modelExecutionOptions) {
	if options.complexity != nil {
		if options.complexityValid {
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

func admissionErrorMessage(err error) *interfaces.ErrorMessage {
	status := http.StatusServiceUnavailable
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		status = http.StatusRequestTimeout
	}
	return &interfaces.ErrorMessage{StatusCode: status, Error: err}
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

func (c *admissionController) acquireUpgrade(ctx context.Context, heldWeight, requestedWeight int) (func(), int, error) {
	if c == nil {
		return func() {}, 0, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	c.mu.Lock()
	if !c.enabled {
		c.mu.Unlock()
		return func() {}, 0, nil
	}
	weight := c.additionalWeightLocked(requestedWeight, heldWeight)
	if weight == 0 {
		c.mu.Unlock()
		return func() {}, 0, nil
	}
	if c.activeWeight+weight > c.capacity {
		c.mu.Unlock()
		return nil, 0, errAdmissionQueueFull
	}
	c.assignWeightLocked(weight)
	c.updateSaturationLocked()
	c.mu.Unlock()
	return c.releaseFunc(weight), weight, nil
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
		c.mu.Unlock()
		return nil, 0, false, errAdmissionQueueFull
	}

	waiter := &admissionWaiter{
		ready:           make(chan struct{}),
		requestedWeight: requestedWeight,
		weight:          weight,
		enqueuedAt:      time.Now(),
	}
	bucket := admissionBucket(weight)
	c.waiters[bucket] = append(c.waiters[bucket], waiter)
	c.dispatchLocked()
	c.updateSaturationLocked()
	c.mu.Unlock()

	select {
	case <-waiter.ready:
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
		c.mu.Lock()
		if !c.removeWaiterLocked(waiter) {
			if waiter.admitted {
				c.releaseAssignedLocked(waiter.weight)
			}
		} else {
			c.dispatchLocked()
			c.updateSaturationLocked()
		}
		c.mu.Unlock()
		return nil, 0, false, ctx.Err()
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

func (c *admissionController) additionalWeightLocked(requestedWeight, heldWeight int) int {
	targetWeight := c.normalizeWeightLocked(requestedWeight)
	if targetWeight <= heldWeight {
		return 0
	}
	return targetWeight - heldWeight
}

func (c *admissionController) updateSettings(enabled bool, capacity, maxQueue int, saturationGrace time.Duration) {
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
	c.capacity = next.capacity
	c.maxQueue = next.maxQueue
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

func (c *admissionController) releaseAssignedLocked(weight int) {
	c.activeWeight -= weight
	if count := c.activeWeights[weight]; count > 1 {
		c.activeWeights[weight] = count - 1
	} else {
		delete(c.activeWeights, weight)
	}
	if c.activeWeight < 0 {
		c.activeWeight = 0
	}
	c.dispatchLocked()
	c.updateSaturationLocked()
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

func (c *admissionController) updateSaturationLocked() {
	if !c.enabled {
		c.saturatedSince = time.Time{}
		return
	}
	saturated := c.queueDepthLocked() >= c.maxQueue
	if saturated {
		if c.saturatedSince.IsZero() {
			c.saturatedSince = time.Now()
		}
		return
	}
	c.saturatedSince = time.Time{}
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
	return time.Since(c.saturatedSince) < c.saturationGrace
}

func (c *admissionController) snapshot() (activeWeight, queueDepth int) {
	if c == nil {
		return 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.activeWeight, c.queueDepthLocked()
}
