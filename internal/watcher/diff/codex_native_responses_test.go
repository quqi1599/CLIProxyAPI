package diff

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestCodexNativeResponsesDiff(t *testing.T) {
	enabled, disabled := true, false
	values := []*bool{nil, &enabled, &disabled}
	for _, oldValue := range values {
		for _, newValue := range values {
			oldCfg := &config.Config{CodexKey: []config.CodexKey{{NativeResponses: oldValue}}}
			newCfg := &config.Config{CodexKey: []config.CodexKey{{NativeResponses: newValue}}}
			details := BuildConfigChangeDetails(oldCfg, newCfg)
			if oldValue == newValue {
				if len(details) != 0 {
					t.Fatalf("unchanged capability produced diff: %v", details)
				}
			} else {
				expectContains(t, details, "codex[0].native-responses: updated")
			}
		}
	}
}
