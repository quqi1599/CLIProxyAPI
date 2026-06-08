package http2pool

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/net/http2"
)

type fakeClientConn struct {
	state     http2.ClientConnState
	canTake   bool
	closeCall int
}

func (f *fakeClientConn) CanTakeNewRequest() bool {
	return f.canTake
}

func (f *fakeClientConn) Close() error {
	f.closeCall++
	f.state.Closed = true
	return nil
}

func (f *fakeClientConn) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("ok")),
	}, nil
}

func (f *fakeClientConn) State() http2.ClientConnState {
	return f.state
}

func TestPoolReusesIdleConnection(t *testing.T) {
	var dialCount int
	pool := New(4, func(host, addr string) (ClientConn, error) {
		dialCount++
		return &fakeClientConn{canTake: true}, nil
	})

	first, err := pool.Get("api.anthropic.com", "api.anthropic.com:443")
	if err != nil {
		t.Fatalf("first Get() error = %v", err)
	}
	second, err := pool.Get("api.anthropic.com", "api.anthropic.com:443")
	if err != nil {
		t.Fatalf("second Get() error = %v", err)
	}

	if first != second {
		t.Fatalf("expected idle connection reuse, got %p and %p", first, second)
	}
	if dialCount != 1 {
		t.Fatalf("dialCount = %d, want 1", dialCount)
	}
}

func TestPoolGrowsWhenExistingConnectionIsBusy(t *testing.T) {
	var dialCount int
	var firstConn *fakeClientConn
	pool := New(4, func(host, addr string) (ClientConn, error) {
		dialCount++
		conn := &fakeClientConn{canTake: true}
		if firstConn == nil {
			firstConn = conn
		}
		return conn, nil
	})

	first, err := pool.Get("api.anthropic.com", "api.anthropic.com:443")
	if err != nil {
		t.Fatalf("first Get() error = %v", err)
	}
	firstConn.state.StreamsActive = 1

	second, err := pool.Get("api.anthropic.com", "api.anthropic.com:443")
	if err != nil {
		t.Fatalf("second Get() error = %v", err)
	}

	if first == second {
		t.Fatalf("expected busy connection to trigger pool growth, got same conn %p", first)
	}
	if dialCount != 2 {
		t.Fatalf("dialCount = %d, want 2", dialCount)
	}
}

func TestPoolCapsGrowthAtConfiguredLimit(t *testing.T) {
	var dialCount int
	var issued []*fakeClientConn
	pool := New(2, func(host, addr string) (ClientConn, error) {
		dialCount++
		conn := &fakeClientConn{canTake: true, state: http2.ClientConnState{StreamsActive: 1}}
		issued = append(issued, conn)
		return conn, nil
	})

	first, err := pool.Get("api.anthropic.com", "api.anthropic.com:443")
	if err != nil {
		t.Fatalf("first Get() error = %v", err)
	}
	second, err := pool.Get("api.anthropic.com", "api.anthropic.com:443")
	if err != nil {
		t.Fatalf("second Get() error = %v", err)
	}
	third, err := pool.Get("api.anthropic.com", "api.anthropic.com:443")
	if err != nil {
		t.Fatalf("third Get() error = %v", err)
	}

	if first == second {
		t.Fatalf("expected second request to grow the pool, got same conn %p", first)
	}
	if third != first && third != second {
		t.Fatalf("expected third request to reuse one of the pooled conns, got %p", third)
	}
	if dialCount != 2 {
		t.Fatalf("dialCount = %d, want 2", dialCount)
	}
}

func TestPoolForgetRemovesConnection(t *testing.T) {
	var dialCount int
	var firstConn *fakeClientConn
	pool := New(4, func(host, addr string) (ClientConn, error) {
		dialCount++
		conn := &fakeClientConn{canTake: true}
		if firstConn == nil {
			firstConn = conn
		}
		return conn, nil
	})

	first, err := pool.Get("api.anthropic.com", "api.anthropic.com:443")
	if err != nil {
		t.Fatalf("first Get() error = %v", err)
	}
	pool.Forget("api.anthropic.com", first)

	second, err := pool.Get("api.anthropic.com", "api.anthropic.com:443")
	if err != nil {
		t.Fatalf("second Get() error = %v", err)
	}

	if first == second {
		t.Fatalf("expected forgotten connection to be replaced, got same conn %p", first)
	}
	if firstConn.closeCall != 1 {
		t.Fatalf("closeCall = %d, want 1", firstConn.closeCall)
	}
	if dialCount != 2 {
		t.Fatalf("dialCount = %d, want 2", dialCount)
	}
}
