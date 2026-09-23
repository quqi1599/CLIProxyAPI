package synthesizer

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestCodexNativeResponsesSynthesis(t *testing.T) {
	enabled, disabled := true, false
	for _, tc := range []struct {
		name  string
		value *bool
		want  string
	}{
		{"unspecified", nil, ""},
		{"enabled", &enabled, "true"},
		{"disabled", &disabled, "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auths, err := NewConfigSynthesizer().Synthesize(&SynthesisContext{
				Config: &config.Config{CodexKey: []config.CodexKey{{
					APIKey: "test-key", BaseURL: "https://example.test/v1", NativeResponses: tc.value,
				}}},
				Now: time.Now(), IDGenerator: NewStableIDGenerator(),
			})
			if err != nil || len(auths) != 1 {
				t.Fatalf("Synthesize() count=%d error=%v", len(auths), err)
			}
			got, present := auths[0].Attributes["native_responses"]
			if got != tc.want || present != (tc.value != nil) {
				t.Fatalf("native_responses=%q present=%v, want %q", got, present, tc.want)
			}
		})
	}
}
