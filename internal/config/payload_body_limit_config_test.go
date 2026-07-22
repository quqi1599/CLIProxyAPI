package config

import "testing"

func TestParseConfigBytesDefaultsPayloadBodyLimitToObserve(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("port: 8317\n"))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	if cfg.RequestGuards.PayloadBodyLimit.Mode != "observe" {
		t.Fatalf("payload body-limit mode = %q, want observe", cfg.RequestGuards.PayloadBodyLimit.Mode)
	}
}

func TestParseConfigBytesPreservesPayloadBodyLimitOverrides(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(`request-guards:
  payload-body-limit:
    mode: enforce
    json-bytes: 1024
    multipart-bytes: 2048
    websocket-bytes: 4096
`))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	settings := cfg.RequestGuards.PayloadBodyLimit
	if settings.Mode != "enforce" || settings.JSONBytes != 1024 || settings.MultipartBytes != 2048 || settings.WebsocketBytes != 4096 {
		t.Fatalf("payload body-limit settings = %+v", settings)
	}
}
