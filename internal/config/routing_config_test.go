package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRoutingGroupSessionAffinityFalseRoundTrips(t *testing.T) {
	var cfg Config
	if errUnmarshal := yaml.Unmarshal([]byte(`
routing:
  strategy: round-robin
  session-affinity: true
  group-strategies:
    kimi: spread
  group-session-affinity:
    kimi: false
`), &cfg); errUnmarshal != nil {
		t.Fatalf("yaml.Unmarshal() error = %v", errUnmarshal)
	}
	enabled, ok := cfg.Routing.GroupSessionAffinity["kimi"]
	if !ok || enabled {
		t.Fatalf("group-session-affinity.kimi = (%t, %t), want explicitly configured false", enabled, ok)
	}

	encoded, errMarshal := yaml.Marshal(&cfg)
	if errMarshal != nil {
		t.Fatalf("yaml.Marshal() error = %v", errMarshal)
	}
	if !strings.Contains(string(encoded), "group-session-affinity:\n        kimi: false") {
		t.Fatalf("marshaled config lost explicit false override:\n%s", encoded)
	}
}
