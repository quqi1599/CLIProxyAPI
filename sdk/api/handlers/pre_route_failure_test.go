package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestProviderResolutionBadGatewayLogsSafePreRouteCorrelation(t *testing.T) {
	const (
		requestID       = "cpa-502-test"
		clientRequestID = "oneapi-502-test"
		model           = "missing-provider-observability-model"
		secret          = "secret-prompt-must-not-appear"
	)
	rawJSON := []byte(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":%q}],"tools":[{"type":"function","function":{"name":"lookup","description":%q,"parameters":{"type":"object"}}}],"reasoning_effort":"high"}`, model, secret, secret))

	hook := installPreRouteFailureTestHook(t)
	ctx := preRouteFailureTestContext(t, requestID, clientRequestID, "/v1/chat/completions")
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)

	_, _, errMsg := handler.ExecuteWithAuthManager(ctx, "openai", model, rawJSON, "")
	if errMsg == nil || errMsg.StatusCode != http.StatusBadGateway {
		t.Fatalf("ExecuteWithAuthManager() error = %+v, want 502", errMsg)
	}

	entry := findPreRouteFailureEntry(t, hook)
	wantDigest := sha256.Sum256(rawJSON)
	wantFields := map[string]any{
		"request_id":          requestID,
		"client_request_id":   clientRequestID,
		"routing_phase":       "pre_route",
		"failure_class":       "pre_route_bad_gateway",
		"failure_kind":        preRouteFailureKindProviderResolution,
		"failure_scope":       "request",
		"status_code":         http.StatusBadGateway,
		"error_code":          preRouteErrorProviderNotResolved,
		"attempt_count":       0,
		"endpoint_method":     http.MethodPost,
		"endpoint_path":       "/v1/chat/completions",
		"request_path":        "/v1/chat/completions",
		"source_format":       "openai",
		"request_stream":      false,
		"requested_model":     model,
		"client_profile":      "workbuddy",
		"payload_bytes":       len(rawJSON),
		"payload_sha256":      hex.EncodeToString(wantDigest[:]),
		"message_count":       1,
		"tool_count":          1,
		"declared_tool_count": 1,
		"reasoning_effort":    "high",
	}
	for field, want := range wantFields {
		if got := entry.Data[field]; got != want {
			t.Fatalf("field %s = %#v, want %#v; fields=%#v", field, got, want, entry.Data)
		}
	}
	if strings.Contains(fmt.Sprint(entry.Data), secret) || strings.Contains(entry.Message, secret) {
		t.Fatalf("pre-route log leaked request content: %#v", entry.Data)
	}
}

func TestProviderResolutionBadGatewayLogsStreamingPreRoutePhase(t *testing.T) {
	hook := installPreRouteFailureTestHook(t)
	ctx := preRouteFailureTestContext(t, "cpa-stream-502", "oneapi-stream-502", "/v1/chat/completions")
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	rawJSON := []byte(`{"model":"missing-stream-provider","stream":true,"messages":[{"role":"user","content":"hello"}]}`)

	_, _, errChan := handler.ExecuteStreamWithAuthManager(ctx, "openai", "missing-stream-provider", rawJSON, "")
	errMsg, ok := <-errChan
	if !ok || errMsg == nil || errMsg.StatusCode != http.StatusBadGateway {
		t.Fatalf("stream error = %+v, %v; want 502", errMsg, ok)
	}

	entry := findPreRouteFailureEntry(t, hook)
	if entry.Data["request_stream"] != true || entry.Data["error_code"] != preRouteErrorProviderNotResolved {
		t.Fatalf("unexpected stream pre-route fields: %#v", entry.Data)
	}
}

func TestPluginHostBadGatewayLogsSeparatePreRouteClassification(t *testing.T) {
	const model = "plugin-routed-model"
	hook := installPreRouteFailureTestHook(t)
	ctx := preRouteFailureTestContext(t, "cpa-plugin-502", "oneapi-plugin-502", "/v1/chat/completions")
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	handler.SetModelRouterHost(&handlerRouterOnlyTestHost{
		hasRouters: true,
		route: func(context.Context, pluginapi.ModelRouteRequest) (pluginapi.ModelRouteResponse, bool) {
			return pluginapi.ModelRouteResponse{
				Handled:    true,
				TargetKind: pluginapi.ModelRouteTargetExecutor,
				Target:     "safe-plugin-id",
			}, true
		},
	})

	_, _, errMsg := handler.ExecuteWithAuthManager(ctx, "openai", model, []byte(`{"model":"plugin-routed-model"}`), "")
	if errMsg == nil || errMsg.StatusCode != http.StatusBadGateway {
		t.Fatalf("ExecuteWithAuthManager() error = %+v, want 502", errMsg)
	}

	entry := findPreRouteFailureEntry(t, hook)
	if entry.Data["failure_kind"] != preRouteFailureKindPluginExecutor ||
		entry.Data["error_code"] != preRouteErrorPluginHostUnavailable ||
		entry.Data["plugin_id"] != "safe-plugin-id" {
		t.Fatalf("unexpected plugin pre-route fields: %#v", entry.Data)
	}
}

func TestPreRouteFailureHashesUnsafeIdentifiersAndSkipsNon502(t *testing.T) {
	hook := installPreRouteFailureTestHook(t)
	ctx := logging.WithRequestID(context.Background(), "safe-request-id")
	unsafeModel := "model\nsecret value"

	logPreRouteFailure(ctx, preRouteFailureLog{
		StatusCode:     http.StatusBadGateway,
		FailureKind:    preRouteFailureKindProviderResolution,
		ErrorCode:      preRouteErrorProviderNotResolved,
		RequestedModel: unsafeModel,
	})
	entry := findPreRouteFailureEntry(t, hook)
	if _, exists := entry.Data["requested_model"]; exists {
		t.Fatalf("unsafe model was logged verbatim: %#v", entry.Data)
	}
	modelHash, hasModelHash := entry.Data["requested_model_hash"]
	if !hasModelHash || strings.TrimSpace(fmt.Sprint(modelHash)) == "" || entry.Data["requested_model_bytes"] != len(unsafeModel) {
		t.Fatalf("unsafe model correlation metadata missing: %#v", entry.Data)
	}
	if strings.Contains(fmt.Sprint(entry.Data), "secret value") {
		t.Fatalf("unsafe model content leaked: %#v", entry.Data)
	}

	hook.Reset()
	logPreRouteFailure(ctx, preRouteFailureLog{StatusCode: http.StatusBadRequest})
	if entries := hook.AllEntries(); len(entries) != 0 {
		t.Fatalf("non-502 emitted pre-route log: %#v", entries)
	}
}

func installPreRouteFailureTestHook(t *testing.T) *logtest.Hook {
	t.Helper()
	hook := logtest.NewGlobal()
	hook.Reset()
	previousLevel := log.GetLevel()
	log.SetLevel(log.WarnLevel)
	t.Cleanup(func() {
		log.SetLevel(previousLevel)
		hook.Reset()
	})
	return hook
}

func preRouteFailureTestContext(t *testing.T, requestID, clientRequestID, path string) context.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, path, nil)
	ginCtx.Request.Header.Set("X-App-Name", "WorkBuddy")

	ctx := logging.WithRequestID(context.Background(), requestID)
	ctx = logging.WithClientRequestID(ctx, clientRequestID)
	ctx = logging.WithEndpointParts(ctx, http.MethodPost, path)
	return context.WithValue(ctx, "gin", ginCtx)
}

func findPreRouteFailureEntry(t *testing.T, hook *logtest.Hook) *log.Entry {
	t.Helper()
	for _, entry := range hook.AllEntries() {
		if entry.Data["event"] == preRouteFailureEvent {
			return entry
		}
	}
	t.Fatalf("pre-route failure log not found: %#v", hook.AllEntries())
	return nil
}
