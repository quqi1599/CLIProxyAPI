package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCodexNativeResponsesRoundTrip(t *testing.T) {
	for _, value := range []string{"", "true", "false"} {
		t.Run("value="+value, func(t *testing.T) {
			input := "codex-api-key:\n  - api-key: test-key\n    base-url: https://example.test/v1\n"
			if value != "" {
				input += "    native-responses: " + value + "\n"
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(input), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			check := func(got *bool) {
				t.Helper()
				if value == "" {
					if got != nil {
						t.Fatalf("want unspecified capability, got %v", *got)
					}
				} else if got == nil || *got != (value == "true") {
					t.Fatalf("capability = %v, want %s", got, value)
				}
			}
			check(cfg.CodexKey[0].NativeResponses)
			clone := cfg.CloneForRuntime()
			check(clone.CodexKey[0].NativeResponses)
			if value != "" && clone.CodexKey[0].NativeResponses == cfg.CodexKey[0].NativeResponses {
				t.Fatal("runtime snapshot shares capability pointer")
			}
			data, err := json.Marshal(cfg.CodexKey[0])
			if err != nil {
				t.Fatal(err)
			}
			var jsonKey CodexKey
			if err = json.Unmarshal(data, &jsonKey); err != nil {
				t.Fatal(err)
			}
			check(jsonKey.NativeResponses)
			if value == "" && strings.Contains(string(data), "native-responses") {
				t.Fatal("unspecified capability must be omitted from JSON")
			}
			if err = SaveConfigPreserveComments(path, clone); err != nil {
				t.Fatal(err)
			}
			saved, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			check(saved.CodexKey[0].NativeResponses)
			clone.CodexKey[0].NativeResponses = nil
			if err = SaveConfigPreserveComments(path, clone); err != nil {
				t.Fatal(err)
			}
			cleared, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if cleared.CodexKey[0].NativeResponses != nil {
				t.Fatal("save must remove a cleared capability declaration")
			}
		})
	}
}

func TestCodexNativeResponsesRejectsInvalidTypes(t *testing.T) {
	for _, value := range []string{`"true"`, "1", "{}", "[]"} {
		var key CodexKey
		if err := json.Unmarshal([]byte(`{"native-responses":`+value+`}`), &key); err == nil {
			t.Errorf("JSON accepted invalid capability %s", value)
		}
	}
	var key CodexKey
	if err := yaml.Unmarshal([]byte("native-responses: unexpected\n"), &key); err == nil {
		t.Fatal("YAML accepted invalid capability")
	}
}
