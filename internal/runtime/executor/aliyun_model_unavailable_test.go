package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	auth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestAliyunModelUnavailableRealExecutor(t *testing.T) {
	for _, allMissing := range []bool{false, true} {
		t.Run(map[bool]string{false: "fallback", true: "exhausted"}[allMissing], func(t *testing.T) {
			var badCalls, goodCalls atomic.Int32
			const model = "qwen3.8-omni-flash"
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if gjson.GetBytes(body, "model").String() != model {
					t.Error("model silently changed")
				}
				w.Header().Set("Content-Type", "application/json")
				bad := r.Header.Get("X-Api-Key") == "bad-fixture" || r.Header.Get("Authorization") == "Bearer bad-fixture"
				if bad {
					badCalls.Add(1)
				} else {
					goodCalls.Add(1)
				}
				if bad || allMissing {
					w.WriteHeader(400)
					_, _ = w.Write([]byte(`{"code":"InvalidParameter","message":"Model not exist.","request_id":"private-sentinel"}`))
					return
				}
				_, _ = w.Write([]byte(`{"id":"msg_test","type":"message","role":"assistant","model":"qwen3.8-omni-flash","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
			}))
			defer upstream.Close()
			manager := auth.NewManager(nil, nil, nil)
			manager.RegisterExecutor(NewClaudeExecutor(&config.Config{DisableClaudeCloakMode: true}))
			reg := registry.GetGlobalRegistry()
			for _, item := range []struct{ id, key string }{{"aa-omni-http", "bad-fixture"}, {"bb-omni-http", "good-fixture"}} {
				reg.RegisterClient(item.id, "claude", []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { reg.UnregisterClient(item.id) })
				priority := "0"
				if item.key == "bad-fixture" {
					priority = "100"
				}
				if _, err := manager.Register(context.Background(), &auth.Auth{ID: item.id, Provider: "claude", Attributes: map[string]string{"api_key": item.key, "base_url": upstream.URL, "priority": priority}}); err != nil {
					t.Fatal(err)
				}
			}
			response, err := manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: model, Payload: []byte(`{"model":"qwen3.8-omni-flash","max_tokens":128,"messages":[{"role":"user","content":"OK"}]}`)}, cliproxyexecutor.Options{SourceFormat: translator.FromString("claude")})
			if badCalls.Load() != 1 || goodCalls.Load() != 1 {
				t.Fatalf("HTTP calls bad=%d good=%d", badCalls.Load(), goodCalls.Load())
			}
			if allMissing {
				if err == nil {
					t.Fatal("missing model reported success")
				}
				body := string(handlers.BuildErrorResponseBody(400, err.Error()))
				if !strings.Contains(body, "model_not_supported") || strings.Contains(body, "private-sentinel") {
					t.Fatalf("wrong or leaking public error: %s", body)
				}
			} else if err != nil || !strings.Contains(string(response.Payload), "OK") {
				t.Fatalf("response=%s err=%v", response.Payload, err)
			}
		})
	}
}
