package executor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// Synthetic bodies deliberately place controls after the large content field.
// No production prompts, credentials, or upstream requests are used.
func deepSeekPreparationFixture(size int, responses bool) []byte {
	content := strings.Repeat("ordinary text ", size/14+1)[:size]
	field := `"messages":[{"role":"user","content":"` + content + `"}]`
	tool := `{"type":"function","function":{"name":"echo","parameters":{"type":"object","required":null}}}`
	if responses {
		field = `"input":[{"role":"user","content":[{"type":"input_text","text":"` + content + `"}]}]`
		tool = `{"type":"function","name":"echo","parameters":{"type":"object","required":null}}`
	}
	return []byte(`{"model":"deepseek-flash",` + field + `,"tools":[` + tool + `],"tool_choice":"required","reasoning":{"effort":"high","summary":"auto"},"thinking_budget":1024,"store":false,"stream":true}`)
}

func BenchmarkDeepSeekPreparation(b *testing.B) {
	for _, responses := range []bool{true, false} {
		format := sdktranslator.FormatOpenAI
		name := "chat"
		if responses {
			format = sdktranslator.FormatOpenAIResponse
			name = "responses"
		}
		for _, size := range []int{8 << 10, 1 << 20, 16 << 20, 64 << 20} {
			for _, workers := range []int{1, 2} {
				b.Run(fmt.Sprintf("%s/bytes%d/workers%d", name, size, workers), func(b *testing.B) {
					body := deepSeekPreparationFixture(size, responses)
					e := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
					run := func() {
						plan, err := e.prepareOpenAICompatRequest(context.Background(), nil,
							cliproxyexecutor.Request{Model: "deepseek-flash", Payload: body},
							cliproxyexecutor.Options{SourceFormat: format, Stream: true},
							"https://api.deepseek.com/v1", "deepseek-flash", openAICompatProfileForKind("deepseek"), true)
						if err != nil || len(plan.body) < size {
							b.Errorf("preparation failed: %v; output bytes %d", err, len(plan.body))
						}
					}
					b.ReportAllocs()
					b.SetBytes(int64(len(body)))
					b.ResetTimer()
					if workers == 1 {
						for i := 0; i < b.N; i++ {
							run()
						}
					} else {
						// Run with -cpu=2 to keep exactly two concurrent workers.
						b.RunParallel(func(pb *testing.PB) {
							for pb.Next() {
								run()
							}
						})
					}
				})
			}
		}
	}
}
