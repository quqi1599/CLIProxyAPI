package config

import "testing"

func TestParseConfigBytesDefaultsAmplificationToObserve(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("port: 8317\n"))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	if cfg.RequestGuards.Amplification.Mode != "observe" {
		t.Fatalf("amplification mode = %q, want observe", cfg.RequestGuards.Amplification.Mode)
	}
}

func TestParseConfigBytesPreservesAmplificationEnforce(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("request-guards:\n  amplification:\n    mode: enforce\n"))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	if cfg.RequestGuards.Amplification.Mode != "enforce" {
		t.Fatalf("amplification mode = %q, want enforce", cfg.RequestGuards.Amplification.Mode)
	}
}

func TestProgrammaticSDKConfigLeavesAmplificationUnconfigured(t *testing.T) {
	var cfg SDKConfig
	if cfg.RequestGuards.Amplification.Mode != "" {
		t.Fatalf("programmatic amplification mode = %q, want empty", cfg.RequestGuards.Amplification.Mode)
	}
}
