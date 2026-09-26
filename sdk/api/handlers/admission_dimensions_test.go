package handlers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func dimensionTestHandler() *BaseAPIHandler {
	cfg := &config.SDKConfig{}
	cfg.RequestGuards.GlobalAdmission = config.GlobalAdmissionConfig{Enabled: true, Capacity: 8, ReadCapacity: 4, BodyCapacityBytes: 64 << 20, TransformCapacity: 16, TransformMaxQueue: 2, TransformMaxWaitMilliseconds: 100}
	return NewBaseAPIHandlers(cfg, nil)
}

func TestAdmissionDimensionsDoNotChargeExecutionDuringRead(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprint(unknown), func(t *testing.T) {
			h := dimensionTestHandler()
			// Fill execution capacity: ingress reading must still proceed independently.
			release, err := h.admission.Load().acquire(context.Background(), 8)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			body := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
			engine := gin.New()
			engine.Use(h.PreAuthIngressAdmissionMiddleware(), h.IngressAdmissionMiddleware())
			engine.POST("/v1/chat/completions", func(c *gin.Context) {
				if _, err := ReadRequestBody(c); err != nil {
					t.Fatal(err)
				}
				s := h.AdmissionSnapshot()
				if s.Body.Active != len(body) || s.Execution.Active != 8 || s.Read.Active != 0 || s.Transform.Active != 0 {
					t.Fatalf("mixed resources: %+v", s)
				}
				c.Status(204)
			})
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
			if unknown {
				request.ContentLength = -1
			}
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, request)
			if w.Code != 204 {
				t.Fatalf("status %d", w.Code)
			}
			s := h.AdmissionSnapshot()
			if s.Body.Active != 0 || s.Read.Active != 0 {
				t.Fatalf("leaked leases: %+v", s)
			}
		})
	}
}

func TestBodyCapacityRejectsKnownInputBeforeReadAndDoesNotClamp(t *testing.T) {
	h := dimensionTestHandler()
	h.bodyAdmission.Load().updateSettings(true, 4, 1, time.Second, time.Second)
	reader := &countingRequestReader{data: strings.NewReader(`{"x":1}`)}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", reader)
	req.ContentLength = 7
	engine := gin.New()
	engine.Use(h.PreAuthIngressAdmissionMiddleware())
	engine.POST("/v1/chat/completions", func(c *gin.Context) { t.Error("oversize reached handler") })
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != 503 || reader.reads != 0 {
		t.Fatalf("status=%d reads=%d", w.Code, reader.reads)
	}
	if _, _, err := h.acquireExecutionBody(context.Background(), 5); !errors.Is(err, errAdmissionQueueFull) {
		t.Fatalf("SDK body was clamped: %v", err)
	}
	if s := h.AdmissionSnapshot(); s.Execution.Active != 0 || s.Body.Active != 0 {
		t.Fatalf("leaked resources: %+v", s)
	}
}

func TestLargeRequestConcurrencyAndSlowTransformFeedback(t *testing.T) {
	h := dimensionTestHandler()
	if small, large := executionAdmissionWeight(complexityVector{DecodedBytes: 16 << 20}), executionAdmissionWeight(complexityVector{DecodedBytes: 16<<20 + 1}); small != 1 || large != 4 {
		t.Fatalf("boundary %d/%d", small, large)
	}
	finish, err := h.AcquireTransform(context.Background(), 17<<20)
	if err != nil {
		t.Fatal(err)
	}
	s := h.AdmissionSnapshot()
	if s.Transform.Active != 8 || s.Execution.Active != 0 || s.Body.Active != 0 {
		t.Fatalf("transform charged other pools: %+v", s)
	}
	finish(2 * time.Second)
	finish(2 * time.Second)
	finish, err = h.AcquireTransform(context.Background(), 17<<20)
	if err != nil {
		t.Fatal(err)
	}
	if s = h.AdmissionSnapshot(); s.Transform.Active != 16 {
		t.Fatalf("slow size bucket did not reduce concurrency: %+v", s)
	}
	finish(0)
	small, err := h.AcquireTransform(context.Background(), 8<<10)
	if err != nil {
		t.Fatal(err)
	}
	if s = h.AdmissionSnapshot(); s.Transform.Active != 1 {
		t.Fatalf("large feedback penalized small requests: %+v", s)
	}
	small(0)
}

func TestTransformQueueBudgetCancellationAndNoNetworkDeadline(t *testing.T) {
	h := dimensionTestHandler()
	c := h.transformAdmission.Load()
	c.updateSettings(true, 1, 1, 20*time.Millisecond, time.Second)
	held, err := h.AcquireTransform(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(h.withTransformAdmission(context.Background()))
	done := make(chan error, 1)
	go func() { _, err := h.AcquireTransform(ctx, 100); done <- err }()
	waitAdmissionQueueDepth(t, c, 1)
	if _, err := h.AcquireTransform(context.Background(), 100); !errors.Is(err, errAdmissionQueueFull) {
		t.Fatalf("want full transform queue: %v", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	budget := &admissionQueueBudget{left: time.Millisecond}
	ctx = context.WithValue(context.Background(), admissionQueueBudgetKey{}, budget)
	if _, err := h.AcquireTransform(ctx, 100); !errors.Is(err, errAdmissionWaitTimeout) {
		t.Fatalf("want queue budget rejection: %v", err)
	}
	if budget.remaining() != 0 {
		t.Fatal("queue wait was not charged")
	}
	held(0)
	admitted, release, err := internalpayload.BeginTransformScope(h.withTransformAdmission(context.Background()), 100)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if _, ok := admitted.Deadline(); ok {
		t.Fatal("admission imposed a network deadline")
	}
	if s := h.AdmissionSnapshot(); s.Transform.Active != 0 || s.Transform.QueueDepth != 0 || s.Transform.Rejects.WaitTimeout != 1 {
		t.Fatalf("bad counters: %+v", s)
	}
}

func TestDimensionsHotReloadPreservesLeases(t *testing.T) {
	h := dimensionTestHandler()
	oldBody, oldTransform := h.bodyAdmission.Load(), h.transformAdmission.Load()
	ctx, releaseBody, err := h.acquireExecutionBody(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	finish, err := h.AcquireTransform(ctx, 17<<20)
	if err != nil {
		t.Fatal(err)
	}
	h.updateAdmissionController(&config.SDKConfig{RequestGuards: config.RequestGuardsConfig{GlobalAdmission: config.GlobalAdmissionConfig{Enabled: true, Capacity: 2, BodyCapacityBytes: 4, TransformCapacity: 2}}})
	if h.bodyAdmission.Load() != oldBody || h.transformAdmission.Load() != oldTransform {
		t.Fatal("hot reload replaced live pool")
	}
	if _, _, err := h.acquireExecutionBody(context.Background(), 5); !errors.Is(err, errAdmissionQueueFull) {
		t.Fatalf("shrunk body pool bypassed: %v", err)
	}
	finish(0)
	releaseBody()
	if s := h.AdmissionSnapshot(); s.Transform.Active != 0 || s.Body.Active != 0 {
		t.Fatalf("reload leaked capacity: %+v", s)
	}
	h.updateAdmissionController(nil)
	if s := h.AdmissionSnapshot(); s.Body.Enabled || s.Transform.Enabled || s.Execution.Enabled || s.Read.Enabled {
		t.Fatalf("disable incomplete: %+v", s)
	}
}

func TestIndependentExecutionAndBodyLeasesSurviveNestedStreamLifetime(t *testing.T) {
	h := dimensionTestHandler()
	body := []byte(`{"messages":[]}`)
	ctx, outer, err := h.inspectAndAcquireAdmission(context.Background(), body, &modelExecutionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, child, err := h.inspectAndAcquireAdmission(ctx, body, &modelExecutionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	outer()
	s := h.AdmissionSnapshot()
	if s.Execution.Active != 1 || s.Body.Active != len(body) {
		t.Fatalf("child lost capacity: %+v", s)
	}
	child()
	child()
	if s = h.AdmissionSnapshot(); s.Execution.Active != 0 || s.Body.Active != 0 {
		t.Fatalf("child leaked: %+v", s)
	}
}

func BenchmarkAdmissionDimensions(b *testing.B) {
	for _, size := range []int{8 << 10, 16 << 20, 17 << 20, 64 << 20} {
		for _, enabled := range []bool{false, true} {
			b.Run(fmt.Sprintf("bytes=%d/enabled=%t", size, enabled), func(b *testing.B) {
				h := dimensionTestHandler()
				if !enabled {
					h.updateAdmissionController(nil)
				}
				ctx := h.withTransformAdmission(context.Background())
				b.ReportAllocs()
				b.ResetTimer()
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						_, release, err := internalpayload.BeginTransformScope(ctx, int64(size))
						if err != nil {
							b.Error(err)
							return
						}
						release()
					}
				})
			})
		}
	}
}

func TestConcurrentBodyLeasesNeverExceedBudget(t *testing.T) {
	h := dimensionTestHandler()
	h.bodyAdmission.Load().updateSettings(true, 128, 1, time.Second, time.Second)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				_, release, err := h.acquireExecutionBody(context.Background(), 32)
				if err == nil {
					if active, _ := h.bodyAdmission.Load().snapshot(); active > 128 {
						t.Errorf("bytes oversubscribed: %d", active)
					}
					release()
				} else if !errors.Is(err, errAdmissionQueueFull) {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	if active, _ := h.bodyAdmission.Load().snapshot(); active != 0 {
		t.Fatalf("leaked %d bytes", active)
	}
}

func TestAdmittedWaiterCannotRunAfterQueueDeadline(t *testing.T) {
	c := newAdmissionController(1, 1, time.Second)
	_, err := c.acquire(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	close(ready)
	waiter := &admissionWaiter{ready: ready, admitted: true, weight: 1, enqueuedAt: time.Now().Add(-time.Second), deadline: time.Now().Add(-time.Millisecond)}
	if _, _, _, err := c.waitForAdmission(context.Background(), waiter); !errors.Is(err, errAdmissionWaitTimeout) {
		t.Fatalf("expired admission ran: %v", err)
	}
	if active, _ := c.snapshot(); active != 0 {
		t.Fatal("expired assignment leaked")
	}
}

func TestQueueBudgetSurvivesHandlerContextBridge(t *testing.T) {
	h := dimensionTestHandler()
	requestCtx := h.withTransformAdmission(context.Background())
	budget := admissionQueueBudgetFromContext(requestCtx)
	budget.consume(7 * time.Second)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(requestCtx)
	ctx, cancel := h.GetContextWithCancel(nil, c, context.Background())
	defer cancel()
	ctx = h.withTransformAdmission(ctx)
	if got := admissionQueueBudgetFromContext(ctx); got != budget || got.remaining() != 23*time.Second {
		t.Fatal("handler reset the ingress queue budget")
	}
}
