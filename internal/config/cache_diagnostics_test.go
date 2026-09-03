package config

import "testing"

func TestDeepSeekAnthropicCacheDiagnosticsDefaults(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("port: 8317\n"))
	if err != nil {
		t.Fatal(err)
	}
	diagnostic := cfg.CacheDiagnostics.DeepSeekAnthropic
	if diagnostic.Enabled {
		t.Fatal("diagnostics must default to disabled")
	}
	if diagnostic.EffectiveSampleRate() != 0.05 || diagnostic.CompareWindowSeconds != 600 || diagnostic.StableMissThreshold != 3 || diagnostic.MaxEntries != 10000 {
		t.Fatalf("diagnostic defaults = %+v, sample=%v", diagnostic, diagnostic.EffectiveSampleRate())
	}
}

func TestDeepSeekAnthropicCacheDiagnosticsAllowsZeroSampleAndClampsHigh(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("cache-diagnostics:\n  deepseek-anthropic:\n    enabled: true\n    sample-rate: 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.CacheDiagnostics.DeepSeekAnthropic.EffectiveSampleRate(); got != 0 {
		t.Fatalf("zero sample rate = %v", got)
	}
	cfg, err = ParseConfigBytes([]byte("cache-diagnostics:\n  deepseek-anthropic:\n    enabled: true\n    sample-rate: 2\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.CacheDiagnostics.DeepSeekAnthropic.EffectiveSampleRate(); got != 1 {
		t.Fatalf("high sample rate = %v", got)
	}
}
