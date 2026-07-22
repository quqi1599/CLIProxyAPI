package config

import "testing"

func TestParseConfigBytesWebsocketConnectionLimits(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(`
ws-auth: false
ws-max-connections: 48
ws-max-connections-per-ip: 6
`))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	if cfg.WebsocketAuth {
		t.Fatal("ws-auth = true, want false")
	}
	if cfg.WebsocketMaxConnections != 48 || cfg.WebsocketMaxConnectionsPerIP != 6 {
		t.Fatalf("websocket connection limits = (%d, %d), want (48, 6)", cfg.WebsocketMaxConnections, cfg.WebsocketMaxConnectionsPerIP)
	}
}
