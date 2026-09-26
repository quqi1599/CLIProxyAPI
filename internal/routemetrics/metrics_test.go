package routemetrics

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

type metricTestTransport func(*http.Request) (*http.Response, error)

func (f metricTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRawUpstreamStatusIsSeparateFromNormalizedAttempt(t *testing.T) {
	c := NewCollector()
	ctx := Start(context.Background(), "/v1/messages", c)
	SetModel(ctx, "model")
	SetProvider(ctx, "kimi")
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.invalid", nil)
	_, err := WrapTransport(ctx, metricTestTransport(func(*http.Request) (*http.Response, error) { return &http.Response{StatusCode: 403}, nil })).RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	Attempt(ctx, "kimi", 429)
	Finish(ctx, 503)
	s := c.Snapshot().Series[0]
	if s.UpstreamHTTP[403] != 1 || s.Attempts[429] != 1 || s.Requests[503] != 1 {
		t.Fatalf("status boundaries collapsed: %+v", s)
	}
}

func TestConcurrentRequestAttributionAndBounds(t *testing.T) {
	c := NewCollector()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := Start(context.Background(), "/v1/responses", c)
			SetModel(ctx, "gpt-5.5")
			Observe(ctx, "audit_write", time.Millisecond)
			SetProvider(ctx, "codex")
			Attempt(ctx, "claude", 429)
			Attempt(ctx, "codex", 200)
			ctx = Copy(context.Background(), ctx)
			Observe(ctx, "transform", 2*time.Millisecond)
			FirstToken(ctx)
			FirstToken(ctx)
			Finish(ctx, 499)
			Finish(ctx, 200)
		}()
	}
	wg.Wait()
	s := c.Snapshot()
	if len(s.Series) != 2 {
		t.Fatalf("series=%+v", s)
	}
	for _, r := range s.Series {
		if r.Provider == "codex" {
			if r.Requests[499] != 100 || r.Requests[200] != 0 || r.Attempts[200] != 100 || r.Timings["first_token"].Count != 100 || r.Timings["audit_write"].Count != 100 {
				t.Fatalf("attribution=%+v", r)
			}
		} else if r.Attempts[429] != 100 || len(r.Requests) != 0 {
			t.Fatalf("retry=%+v", r)
		}
	}
	for i := 0; i < MaxSeries*2; i++ {
		ctx := Start(context.Background(), "/v1/messages", c)
		SetModel(ctx, fmt.Sprint(i))
		Observe(ctx, fmt.Sprint(i), time.Second)
		Finish(ctx, 503)
	}
	s = c.Snapshot()
	if len(s.Series) > MaxSeries+1 || s.OverflowObservations == 0 {
		t.Fatalf("unbounded=%d", len(s.Series))
	}
	for _, r := range s.Series {
		if len(r.Timings) > 6 {
			t.Fatalf("unbounded stages=%v", r.Timings)
		}
	}
}
