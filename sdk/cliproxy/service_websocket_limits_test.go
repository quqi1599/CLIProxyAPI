package cliproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestWebsocketGatewayUsesConfiguredConnectionLimits(t *testing.T) {
	service := &Service{cfg: &config.Config{
		WebsocketMaxConnections:      2,
		WebsocketMaxConnectionsPerIP: 2,
	}}
	service.ensureWebsocketGateway()
	server := httptest.NewServer(service.wsGateway.Handler())
	t.Cleanup(func() {
		_ = service.wsGateway.Stop(context.Background())
		server.Close()
	})

	first := dialServiceWebsocket(t, server.URL, service.wsGateway.Path())
	second := dialServiceWebsocket(t, server.URL, service.wsGateway.Path())

	service.applyConfigUpdate(&config.Config{
		WebsocketAuth:                false,
		WebsocketMaxConnections:      1,
		WebsocketMaxConnectionsPerIP: 1,
	})
	assertServiceWebsocketRejected(t, server.URL, service.wsGateway.Path())
	if service.cfg.WebsocketAuth {
		t.Fatal("websocket limit update unexpectedly enabled ws-auth")
	}

	_ = first.Close()
	_ = second.Close()
	third := dialServiceWebsocketEventually(t, server.URL, service.wsGateway.Path())
	_ = third.Close()
}

func TestWebsocketGatewayAuthEnableClosesExistingAndRejectsMissingCredentials(t *testing.T) {
	accessManager := sdkaccess.NewManager()
	accessManager.SetProviders([]sdkaccess.Provider{rejectingWebsocketAccessProvider{}})
	service := &Service{
		cfg: &config.Config{
			WebsocketAuth:                false,
			WebsocketMaxConnections:      2,
			WebsocketMaxConnectionsPerIP: 2,
		},
		accessManager: accessManager,
	}
	service.ensureWebsocketGateway()
	server := httptest.NewServer(service.wsGateway.Handler())
	t.Cleanup(func() {
		_ = service.wsGateway.Stop(context.Background())
		server.Close()
	})

	existing := dialServiceWebsocket(t, server.URL, service.wsGateway.Path())
	service.applyConfigUpdate(&config.Config{
		WebsocketAuth:                true,
		WebsocketMaxConnections:      2,
		WebsocketMaxConnectionsPerIP: 2,
	})
	if err := existing.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set existing websocket deadline: %v", err)
	}
	if _, _, err := existing.ReadMessage(); err == nil {
		t.Fatal("existing websocket survived ws-auth enable")
	}
	_ = existing.Close()

	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+service.wsGateway.Path(), nil)
	if conn != nil {
		_ = conn.Close()
	}
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated websocket result = (err=%v, response=%v), want HTTP %d", err, response, http.StatusUnauthorized)
	}
}

type rejectingWebsocketAccessProvider struct{}

func (rejectingWebsocketAccessProvider) Identifier() string { return "reject-websocket" }

func (rejectingWebsocketAccessProvider) Authenticate(context.Context, *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	return nil, sdkaccess.NewNoCredentialsError()
}

func dialServiceWebsocket(t *testing.T, serverURL, path string) *websocket.Conn {
	t.Helper()
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(serverURL, "http")+path, nil)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	return conn
}

func assertServiceWebsocketRejected(t *testing.T, serverURL, path string) {
	t.Helper()
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(serverURL, "http")+path, nil)
	if conn != nil {
		_ = conn.Close()
	}
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("websocket rejection = (err=%v, response=%v), want HTTP %d", err, response, http.StatusTooManyRequests)
	}
}

func dialServiceWebsocketEventually(t *testing.T, serverURL, path string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(serverURL, "http") + path
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		conn, response, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if err == nil {
			return conn
		}
		if conn != nil {
			_ = conn.Close()
		}
		if response == nil || response.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("dial websocket while waiting for released capacity: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for released websocket capacity")
	return nil
}
