package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/compat"
)

// Compare the selected view with both the old full-body sequence and a
// decode-once RawMessage map candidate. The latter copies the prompt and loses
// field order when marshaling, so it is a measurement candidate, not production.
func BenchmarkDeepSeekPolicyStrategies(b *testing.B) {
	for _, size := range []int{1 << 20, 16 << 20} {
		for _, strategy := range []string{"legacy", "raw_map", "field_view"} {
			b.Run(fmt.Sprintf("%s/bytes%d", strategy, size), func(b *testing.B) {
				body := deepSeekPreparationFixture(size, true)
				state := openAICompatPolicyContext{model: "deepseek-flash", baseURL: "https://api.deepseek.com/v1", endpoint: compat.EndpointKind("responses")}
				ctx := context.WithValue(context.Background(), openAICompatPolicyContextKey{}, state)
				run := func(input []byte) []byte {
					return legacyDeepSeekPolicy(input, state.model, state.baseURL, state.endpoint).Payload
				}
				if strategy == "field_view" {
					run = func(input []byte) []byte {
						result, _ := applyOpenAICompatDeepSeekPolicy(ctx, input)
						return result.Payload
					}
				} else if strategy == "raw_map" {
					legacy := run
					run = func(input []byte) []byte {
						var root map[string]json.RawMessage
						if err := json.Unmarshal(input, &root); err != nil {
							b.Fatal(err)
						}
						controls := make(map[string]json.RawMessage)
						for _, name := range []string{"model", "thinking", "reasoning", "reasoning_effort", "thinking_budget", "enable_thinking", "max_completion_tokens", "max_tokens", "tool_choice", "output_config"} {
							if value, exists := root[name]; exists {
								controls[name] = value
								delete(root, name)
							}
						}
						projected, _ := json.Marshal(controls)
						controls = nil
						_ = json.Unmarshal(legacy(projected), &controls)
						for name, value := range controls {
							root[name] = value
						}
						out, _ := json.Marshal(root)
						return out
					}
				}
				b.ReportAllocs()
				b.SetBytes(int64(len(body)))
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if out := run(body); len(out) < size {
						b.Fatal("invalid output")
					}
				}
			})
		}
	}
}
