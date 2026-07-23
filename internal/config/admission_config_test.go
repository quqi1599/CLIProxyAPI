package config

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestParseConfigBytesGlobalAdmission(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(`
request-guards:
  global-admission:
    enabled: true
    capacity: 96
    max-queue: 24
    max-wait-seconds: 30
    saturation-grace-seconds: 7
`))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	admission := cfg.RequestGuards.GlobalAdmission
	if !admission.Enabled || admission.Capacity != 96 || admission.MaxQueue != 24 || admission.MaxWaitSeconds != 30 || admission.SaturationGraceSeconds != 7 {
		t.Fatalf("global admission config = %+v", admission)
	}
}

func TestGlobalAdmissionDefaultsDisabled(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("{}"))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	if cfg.RequestGuards.GlobalAdmission.Enabled {
		t.Fatal("global admission must require an explicit opt-in")
	}
}

func TestGlobalAdmissionCanBeExplicitlyDisabled(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("request-guards:\n  global-admission:\n    enabled: false\n"))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	if cfg.RequestGuards.GlobalAdmission.Enabled {
		t.Fatal("global admission must honor an explicit opt-out")
	}
}

func TestGlobalAdmissionExplicitOptOutSurvivesSerialization(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("request-guards:\n  global-admission:\n    enabled: false\n"))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}

	rendered, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("yaml.Marshal() error = %v", err)
	}
	reloaded, err := ParseConfigBytes(rendered)
	if err != nil {
		t.Fatalf("ParseConfigBytes(serialized) error = %v", err)
	}
	if reloaded.RequestGuards.GlobalAdmission.Enabled {
		t.Fatalf("serialized opt-out was lost:\n%s", rendered)
	}
}

func TestLoadConfigOptionalRejectsInvalidYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("request-guards: ["), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadConfigOptional(path, true)
	if err == nil || cfg != nil {
		t.Fatalf("invalid optional config = cfg:%#v error:%v, want nil config and parse error", cfg, err)
	}
}

func TestLoadConfigOptionalDefaultsAdmissionDisabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadConfigOptional(path, true)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}
	if cfg.RequestGuards.GlobalAdmission.Enabled {
		t.Fatal("file config enabled global admission without an explicit opt-in")
	}
}

func TestLoadConfigOptionalPreservesExplicitAdmissionOptOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("request-guards:\n  global-admission:\n    enabled: false\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadConfigOptional(path, true)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}
	if cfg.RequestGuards.GlobalAdmission.Enabled {
		t.Fatal("optional config load lost explicit admission opt-out")
	}
}
