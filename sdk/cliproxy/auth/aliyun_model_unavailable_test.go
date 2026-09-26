package auth

import (
	"context"
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestAliyunModelUnavailableFallback(t *testing.T) {
	for _, message := range []string{
		`{"code":"InvalidParameter","message":"Model not exist."}`,
		"upstream request failed reason=model_not_supported model unavailable,invalidparameter [BODY METADATA v1]",
		`{"code":"InvalidParameter","message":"invalid temperature"}`,
	} {
		for _, mode := range []string{"execute", "stream", "count"} {
			t.Run(message+"/"+mode, func(t *testing.T) {
				model := "qwen3.8-omni-flash"
				modelError := isModelSupportErrorMessage(message)
				m := NewManager(nil, nil, nil)
				rejection := &Error{HTTPStatus: 400, Message: message}
				if isRequestInvalidError(rejection) == modelError {
					t.Fatal("incorrect request/model classification")
				}
				failures := map[string]error{"aa-omni-bad": rejection}
				e := &authFallbackExecutor{id: "claude", executeErrors: failures, streamFirstErrors: failures, countErrors: failures}
				m.RegisterExecutor(e)
				reg := registry.GetGlobalRegistry()
				for _, id := range []string{"aa-omni-bad", "bb-omni-good"} {
					reg.RegisterClient(id, "claude", []*registry.ModelInfo{{ID: model}, {ID: "qwen3.8-flash"}})
					t.Cleanup(func() { reg.UnregisterClient(id) })
					if _, err := m.Register(context.Background(), &Auth{ID: id, Provider: "claude"}); err != nil {
						t.Fatal(err)
					}
				}
				invoke := func() (string, error) {
					request := cliproxyexecutor.Request{Model: model}
					switch mode {
					case "stream":
						r, err := m.ExecuteStream(context.Background(), []string{"claude"}, request, cliproxyexecutor.Options{})
						if err != nil {
							return "", err
						}
						var payload []byte
						for chunk := range r.Chunks {
							if chunk.Err != nil {
								return "", chunk.Err
							}
							payload = append(payload, chunk.Payload...)
						}
						return string(payload), nil
					case "count":
						r, err := m.ExecuteCount(context.Background(), []string{"claude"}, request, cliproxyexecutor.Options{})
						return string(r.Payload), err
					default:
						r, err := m.Execute(context.Background(), []string{"claude"}, request, cliproxyexecutor.Options{})
						return string(r.Payload), err
					}
				}
				payload, err := invoke()
				if modelError {
					if err != nil || payload != "bb-omni-good" {
						t.Fatalf("fallback payload=%q err=%v", payload, err)
					}
					if _, err = invoke(); err != nil {
						t.Fatal(err)
					}
				} else if err == nil {
					t.Fatal("parameter error unexpectedly succeeded")
				}
				calls := e.ExecuteCalls()
				if mode == "stream" {
					calls = e.StreamCalls()
				}
				if mode == "count" {
					calls = e.CountCalls()
				}
				want := []string{"aa-omni-bad"}
				if modelError {
					want = append(want, "bb-omni-good", "bb-omni-good")
				}
				if !reflect.DeepEqual(calls, want) {
					t.Fatalf("calls=%v want=%v", calls, want)
				}
				bad, _ := m.GetByID("aa-omni-bad")
				if bad.Disabled {
					t.Fatal("entire credential disabled")
				}
				if modelError {
					state := bad.ModelStates[model]
					if state == nil || !state.Unavailable || state.NextRetryAfter.IsZero() {
						t.Fatalf("missing model quarantine: %+v", state)
					}
					if state := bad.ModelStates["qwen3.8-flash"]; state != nil && state.Unavailable {
						t.Fatal("unrelated model quarantined")
					}
				} else if state := bad.ModelStates[model]; state != nil && state.Unavailable {
					t.Fatal("parameter error quarantined model")
				}
			})
		}
	}
}
