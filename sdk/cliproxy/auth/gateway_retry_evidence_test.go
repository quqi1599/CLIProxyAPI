package auth

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"testing"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
)

type gatewayRetryRoundTripFunc func(*http.Request) (*http.Response, error)

func (f gatewayRetryRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGatewayRetryEvidenceTracksTransportNotSelection(t *testing.T) {
	ctx, evidence := WithGatewayRetryEvidence(context.Background(), nil)
	ctx, trace := ensureRequestAttemptTrace(ctx)
	// These existing counters are not paired: skipped selections have no result,
	// and a selected credential may try several models or refresh authentication.
	trace.nextAttempt("")
	trace.nextAttempt("")
	m := NewManager(nil, nil, nil)
	m.MarkResult(ctx, Result{AuthID: "preparation-only", Cause: &failurecontract.Failure{Kind: failurecontract.TransportError, HTTPStatus: 503}})
	if n, reason := evidence.Snapshot(&failurecontract.Failure{Kind: failurecontract.TransportError, HTTPStatus: 503}); n != 0 || reason != "" {
		t.Fatalf("preparation invented dispatch: %d/%s", n, reason)
	}
	rt := WrapGatewayRetryTransport(gatewayRetryRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("rejected"))}, nil
	}))
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequestWithContext(ctx, "POST", "http://fixture.invalid", nil)
		resp, err := rt.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if n, reason := evidence.Snapshot(nil); n != 3 || reason != "" {
		t.Fatalf("HTTP calls = %d/%s", n, reason)
	}
}

func TestGatewayRetryTransportBoundary(t *testing.T) {
	for _, mode := range []string{"rejected", "accepted", "sent-disconnected", "connection-refused"} {
		t.Run(mode, func(t *testing.T) {
			ctx, evidence := WithGatewayRetryEvidence(context.Background(), nil)
			target := ""
			if mode == "connection-refused" {
				l, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				target = "http://" + l.Addr().String()
				l.Close()
			} else {
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					switch mode {
					case "rejected":
						w.WriteHeader(503)
					case "accepted":
						w.Header().Set("Content-Length", "50")
						w.WriteHeader(200)
						_, _ = io.WriteString(w, "short")
					case "sent-disconnected":
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						conn.Close()
					}
				}))
				defer s.Close()
				target = s.URL
			}
			tr := http.DefaultTransport.(*http.Transport).Clone()
			defer tr.CloseIdleConnections()
			client := &http.Client{Transport: WrapGatewayRetryTransport(tr)}
			req, _ := http.NewRequestWithContext(ctx, "POST", target, strings.NewReader("synthetic-model-request"))
			resp, err := client.Do(req)
			if resp != nil {
				_, _ = io.ReadAll(resp.Body)
				resp.Body.Close()
			}
			if (mode == "connection-refused" || mode == "sent-disconnected") && err == nil {
				t.Fatal("expected transport failure")
			}
			n, reason := evidence.Snapshot(&failurecontract.Failure{Kind: failurecontract.TransportError, HTTPStatus: 502})
			want := ""
			if mode == "accepted" || mode == "sent-disconnected" {
				want = "accepted_outcome_unknown"
			}
			if n != 1 || reason != want {
				t.Fatalf("snapshot %d/%s want 1/%s", n, reason, want)
			}
		})
	}
}

func TestGatewayRetryEvidenceEarlierAcceptanceAndConcurrency(t *testing.T) {
	ctx, evidence := WithGatewayRetryEvidence(context.Background(), nil)
	rt := WrapGatewayRetryTransport(gatewayRetryRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		status := 503
		if r.URL.Path == "/accepted" {
			status = 200
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("fixture"))}, nil
	}))
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://fixture.invalid/accepted", nil)
	resp, _ := rt.RoundTrip(req)
	resp.Body.Close()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, _ := http.NewRequestWithContext(ctx, "POST", "http://fixture.invalid/rejected", nil)
			resp, err := rt.RoundTrip(r)
			if err == nil {
				resp.Body.Close()
			}
			evidence.Snapshot(nil)
		}()
	}
	wg.Wait()
	if n, reason := evidence.Snapshot(errors.New("later local failure")); n != 101 || reason != "accepted_outcome_unknown" {
		t.Fatalf("earlier acceptance lost: %d/%s", n, reason)
	}
	if _, reason := evidence.Snapshot(&failurecontract.Failure{OutputCommitted: true}); reason != "upstream_output_observed" {
		t.Fatal("output evidence missing")
	}
	evidence.attempts = 1 << 20
	evidence.attempted()
	evidence.attempted()
	if n, _ := evidence.Snapshot(nil); n != 1<<20+1 {
		t.Fatalf("counter = %d", n)
	}
}

func TestGatewayRetryUninstrumentedAndNestedTransports(t *testing.T) {
	ctx, evidence := WithGatewayRetryEvidence(context.Background(), nil)
	rt := WrapGatewayRetryTransport(WrapGatewayRetryTransport(gatewayRetryRoundTripFunc(func(*http.Request) (*http.Response, error) { return nil, io.ErrUnexpectedEOF })))
	r, _ := http.NewRequestWithContext(ctx, "POST", "http://fixture.invalid", nil)
	_, _ = rt.RoundTrip(r)
	if n, reason := evidence.Snapshot(nil); n != 1 || reason != "accepted_outcome_unknown" {
		t.Fatalf("unknown transport %d/%s", n, reason)
	}
}

func TestGatewayRetryTraceAndTypedErrorBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		trace  func(*httptrace.ClientTrace)
		err    error
		status int
		want   string
	}{
		{name: "traced-connect-failure", trace: func(t *httptrace.ClientTrace) {}, err: errors.New("fixture")},
		{name: "typed-dns", err: &net.DNSError{Err: "fixture"}},
		{name: "typed-dial", err: &net.OpError{Op: "dial", Err: errors.New("fixture")}},
		{name: "error-string-is-not-evidence", err: errors.New("dial tcp: connection refused"), want: "accepted_outcome_unknown"},
		{name: "connected-before-failure", trace: func(t *httptrace.ClientTrace) { t.GotConn(httptrace.GotConnInfo{}) }, err: io.EOF, want: "accepted_outcome_unknown"},
		{name: "failed-write", trace: func(t *httptrace.ClientTrace) { t.WroteRequest(httptrace.WroteRequestInfo{Err: io.EOF}) }, err: io.EOF, want: "accepted_outcome_unknown"},
		{name: "resend-then-rejection", trace: func(t *httptrace.ClientTrace) {
			t.WroteRequest(httptrace.WroteRequestInfo{})
			t.WroteRequest(httptrace.WroteRequestInfo{})
		}, status: 503, want: "accepted_outcome_unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, evidence := WithGatewayRetryEvidence(context.Background(), nil)
			composed := 0
			ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GetConn: func(string) { composed++ }})
			rt := WrapGatewayRetryTransport(gatewayRetryRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				trace := httptrace.ContextClientTrace(r.Context())
				if tc.trace != nil {
					trace.GetConn("fixture.invalid")
					tc.trace(trace)
				}
				if tc.status != 0 {
					return &http.Response{StatusCode: tc.status, Body: http.NoBody}, nil
				}
				return nil, tc.err
			}))
			r, _ := http.NewRequestWithContext(ctx, "POST", "http://fixture.invalid", nil)
			_, _ = rt.RoundTrip(r)
			wantComposed := 0
			if tc.trace != nil {
				wantComposed = 1
			}
			if n, reason := evidence.Snapshot(nil); n != 1 || reason != tc.want || composed != wantComposed {
				t.Fatalf("snapshot %d/%s composed=%d want 1/%s/1", n, reason, composed, tc.want)
			}
		})
	}
}
