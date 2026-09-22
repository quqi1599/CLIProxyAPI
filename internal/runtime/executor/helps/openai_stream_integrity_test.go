package helps

import "testing"

func TestAliyunDeepSeekStreamIntegrityScope(t *testing.T) {
	for _, tt := range []struct {
		kind, model string
		enabled     bool
	}{
		{"qwen", "deepseek-v4.1-flash", true},
		{"QWEN", "DeepSeek-v4.1-flash", true},
		{"qwen", "qwen3-max", false},
		{"deepseek", "deepseek-v4.1-flash", false},
		{"minimax", "MiniMax-M3", false},
		{"generic", "deepseek-v4.1-flash", false},
	} {
		if got := NewAliyunDeepSeekStreamIntegrity(tt.kind, tt.model); (got != nil) != tt.enabled {
			t.Errorf("%s/%s: enabled=%t", tt.kind, tt.model, got != nil)
		}
	}
	var disabled *OpenAIStreamIntegrity
	if err := disabled.Observe([]byte("data: [DONE]")); err != nil {
		t.Fatal(err)
	}
	if err := disabled.Finish(); err != nil {
		t.Fatal(err)
	}
}
