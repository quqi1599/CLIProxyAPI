package wsrelay

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	defaultMaxConnections      = 128
	defaultMaxConnectionsPerIP = 16
	defaultMaxInboundBytes     = 128 << 20
)

var (
	errConnectionLimit      = errors.New("wsrelay: connection limit reached")
	errConnectionLimitPerIP = errors.New("wsrelay: connection limit reached for client IP")
	errAdmissionClosed      = errors.New("wsrelay: admission closed")
	errAdmissionChanged     = errors.New("wsrelay: admission changed during handshake")
	errAuthenticationFailed = errors.New("wsrelay: authentication failed")
)

// Manager exposes a websocket endpoint that proxies Gemini requests to
// connected clients.
type Manager struct {
	path      string
	upgrader  websocket.Upgrader
	sessions  map[string]*session
	sessMutex sync.RWMutex
	sessRuns  sync.WaitGroup
	inbound   *inboundBudget

	limitMutex          sync.Mutex
	registrationMutex   sync.Mutex
	maxConnections      int
	maxConnectionsPerIP int
	activeConnections   int
	connectionsByIP     map[string]int
	leases              map[*connectionLease]struct{}
	admissionClosed     bool
	admissionEpoch      uint64
	authRequired        bool
	authenticate        func(*http.Request) error

	providerFactory func(*http.Request) (string, error)
	onConnected     func(string)
	onDisconnected  func(string, error)

	logDebugf func(string, ...any)
	logInfof  func(string, ...any)
	logWarnf  func(string, ...any)
}

// Options configures a Manager instance.
type Options struct {
	Path                string
	MaxConnections      int
	MaxConnectionsPerIP int
	AuthRequired        bool
	Authenticate        func(*http.Request) error
	ProviderFactory     func(*http.Request) (string, error)
	OnConnected         func(string)
	OnDisconnected      func(string, error)
	LogDebugf           func(string, ...any)
	LogInfof            func(string, ...any)
	LogWarnf            func(string, ...any)
}

// NewManager builds a websocket relay manager with the supplied options.
func NewManager(opts Options) *Manager {
	path := strings.TrimSpace(opts.Path)
	if path == "" {
		path = "/v1/ws"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	maxConnections, maxConnectionsPerIP := normalizeConnectionLimits(opts.MaxConnections, opts.MaxConnectionsPerIP)
	mgr := &Manager{
		path:                path,
		sessions:            make(map[string]*session),
		inbound:             newInboundBudget(defaultMaxInboundBytes, maxInboundMessageLen),
		maxConnections:      maxConnections,
		maxConnectionsPerIP: maxConnectionsPerIP,
		connectionsByIP:     make(map[string]int),
		leases:              make(map[*connectionLease]struct{}),
		admissionEpoch:      1,
		authRequired:        opts.AuthRequired,
		authenticate:        opts.Authenticate,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
		providerFactory: opts.ProviderFactory,
		onConnected:     opts.OnConnected,
		onDisconnected:  opts.OnDisconnected,
		logDebugf:       opts.LogDebugf,
		logInfof:        opts.LogInfof,
		logWarnf:        opts.LogWarnf,
	}
	if mgr.logDebugf == nil {
		mgr.logDebugf = func(string, ...any) {}
	}
	if mgr.logInfof == nil {
		mgr.logInfof = func(string, ...any) {}
	}
	if mgr.logWarnf == nil {
		mgr.logWarnf = func(s string, args ...any) { fmt.Printf(s+"\n", args...) }
	}
	return mgr
}

// SetAuthenticationRequired updates websocket admission without reopening a
// manager that has been stopped. Enabling authentication disconnects existing
// sessions; disabling it leaves them connected.
func (m *Manager) SetAuthenticationRequired(required bool) {
	if m == nil {
		return
	}

	m.registrationMutex.Lock()
	defer m.registrationMutex.Unlock()
	m.limitMutex.Lock()
	if m.authRequired == required {
		m.limitMutex.Unlock()
		return
	}
	m.authRequired = required
	m.admissionEpoch++
	staleSessions, staleConnections := m.staleHandshakesLocked(m.admissionEpoch, false)
	m.limitMutex.Unlock()

	for _, sess := range staleSessions {
		sess.cleanup(errAdmissionChanged)
	}
	for _, conn := range staleConnections {
		_ = conn.Close()
	}
	if !required {
		return
	}

	sessions := m.takeSessions()
	for _, sess := range sessions {
		if sess != nil {
			sess.cleanup(errors.New("wsrelay: authentication enabled"))
		}
	}
}

// Path returns the HTTP path the manager expects for websocket upgrades.
func (m *Manager) Path() string {
	if m == nil {
		return "/v1/ws"
	}
	return m.path
}

// Handler exposes an http.Handler that upgrades connections to websocket sessions.
func (m *Manager) Handler() http.Handler {
	return http.HandlerFunc(m.handleWebsocket)
}

// SetConnectionLimits updates admission limits for new websocket connections.
// Existing sessions remain connected; lower limits take effect as they drain.
func (m *Manager) SetConnectionLimits(maxConnections, maxConnectionsPerIP int) {
	if m == nil {
		return
	}
	maxConnections, maxConnectionsPerIP = normalizeConnectionLimits(maxConnections, maxConnectionsPerIP)
	m.limitMutex.Lock()
	m.maxConnections = maxConnections
	m.maxConnectionsPerIP = maxConnectionsPerIP
	m.limitMutex.Unlock()
}

// Stop closes admission permanently and then closes every registered or
// hijacked websocket owned by the manager.
func (m *Manager) Stop(_ context.Context) error {
	if m == nil {
		return nil
	}

	m.registrationMutex.Lock()
	defer m.registrationMutex.Unlock()
	m.limitMutex.Lock()
	if !m.admissionClosed {
		m.admissionClosed = true
		m.admissionEpoch++
	}
	staleSessions, connections := m.staleHandshakesLocked(m.admissionEpoch, true)
	m.limitMutex.Unlock()

	for _, sess := range staleSessions {
		sess.cleanup(errors.New("wsrelay: manager stopped"))
	}
	for _, conn := range connections {
		_ = conn.Close()
	}

	sessions := m.takeSessions()
	for _, sess := range sessions {
		if sess != nil {
			sess.cleanup(errors.New("wsrelay: manager stopped"))
		}
	}
	m.sessRuns.Wait()
	return nil
}

func (m *Manager) takeSessions() []*session {
	m.sessMutex.Lock()
	sessions := make([]*session, 0, len(m.sessions))
	for _, sess := range m.sessions {
		sessions = append(sessions, sess)
	}
	m.sessions = make(map[string]*session)
	m.sessMutex.Unlock()
	return sessions
}

// handleWebsocket upgrades the connection and wires the session into the pool.
func (m *Manager) handleWebsocket(w http.ResponseWriter, r *http.Request) {
	expectedPath := m.Path()
	if expectedPath != "" && r.URL != nil && r.URL.Path != expectedPath {
		http.NotFound(w, r)
		return
	}
	if !strings.EqualFold(r.Method, http.MethodGet) {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	lease, err := m.authorizeAndAcquire(r)
	if err != nil {
		writeAdmissionError(w, err)
		return
	}
	conn, err := m.upgrader.Upgrade(w, r, nil)
	if err != nil {
		lease.release()
		m.logWarnf("wsrelay: upgrade failed: %v", err)
		return
	}
	if !lease.attach(conn) {
		lease.release()
		return
	}
	s := newSession(conn, m, randomProviderName(), lease)
	if !lease.bindSession(s) {
		s.cleanup(errAdmissionChanged)
		return
	}
	if m.providerFactory != nil {
		name, err := m.providerFactory(r)
		if err != nil {
			s.cleanup(err)
			return
		}
		if strings.TrimSpace(name) != "" {
			s.provider = strings.ToLower(name)
		}
	}
	if s.provider == "" {
		s.provider = strings.ToLower(s.id)
	}
	if !m.registerSession(s) {
		s.cleanup(errAdmissionChanged)
		return
	}
}

type connectionLease struct {
	manager       *Manager
	clientIP      string
	epoch         uint64
	conn          *websocket.Conn
	session       *session
	authenticated bool
	registered    bool
	once          sync.Once
}

func (m *Manager) authorizeAndAcquire(r *http.Request) (*connectionLease, error) {
	if r == nil {
		return nil, errAuthenticationFailed
	}
	clientIP := connectionClientIP(r.RemoteAddr)
	for {
		m.limitMutex.Lock()
		if m.admissionClosed {
			m.limitMutex.Unlock()
			return nil, errAdmissionClosed
		}
		epoch := m.admissionEpoch
		required := m.authRequired
		authenticate := m.authenticate
		if !required {
			lease, err := m.acquireConnectionLocked(clientIP, epoch, false)
			m.limitMutex.Unlock()
			return lease, err
		}
		m.limitMutex.Unlock()

		if authenticate == nil {
			return nil, errAuthenticationFailed
		}
		if err := authenticate(r); err != nil {
			return nil, fmt.Errorf("%w: %w", errAuthenticationFailed, err)
		}

		m.limitMutex.Lock()
		if m.admissionClosed {
			m.limitMutex.Unlock()
			return nil, errAdmissionClosed
		}
		if epoch != m.admissionEpoch || required != m.authRequired {
			m.limitMutex.Unlock()
			continue
		}
		lease, err := m.acquireConnectionLocked(clientIP, epoch, true)
		m.limitMutex.Unlock()
		return lease, err
	}
}

func (m *Manager) acquireConnection(remoteAddr string) (*connectionLease, error) {
	clientIP := connectionClientIP(remoteAddr)
	m.limitMutex.Lock()
	defer m.limitMutex.Unlock()
	if m.admissionClosed {
		return nil, errAdmissionClosed
	}
	return m.acquireConnectionLocked(clientIP, m.admissionEpoch, false)
}

func (m *Manager) acquireConnectionLocked(clientIP string, epoch uint64, authenticated bool) (*connectionLease, error) {
	if m.activeConnections >= m.maxConnections {
		return nil, errConnectionLimit
	}
	if m.connectionsByIP[clientIP] >= m.maxConnectionsPerIP {
		return nil, errConnectionLimitPerIP
	}
	m.activeConnections++
	m.connectionsByIP[clientIP]++
	lease := &connectionLease{manager: m, clientIP: clientIP, epoch: epoch, authenticated: authenticated}
	m.leases[lease] = struct{}{}
	return lease, nil
}

func (l *connectionLease) attach(conn *websocket.Conn) bool {
	if l == nil || l.manager == nil || conn == nil {
		return false
	}
	m := l.manager
	m.limitMutex.Lock()
	if m.admissionClosed || l.epoch != m.admissionEpoch {
		m.limitMutex.Unlock()
		_ = conn.Close()
		return false
	}
	l.conn = conn
	m.limitMutex.Unlock()
	return true
}

func (l *connectionLease) bindSession(s *session) bool {
	if l == nil || l.manager == nil || s == nil {
		return false
	}
	m := l.manager
	m.limitMutex.Lock()
	_, active := m.leases[l]
	valid := active && !m.admissionClosed && l.epoch == m.admissionEpoch && (!m.authRequired || l.authenticated)
	if valid {
		l.session = s
	}
	m.limitMutex.Unlock()
	return valid
}

func (l *connectionLease) release() {
	if l == nil || l.manager == nil {
		return
	}
	l.once.Do(func() {
		m := l.manager
		m.limitMutex.Lock()
		delete(m.leases, l)
		l.conn = nil
		l.session = nil
		if m.activeConnections > 0 {
			m.activeConnections--
		}
		if count := m.connectionsByIP[l.clientIP]; count <= 1 {
			delete(m.connectionsByIP, l.clientIP)
		} else {
			m.connectionsByIP[l.clientIP] = count - 1
		}
		m.limitMutex.Unlock()
	})
}

func (l *connectionLease) wasRegistered() bool {
	if l == nil || l.manager == nil {
		return false
	}
	m := l.manager
	m.limitMutex.Lock()
	registered := l.registered
	m.limitMutex.Unlock()
	return registered
}

func (m *Manager) staleHandshakesLocked(currentEpoch uint64, includeRegistered bool) ([]*session, []*websocket.Conn) {
	sessions := make([]*session, 0)
	connections := make([]*websocket.Conn, 0)
	for lease := range m.leases {
		if lease == nil || lease.epoch == currentEpoch || (!includeRegistered && lease.registered) {
			continue
		}
		if lease.session != nil {
			sessions = append(sessions, lease.session)
			continue
		}
		if lease.conn == nil {
			continue
		}
		connections = append(connections, lease.conn)
	}
	return sessions, connections
}

func (m *Manager) registerSession(s *session) bool {
	if m == nil || s == nil || s.lease == nil {
		return false
	}
	m.registrationMutex.Lock()
	defer m.registrationMutex.Unlock()

	m.limitMutex.Lock()
	_, active := m.leases[s.lease]
	valid := active && !m.admissionClosed && s.lease.epoch == m.admissionEpoch && (!m.authRequired || s.lease.authenticated)
	if valid {
		s.lease.registered = true
	}
	m.limitMutex.Unlock()
	if !valid {
		return false
	}

	m.sessMutex.Lock()
	replaced := m.sessions[s.provider]
	m.sessions[s.provider] = s
	m.sessMutex.Unlock()
	if replaced != nil {
		replaced.cleanup(errors.New("replaced by new connection"))
	}
	if m.onConnected != nil {
		m.onConnected(s.provider)
	}
	m.sessRuns.Add(1)
	go func() {
		defer m.sessRuns.Done()
		s.run(context.Background())
	}()
	return true
}

func normalizeConnectionLimits(maxConnections, maxConnectionsPerIP int) (int, int) {
	if maxConnections <= 0 {
		maxConnections = defaultMaxConnections
	}
	if maxConnectionsPerIP <= 0 {
		maxConnectionsPerIP = defaultMaxConnectionsPerIP
	}
	if maxConnectionsPerIP > maxConnections {
		maxConnectionsPerIP = maxConnections
	}
	return maxConnections, maxConnectionsPerIP
}

func connectionClientIP(remoteAddr string) string {
	remoteAddr = strings.TrimSpace(remoteAddr)
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		if ip := net.ParseIP(host); ip != nil {
			return ip.String()
		}
		return strings.ToLower(host)
	}
	if ip := net.ParseIP(remoteAddr); ip != nil {
		return ip.String()
	}
	if remoteAddr == "" {
		return "unknown"
	}
	return strings.ToLower(remoteAddr)
}

func writeAdmissionError(w http.ResponseWriter, err error) {
	if errors.Is(err, errAdmissionClosed) {
		writeAdmissionErrorResponse(w, http.StatusServiceUnavailable, "websocket admission is closed", "websocket_admission_closed")
		return
	}
	if errors.Is(err, errAuthenticationFailed) {
		status := http.StatusUnauthorized
		var statusCoder interface{ HTTPStatusCode() int }
		if errors.As(err, &statusCoder) {
			if candidate := statusCoder.HTTPStatusCode(); candidate >= 400 && candidate <= 599 {
				status = candidate
			}
		}
		writeAdmissionErrorResponse(w, status, "websocket authentication failed", "websocket_authentication_failed")
		return
	}
	code := "websocket_connection_limit"
	message := "websocket connection limit reached"
	if errors.Is(err, errConnectionLimitPerIP) {
		code = "websocket_connection_limit_per_ip"
		message = "websocket connection limit reached for client IP"
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "1")
	writeAdmissionErrorResponse(w, http.StatusTooManyRequests, message, code)
}

func writeAdmissionErrorResponse(w http.ResponseWriter, status int, message, code string) {
	errorType := "request_error"
	if status == http.StatusTooManyRequests {
		errorType = "rate_limit_error"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(fmt.Sprintf(`{"error":{"message":%q,"type":%q,"code":%q}}`, message, errorType, code)))
}

// Send forwards the message to the specific provider connection and returns a
// channel yielding response messages. The receiver must call Message.Release
// after it finishes reading each message payload.
func (m *Manager) Send(ctx context.Context, provider string, msg Message) (<-chan Message, error) {
	s := m.session(provider)
	if s == nil {
		return nil, fmt.Errorf("wsrelay: provider %s not connected", provider)
	}
	return s.request(ctx, msg)
}

func (m *Manager) session(provider string) *session {
	key := strings.ToLower(strings.TrimSpace(provider))
	m.sessMutex.RLock()
	s := m.sessions[key]
	m.sessMutex.RUnlock()
	return s
}

func (m *Manager) handleSessionClosed(s *session, cause error) {
	if s == nil {
		return
	}
	key := strings.ToLower(strings.TrimSpace(s.provider))
	m.sessMutex.Lock()
	if cur, ok := m.sessions[key]; ok && cur == s {
		delete(m.sessions, key)
	}
	m.sessMutex.Unlock()
	if s.lease != nil && s.lease.wasRegistered() && m.onDisconnected != nil {
		m.onDisconnected(s.provider, cause)
	}
}

func randomProviderName() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("aistudio-%x", time.Now().UnixNano())
	}
	for i := range buf {
		buf[i] = alphabet[int(buf[i])%len(alphabet)]
	}
	return "aistudio-" + string(buf)
}
