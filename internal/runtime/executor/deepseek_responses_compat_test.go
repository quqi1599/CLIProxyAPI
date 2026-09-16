package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestDeepSeekResponsesNamespaceExecutorRoundtrip(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				body, _ := io.ReadAll(r.Body)
				if r.URL.Path != "/v1/responses" {
					t.Errorf("path=%s", r.URL.Path)
				}
				wire := gjson.GetBytes(body, "tools.0.name").String()
				if !strings.HasPrefix(wire, "ns_") || gjson.GetBytes(body, "tools.0.type").String() != "function" {
					t.Errorf("tools=%s", gjson.GetBytes(body, "tools").Raw)
				}
				if got := gjson.GetBytes(body, "input.0.name").String(); got != wire {
					t.Errorf("history name=%s", got)
				}
				response := fmt.Sprintf(`{"object":"response","id":"resp_1","status":"completed","output":[{"type":"function_call","name":%q,"call_id":"c2","arguments":"{}"}],"usage":{"input_tokens":10,"output_tokens":5}}`, wire)
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"name\":%q,\"call_id\":\"c2\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":%s}\n\n", wire, response)
				} else {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, response)
				}
			}))
			defer server.Close()
			e := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
			auth := &cliproxyauth.Auth{Provider: "openai-compatibility", Attributes: map[string]string{"base_url": server.URL + "/v1", "api_key": "test", "compat_kind": "deepseek"}}
			payload := []byte(`{"model":"deepseek-v4-pro","reasoning":{"effort":"none"},"tools":[{"type":"namespace","name":"files","tools":[{"type":"function","name":"read","parameters":{"type":"object"}}]},{"type":"custom","name":"apply_patch"}],"input":[{"type":"function_call","namespace":"files","name":"read","call_id":"c1","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"ok"},{"role":"user","content":"read again"}]}`)
			req := cliproxyexecutor.Request{Model: "deepseek-v4-pro", Payload: payload}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: payload}
			if stream {
				result, err := e.ExecuteStream(context.Background(), auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				var joined strings.Builder
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					joined.Write(chunk.Payload)
				}
				if !strings.Contains(joined.String(), `"namespace":"files"`) || strings.Contains(joined.String(), `"name":"ns_`) {
					t.Fatal(joined.String())
				}
			} else {
				resp, err := e.Execute(context.Background(), auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				if gjson.GetBytes(resp.Payload, "output.0.namespace").String() != "files" || gjson.GetBytes(resp.Payload, "output.0.name").String() != "read" {
					t.Fatal(string(resp.Payload))
				}
			}
			if calls != 1 {
				t.Fatalf("calls=%d", calls)
			}
		})
	}
}

func TestDeepSeekCodingResponsesRejectedBeforeTranslation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		e := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
		_, err := e.prepareOpenAICompatRequest(context.Background(), nil, cliproxyexecutor.Request{Model: "deepseek-v4-pro", Payload: []byte(`{"input":"hi"}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}, "https://ark.cn-beijing.volces.com/api/coding/v1", "deepseek-v4-pro", openAICompatProfileForKind("doubao"), stream)
		if err == nil || !strings.Contains(err.Error(), "deepseek_responses_route_unsupported") {
			t.Fatalf("stream=%v err=%v", stream, err)
		}
	}
}
