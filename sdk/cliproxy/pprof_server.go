package cliproxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	log "github.com/sirupsen/logrus"
)

const (
	pprofServerTTL       = 30 * time.Minute
	pprofShutdownTimeout = 5 * time.Second
)

type pprofServer struct {
	lifecycleMu sync.Mutex
	mu          sync.Mutex
	server      *http.Server
	listener    net.Listener
	timer       *time.Timer
	addr        string
	enabled     bool
	closed      bool
	generation  uint64
	ttl         time.Duration
}

func newPprofServer() *pprofServer {
	return newPprofServerWithTTL(pprofServerTTL)
}

func newPprofServerWithTTL(ttl time.Duration) *pprofServer {
	if ttl <= 0 {
		ttl = pprofServerTTL
	}
	return &pprofServer{ttl: ttl}
}

func (s *Service) applyPprofConfig(cfg *config.Config) {
	if s == nil || cfg == nil {
		return
	}
	s.cfgMu.Lock()
	if s.pprofServer == nil {
		s.pprofServer = newPprofServer()
	}
	server := s.pprofServer
	s.cfgMu.Unlock()

	if errApply := server.Apply(cfg); errApply != nil {
		log.Errorf("pprof configuration rejected: %v", errApply)
	}
}

func (s *Service) shutdownPprof(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.cfgMu.Lock()
	if s.pprofServer == nil {
		s.pprofServer = newPprofServer()
	}
	server := s.pprofServer
	s.cfgMu.Unlock()
	return server.Shutdown(ctx)
}

func (p *pprofServer) Apply(cfg *config.Config) error {
	if p == nil || cfg == nil {
		return nil
	}
	requestedAddr := strings.TrimSpace(cfg.Pprof.Addr)
	if requestedAddr == "" {
		requestedAddr = config.DefaultPprofAddr
	}

	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return errors.New("pprof server has been shut down")
	}

	if !cfg.Pprof.Enable {
		server, listener, timer, addr := p.detach(requestedAddr)
		stopTimer(timer)
		if server != nil {
			p.stopServer(server, listener, addr, "disabled")
		}
		return nil
	}

	addr, errValidate := validatePprofAddr(requestedAddr)
	if errValidate != nil {
		server, listener, timer, currentAddr := p.detach(requestedAddr)
		stopTimer(timer)
		if server != nil {
			p.stopServer(server, listener, currentAddr, "invalid configuration")
		}
		return errValidate
	}

	p.mu.Lock()
	if p.server != nil && p.addr == addr {
		server := p.server
		oldTimer := p.timer
		p.generation++
		generation := p.generation
		p.enabled = true
		p.timer = time.AfterFunc(p.ttl, func() {
			p.expire(generation, server, addr)
		})
		p.mu.Unlock()
		stopTimer(oldTimer)
		return nil
	}
	p.mu.Unlock()

	currentServer, currentListener, currentTimer, currentAddr := p.detach(addr)
	stopTimer(currentTimer)
	if currentServer != nil {
		p.stopServer(currentServer, currentListener, currentAddr, "restarted")
	}

	listener, errListen := net.Listen("tcp", addr)
	if errListen != nil {
		return fmt.Errorf("listen on pprof address %q: %w", addr, errListen)
	}

	server := &http.Server{
		Addr:              listener.Addr().String(),
		Handler:           newPprofMux(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	p.mu.Lock()
	p.generation++
	generation := p.generation
	p.server = server
	p.listener = listener
	p.addr = addr
	p.enabled = true
	p.timer = time.AfterFunc(p.ttl, func() {
		p.expire(generation, server, addr)
	})
	p.mu.Unlock()

	log.Infof("pprof server started on %s; automatic shutdown in %s", server.Addr, p.ttl)
	go p.serve(server, listener)
	return nil
}

func (p *pprofServer) Shutdown(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()

	server, listener, timer, addr := p.detach("")
	stopTimer(timer)
	if server == nil {
		return nil
	}
	return p.stopServerWithContext(ctx, server, listener, addr, "shutdown")
}

func (p *pprofServer) detach(nextAddr string) (*http.Server, net.Listener, *time.Timer, string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	server := p.server
	listener := p.listener
	timer := p.timer
	addr := p.addr
	p.server = nil
	p.listener = nil
	p.timer = nil
	p.addr = nextAddr
	p.enabled = false
	p.generation++
	return server, listener, timer, addr
}

func (p *pprofServer) expire(generation uint64, server *http.Server, addr string) {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()

	p.mu.Lock()
	if p.generation != generation || p.server != server {
		p.mu.Unlock()
		return
	}
	listener := p.listener
	p.server = nil
	p.listener = nil
	p.timer = nil
	p.enabled = false
	p.generation++
	p.mu.Unlock()

	p.stopServer(server, listener, addr, "TTL expired")
}

func (p *pprofServer) serve(server *http.Server, listener net.Listener) {
	errServe := server.Serve(listener)
	if errServe != nil && !errors.Is(errServe, http.ErrServerClosed) && !errors.Is(errServe, net.ErrClosed) {
		log.Errorf("pprof server failed on %s: %v", server.Addr, errServe)
	}

	p.mu.Lock()
	if p.server != server {
		p.mu.Unlock()
		return
	}
	timer := p.timer
	p.server = nil
	p.listener = nil
	p.timer = nil
	p.enabled = false
	p.generation++
	p.mu.Unlock()
	stopTimer(timer)
}

func validatePprofAddr(addr string) (string, error) {
	host, port, errSplit := net.SplitHostPort(addr)
	if errSplit != nil {
		return "", fmt.Errorf("invalid pprof TCP address %q: %w", addr, errSplit)
	}
	if _, errPort := strconv.ParseUint(port, 10, 16); errPort != nil {
		return "", fmt.Errorf("invalid pprof TCP port %q: %w", port, errPort)
	}
	if strings.EqualFold(host, "localhost") {
		return net.JoinHostPort("127.0.0.1", port), nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("pprof address %q must use localhost or a loopback IP", addr)
	}
	return net.JoinHostPort(ip.String(), port), nil
}

func stopTimer(timer *time.Timer) {
	if timer != nil {
		timer.Stop()
	}
}

func (p *pprofServer) stopServer(server *http.Server, listener net.Listener, addr string, reason string) {
	_ = p.stopServerWithContext(context.Background(), server, listener, addr, reason)
}

func (p *pprofServer) stopServerWithContext(ctx context.Context, server *http.Server, listener net.Listener, addr string, reason string) error {
	if server == nil {
		return nil
	}
	if listener != nil {
		if errCloseListener := listener.Close(); errCloseListener != nil && !errors.Is(errCloseListener, net.ErrClosed) {
			log.Errorf("pprof listener close failed on %s: %v", addr, errCloseListener)
		}
	}
	stopCtx := ctx
	if stopCtx == nil {
		stopCtx = context.Background()
	}
	stopCtx, cancel := context.WithTimeout(stopCtx, pprofShutdownTimeout)
	defer cancel()
	if errStop := server.Shutdown(stopCtx); errStop != nil {
		errClose := server.Close()
		if errClose != nil && !errors.Is(errClose, http.ErrServerClosed) {
			errStop = errors.Join(errStop, errClose)
		}
		log.Errorf("pprof server stop failed on %s: %v", addr, errStop)
		return errStop
	}
	log.Infof("pprof server stopped on %s (%s)", addr, reason)
	return nil
}

func newPprofMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.Handle("/debug/pprof/allocs", pprof.Handler("allocs"))
	mux.Handle("/debug/pprof/block", pprof.Handler("block"))
	mux.Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
	mux.Handle("/debug/pprof/heap", pprof.Handler("heap"))
	mux.Handle("/debug/pprof/mutex", pprof.Handler("mutex"))
	mux.Handle("/debug/pprof/threadcreate", pprof.Handler("threadcreate"))
	return mux
}
