package executor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// All upstream responses are synthetic. The parameter path demonstrates safe
// diagnostics and makes no claim about the field in historical customer errors.
func TestCodexLengthErrorsAcrossTransportsDoNotFailover(t *testing.T) {
	const model = "gpt-6-astra"
	const partial = `{"type":"response.output_text.delta","item_id":"msg_partial","output_index":0,"content_index":0,"delta":"visible fixture output"}`
	for _, envelopeType := range []string{"invalid_request_error", "server_error"} {
		for _, tc := range []struct {
			name      string
			transport string
			stream    bool
			output    bool
			status    bool
			failed    bool
			param     string
			reported  bool
			wantOuter int
		}{
			{name: "http_complete", transport: "http", reported: true, wantOuter: 400},
			{name: "http_stream_handshake", transport: "http", stream: true, wantOuter: 400},
			{name: "sse_error", transport: "sse", stream: true, wantOuter: 200},
			{name: "sse_response_failed", transport: "sse", stream: true, failed: true, reported: true, wantOuter: 200},
			{name: "sse_after_output", transport: "sse", stream: true, output: true, wantOuter: 200},
			{name: "websocket_status", transport: "ws", stream: true, status: true, wantOuter: 400},
			{name: "websocket_no_status", transport: "ws", stream: true, wantOuter: 101},
			{name: "websocket_response_failed", transport: "ws", stream: true, failed: true, wantOuter: 101},
			{name: "websocket_complete_private_param", transport: "ws", param: "input[private-param-sentinel].call_id", wantOuter: 101},
			{name: "websocket_after_output", transport: "ws", stream: true, output: true, wantOuter: 101},
		} {
			t.Run(envelopeType+"/"+tc.name, func(t *testing.T) {
				param := tc.param
				if param == "" {
					param = "input[2].call_id"
				}
				errorValue := map[string]any{
					"type": envelopeType, "code": "string_above_max_length",
					"param": param, "message": "private-value-sentinel is too long",
				}
				if tc.reported {
					errorValue["message"] = "Invalid 'input[2].call_id': string too long. Expected a string with maximum length 64, but got a string with length 85 instead."
				}
				rejection := map[string]any{"error": errorValue}
				if tc.transport != "http" {
					rejection["type"] = "error"
				}
				if tc.failed {
					rejection = map[string]any{"type": "response.failed", "response": map[string]any{"id": "resp_length", "status": "failed", "error": errorValue}}
				}
				if tc.status {
					rejection["status"] = 400
				}
				rejectionBytes, err := json.Marshal(rejection)
				if err != nil {
					t.Fatal(err)
				}
				var primaryCalls, backupCalls atomic.Int32
				primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					primaryCalls.Add(1)
					w.Header().Set("X-Request-Id", "length-fixture")
					if tc.transport == "http" {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusBadRequest)
						_, _ = w.Write(rejectionBytes)
						return
					}
					events := []string{`{"type":"response.created","response":{"id":"resp_length","model":"gpt-6-astra"}}`}
					if tc.output {
						events = append(events, partial)
					}
					events = append(events, string(rejectionBytes))
					writeCodexLengthFixture(t, w, r, tc.transport, events)
				}))
				t.Cleanup(primary.Close)
				backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					backupCalls.Add(1)
					writeCodexLengthFixture(t, w, r, tc.transport, []string{`{"type":"response.completed","response":{"id":"resp_backup","model":"gpt-6-astra","status":"completed","output":[{"id":"msg_backup","type":"message","role":"assistant","content":[{"type":"output_text","text":"backup answer"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`})
				}))
				t.Cleanup(backup.Close)

				manager := cliproxyauth.NewManager(nil, &cliproxyauth.FillFirstSelector{}, nil)
				manager.SetRetryConfig(3, time.Second, 3)
				cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}
				var exec cliproxyauth.ProviderExecutor = NewCodexExecutor(cfg)
				if tc.transport == "ws" {
					exec = NewCodexWebsocketsExecutor(cfg)
				}
				manager.RegisterExecutor(exec)
				for i, route := range []struct{ role, url string }{{"primary", primary.URL}, {"backup", backup.URL}} {
					auth := &cliproxyauth.Auth{
						ID:       "length-" + envelopeType + "-" + tc.name + "-" + route.role,
						Provider: "codex", Status: cliproxyauth.StatusActive,
						Attributes: map[string]string{"base_url": route.url, "api_key": "fixture", "priority": fmt.Sprint(10 - i)},
					}
					registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: model}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
					if _, errRegister := manager.Register(t.Context(), auth); errRegister != nil {
						t.Fatal(errRegister)
					}
				}
				request := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"model":"gpt-6-astra","input":"synthetic fixture"}`)}
				options := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), ResponseFormat: sdktranslator.FromString("openai-response"), Stream: tc.stream}
				var output strings.Builder
				var terminalErr error
				if tc.stream {
					result, errExecute := manager.ExecuteStream(t.Context(), []string{"codex"}, request, options)
					terminalErr = errExecute
					if result != nil {
						defer result.Close()
						for chunk := range result.Chunks {
							output.Write(chunk.Payload)
							if chunk.Err != nil {
								if terminalErr != nil {
									t.Fatal("duplicate terminal error")
								}
								terminalErr = chunk.Err
							}
						}
					}
				} else {
					response, errExecute := manager.Execute(t.Context(), []string{"codex"}, request, options)
					output.Write(response.Payload)
					terminalErr = errExecute
				}
				f := failurecontract.Classify(terminalErr)
				wantPhase := failurecontract.StreamPhaseBeforeOutput
				if tc.output {
					wantPhase = failurecontract.StreamPhaseAfterOutput
				}
				if f == nil {
					t.Fatalf("length failure disappeared: primary=%d backup=%d output=%s", primaryCalls.Load(), backupCalls.Load(), output.String())
				}
				if f.Kind != failurecontract.InvalidRequest || f.Scope != failurecontract.ScopeRequest || f.HTTPStatus != http.StatusBadRequest || f.OuterStatus != tc.wantOuter || f.SemanticCode != "string_above_max_length" || f.ProviderCode != "string_above_max_length" || f.Retryable || f.OutputCommitted != tc.output || f.StreamPhase != wantPhase {
					t.Fatalf("length classification changed: kind=%s scope=%s status=%d outer=%d code=%s retry=%t committed=%t phase=%s primary=%d backup=%d", f.Kind, f.Scope, f.HTTPStatus, f.OuterStatus, f.SemanticCode, f.Retryable, f.OutputCommitted, f.StreamPhase, primaryCalls.Load(), backupCalls.Load())
				}
				if primaryCalls.Load() != 1 || backupCalls.Load() != 0 {
					t.Fatalf("deterministic request was replayed: primary=%d backup=%d", primaryCalls.Load(), backupCalls.Load())
				}
				wantParam := "input[].call_id"
				if tc.param != "" {
					wantParam = ""
				}
				diagnostic := f.ErrorDiagnostic
				if diagnostic.Param != wantParam || diagnostic.LengthUnit != "" {
					t.Fatalf("safe parameter lost or unverified lengths invented: %+v", diagnostic)
				}
				if tc.reported {
					if diagnostic.ReportedMaxLength == nil || *diagnostic.ReportedMaxLength != 64 || diagnostic.ReportedActualLength == nil || *diagnostic.ReportedActualLength != 85 {
						t.Fatalf("explicit upstream length bounds lost: %+v", diagnostic)
					}
				} else if diagnostic.ReportedMaxLength != nil || diagnostic.ReportedActualLength != nil {
					t.Fatalf("unverified error text produced invented lengths: %+v", diagnostic)
				}
				serialized, errMarshal := json.Marshal(f)
				if errMarshal != nil {
					t.Fatal(errMarshal)
				}
				if strings.Contains(string(serialized), "private-") {
					t.Fatalf("private error content entered the typed failure: %s", serialized)
				}
				if strings.Contains(terminalErr.Error(), "private-") || strings.Contains(output.String(), "private-") || strings.Contains(output.String(), "response.completed") || strings.Contains(output.String(), "backup answer") {
					t.Fatalf("failure leaked a private value or invented success: error=%v output=%s", terminalErr, output.String())
				}
				if strings.Contains(output.String(), "visible fixture output") != tc.output {
					t.Fatalf("output state does not match delivered text: %s", output.String())
				}
			})
		}
	}
}

func writeCodexLengthFixture(t *testing.T, w http.ResponseWriter, r *http.Request, transport string, events []string) {
	t.Helper()
	if transport == "ws" {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, http.Header{"X-Request-Id": []string{"length-fixture"}})
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, errRead := conn.ReadMessage(); errRead != nil {
			t.Errorf("read websocket request: %v", errRead)
			return
		}
		for _, event := range events {
			if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(event)); errWrite != nil {
				t.Errorf("write websocket response: %v", errWrite)
				return
			}
		}
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range events {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
	}
}
