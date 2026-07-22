package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestOpenAICompatExecutorMiniMaxImageGenerationUsesNativeEndpoint(t *testing.T) {
	var gotPath string
	var gotBody []byte
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"img_1","data":{"image_base64":["abc"]},"base_resp":{"status_code":0}}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("minimax", nil)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url":    server.URL + "/v1",
		"api_key":     "test-key",
		"compat_kind": "minimax",
	}}

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "image-01",
		Payload: []byte(`{"model":"client-alias","prompt":"draw","response_format":"base64"}`),
	}, cliproxyexecutor.Options{
		Alt:          openAICompatAltMiniMaxImageGeneration,
		SourceFormat: sdktranslator.FromString("openai"),
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if gotPath != "/v1/image_generation" {
		t.Fatalf("path = %q, want /v1/image_generation", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("Authorization = %q, want bearer key", gotAuth)
	}
	if got := gjson.GetBytes(gotBody, "model").String(); got != "image-01" {
		t.Fatalf("upstream model = %q, want image-01", got)
	}
	if got := gjson.GetBytes(resp.Payload, "data.0.b64_json").String(); got != "abc" {
		t.Fatalf("response b64_json = %q, want abc", got)
	}
}

func TestOpenAICompatExecutorMiniMaxImageGenerationRejectsOversizedErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(strings.Repeat("x", int(helps.DefaultUpstreamErrorBodyBytes)+1)))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("minimax", nil)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url":    server.URL,
		"api_key":     "test-key",
		"compat_kind": "minimax",
	}}
	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "image-01",
		Payload: []byte(`{"prompt":"draw"}`),
	}, cliproxyexecutor.Options{Alt: openAICompatAltMiniMaxImageGeneration, SourceFormat: sdktranslator.FromString("openai")})
	if err == nil {
		t.Fatal("Execute() error = nil, want bounded upstream failure")
	}
	typed, ok := failurecontract.As(err)
	if !ok || typed.Kind != failurecontract.UpstreamProtocolError || typed.Scope != failurecontract.ScopeProvider || typed.ProviderCode != "upstream_error_body_too_large" {
		t.Fatalf("failure = %#v, want upstream error body limit", typed)
	}
}

func TestBuildOpenAIImagesResponseFromMiniMaxURL(t *testing.T) {
	body := []byte(`{"id":"img_1","data":{"image_urls":["https://example.com/a.png"]},"base_resp":{"status_code":0}}`)

	out, err := buildOpenAIImagesResponseFromMiniMax(body)
	if err != nil {
		t.Fatalf("buildOpenAIImagesResponseFromMiniMax() error = %v", err)
	}
	if got := gjson.GetBytes(out, "data.0.url").String(); got != "https://example.com/a.png" {
		t.Fatalf("url = %q", got)
	}
}

func TestBuildOpenAIImagesResponseFromMiniMaxBuildsAllItemsOnce(t *testing.T) {
	body := []byte(`{"data":{"image_base64":["a","b"],"image_urls":["https://example.com/c.png"],"revised_prompt":"refined"},"base_resp":{"status_code":0}}`)

	out, err := buildOpenAIImagesResponseFromMiniMax(body)
	if err != nil {
		t.Fatalf("buildOpenAIImagesResponseFromMiniMax() error = %v", err)
	}
	items := gjson.GetBytes(out, "data").Array()
	if len(items) != 3 {
		t.Fatalf("data length = %d, want 3; body=%s", len(items), out)
	}
	for idx, item := range items {
		if got := item.Get("revised_prompt").String(); got != "refined" {
			t.Fatalf("data.%d.revised_prompt = %q", idx, got)
		}
	}
}

func BenchmarkPayloadGrowthMiniMaxImageResponse(b *testing.B) {
	images := make([]string, 256)
	for idx := range images {
		images[idx] = strings.Repeat("A", 4090) + strconv.Itoa(idx)
	}
	body, errMarshal := json.Marshal(map[string]any{
		"data":      map[string]any{"image_base64": images, "revised_prompt": "refined"},
		"base_resp": map[string]any{"status_code": 0},
	})
	if errMarshal != nil {
		b.Fatal(errMarshal)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for idx := 0; idx < b.N; idx++ {
		if out, err := buildOpenAIImagesResponseFromMiniMax(body); err != nil || len(out) == 0 {
			b.Fatalf("build response: len=%d err=%v", len(out), err)
		}
	}
}

func TestBuildOpenAIImagesResponseFromMiniMaxLogicalError(t *testing.T) {
	secret := "sentinel-minimax-image-secret"
	body := []byte(`{"base_resp":{"status_code":1008,"status_msg":"insufficient balance ` + secret + `"}}`)

	_, err := buildOpenAIImagesResponseFromMiniMax(body)
	if err == nil {
		t.Fatalf("expected error")
	}
	status, ok := err.(statusErr)
	if !ok {
		t.Fatalf("error type = %T, want statusErr", err)
	}
	if status.StatusCode() != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", status.StatusCode())
	}
	if status.ErrorCode() != "minimax_1008" {
		t.Fatalf("error code = %q, want minimax_1008", status.ErrorCode())
	}
	if status.ProviderStatusCode() != http.StatusOK {
		t.Fatalf("provider status = %d, want 200", status.ProviderStatusCode())
	}
	got := status.Error()
	if strings.Contains(got, secret) || strings.Contains(got, "status_msg") {
		t.Fatalf("unsafe upstream body exposure: %s", got)
	}
	if !strings.Contains(got, "reason=usage limit") || !strings.Contains(got, `"bytes":`) || !strings.Contains(got, `"sha256":`) || !strings.Contains(got, `"content_type":"application/json"`) {
		t.Fatalf("missing safe classification or metadata: %s", got)
	}
}
