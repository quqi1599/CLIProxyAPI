package cliproxy

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestValidatePprofAddr(t *testing.T) {
	t.Parallel()

	valid := map[string]string{
		"localhost:8316": "127.0.0.1:8316",
		"127.0.0.1:0":    "127.0.0.1:0",
		"[::1]:8316":     "[::1]:8316",
	}
	for input, want := range valid {
		input, want := input, want
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			got, errValidate := validatePprofAddr(input)
			if errValidate != nil {
				t.Fatalf("validatePprofAddr(%q) returned error: %v", input, errValidate)
			}
			if got != want {
				t.Fatalf("validatePprofAddr(%q) = %q, want %q", input, got, want)
			}
		})
	}

	invalid := []string{
		"0.0.0.0:8316",
		"[::]:8316",
		"192.0.2.10:8316",
		"example.com:8316",
		"unresolved.invalid:8316",
		":8316",
		"127.0.0.1",
		"127.0.0.1:http",
	}
	for _, input := range invalid {
		input := input
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			if _, errValidate := validatePprofAddr(input); errValidate == nil {
				t.Fatalf("validatePprofAddr(%q) unexpectedly succeeded", input)
			}
		})
	}
}

func TestPprofApplyRejectsUnsafeAddressAndStopsCurrentServer(t *testing.T) {
	p := newPprofServerWithTTL(time.Hour)
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	if errApply := p.Apply(pprofConfig(true, "127.0.0.1:0")); errApply != nil {
		t.Fatalf("start pprof server: %v", errApply)
	}
	server, _, _, _, _ := pprofSnapshot(p)
	if server == nil {
		t.Fatal("pprof server was not started")
	}
	listenAddr := server.Addr

	if errApply := p.Apply(pprofConfig(true, "0.0.0.0:8316")); errApply == nil {
		t.Fatal("unsafe pprof address unexpectedly succeeded")
	}
	server, timer, _, _, enabled := pprofSnapshot(p)
	if server != nil || timer != nil || enabled {
		t.Fatalf("unsafe config left pprof active: server=%v timer=%v enabled=%t", server, timer, enabled)
	}
	assertDialFails(t, listenAddr)
}

func TestPprofTTLExpiresServer(t *testing.T) {
	p := newPprofServerWithTTL(10 * time.Millisecond)
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	if errApply := p.Apply(pprofConfig(true, "127.0.0.1:0")); errApply != nil {
		t.Fatalf("start pprof server: %v", errApply)
	}
	server, _, _, _, _ := pprofSnapshot(p)
	if server == nil {
		t.Fatal("pprof server was not started")
	}
	listenAddr := server.Addr

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		server, timer, _, _, enabled := pprofSnapshot(p)
		if server == nil && timer == nil && !enabled {
			assertDialFails(t, listenAddr)
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("pprof server did not expire")
}

func TestPprofReapplyInvalidatesOldTTLGeneration(t *testing.T) {
	p := newPprofServerWithTTL(time.Hour)
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	cfg := pprofConfig(true, "127.0.0.1:0")

	if errApply := p.Apply(cfg); errApply != nil {
		t.Fatalf("start pprof server: %v", errApply)
	}
	server, firstTimer, addr, firstGeneration, _ := pprofSnapshot(p)
	if errApply := p.Apply(cfg); errApply != nil {
		t.Fatalf("reapply pprof config: %v", errApply)
	}
	reappliedServer, secondTimer, _, secondGeneration, _ := pprofSnapshot(p)
	if reappliedServer != server {
		t.Fatal("same-address reapply restarted the server")
	}
	if firstTimer == secondTimer || secondGeneration <= firstGeneration {
		t.Fatal("same-address reapply did not replace the TTL generation")
	}

	p.expire(firstGeneration, server, addr)
	currentServer, _, _, _, enabled := pprofSnapshot(p)
	if currentServer != server || !enabled {
		t.Fatal("stale TTL generation stopped the current server")
	}

	p.expire(secondGeneration, server, addr)
	currentServer, timer, _, _, enabled := pprofSnapshot(p)
	if currentServer != nil || timer != nil || enabled {
		t.Fatal("current TTL generation did not stop the server")
	}
}

func TestPprofAddressSwitchDisableAndConcurrentShutdown(t *testing.T) {
	p := newPprofServerWithTTL(time.Hour)
	firstAddr := reserveLoopbackAddr(t)
	secondAddr := reserveLoopbackAddr(t)

	if errApply := p.Apply(pprofConfig(true, firstAddr)); errApply != nil {
		t.Fatalf("start first pprof server: %v", errApply)
	}
	firstServer, _, firstConfiguredAddr, firstGeneration, _ := pprofSnapshot(p)
	if errApply := p.Apply(pprofConfig(true, secondAddr)); errApply != nil {
		t.Fatalf("switch pprof address: %v", errApply)
	}
	secondServer, _, _, _, _ := pprofSnapshot(p)
	if firstServer == secondServer {
		t.Fatal("address switch did not replace the server")
	}
	assertDialFails(t, firstServer.Addr)
	p.expire(firstGeneration, firstServer, firstConfiguredAddr)
	currentServer, _, _, _, enabled := pprofSnapshot(p)
	if currentServer != secondServer || !enabled {
		t.Fatal("old address TTL generation stopped the replacement server")
	}
	if errApply := p.Apply(pprofConfig(false, secondAddr)); errApply != nil {
		t.Fatalf("disable pprof server: %v", errApply)
	}
	disabledServer, disabledTimer, _, _, enabled := pprofSnapshot(p)
	if disabledServer != nil || disabledTimer != nil || enabled {
		t.Fatal("disable left pprof active")
	}
	assertDialFails(t, secondServer.Addr)
	if errApply := p.Apply(pprofConfig(true, secondAddr)); errApply != nil {
		t.Fatalf("re-enable pprof server: %v", errApply)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 10; i++ {
			_ = p.Apply(pprofConfig(true, secondAddr))
		}
	}()
	for i := 0; i < 10; i++ {
		_ = p.Shutdown(context.Background())
	}
	<-done

	if errShutdown := p.Shutdown(context.Background()); errShutdown != nil {
		t.Fatalf("final pprof shutdown: %v", errShutdown)
	}
	if errApply := p.Apply(pprofConfig(true, secondAddr)); errApply == nil {
		t.Fatal("pprof server restarted after terminal shutdown")
	}
	server, timer, _, _, enabled := pprofSnapshot(p)
	if server != nil || timer != nil || enabled {
		t.Fatalf("shutdown left pprof active: server=%v timer=%v enabled=%t", server, timer, enabled)
	}
}

func TestServicePprofApplyAndShutdownRace(t *testing.T) {
	service := &Service{}
	cfg := pprofConfig(true, "127.0.0.1:0")
	start := make(chan struct{})
	done := make(chan struct{}, 2)

	go func() {
		<-start
		service.applyPprofConfig(cfg)
		done <- struct{}{}
	}()
	go func() {
		<-start
		_ = service.shutdownPprof(context.Background())
		done <- struct{}{}
	}()
	close(start)
	<-done
	<-done

	if errShutdown := service.shutdownPprof(context.Background()); errShutdown != nil {
		t.Fatalf("final service pprof shutdown: %v", errShutdown)
	}
	service.cfgMu.RLock()
	p := service.pprofServer
	service.cfgMu.RUnlock()
	server, timer, _, _, enabled := pprofSnapshot(p)
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if server != nil || timer != nil || enabled || !closed {
		t.Fatalf("service shutdown left pprof active: server=%v timer=%v enabled=%t closed=%t", server, timer, enabled, closed)
	}
}

func pprofConfig(enabled bool, addr string) *config.Config {
	return &config.Config{Pprof: config.PprofConfig{Enable: enabled, Addr: addr}}
}

func pprofSnapshot(p *pprofServer) (*http.Server, *time.Timer, string, uint64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.server, p.timer, p.addr, p.generation, p.enabled
}

func reserveLoopbackAddr(t *testing.T) string {
	t.Helper()
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("reserve loopback address: %v", errListen)
	}
	addr := listener.Addr().String()
	if errClose := listener.Close(); errClose != nil {
		t.Fatalf("release loopback address: %v", errClose)
	}
	return addr
}

func assertDialFails(t *testing.T, addr string) {
	t.Helper()
	conn, errDial := net.DialTimeout("tcp", addr, 100*time.Millisecond)
	if errDial == nil {
		_ = conn.Close()
		t.Fatalf("unexpectedly connected to stopped pprof server at %s", addr)
	}
}
