package config

import "testing"

func TestParseConfigBytesGlobalAdmission(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(`
request-guards:
  global-admission:
    enabled: true
    capacity: 96
    max-queue: 24
    saturation-grace-seconds: 7
`))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	admission := cfg.RequestGuards.GlobalAdmission
	if !admission.Enabled || admission.Capacity != 96 || admission.MaxQueue != 24 || admission.SaturationGraceSeconds != 7 {
		t.Fatalf("global admission config = %+v", admission)
	}
}

func TestGlobalAdmissionDefaultsDisabled(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("{}"))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	if cfg.RequestGuards.GlobalAdmission.Enabled {
		t.Fatal("global admission must be disabled when omitted")
	}
}
