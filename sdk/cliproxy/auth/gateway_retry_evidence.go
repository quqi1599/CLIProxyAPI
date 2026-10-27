package auth

import (
	"context"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"sync/atomic"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
)

type gatewayRetryEvidenceKey struct{}
type gatewayRetryTransportKey struct{}

// GatewayRetryEvidence is populated only at model transport boundaries. Route
// selection, preparation, admission and accounting sequences are not evidence
// that a model request was sent. It never changes the gateway's inner retries.
type GatewayRetryEvidence struct {
	mu       sync.Mutex
	attempts int
	reason   string
}

func WithGatewayRetryEvidence(ctx context.Context, evidence *GatewayRetryEvidence) (context.Context, *GatewayRetryEvidence) {
	if ctx == nil {
		ctx = context.Background()
	}
	if evidence == nil {
		evidence = &GatewayRetryEvidence{}
	}
	return context.WithValue(ctx, gatewayRetryEvidenceKey{}, evidence), evidence
}
func gatewayRetryEvidenceFromContext(ctx context.Context) *GatewayRetryEvidence {
	if ctx == nil {
		return nil
	}
	evidence, _ := ctx.Value(gatewayRetryEvidenceKey{}).(*GatewayRetryEvidence)
	return evidence
}
func (e *GatewayRetryEvidence) attempted() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.attempts <= 1<<20 {
		e.attempts++
	}
}
func (e *GatewayRetryEvidence) retain(reason string) {
	if reason == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.reason == "" || reason == "upstream_output_observed" {
		e.reason = reason
	}
}

// Snapshot preserves transport evidence from earlier attempts. A final typed
// output flag can strengthen that evidence, but error families and differences
// between unrelated counters must never invent acceptance or a dispatch.
func (e *GatewayRetryEvidence) Snapshot(finalErr error) (int, string) {
	if e == nil {
		return 0, ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	reason := e.reason
	if f, ok := failurecontract.As(finalErr); ok && (f.OutputCommitted || f.StreamPhase == failurecontract.StreamPhaseAfterOutput) {
		reason = "upstream_output_observed"
	} else if ok && f.OuterStatus >= 200 && f.OuterStatus < 300 && reason == "" {
		reason = "accepted_outcome_unknown"
	}
	return e.attempts, reason
}

type gatewayRetryTransport struct{ base http.RoundTripper }

// WrapGatewayRetryTransport belongs around the model request client, not an
// authentication client or a global transport. It composes existing httptrace
// callbacks and observes metadata only; neither payloads nor headers are saved.
func WrapGatewayRetryTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return gatewayRetryTransport{base: base}
}

func (t gatewayRetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	evidence := gatewayRetryEvidenceFromContext(req.Context())
	if evidence == nil || req.Context().Value(gatewayRetryTransportKey{}) != nil {
		return t.base.RoundTrip(req)
	}
	evidence.attempted()
	var connecting, connected, writeStarted atomic.Bool
	var writes atomic.Int32
	trace := &httptrace.ClientTrace{
		GetConn:      func(string) { connecting.Store(true) },
		GotConn:      func(httptrace.GotConnInfo) { connected.Store(true) },
		WroteHeaders: func() { writeStarted.Store(true) },
		WroteRequest: func(httptrace.WroteRequestInfo) { writeStarted.Store(true); writes.Add(1) },
	}
	ctx := context.WithValue(req.Context(), gatewayRetryTransportKey{}, true)
	ctx = httptrace.WithClientTrace(ctx, trace)
	resp, err := t.base.RoundTrip(req.Clone(ctx))
	switch {
	case writes.Load() > 1:
		// A transport may internally resend. A later rejection cannot explain an
		// earlier write whose outcome was not surfaced to this wrapper.
		evidence.retain("accepted_outcome_unknown")
	case resp != nil && resp.StatusCode >= 200 && resp.StatusCode < 300:
		evidence.retain("accepted_outcome_unknown")
	case resp != nil && resp.StatusCode >= 400 && resp.StatusCode <= 599:
		// An explicit upstream rejection leaves independent fallback available.
	case err != nil && !writeStarted.Load() && connecting.Load() && !connected.Load():
		// Standard HTTP traces establish failure before a usable connection.
	case err != nil && !writeStarted.Load() && !connected.Load() && definitelyPreconnectError(err):
		// Custom transports may preserve typed DNS/dial failures without tracing.
	default:
		// A write with no explicit rejection, or an uninstrumented custom
		// transport failure, has an unknown outcome. Never infer safety from EOF.
		evidence.retain("accepted_outcome_unknown")
	}
	return resp, err
}

func definitelyPreconnectError(err error) bool {
	// RoundTrip normally returns these directly. Unwrap only typed wrappers;
	// matching diagnostic strings would confuse send/read failures with dial.
	for err != nil {
		switch e := err.(type) {
		case *net.DNSError:
			return true
		case *net.OpError:
			return e.Op == "dial"
		}
		wrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			break
		}
		err = wrapped.Unwrap()
	}
	return false
}

// ObserveGatewayWebsocketSend is called immediately before writing a model
// message on an established socket. A failed write may be partial; a handshake
// alone is not a model dispatch and does not call this function.
func ObserveGatewayWebsocketSend(ctx context.Context) {
	if evidence := gatewayRetryEvidenceFromContext(ctx); evidence != nil {
		evidence.attempted()
		evidence.retain("accepted_outcome_unknown")
	}
}
