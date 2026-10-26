package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestKimiK3CodeFixedTemperatureAfterPayloadOverrides(t *testing.T) {
	for _, dedicated := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, override := range []bool{false, true} {
				t.Run(fmt.Sprintf("dedicated=%t/stream=%t/override=%t", dedicated, stream, override), func(t *testing.T) {
					var wire []byte
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						wire, _ = io.ReadAll(r.Body)
						if got := gjson.GetBytes(wire, "temperature"); got.Type != gjson.Number || got.Float() != 1 {
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(http.StatusBadRequest)
							_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"invalid temperature: only 1 is allowed for this model"}}`)
							return
						}
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(w, "data: {\"id\":\"synthetic\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"{}\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
						} else {
							w.Header().Set("Content-Type", "application/json")
							_, _ = io.WriteString(w, `{"id":"synthetic","choices":[{"index":0,"message":{"role":"assistant","content":"{}"},"finish_reason":"stop"}]}`)
						}
					}))
					defer server.Close()
					cfg := &config.Config{}
					if override {
						cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "k3"}}, Params: map[string]any{"temperature": 0.2}}}
					}
					var executor kimiCodeTestExecutor = NewOpenAICompatExecutor("kimi", cfg)
					if dedicated {
						executor = NewKimiExecutor(cfg)
					}
					auth := &cliproxyauth.Auth{Provider: "kimi", Attributes: map[string]string{"base_url": server.URL + "/coding/v1", "api_key": "synthetic", "compat_kind": "kimi"}}
					body := []byte(`{"model":"kimi-k3","messages":[{"role":"user","content":"Return JSON"}],"max_tokens":128,"temperature":0,"reasoning_effort":"low","response_format":{"type":"json_object"}}`)
					req := cliproxyexecutor.Request{Model: "k3", Payload: body}
					opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, OriginalRequest: body, Stream: stream}
					if stream {
						resp, err := executor.ExecuteStream(context.Background(), auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range resp.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					} else if _, err := executor.Execute(context.Background(), auth, req, opts); err != nil {
						t.Fatal(err)
					}
					if gjson.GetBytes(wire, "reasoning_effort").String() != "low" || gjson.GetBytes(wire, "response_format.type").String() != "json_object" || gjson.GetBytes(wire, "max_tokens").Int() != 128 {
						t.Fatalf("sampling repair changed other controls: %s", wire)
					}
				})
			}
		}
	}
}
