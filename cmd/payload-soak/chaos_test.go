package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestEvaluateHTTPScenarioAcceptsExpectedFailures(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		headers time.Duration
	}{
		{name: "slow_upstream", status: 200, body: soakUpstreamMarker, headers: minimumSlowHeaderLatency},
		{name: "http_half_close", status: 502, body: `{"error":"read failed"}`},
		{name: "http_reset", status: 500, body: `{"error":"transport failed"}`},
		{name: "rate_limit_429", status: 429, body: `{"error":"limited"}`},
		{name: "upstream_5xx", status: 503, body: `{"error":"unavailable"}`},
		{name: "bad_gzip", status: 502, body: `{"error":"decode failed"}`},
		{name: "malformed_sse", status: 200, body: `event: error\ndata: {"type":"error"}\n\n`},
		{name: "downstream_stream", status: 200, body: "data: {}\n\ndata: [DONE]\n\n"},
		{name: "client_mid_stream_cancel", status: 200, body: "data: first\n\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			passed, category, err := evaluateHTTPScenario(test.name, test.status, []byte(test.body), test.headers, nil)
			if !passed || err != nil || category == "" {
				t.Fatalf("evaluate = passed %v category %q error %v", passed, category, err)
			}
		})
	}
	passed, category, err := evaluateHTTPScenario("malformed_sse", http.StatusBadGateway, []byte(`{"error":"malformed"}`), 0, nil)
	if !passed || category != "stream_protocol_error" || err != nil {
		t.Fatalf("bootstrap malformed SSE = passed %v category %q error %v", passed, category, err)
	}
}

func TestValidateReleaseScenariosRequiresEverySuccessfulScenario(t *testing.T) {
	report := soakReport{ScenarioMatrixRuns: 2, Scenarios: make(map[string]scenarioResult, len(requiredReleaseScenarios))}
	for _, name := range requiredReleaseScenarios {
		report.Scenarios[name] = scenarioResult{Expected: expectedScenarioOutcome(name), Attempts: 2, Succeeded: 2}
	}
	if err := validateReleaseScenarios(report); err != nil {
		t.Fatalf("valid scenario report error = %v", err)
	}
	delete(report.Scenarios, "bad_gzip")
	if err := validateReleaseScenarios(report); err == nil || !strings.Contains(err.Error(), "bad_gzip") {
		t.Fatalf("missing scenario error = %v", err)
	}
}

func TestRunSoakRequestLatencyIncludesBodyReadAndClose(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		time.Sleep(40 * time.Millisecond)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	stats := &soakStats{
		byProfile:          map[string]uint64{},
		succeededByProfile: map[string]uint64{},
		byStatus:           map[int]uint64{},
		byError:            map[string]uint64{},
	}
	runSoakRequest(context.Background(), server.Client(), configuration{
		baseURL:        server.URL,
		endpoint:       "/",
		requestTimeout: time.Second,
		apiKey:         "test",
	}, payloadProfile{name: "small", body: []byte(`{}`)}, stats)
	headers := time.Duration(stats.headerLatencyMaxNS.Load())
	endToEnd := time.Duration(stats.latencyMaxNS.Load())
	if endToEnd < 35*time.Millisecond || endToEnd <= headers {
		t.Fatalf("headers latency = %s, end-to-end = %s", headers, endToEnd)
	}
}

func TestWaitForScenarioRouteRecovery(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(soakUpstreamMarker))
	}))
	defer server.Close()
	err := waitForScenarioRouteRecovery(context.Background(), server.Client(), configuration{
		baseURL:         server.URL,
		model:           "payload-soak-model",
		apiKey:          "test",
		scenarioTimeout: time.Second,
	})
	if err != nil || attempts.Load() < 2 {
		t.Fatalf("route recovery attempts = %d, error = %v", attempts.Load(), err)
	}
}

func TestResponsesWebsocketScenarios(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, request, errRead := conn.ReadMessage()
		if errRead != nil {
			return
		}
		if !strings.Contains(string(request), soakScenarioPrefix) {
			return
		}
		_ = conn.WriteJSON(map[string]any{"type": "response.created", "response": map[string]string{"id": "resp-soak"}})
		if strings.Contains(string(request), "client_mid_stream_cancel") {
			return
		}
		_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp-soak", "output": []any{}}})
	}))
	defer server.Close()
	cfg := configuration{
		baseURL:         server.URL,
		model:           "payload-soak-model",
		apiKey:          "test-key",
		scenarioTimeout: time.Second,
		websocketIdle:   5 * time.Millisecond,
	}
	for _, name := range []string{"responses_ws_connect", "responses_ws_idle", "responses_ws_frame", "responses_ws_cancel"} {
		t.Run(name, func(t *testing.T) {
			observation := runResponsesWebsocketScenario(context.Background(), cfg, name)
			if !observation.passed || observation.err != nil {
				t.Fatalf("scenario = status %d category %s error %v", observation.status, observation.category, observation.err)
			}
		})
	}
}

func TestResponsesWebsocketURL(t *testing.T) {
	for _, test := range []struct {
		base string
		want string
	}{
		{base: "http://127.0.0.1:8317/base", want: "ws://127.0.0.1:8317/v1/responses"},
		{base: "https://example.com", want: "wss://example.com/v1/responses"},
	} {
		got, err := responsesWebsocketURL(test.base)
		if err != nil || got != test.want {
			t.Fatalf("responsesWebsocketURL(%q) = %q, %v; want %q", test.base, got, err, test.want)
		}
	}
	if _, err := responsesWebsocketURL("ftp://example.com"); err == nil {
		t.Fatal("unsupported websocket URL unexpectedly succeeded")
	}
}
