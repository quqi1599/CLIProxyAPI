package wsrelay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWebsocketGlobalConnectionLimitRejectsBeforeUpgrade(t *testing.T) {
	var factoryCalls atomic.Int32
	factoryCalled := make(chan struct{}, 1)
	mgr := NewManager(Options{
		MaxConnections:      1,
		MaxConnectionsPerIP: 2,
		ProviderFactory: func(*http.Request) (string, error) {
			factoryCalls.Add(1)
			factoryCalled <- struct{}{}
			return "global-limit", nil
		},
	})
	server := httptest.NewServer(mgr.Handler())
	t.Cleanup(func() {
		_ = mgr.Stop(context.Background())
		server.Close()
	})

	first := dialWebsocket(t, server.URL, mgr.Path())
	waitForSignal(t, factoryCalled)
	waitForActiveConnections(t, mgr, 1)

	response := dialRejectedWebsocket(t, server.URL, mgr.Path())
	assertLimitResponse(t, response, "websocket_connection_limit")
	if got := factoryCalls.Load(); got != 1 {
		t.Fatalf("provider factory calls = %d, want 1; rejected connection advanced past admission", got)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("close first websocket: %v", err)
	}
	waitForActiveConnections(t, mgr, 0)

	third := dialWebsocket(t, server.URL, mgr.Path())
	if err := third.Close(); err != nil {
		t.Fatalf("close third websocket: %v", err)
	}
	waitForActiveConnections(t, mgr, 0)
}

func TestWebsocketPerIPConnectionLimit(t *testing.T) {
	mgr := NewManager(Options{MaxConnections: 2, MaxConnectionsPerIP: 1})
	server := httptest.NewServer(mgr.Handler())
	t.Cleanup(func() {
		_ = mgr.Stop(context.Background())
		server.Close()
	})

	first := dialWebsocket(t, server.URL, mgr.Path())
	waitForActiveConnections(t, mgr, 1)
	response := dialRejectedWebsocket(t, server.URL, mgr.Path())
	assertLimitResponse(t, response, "websocket_connection_limit_per_ip")
	if err := first.Close(); err != nil {
		t.Fatalf("close first websocket: %v", err)
	}
	waitForActiveConnections(t, mgr, 0)
}

func TestWebsocketUpgradeFailureReleasesConnection(t *testing.T) {
	mgr := NewManager(Options{MaxConnections: 1, MaxConnectionsPerIP: 1})
	req := httptest.NewRequest(http.MethodGet, mgr.Path(), nil)
	req.RemoteAddr = "192.0.2.10:1234"
	recorder := httptest.NewRecorder()

	mgr.handleWebsocket(recorder, req)

	active, perIP := connectionCounts(mgr)
	if active != 0 || perIP != 0 {
		t.Fatalf("connection counts after failed upgrade = (%d, %d), want (0, 0)", active, perIP)
	}
}

func TestWebsocketProviderFailureReleasesConnection(t *testing.T) {
	mgr := NewManager(Options{
		MaxConnections:      1,
		MaxConnectionsPerIP: 1,
		ProviderFactory: func(*http.Request) (string, error) {
			return "", errors.New("provider rejected")
		},
	})
	server := httptest.NewServer(mgr.Handler())
	t.Cleanup(func() {
		_ = mgr.Stop(context.Background())
		server.Close()
	})

	client := dialWebsocket(t, server.URL, mgr.Path())
	waitForActiveConnections(t, mgr, 0)
	_ = client.Close()

	retry := dialWebsocket(t, server.URL, mgr.Path())
	waitForActiveConnections(t, mgr, 0)
	_ = retry.Close()
}

func TestWebsocketReplacementReleasesOldConnection(t *testing.T) {
	connected := make(chan struct{}, 2)
	mgr := NewManager(Options{
		MaxConnections:      2,
		MaxConnectionsPerIP: 2,
		ProviderFactory: func(*http.Request) (string, error) {
			return "replacement", nil
		},
		OnConnected: func(string) {
			connected <- struct{}{}
		},
	})
	server := httptest.NewServer(mgr.Handler())
	t.Cleanup(func() {
		_ = mgr.Stop(context.Background())
		server.Close()
	})

	first := dialWebsocket(t, server.URL, mgr.Path())
	waitForSignal(t, connected)
	second := dialWebsocket(t, server.URL, mgr.Path())
	waitForSignal(t, connected)
	waitForActiveConnections(t, mgr, 1)

	if err := second.Close(); err != nil {
		t.Fatalf("close replacement websocket: %v", err)
	}
	waitForActiveConnections(t, mgr, 0)
	_ = first.Close()
}

func TestWebsocketStopReleasesConnectionsAndClosesAdmission(t *testing.T) {
	connected := make(chan struct{}, 1)
	mgr := NewManager(Options{
		MaxConnections:      1,
		MaxConnectionsPerIP: 1,
		OnConnected: func(string) {
			connected <- struct{}{}
		},
	})
	server := httptest.NewServer(mgr.Handler())
	t.Cleanup(func() {
		_ = mgr.Stop(context.Background())
		server.Close()
	})

	first := dialWebsocket(t, server.URL, mgr.Path())
	waitForSignal(t, connected)
	if err := mgr.Stop(context.Background()); err != nil {
		t.Fatalf("stop manager: %v", err)
	}
	waitForActiveConnections(t, mgr, 0)
	_ = first.Close()

	response := dialRejectedWebsocket(t, server.URL, mgr.Path())
	assertAdmissionResponse(t, response, http.StatusServiceUnavailable, "websocket_admission_closed")
}

func TestWebsocketStopInvalidatesHijackedHandshakeBeforeRegistration(t *testing.T) {
	factoryEntered := make(chan struct{})
	releaseFactory := make(chan struct{})
	var connected atomic.Int32
	mgr := NewManager(Options{
		MaxConnections:      1,
		MaxConnectionsPerIP: 1,
		ProviderFactory: func(*http.Request) (string, error) {
			close(factoryEntered)
			<-releaseFactory
			return "stopped-handshake", nil
		},
		OnConnected: func(string) {
			connected.Add(1)
		},
	})
	server := httptest.NewServer(mgr.Handler())
	t.Cleanup(server.Close)

	dialResult := dialWebsocketResultAsync(server.URL, mgr.Path())
	waitForSignal(t, factoryEntered)
	client := requireWebsocketDial(t, <-dialResult)

	if err := mgr.Stop(context.Background()); err != nil {
		t.Fatalf("stop manager: %v", err)
	}
	waitForActiveConnections(t, mgr, 0)
	if got := connected.Load(); got != 0 {
		t.Fatalf("connected callbacks after stop = %d, want 0", got)
	}
	if _, _, err := client.ReadMessage(); err == nil {
		t.Fatal("hijacked websocket remained open after manager stop")
	}
	_ = client.Close()
	close(releaseFactory)
}

func TestAuthenticationEnableRejectsStaleConditionalDecision(t *testing.T) {
	var authCalls atomic.Int32
	mgr := NewManager(Options{
		AuthRequired: false,
		Authenticate: func(*http.Request) error {
			authCalls.Add(1)
			return errors.New("missing credentials")
		},
	})
	decisionMade := make(chan struct{})
	resume := make(chan struct{})
	inner := mgr.Handler()
	outer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(decisionMade) // Simulate conditionalAuth observing ws-auth=false.
		<-resume
		inner.ServeHTTP(w, r)
	})
	server := httptest.NewServer(outer)
	t.Cleanup(func() {
		_ = mgr.Stop(context.Background())
		server.Close()
	})

	dialResult := dialWebsocketResultAsync(server.URL, mgr.Path())
	waitForSignal(t, decisionMade)
	mgr.SetAuthenticationRequired(true)
	close(resume)
	result := <-dialResult
	if result.conn != nil {
		_ = result.conn.Close()
		t.Fatal("stale unauthenticated request upgraded successfully")
	}
	if result.err == nil || result.response == nil {
		t.Fatalf("stale auth decision result = (err=%v, response=%v), want HTTP rejection", result.err, result.response)
	}
	assertAdmissionResponse(t, result.response, http.StatusUnauthorized, "websocket_authentication_failed")
	if got := authCalls.Load(); got != 1 {
		t.Fatalf("manager authentication calls = %d, want 1", got)
	}
}

func TestAuthenticationEnableInvalidatesHandshakeBeforeRegistration(t *testing.T) {
	factoryEntered := make(chan struct{})
	releaseFactory := make(chan struct{})
	var connected atomic.Int32
	mgr := NewManager(Options{
		AuthRequired: false,
		Authenticate: func(*http.Request) error {
			return errors.New("missing credentials")
		},
		ProviderFactory: func(*http.Request) (string, error) {
			close(factoryEntered)
			<-releaseFactory
			return "stale-auth", nil
		},
		OnConnected: func(string) {
			connected.Add(1)
		},
	})
	server := httptest.NewServer(mgr.Handler())
	t.Cleanup(func() {
		_ = mgr.Stop(context.Background())
		server.Close()
	})

	dialResult := dialWebsocketResultAsync(server.URL, mgr.Path())
	waitForSignal(t, factoryEntered)
	client := requireWebsocketDial(t, <-dialResult)
	mgr.SetAuthenticationRequired(true)
	waitForActiveConnections(t, mgr, 0)
	if got := connected.Load(); got != 0 {
		t.Fatalf("connected callbacks after auth epoch change = %d, want 0", got)
	}
	if _, _, err := client.ReadMessage(); err == nil {
		t.Fatal("stale unauthenticated websocket remained open after auth enable")
	}
	_ = client.Close()
	close(releaseFactory)
}

func TestAuthenticationDisableKeepsExistingSession(t *testing.T) {
	mgr := NewManager(Options{
		AuthRequired: true,
		Authenticate: func(*http.Request) error {
			return nil
		},
	})
	server := httptest.NewServer(mgr.Handler())
	t.Cleanup(func() {
		_ = mgr.Stop(context.Background())
		server.Close()
	})

	client := dialWebsocket(t, server.URL, mgr.Path())
	waitForActiveConnections(t, mgr, 1)
	mgr.SetAuthenticationRequired(false)
	waitForActiveConnections(t, mgr, 1)
	if err := client.WriteJSON(Message{ID: "ping", Type: MessageTypePing}); err != nil {
		t.Fatalf("write to existing websocket after auth disable: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close websocket: %v", err)
	}
	waitForActiveConnections(t, mgr, 0)
}

func TestConnectionLimiterConcurrentAcquireRelease(t *testing.T) {
	mgr := NewManager(Options{MaxConnections: 32, MaxConnectionsPerIP: 4})
	const workers = 64
	const attempts = 100
	start := make(chan struct{})
	var accepted atomic.Int64
	var wg sync.WaitGroup
	wg.Add(workers)
	for worker := 0; worker < workers; worker++ {
		go func(worker int) {
			defer wg.Done()
			<-start
			for attempt := 0; attempt < attempts; attempt++ {
				addr := fmt.Sprintf("192.0.2.%d:%d", worker%16+1, 1000+worker)
				lease, err := mgr.acquireConnection(addr)
				if err != nil {
					runtime.Gosched()
					continue
				}
				accepted.Add(1)
				runtime.Gosched()
				lease.release()
				lease.release()
			}
		}(worker)
	}
	close(start)
	wg.Wait()

	if accepted.Load() == 0 {
		t.Fatal("connection limiter accepted no test leases")
	}
	active, perIP := connectionCounts(mgr)
	if active != 0 || perIP != 0 {
		t.Fatalf("connection counts after concurrent release = (%d, %d), want (0, 0)", active, perIP)
	}
}

func TestSetConnectionLimitsAppliesWithoutDisconnectingExistingSessions(t *testing.T) {
	mgr := NewManager(Options{MaxConnections: 4, MaxConnectionsPerIP: 4})
	first, err := mgr.acquireConnection("192.0.2.1:1000")
	if err != nil {
		t.Fatalf("acquire first connection: %v", err)
	}
	second, err := mgr.acquireConnection("192.0.2.2:1000")
	if err != nil {
		t.Fatalf("acquire second connection: %v", err)
	}

	mgr.SetConnectionLimits(1, 1)
	if _, err = mgr.acquireConnection("192.0.2.3:1000"); !errors.Is(err, errConnectionLimit) {
		t.Fatalf("acquire above lowered limit error = %v, want %v", err, errConnectionLimit)
	}
	first.release()
	second.release()
	third, err := mgr.acquireConnection("192.0.2.3:1000")
	if err != nil {
		t.Fatalf("acquire after existing connections drained: %v", err)
	}
	third.release()
}

func TestConnectionLimitDefaultsAndClientIPNormalization(t *testing.T) {
	global, perIP := normalizeConnectionLimits(0, 0)
	if global != defaultMaxConnections || perIP != defaultMaxConnectionsPerIP {
		t.Fatalf("default limits = (%d, %d), want (%d, %d)", global, perIP, defaultMaxConnections, defaultMaxConnectionsPerIP)
	}
	if got := connectionClientIP("[2001:db8::1]:443"); got != "2001:db8::1" {
		t.Fatalf("IPv6 client IP = %q, want %q", got, "2001:db8::1")
	}
	if got := connectionClientIP(""); got != "unknown" {
		t.Fatalf("empty client IP = %q, want unknown", got)
	}
}

func dialWebsocket(t *testing.T, serverURL, path string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(serverURL, "http") + path
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	return conn
}

func dialRejectedWebsocket(t *testing.T, serverURL, path string) *http.Response {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(serverURL, "http") + path
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if conn != nil {
		_ = conn.Close()
	}
	if err == nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		t.Fatal("websocket connection unexpectedly passed admission")
	}
	if response == nil {
		t.Fatalf("rejected websocket response is nil: %v", err)
	}
	return response
}

func assertLimitResponse(t *testing.T, response *http.Response, code string) {
	t.Helper()
	if response.Header.Get("Retry-After") != "1" {
		t.Fatalf("Retry-After = %q, want 1", response.Header.Get("Retry-After"))
	}
	assertAdmissionResponse(t, response, http.StatusTooManyRequests, code)
}

func assertAdmissionResponse(t *testing.T, response *http.Response, status int, code string) {
	t.Helper()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read limit response: %v", err)
	}
	if response.StatusCode != status {
		t.Fatalf("admission response status = %d, want %d", response.StatusCode, status)
	}
	if !strings.Contains(string(body), `"code":"`+code+`"`) {
		t.Fatalf("admission response body = %q, want code %q", body, code)
	}
}

type websocketDialResult struct {
	conn     *websocket.Conn
	response *http.Response
	err      error
}

func dialWebsocketResultAsync(serverURL, path string) <-chan websocketDialResult {
	result := make(chan websocketDialResult, 1)
	go func() {
		conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(serverURL, "http")+path, nil)
		result <- websocketDialResult{conn: conn, response: response, err: err}
	}()
	return result
}

func requireWebsocketDial(t *testing.T, result websocketDialResult) *websocket.Conn {
	t.Helper()
	if result.response != nil && result.response.Body != nil {
		defer result.response.Body.Close()
	}
	if result.err != nil || result.conn == nil {
		t.Fatalf("dial websocket: %v", result.err)
	}
	return result.conn
}

func connectionCounts(mgr *Manager) (int, int) {
	mgr.limitMutex.Lock()
	defer mgr.limitMutex.Unlock()
	return mgr.activeConnections, len(mgr.connectionsByIP)
}

func waitForActiveConnections(t *testing.T, mgr *Manager, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		active, _ := connectionCounts(mgr)
		if active == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	active, _ := connectionCounts(mgr)
	t.Fatalf("active websocket connections = %d, want %d", active, want)
}

func waitForSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for websocket connection")
	}
}
