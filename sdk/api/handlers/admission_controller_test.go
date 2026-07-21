package handlers

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestComplexityAdmissionWeightChargesLargeRequestsMore(t *testing.T) {
	small := complexityAdmissionWeight(complexityVector{
		BodyBytes:        20 << 10,
		MessageCount:     1,
		ContentPartCount: 1,
	})
	large := complexityAdmissionWeight(complexityVector{
		BodyBytes:        20 << 20,
		MessageCount:     513,
		ContentPartCount: 1025,
		toolShapeTelemetry: toolShapeTelemetry{
			DeclaredToolCount: 65,
			InteractionCount:  129,
		},
	})
	if large <= small {
		t.Fatalf("large request weight = %d, want greater than small request weight %d", large, small)
	}
}

func TestAdmissionControllerWeightedCapacityAndExactlyOnceRelease(t *testing.T) {
	controller := newAdmissionController(4, 2, time.Second)
	releaseFirst, err := controller.acquire(context.Background(), 3)
	if err != nil {
		t.Fatalf("first acquire error: %v", err)
	}

	secondDone := make(chan admissionAcquireResult, 1)
	go func() {
		release, errAcquire := controller.acquire(context.Background(), 2)
		secondDone <- admissionAcquireResult{release: release, err: errAcquire}
	}()
	waitAdmissionQueueDepth(t, controller, 1)

	releaseFirst()
	releaseFirst()
	second := waitAdmissionAcquire(t, secondDone)
	if second.err != nil {
		t.Fatalf("second acquire error: %v", second.err)
	}
	if active, queued := controller.snapshot(); active != 2 || queued != 0 {
		t.Fatalf("snapshot after handoff = active:%d queued:%d, want 2/0", active, queued)
	}
	second.release()
	second.release()
	if active, queued := controller.snapshot(); active != 0 || queued != 0 {
		t.Fatalf("snapshot after repeated release = active:%d queued:%d, want 0/0", active, queued)
	}
}

func TestAdmissionControllerCancellationRemovesQueuedRequest(t *testing.T) {
	controller := newAdmissionController(1, 1, time.Second)
	releaseFirst, err := controller.acquire(context.Background(), 1)
	if err != nil {
		t.Fatalf("first acquire error: %v", err)
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
	}
	cancel()
	cancelled := waitAdmissionAcquire(t, waitDone)
	if !errors.Is(cancelled.err, context.Canceled) {
		t.Fatalf("cancelled acquire error = %v, want context canceled", cancelled.err)
	}
	if cancelled.release != nil {
		t.Fatal("cancelled queued request received a release function")
	}

	releaseFirst()
	releaseNext, errNext := controller.acquire(context.Background(), 1)
	if errNext != nil {
		t.Fatalf("acquire after cancellation error: %v", errNext)
	}
	releaseNext()
}

func TestAdmissionControllerSizeBucketsAvoidLightAndHeavyStarvation(t *testing.T) {
	controller := newAdmissionController(6, 4, time.Second)
	releaseActive, err := controller.acquire(context.Background(), 4)
	if err != nil {
		t.Fatalf("active acquire error: %v", err)
	}

	heavyDone := make(chan admissionAcquireResult, 1)
	go func() {
		release, errAcquire := controller.acquire(context.Background(), 5)
		heavyDone <- admissionAcquireResult{release: release, err: errAcquire}
	}()
	waitAdmissionQueueDepth(t, controller, 1)

	lightDone := make(chan admissionAcquireResult, 1)
	go func() {
		release, errAcquire := controller.acquire(context.Background(), 1)
		lightDone <- admissionAcquireResult{release: release, err: errAcquire}
	}()
	light := waitAdmissionAcquire(t, lightDone)
	if light.err != nil {
		t.Fatalf("light acquire error: %v", light.err)
	}
	select {
	case heavy := <-heavyDone:
		if heavy.release != nil {
			heavy.release()
		}
		t.Fatal("heavy request acquired before enough capacity was available")
	default:
	}

	light.release()
	releaseActive()
	heavy := waitAdmissionAcquire(t, heavyDone)
	if heavy.err != nil {
		t.Fatalf("heavy acquire error: %v", heavy.err)
	}
	heavy.release()
}

func TestAdmissionControllerStaysWorkConservingBeforeHeavyAges(t *testing.T) {
	controller := newAdmissionController(10, 4, time.Second)
	releaseActive, err := controller.acquire(context.Background(), 6)
	if err != nil {
		t.Fatalf("active acquire error: %v", err)
	}

	heavyCtx, cancelHeavy := context.WithCancel(context.Background())
	defer cancelHeavy()
	heavyDone := make(chan admissionAcquireResult, 1)
	go func() {
		release, errAcquire := controller.acquire(heavyCtx, 7)
		heavyDone <- admissionAcquireResult{release: release, err: errAcquire}
	}()
	waitAdmissionQueueDepth(t, controller, 1)

	for i := 0; i < 2; i++ {
		lightDone := make(chan admissionAcquireResult, 1)
		go func() {
			release, errAcquire := controller.acquire(context.Background(), 1)
			lightDone <- admissionAcquireResult{release: release, err: errAcquire}
		}()
		light := waitAdmissionAcquire(t, lightDone)
		if light.err != nil {
			t.Fatalf("light acquire %d error: %v", i, light.err)
		}
		light.release()
	}

	cancelHeavy()
	heavy := waitAdmissionAcquire(t, heavyDone)
	if !errors.Is(heavy.err, context.Canceled) {
		t.Fatalf("heavy acquire error = %v, want context canceled", heavy.err)
	}
	releaseActive()
}

func TestAdmissionControllerAgedHeavyReservesCapacityUnderLightBacklog(t *testing.T) {
	controller := newAdmissionController(3, 16, time.Second)
	activeReleases := make([]func(), 0, 3)
	for range 3 {
		release, err := controller.acquire(context.Background(), 1)
		if err != nil {
			t.Fatalf("active acquire error: %v", err)
		}
		activeReleases = append(activeReleases, release)
	}

	heavyDone := make(chan admissionAcquireResult, 1)
	go func() {
		release, errAcquire := controller.acquire(context.Background(), 3)
		heavyDone <- admissionAcquireResult{release: release, err: errAcquire}
	}()
	waitAdmissionQueueDepth(t, controller, 1)
	time.Sleep(heavyAdmissionAging + 10*time.Millisecond)

	const lightRequests = 8
	lightDone := make(chan admissionAcquireResult, lightRequests)
	for range lightRequests {
		go func() {
			release, errAcquire := controller.acquire(context.Background(), 1)
			lightDone <- admissionAcquireResult{release: release, err: errAcquire}
		}()
	}
	waitAdmissionQueueDepth(t, controller, 1+lightRequests)
	for _, release := range activeReleases {
		release()
	}

	heavy := waitAdmissionAcquire(t, heavyDone)
	if heavy.err != nil {
		t.Fatalf("aged heavy acquire error: %v", heavy.err)
	}
	select {
	case light := <-lightDone:
		if light.release != nil {
			light.release()
		}
		t.Fatal("light request acquired before reserved heavy request")
	default:
	}
	heavy.release()
	for range lightRequests {
		light := waitAdmissionAcquire(t, lightDone)
		if light.err != nil {
			t.Fatalf("light acquire after heavy release error: %v", light.err)
		}
		light.release()
	}
}

func TestAdmissionControllerAgedHeavyUsesCapacityAroundUnavoidableBlocker(t *testing.T) {
	controller := newAdmissionController(10, 8, time.Second)
	releaseActive, err := controller.acquire(context.Background(), 6)
	if err != nil {
		t.Fatalf("active acquire error: %v", err)
	}
	heavyDone := make(chan admissionAcquireResult, 1)
	go func() {
		release, errAcquire := controller.acquire(context.Background(), 7)
		heavyDone <- admissionAcquireResult{release: release, err: errAcquire}
	}()
	waitAdmissionQueueDepth(t, controller, 1)
	time.Sleep(heavyAdmissionAging + 10*time.Millisecond)

	lightDone := make(chan admissionAcquireResult, 3)
	lightReleases := make([]func(), 0, 3)
	for range 3 {
		go func() {
			release, errAcquire := controller.acquire(context.Background(), 1)
			lightDone <- admissionAcquireResult{release: release, err: errAcquire}
		}()
	}
	for range 3 {
		light := waitAdmissionAcquire(t, lightDone)
		if light.err != nil {
			t.Fatalf("light acquire alongside blocker error: %v", light.err)
		}
		lightReleases = append(lightReleases, light.release)
	}

	fourthLightDone := make(chan admissionAcquireResult, 1)
	go func() {
		release, errAcquire := controller.acquire(context.Background(), 1)
		fourthLightDone <- admissionAcquireResult{release: release, err: errAcquire}
	}()
	waitAdmissionQueueDepth(t, controller, 2)
	releaseActive()
	heavy := waitAdmissionAcquire(t, heavyDone)
	if heavy.err != nil {
		t.Fatalf("heavy acquire after blocker release error: %v", heavy.err)
	}
	select {
	case light := <-fourthLightDone:
		if light.release != nil {
			light.release()
		}
		t.Fatal("reservation admitted more light weight than the heavy-safe target")
	default:
	}
	heavy.release()
	fourthLight := waitAdmissionAcquire(t, fourthLightDone)
	if fourthLight.err != nil {
		t.Fatalf("fourth light acquire error: %v", fourthLight.err)
	}
	fourthLight.release()
	for _, release := range lightReleases {
		release()
	}
}

func TestAdmissionControllerReadinessTracksSustainedSaturation(t *testing.T) {
	controller := newAdmissionController(1, 1, 0)
	releaseActive, err := controller.acquire(context.Background(), 1)
	if err != nil {
		t.Fatalf("acquire error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	waitDone := make(chan admissionAcquireResult, 1)
	go func() {
		release, errAcquire := controller.acquire(ctx, 1)
		waitDone <- admissionAcquireResult{release: release, err: errAcquire}
	}()
	waitAdmissionQueueDepth(t, controller, 1)
	if controller.ready() {
		t.Fatal("controller should not be ready after saturation grace elapses")
	}
	handler := NewBaseAPIHandlers(nil, nil)
	handler.admission.Store(controller)
	if handler.AdmissionReady() {
		t.Fatal("handler readiness should reflect admission saturation")
	}
	cancel()
	cancelled := waitAdmissionAcquire(t, waitDone)
	if !errors.Is(cancelled.err, context.Canceled) {
		t.Fatalf("cancelled acquire error = %v, want context canceled", cancelled.err)
	}
	releaseActive()
	if !handler.AdmissionReady() {
		t.Fatal("handler readiness should recover after weight is released")
	}
}

func TestAdmissionStreamProducerReleasesWeightOnCancellation(t *testing.T) {
	handler := NewBaseAPIHandlers(admissionTestConfig(4, 2, 1), nil)
	controller := handler.admission.Load()
	host := &handlerStuckPluginStreamHost{}
	host.hasRouters = true
	host.route = func(context.Context, pluginapi.ModelRouteRequest) (pluginapi.ModelRouteResponse, bool) {
		return pluginapi.ModelRouteResponse{Handled: true, TargetKind: pluginapi.ModelRouteTargetExecutor, Target: "stuck"}, true
	}
	handler.SetModelRouterHost(host)
	ctx, cancel := context.WithCancel(context.Background())
	data, _, errs := handler.ExecuteStreamWithAuthManager(ctx, "openai", "test-model", []byte(`{"messages":[{"role":"user","content":"test"}]}`), "")
	if data == nil || errs == nil {
		cancel()
		t.Fatal("stream channels must be non-nil after startup")
	}
	cancel()
	waitAdmissionChannelClosed(t, data)
	waitAdmissionChannelClosed(t, errs)
	waitAdmissionSnapshot(t, controller, 0, 0)
}

func TestNestedExecutionReusesAdmissionLease(t *testing.T) {
	handler := NewBaseAPIHandlers(nil, nil)
	controller := newAdmissionController(1, 1, time.Second)
	handler.admission.Store(controller)
	ctx, releaseOuter, err := handler.inspectAndAcquireAdmission(context.Background(), []byte(`{"messages":[]}`), &modelExecutionOptions{})
	if err != nil {
		t.Fatalf("outer acquire error: %v", err)
	}
	_, releaseNested, errNested := handler.inspectAndAcquireAdmission(ctx, []byte(`{"messages":[{"role":"user","content":"nested"}]}`), &modelExecutionOptions{})
	if errNested != nil {
		t.Fatalf("nested acquire error: %v", errNested)
	}
	if active, queued := controller.snapshot(); active != 1 || queued != 0 {
		t.Fatalf("nested snapshot = active:%d queued:%d, want 1/0", active, queued)
	}
	releaseOuter()
	if active, queued := controller.snapshot(); active != 1 || queued != 0 {
		t.Fatalf("snapshot after outer release = active:%d queued:%d, want retained 1/0", active, queued)
	}
	releaseNested()
	releaseNested()
	if active, queued := controller.snapshot(); active != 0 || queued != 0 {
		t.Fatalf("released snapshot = active:%d queued:%d, want 0/0", active, queued)
	}
}

func TestNestedExecutionUpgradesAdmissionLeaseWithoutBypass(t *testing.T) {
	handler := NewBaseAPIHandlers(nil, nil)
	controller := newAdmissionController(5, 2, time.Second)
	handler.admission.Store(controller)
	outerBody := []byte(`{"messages":[]}`)
	nestedBody := admissionRequestWithMessages(257)
	innerBody := admissionRequestWithMessages(513)
	outerVector, _ := inspectRequestComplexity(outerBody)
	nestedVector, _ := inspectRequestComplexity(nestedBody)
	innerVector, _ := inspectRequestComplexity(innerBody)
	outerWeight := complexityAdmissionWeight(outerVector)
	nestedWeight := complexityAdmissionWeight(nestedVector)
	innerWeight := complexityAdmissionWeight(innerVector)
	if !(outerWeight < nestedWeight && nestedWeight < innerWeight && innerWeight == controller.capacity) {
		t.Fatalf("unexpected test weights: outer=%d nested=%d inner=%d capacity=%d", outerWeight, nestedWeight, innerWeight, controller.capacity)
	}

	ctx, releaseOuter, err := handler.inspectAndAcquireAdmission(context.Background(), outerBody, &modelExecutionOptions{})
	if err != nil {
		t.Fatalf("outer acquire error: %v", err)
	}
	releaseBlocker, err := controller.acquire(context.Background(), 2)
	if err != nil {
		t.Fatalf("blocker acquire error: %v", err)
	}
	_, releaseBlocked, errBlocked := handler.inspectAndAcquireAdmission(ctx, nestedBody, &modelExecutionOptions{})
	if !errors.Is(errBlocked, errAdmissionQueueFull) {
		t.Fatalf("blocked nested upgrade error = %v, want retryable admission error", errBlocked)
	}
	if releaseBlocked != nil {
		t.Fatal("blocked nested upgrade returned a release function")
	}
	if active, queued := controller.snapshot(); active != outerWeight+2 || queued != 0 {
		t.Fatalf("blocked nested snapshot = %d/%d, want %d/0", active, queued, outerWeight+2)
	}
	releaseBlocker()

	nestedCtx, releaseNested, errNested := handler.inspectAndAcquireAdmission(ctx, nestedBody, &modelExecutionOptions{})
	if errNested != nil {
		t.Fatalf("nested acquire error: %v", errNested)
	}
	if active, queued := controller.snapshot(); active != nestedWeight || queued != 0 {
		t.Fatalf("nested snapshot = active:%d queued:%d, want %d/0", active, queued, nestedWeight)
	}

	_, releaseInner, errInner := handler.inspectAndAcquireAdmission(nestedCtx, innerBody, &modelExecutionOptions{})
	if errInner != nil {
		t.Fatalf("inner acquire error: %v", errInner)
	}
	if active, queued := controller.snapshot(); active != innerWeight || queued != 0 {
		t.Fatalf("inner snapshot = active:%d queued:%d, want %d/0", active, queued, innerWeight)
	}
	releaseInner()
	releaseOuter()
	if active, queued := controller.snapshot(); active != innerWeight || queued != 0 {
		t.Fatalf("retained snapshot = active:%d queued:%d, want %d/0", active, queued, innerWeight)
	}
	releaseNested()
	if active, queued := controller.snapshot(); active != 0 || queued != 0 {
		t.Fatalf("released snapshot = active:%d queued:%d, want 0/0", active, queued)
	}
}

func TestNestedAdmissionUpgradeCancellationKeepsOuterLease(t *testing.T) {
	handler := NewBaseAPIHandlers(nil, nil)
	controller := newAdmissionController(5, 2, time.Second)
	handler.admission.Store(controller)
	outerBody := admissionRequestWithMessages(0)
	nestedBody := admissionRequestWithMessages(257)
	outerVector, _ := inspectRequestComplexity(outerBody)
	outerWeight := complexityAdmissionWeight(outerVector)
	ctx, releaseOuter, err := handler.inspectAndAcquireAdmission(context.Background(), outerBody, &modelExecutionOptions{})
	if err != nil {
		t.Fatalf("outer acquire error: %v", err)
	}
	releaseBlocker, err := controller.acquire(context.Background(), controller.capacity-outerWeight)
	if err != nil {
		t.Fatalf("blocker acquire error: %v", err)
	}

	nestedCtx, cancelNested := context.WithCancel(ctx)
	cancelNested()
	_, releaseNested, errNested := handler.inspectAndAcquireAdmission(nestedCtx, nestedBody, &modelExecutionOptions{})
	if !errors.Is(errNested, context.Canceled) {
		t.Fatalf("nested upgrade error = %v, want context canceled", errNested)
	}
	if releaseNested != nil {
		t.Fatal("canceled nested upgrade returned a release function")
	}
	if active, queued := controller.snapshot(); active != controller.capacity || queued != 0 {
		t.Fatalf("snapshot after canceled upgrade = %d/%d, want %d/0", active, queued, controller.capacity)
	}
	releaseBlocker()
	releaseOuter()
	if active, queued := controller.snapshot(); active != 0 || queued != 0 {
		t.Fatalf("released snapshot = %d/%d, want 0/0", active, queued)
	}
}

func TestConcurrentNestedAdmissionUpgradesFailWithoutDeadlock(t *testing.T) {
	handler := NewBaseAPIHandlers(nil, nil)
	controller := newAdmissionController(4, 2, time.Second)
	handler.admission.Store(controller)
	outerBody := admissionRequestWithMessages(0)
	nestedBody := admissionRequestWithMessages(257)
	firstCtx, releaseFirst, err := handler.inspectAndAcquireAdmission(context.Background(), outerBody, &modelExecutionOptions{})
	if err != nil {
		t.Fatalf("first outer acquire error: %v", err)
	}
	secondCtx, releaseSecond, err := handler.inspectAndAcquireAdmission(context.Background(), outerBody, &modelExecutionOptions{})
	if err != nil {
		t.Fatalf("second outer acquire error: %v", err)
	}

	start := make(chan struct{})
	results := make(chan admissionAcquireResult, 2)
	for _, ctx := range []context.Context{firstCtx, secondCtx} {
		go func(upgradeCtx context.Context) {
			<-start
			_, release, errUpgrade := handler.inspectAndAcquireAdmission(upgradeCtx, nestedBody, &modelExecutionOptions{})
			results <- admissionAcquireResult{release: release, err: errUpgrade}
		}(ctx)
	}
	close(start)
	for range 2 {
		result := waitAdmissionAcquire(t, results)
		if !errors.Is(result.err, errAdmissionQueueFull) {
			t.Fatalf("concurrent upgrade error = %v, want retryable admission error", result.err)
		}
		if result.release != nil {
			t.Fatal("blocked concurrent upgrade returned a release function")
		}
	}
	if active, queued := controller.snapshot(); active != controller.capacity || queued != 0 {
		t.Fatalf("snapshot after concurrent upgrades = %d/%d, want %d/0", active, queued, controller.capacity)
	}
	releaseFirst()
	releaseSecond()
	if active, queued := controller.snapshot(); active != 0 || queued != 0 {
		t.Fatalf("released snapshot = %d/%d, want 0/0", active, queued)
	}
}

func TestAdmissionConfigurationDefaultsDisabledAndHotReloads(t *testing.T) {
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	if controller := handler.admission.Load(); controller != nil {
		t.Fatal("admission controller must be disabled by default")
	}

	enabled := admissionTestConfig(0, 0, 0)
	handler.UpdateClients(enabled)
	first := handler.admission.Load()
	if first == nil {
		t.Fatal("enabled admission controller is nil")
	}
	if first.capacity != defaultAdmissionCapacity || first.maxQueue != defaultAdmissionQueueSize || first.saturationGrace != defaultAdmissionSaturationGrace {
		t.Fatalf("default settings = %d/%d/%s", first.capacity, first.maxQueue, first.saturationGrace)
	}

	sameSettings := admissionTestConfig(0, 0, 0)
	sameSettings.RequestLog = true
	handler.UpdateClients(sameSettings)
	if current := handler.admission.Load(); current != first {
		t.Fatal("unrelated config update replaced admission controller")
	}

	releaseOld, err := first.acquire(context.Background(), 1)
	if err != nil {
		t.Fatalf("old controller acquire error: %v", err)
	}
	handler.UpdateClients(admissionTestConfig(8, 3, 2))
	if current := handler.admission.Load(); current != first {
		t.Fatal("live settings update replaced the admission controller")
	}
	if first.capacity != 8 || first.maxQueue != 3 || first.saturationGrace != 2*time.Second {
		t.Fatalf("live settings update = %d/%d/%s, want 8/3/2s", first.capacity, first.maxQueue, first.saturationGrace)
	}
	handler.UpdateClients(&sdkconfig.SDKConfig{})
	if current := handler.admission.Load(); current != first {
		t.Fatal("live disable replaced the active admission controller")
	}
	if first.enabled {
		t.Fatal("live disable left the admission controller enabled")
	}
	releaseBypass, errBypass := first.acquire(context.Background(), 8)
	if errBypass != nil {
		t.Fatalf("disabled acquire error: %v", errBypass)
	}
	if active, queued := first.snapshot(); active != 1 || queued != 0 {
		t.Fatalf("disabled acquire snapshot = %d/%d, want existing 1/0", active, queued)
	}
	releaseBypass()
	releaseOld()

	handler.UpdateClients(admissionTestConfig(8, 3, 2))
	if current := handler.admission.Load(); current != first {
		t.Fatal("idle settings update replaced the admission controller")
	}
	if first.capacity != 8 || first.maxQueue != 3 || first.saturationGrace != 2*time.Second {
		t.Fatalf("idle settings update = %d/%d/%s, want 8/3/2s", first.capacity, first.maxQueue, first.saturationGrace)
	}
	if !first.enabled {
		t.Fatal("re-enable left the admission controller disabled")
	}

	handler.UpdateClients(&sdkconfig.SDKConfig{})
	if controller := handler.admission.Load(); controller != first || controller.enabled {
		t.Fatal("disabling admission did not preserve the disabled controller")
	}
}

func TestAdmissionHotReloadPreservesSaturatedPoolAndReadiness(t *testing.T) {
	handler := NewBaseAPIHandlers(admissionTestConfig(1, 1, 0), nil)
	controller := handler.admission.Load()
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
	controller.mu.Lock()
	controller.saturatedSince = time.Now().Add(-2 * time.Second)
	controller.mu.Unlock()
	handler.UpdateClients(admissionTestConfig(1, 1, 1))
	if handler.AdmissionReady() {
		t.Fatal("saturated handler unexpectedly ready before reload")
	}

	handler.UpdateClients(admissionTestConfig(1, 1, 1))
	if current := handler.admission.Load(); current != controller {
		t.Fatal("hot reload replaced the saturated admission pool")
	}
	if controller.capacity != 1 || controller.maxQueue != 1 || controller.saturationGrace != time.Second {
		t.Fatal("hot reload did not apply settings to the live admission pool")
	}
	if handler.AdmissionReady() {
		t.Fatal("hot reload masked saturated admission readiness")
	}

	handler.UpdateClients(admissionTestConfig(8, 8, 10))
	queued := waitAdmissionAcquire(t, waitDone)
	if queued.err != nil {
		t.Fatalf("queued acquire after capacity increase error: %v", queued.err)
	}
	if active, depth := controller.snapshot(); active != 2 || depth != 0 {
		t.Fatalf("resized controller snapshot = %d/%d, want 2/0", active, depth)
	}
	if !handler.AdmissionReady() {
		t.Fatal("handler did not recover readiness after its queue drained")
	}
	queued.release()
	releaseActive()
	cancel()
}

func TestAdmissionHotReloadReweightsQueuedRequestsOnShrink(t *testing.T) {
	handler := NewBaseAPIHandlers(admissionTestConfig(8, 2, 1), nil)
	controller := handler.admission.Load()
	releaseActive, err := controller.acquire(context.Background(), 8)
	if err != nil {
		t.Fatalf("active acquire error: %v", err)
	}
	waitDone := make(chan admissionAcquireResult, 1)
	go func() {
		release, errAcquire := controller.acquire(context.Background(), 8)
		waitDone <- admissionAcquireResult{release: release, err: errAcquire}
	}()
	waitAdmissionQueueDepth(t, controller, 1)

	handler.UpdateClients(admissionTestConfig(4, 2, 1))
	if current := handler.admission.Load(); current != controller {
		t.Fatal("capacity shrink replaced the admission controller")
	}
	if active, queued := controller.snapshot(); active != 8 || queued != 1 {
		t.Fatalf("snapshot after shrink = %d/%d, want 8/1", active, queued)
	}
	releaseActive()
	resized := waitAdmissionAcquire(t, waitDone)
	if resized.err != nil {
		t.Fatalf("resized queued acquire error: %v", resized.err)
	}
	if active, queued := controller.snapshot(); active != 4 || queued != 0 {
		t.Fatalf("resized queued snapshot = %d/%d, want 4/0", active, queued)
	}
	resized.release()

	handler.UpdateClients(admissionTestConfig(8, 2, 1))
	releaseActive, err = controller.acquire(context.Background(), 8)
	if err != nil {
		t.Fatalf("second active acquire error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	waitDone = make(chan admissionAcquireResult, 1)
	go func() {
		release, errAcquire := controller.acquire(ctx, 8)
		waitDone <- admissionAcquireResult{release: release, err: errAcquire}
	}()
	waitAdmissionQueueDepth(t, controller, 1)
	handler.UpdateClients(admissionTestConfig(4, 2, 1))
	cancel()
	cancelled := waitAdmissionAcquire(t, waitDone)
	if !errors.Is(cancelled.err, context.Canceled) {
		t.Fatalf("rebucketed cancellation error = %v, want context canceled", cancelled.err)
	}
	if active, queued := controller.snapshot(); active != 8 || queued != 0 {
		t.Fatalf("snapshot after rebucketed cancellation = %d/%d, want 8/0", active, queued)
	}
	releaseActive()
}

func TestAdmissionStreamPanicBeforeHandoffReleasesLease(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*BaseAPIHandler)
	}{
		{
			name: "router",
			setup: func(handler *BaseAPIHandler) {
				handler.SetModelRouterHost(&handlerRouterOnlyTestHost{
					hasRouters: true,
					route: func(context.Context, pluginapi.ModelRouteRequest) (pluginapi.ModelRouteResponse, bool) {
						panic("router panic")
					},
				})
			},
		},
		{
			name: "plugin executor",
			setup: func(handler *BaseAPIHandler) {
				host := &admissionPanicPluginStreamHost{}
				host.hasRouters = true
				host.route = func(context.Context, pluginapi.ModelRouteRequest) (pluginapi.ModelRouteResponse, bool) {
					return pluginapi.ModelRouteResponse{Handled: true, TargetKind: pluginapi.ModelRouteTargetExecutor, Target: "panic"}, true
				}
				handler.SetModelRouterHost(host)
			},
		},
		{
			name: "auth manager",
			setup: func(handler *BaseAPIHandler) {
				handler.SetModelRouterHost(&handlerRouterOnlyTestHost{
					hasRouters: true,
					route: func(context.Context, pluginapi.ModelRouteRequest) (pluginapi.ModelRouteResponse, bool) {
						return pluginapi.ModelRouteResponse{Handled: true, TargetKind: pluginapi.ModelRouteTargetProvider, Target: "openai"}, true
					},
				})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := NewBaseAPIHandlers(admissionTestConfig(4, 2, 1), nil)
			controller := handler.admission.Load()
			tt.setup(handler)
			func() {
				defer func() {
					if recovered := recover(); recovered == nil {
						t.Fatal("stream call did not panic")
					}
				}()
				handler.ExecuteStreamWithAuthManager(context.Background(), "openai", "test-model", []byte(`{"messages":[]}`), "")
			}()
			if active, queued := controller.snapshot(); active != 0 || queued != 0 {
				t.Fatalf("snapshot after panic = active:%d queued:%d, want 0/0", active, queued)
			}
		})
	}
}

type admissionPanicPluginStreamHost struct {
	handlerDirectExecutorRouteHost
}

func (*admissionPanicPluginStreamHost) ExecutePluginExecutorStream(context.Context, string, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	panic("plugin executor panic")
}

func TestEnabledAdmissionCancellationIsNotAnEmptyModelStream(t *testing.T) {
	handler := NewBaseAPIHandlers(admissionTestConfig(4, 2, 1), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, errMsg := handler.ExecuteModelStream(ctx, ModelExecutionRequest{
		EntryProtocol: "openai",
		ExitProtocol:  "openai",
		Model:         "test-model",
		Stream:        true,
		Body:          []byte(`{"messages":[]}`),
	})
	if errMsg == nil || !errors.Is(errMsg.Error, context.Canceled) {
		t.Fatalf("canceled model stream error = %+v, want context canceled", errMsg)
	}
}

type admissionAcquireResult struct {
	release func()
	err     error
}

func waitAdmissionAcquire(t *testing.T, results <-chan admissionAcquireResult) admissionAcquireResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for admission acquire")
		return admissionAcquireResult{}
	}
}

func waitAdmissionQueueDepth(t *testing.T, controller *admissionController, want int) {
	t.Helper()
	deadline := time.After(time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		_, depth := controller.snapshot()
		if depth == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("admission queue depth did not become %d", want)
		case <-ticker.C:
		}
	}
}

func waitAdmissionSnapshot(t *testing.T, controller *admissionController, wantActive, wantQueued int) {
	t.Helper()
	deadline := time.After(time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		active, queued := controller.snapshot()
		if active == wantActive && queued == wantQueued {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("admission snapshot did not become %d/%d", wantActive, wantQueued)
		case <-ticker.C:
		}
	}
}

func admissionTestConfig(capacity, maxQueue, saturationGraceSeconds int) *sdkconfig.SDKConfig {
	return &sdkconfig.SDKConfig{RequestGuards: sdkconfig.RequestGuardsConfig{
		GlobalAdmission: sdkconfig.GlobalAdmissionConfig{
			Enabled:                true,
			Capacity:               capacity,
			MaxQueue:               maxQueue,
			SaturationGraceSeconds: saturationGraceSeconds,
		},
	}}
}

func admissionRequestWithMessages(count int) []byte {
	if count <= 0 {
		return []byte(`{"messages":[]}`)
	}
	return []byte(`{"messages":[` + strings.Repeat(`{},`, count-1) + `{}` + `]}`)
}

func waitAdmissionChannelClosed[T any](t *testing.T, ch <-chan T) {
	t.Helper()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("channel produced a value, want closed")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for channel close")
	}
}
