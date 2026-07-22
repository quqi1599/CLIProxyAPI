package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestAdmissionWaitTimeoutReturnsRetryable503AndKeepsActiveLease(t *testing.T) {
	controller := newAdmissionController(1, 1, time.Second)
	controller.maxWait = 10 * time.Millisecond

	releaseActive, err := controller.acquire(context.Background(), 1)
	if err != nil {
		t.Fatalf("active acquire error: %v", err)
	}
	releaseQueued, errQueued := controller.acquire(context.Background(), 1)
	if releaseQueued != nil {
		t.Fatal("timed-out waiter received a release function")
	}
	if !errors.Is(errQueued, errAdmissionWaitTimeout) {
		t.Fatalf("queued acquire error = %v, want %v", errQueued, errAdmissionWaitTimeout)
	}

	errMsg := admissionErrorMessage(errQueued)
	if errMsg.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", errMsg.StatusCode)
	}
	if got := errMsg.Addon.Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
	snapshot := controller.metricsSnapshot()
	if snapshot.ActiveWeight != 1 || snapshot.QueueDepth != 0 {
		t.Fatalf("snapshot = active:%d queued:%d, want 1/0", snapshot.ActiveWeight, snapshot.QueueDepth)
	}
	if snapshot.Rejects.WaitTimeout != 1 || snapshot.Rejects.QueueFull != 0 {
		t.Fatalf("reject counters = %+v, want one wait timeout", snapshot.Rejects)
	}
	if admissionWaitObservationCount(snapshot.WaitDurationBuckets) != 1 {
		t.Fatalf("wait buckets = %+v, want one observation", snapshot.WaitDurationBuckets)
	}

	releaseActive()
}

func TestAdmissionQueueFullReturnsRetryAfterAndCountsFixedReason(t *testing.T) {
	controller := newAdmissionController(1, 1, time.Second)
	controller.maxWait = time.Second
	releaseActive, err := controller.acquire(context.Background(), 1)
	if err != nil {
		t.Fatalf("active acquire error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	waitDone := make(chan admissionAcquireResult, 1)
	go func() {
		release, errAcquire := controller.acquire(ctx, 1)
		waitDone <- admissionAcquireResult{release: release, err: errAcquire}
	}()
	waitAdmissionQueueDepth(t, controller, 1)

	if _, errFull := controller.acquire(context.Background(), 1); !errors.Is(errFull, errAdmissionQueueFull) {
		t.Fatalf("queue-full error = %v, want %v", errFull, errAdmissionQueueFull)
	} else if got := admissionErrorMessage(errFull).Addon.Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
	snapshot := controller.metricsSnapshot()
	if snapshot.Rejects.QueueFull != 1 || snapshot.Rejects.WaitTimeout != 0 {
		t.Fatalf("reject counters = %+v, want one queue-full rejection", snapshot.Rejects)
	}

	cancel()
	cancelled := waitAdmissionAcquire(t, waitDone)
	if !errors.Is(cancelled.err, context.Canceled) {
		t.Fatalf("cancelled waiter error = %v, want context canceled", cancelled.err)
	}
	if got := controller.metricsSnapshot().Rejects; got != snapshot.Rejects {
		t.Fatalf("cancellation changed reject counters from %+v to %+v", snapshot.Rejects, got)
	}
	releaseActive()
}

func TestAdmissionDisableWakesExistingWaiterWithoutRejection(t *testing.T) {
	controller := newAdmissionController(1, 1, time.Second)
	controller.maxWait = time.Second
	releaseActive, err := controller.acquire(context.Background(), 1)
	if err != nil {
		t.Fatalf("active acquire error: %v", err)
	}

	waitDone := make(chan admissionAcquireResult, 1)
	go func() {
		release, errAcquire := controller.acquire(context.Background(), 1)
		waitDone <- admissionAcquireResult{release: release, err: errAcquire}
	}()
	waitAdmissionQueueDepth(t, controller, 1)
	controller.updateSettings(false, 0, 0, 0, 0)

	bypassed := waitAdmissionAcquire(t, waitDone)
	if bypassed.err != nil || bypassed.release == nil {
		t.Fatalf("disabled waiter result = release:%v err:%v", bypassed.release != nil, bypassed.err)
	}
	bypassed.release()
	snapshot := controller.metricsSnapshot()
	if snapshot.Enabled || snapshot.QueueDepth != 0 || snapshot.Rejects != (AdmissionRejectCounters{}) {
		t.Fatalf("disabled snapshot = %+v", snapshot)
	}
	releaseActive()
}

func TestAdmissionSnapshotUsesFixedWaitBucketsAndSurvivesHotReload(t *testing.T) {
	controller := newAdmissionController(2, 2, time.Second)
	controller.mu.Lock()
	for _, wait := range []time.Duration{time.Millisecond, 20 * time.Millisecond, 200 * time.Millisecond, 2 * time.Second, 20 * time.Second} {
		controller.observeWaitLocked(wait)
	}
	controller.rejects.QueueFull = 2
	controller.rejects.WaitTimeout = 3
	controller.mu.Unlock()

	before := controller.metricsSnapshot()
	wantBuckets := AdmissionWaitDurationBuckets{
		LessThan10Milliseconds:  1,
		LessThan100Milliseconds: 1,
		LessThanOneSecond:       1,
		LessThanTenSeconds:      1,
		TenSecondsOrMore:        1,
	}
	if before.WaitDurationBuckets != wantBuckets {
		t.Fatalf("wait buckets = %+v, want one observation per bucket", before.WaitDurationBuckets)
	}
	controller.updateSettings(true, 4, 3, 2*time.Second, time.Second)
	after := controller.metricsSnapshot()
	if after.WaitDurationBuckets != before.WaitDurationBuckets || after.Rejects != before.Rejects {
		t.Fatalf("hot reload reset counters: before=%+v after=%+v", before, after)
	}
}

func TestAdmissionClientDeadlineIsNotReportedAsQueueRetry(t *testing.T) {
	errMsg := admissionErrorMessage(context.DeadlineExceeded)
	if errMsg.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("status = %d, want 408", errMsg.StatusCode)
	}
	if got := errMsg.Addon.Get("Retry-After"); got != "" {
		t.Fatalf("client deadline Retry-After = %q, want empty", got)
	}
}

func TestAdmissionCancelWinsWhenQueueTimerIsAlsoReady(t *testing.T) {
	for range 100 {
		controller := newAdmissionController(1, 1, time.Second)
		waiter := &admissionWaiter{
			ready:           make(chan struct{}),
			requestedWeight: 1,
			weight:          1,
			enqueuedAt:      time.Now().Add(-defaultAdmissionMaxWait),
		}
		controller.waiters[0] = append(controller.waiters[0], waiter)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		release, _, admitted, err := controller.waitForAdmission(ctx, waiter)
		if release != nil || admitted || !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel/timer race result = release:%v admitted:%v err:%v", release != nil, admitted, err)
		}
		snapshot := controller.metricsSnapshot()
		if snapshot.QueueDepth != 0 || snapshot.Rejects != (AdmissionRejectCounters{}) {
			t.Fatalf("cancel/timer race snapshot = %+v", snapshot)
		}
	}
}

func TestAdmissionTimeoutDoesNotLetNonStreamingKeepAliveCommit200(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	cfg := admissionWaitConfig(2, 1, 0)
	cfg.NonStreamKeepAliveInterval = 1
	handler := NewBaseAPIHandlers(cfg, nil)
	controller := handler.admission.Load()
	controller.maxWait = 1200 * time.Millisecond
	releaseActive, err := controller.acquire(context.Background(), 2)
	if err != nil {
		t.Fatalf("active acquire error: %v", err)
	}
	defer releaseActive()

	ctx := context.WithValue(context.Background(), "gin", c)
	stopKeepAlive := handler.StartNonStreamingKeepAlive(c, ctx)
	_, release, errAdmission := handler.inspectAndAcquireAdmission(ctx, []byte(`{"messages":[]}`), &modelExecutionOptions{})
	stopKeepAlive()
	if release != nil || !errors.Is(errAdmission, errAdmissionWaitTimeout) {
		t.Fatalf("queued admission = release:%v err:%v", release != nil, errAdmission)
	}
	if c.Writer.Written() {
		t.Fatal("non-streaming keepalive committed a response before admission completed")
	}

	handler.WriteErrorResponse(c, admissionErrorMessage(errAdmission))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	if got := recorder.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
}

func TestAdmissionSuccessOpensNonStreamingKeepAliveGate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	cfg := admissionWaitConfig(4, 1, 0)
	cfg.NonStreamKeepAliveInterval = 1
	handler := NewBaseAPIHandlers(cfg, nil)
	ctx := context.WithValue(context.Background(), "gin", c)
	stopKeepAlive := handler.StartNonStreamingKeepAlive(c, ctx)
	defer stopKeepAlive()

	_, release, err := handler.inspectAndAcquireAdmission(ctx, []byte(`{"messages":[]}`), &modelExecutionOptions{})
	if err != nil {
		t.Fatalf("admission error: %v", err)
	}
	defer release()
	value, exists := c.Get(admissionKeepAliveGateKey)
	gate, _ := value.(*admissionKeepAliveGate)
	if !exists || gate == nil {
		t.Fatal("non-streaming keepalive gate is missing")
	}
	select {
	case <-gate.ready:
	default:
		t.Fatal("successful admission did not open the non-streaming keepalive gate")
	}
}

func TestAdmissionConfigDefaultsAndHotReloadKeepControllerGeneration(t *testing.T) {
	handler := NewBaseAPIHandlers(admissionWaitConfig(4, 2, 0), nil)
	controller := handler.admission.Load()
	if controller == nil {
		t.Fatal("enabled controller is nil")
	}
	if controller.maxWait != defaultAdmissionMaxWait {
		t.Fatalf("default max-wait = %s", controller.maxWait)
	}

	handler.UpdateClients(admissionWaitConfig(8, 3, 12))
	if current := handler.admission.Load(); current != controller {
		t.Fatal("hot reload replaced the admission controller generation")
	}
	if controller.maxWait != 12*time.Second {
		t.Fatalf("hot-reloaded max-wait = %s", controller.maxWait)
	}
}

func admissionWaitConfig(capacity, maxQueue, maxWaitSeconds int) *sdkconfig.SDKConfig {
	return &sdkconfig.SDKConfig{RequestGuards: sdkconfig.RequestGuardsConfig{
		GlobalAdmission: sdkconfig.GlobalAdmissionConfig{
			Enabled:                true,
			Capacity:               capacity,
			MaxQueue:               maxQueue,
			MaxWaitSeconds:         maxWaitSeconds,
			SaturationGraceSeconds: 1,
		},
	}}
}

func admissionWaitObservationCount(buckets AdmissionWaitDurationBuckets) uint64 {
	return buckets.LessThan10Milliseconds + buckets.LessThan100Milliseconds + buckets.LessThanOneSecond + buckets.LessThanTenSeconds + buckets.TenSecondsOrMore
}
