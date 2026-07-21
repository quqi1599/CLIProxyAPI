package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type admissionSaturationHost struct{}

func (admissionSaturationHost) HasModelRouters() bool { return true }

func (admissionSaturationHost) RouteModel(context.Context, pluginapi.ModelRouteRequest) (pluginapi.ModelRouteResponse, bool) {
	return pluginapi.ModelRouteResponse{
		Handled:    true,
		TargetKind: pluginapi.ModelRouteTargetExecutor,
		Target:     "blocked-stream",
	}, true
}

func (admissionSaturationHost) ExecutePluginExecutor(context.Context, string, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, nil
}

func (admissionSaturationHost) ExecutePluginExecutorStream(context.Context, string, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return &coreexecutor.StreamResult{Chunks: make(chan coreexecutor.StreamChunk)}, nil
}

func (admissionSaturationHost) CountPluginExecutor(context.Context, string, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, nil
}

func TestAdmissionSaturationOnlyFailsReadiness(t *testing.T) {
	server := newTestServer(t)
	server.handlers.UpdateClients(&sdkconfig.SDKConfig{RequestGuards: sdkconfig.RequestGuardsConfig{
		GlobalAdmission: sdkconfig.GlobalAdmissionConfig{
			Enabled:                true,
			Capacity:               128,
			MaxQueue:               64,
			SaturationGraceSeconds: 1,
		},
	}})
	server.handlers.SetModelRouterHost(admissionSaturationHost{})
	server.ready.Store(true)

	activeCtx, cancelActive := context.WithCancel(context.Background())
	activeData, _, _ := server.handlers.ExecuteStreamWithAuthManager(activeCtx, "openai", "test-model", admissionCapacityRequestBody(), "")
	if activeData == nil {
		cancelActive()
		t.Fatal("capacity-filling stream did not start")
	}

	const queuedRequests = 65
	queueFull := make(chan struct{}, 1)
	cancels := make([]context.CancelFunc, 0, queuedRequests)
	var waiters sync.WaitGroup
	for range queuedRequests {
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		waiters.Add(1)
		go func() {
			defer waiters.Done()
			_, _, errs := server.handlers.ExecuteStreamWithAuthManager(ctx, "openai", "test-model", []byte(`{"messages":[{"role":"user","content":"queued"}]}`), "")
			for errMsg := range errs {
				if errMsg != nil && errMsg.StatusCode == http.StatusServiceUnavailable {
					select {
					case queueFull <- struct{}{}:
					default:
					}
				}
			}
		}()
	}

	cleanup := func() {
		for _, cancel := range cancels {
			cancel()
		}
		cancelActive()
		waiters.Wait()
	}
	t.Cleanup(cleanup)

	select {
	case <-queueFull:
	case <-time.After(2 * time.Second):
		t.Fatal("admission queue did not reach its bound")
	}
	time.Sleep(1100 * time.Millisecond)

	assertHealthCode := func(path string, want int) {
		t.Helper()
		recorder := httptest.NewRecorder()
		server.engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != want {
			t.Fatalf("%s status = %d, want %d; body=%s", path, recorder.Code, want, recorder.Body.String())
		}
	}
	assertHealthCode("/readyz", http.StatusServiceUnavailable)
	assertHealthCode("/livez", http.StatusOK)
	assertHealthCode("/healthz", http.StatusOK)
}

func admissionCapacityRequestBody() []byte {
	var body strings.Builder
	body.Grow(32_768 * 16)
	body.WriteString(`{"messages":[`)
	for i := range 32_768 {
		if i > 0 {
			body.WriteByte(',')
		}
		body.WriteString(`{"role":"user"}`)
	}
	body.WriteString(`]}`)
	return []byte(body.String())
}
