// Package routemetrics collects bounded, request-safe pipeline measurements.
package routemetrics

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

const MaxSeries = 512

type Labels struct {
	Route    string `json:"route"`
	Model    string `json:"model"`
	Provider string `json:"provider"`
}

// Buckets are non-cumulative counts with upper bounds in milliseconds.
type Duration struct {
	Count          uint64    `json:"count"`
	Nanoseconds    uint64    `json:"nanoseconds"`
	MaxNanoseconds uint64    `json:"max_nanoseconds"`
	Buckets        [9]uint64 `json:"buckets"`
}

var BoundsMilliseconds = [...]int64{1, 5, 10, 50, 100, 500, 1000, 5000}

func (d *Duration) add(elapsed time.Duration) {
	n := uint64(max(elapsed, 0))
	d.Count++
	d.Nanoseconds += n
	d.MaxNanoseconds = max(d.MaxNanoseconds, n)
	i := 0
	for i < len(BoundsMilliseconds) && elapsed > time.Duration(BoundsMilliseconds[i])*time.Millisecond {
		i++
	}
	d.Buckets[i]++
}

type Series struct {
	Labels
	Requests     map[int]uint64      `json:"requests_by_status"`
	Attempts     map[int]uint64      `json:"attempts_by_status"`
	UpstreamHTTP map[int]uint64      `json:"upstream_http_by_status"`
	Timings      map[string]Duration `json:"timings"`
}

type Collector struct {
	mu       sync.Mutex
	series   map[Labels]*Series
	overflow uint64
}

var Default = NewCollector()

func NewCollector() *Collector { return &Collector{series: make(map[Labels]*Series)} }

type Snapshot struct {
	Series               []Series `json:"series"`
	OverflowObservations uint64   `json:"overflow_observations"`
	BoundsMilliseconds   [8]int64 `json:"duration_bucket_upper_bounds_ms"`
}

func label(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "unresolved"
	}
	if len(s) > 160 {
		return "other"
	}
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("-_.:/()+", r) {
			return "other"
		}
	}
	return s
}

func (c *Collector) update(key Labels, fn func(*Series)) {
	if c == nil {
		return
	}
	key = Labels{label(key.Route), label(key.Model), label(key.Provider)}
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.series[key]
	if s == nil {
		if len(c.series) >= MaxSeries {
			c.overflow++
			key = Labels{"overflow", "overflow", "overflow"}
			s = c.series[key]
		}
		if s == nil {
			s = &Series{Labels: key, Requests: map[int]uint64{}, Attempts: map[int]uint64{}, UpstreamHTTP: map[int]uint64{}, Timings: map[string]Duration{}}
			c.series[key] = s
		}
	}
	fn(s)
}

func (c *Collector) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := Snapshot{BoundsMilliseconds: BoundsMilliseconds, OverflowObservations: c.overflow, Series: make([]Series, 0, len(c.series))}
	for _, s := range c.series {
		copy := Series{Labels: s.Labels, Requests: map[int]uint64{}, Attempts: map[int]uint64{}, UpstreamHTTP: map[int]uint64{}, Timings: map[string]Duration{}}
		for k, v := range s.Requests {
			copy.Requests[k] = v
		}
		for k, v := range s.Attempts {
			copy.Attempts[k] = v
		}
		for k, v := range s.UpstreamHTTP {
			copy.UpstreamHTTP[k] = v
		}
		for k, v := range s.Timings {
			copy.Timings[k] = v
		}
		out.Series = append(out.Series, copy)
	}
	sort.Slice(out.Series, func(i, j int) bool {
		a, b := out.Series[i], out.Series[j]
		if a.Route != b.Route {
			return a.Route < b.Route
		}
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		return a.Provider < b.Provider
	})
	return out
}

type contextKey struct{}
type Recorder struct {
	mu         sync.Mutex
	collector  *Collector
	labels     Labels
	started    time.Time
	finished   bool
	firstToken bool
	timings    map[string]Duration
}

func Start(ctx context.Context, route string, collector *Collector) context.Context {
	if collector == nil {
		collector = Default
	}
	return context.WithValue(ctx, contextKey{}, &Recorder{collector: collector, labels: Labels{Route: route}, started: time.Now(), timings: make(map[string]Duration)})
}
func recorder(ctx context.Context) *Recorder {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(contextKey{}).(*Recorder)
	return r
}

func Enabled(ctx context.Context) bool { return recorder(ctx) != nil }
func Copy(dst, src context.Context) context.Context {
	if r := recorder(src); r != nil {
		return context.WithValue(dst, contextKey{}, r)
	}
	return dst
}
func SetModel(ctx context.Context, model string) {
	if r := recorder(ctx); r != nil {
		r.mu.Lock()
		if r.labels.Model == "" {
			r.labels.Model = label(model)
		}
		r.mu.Unlock()
	}
}
func SetProvider(ctx context.Context, provider string) {
	if r := recorder(ctx); r != nil {
		r.mu.Lock()
		r.labels.Provider = label(provider)
		r.mu.Unlock()
	}
}

// Observe accepts fixed stage names only, preventing user-controlled cardinality.
func Observe(ctx context.Context, stage string, elapsed time.Duration) {
	switch stage {
	case "transform", "audit_write", "selection", "upstream_headers", "first_token", "total":
	default:
		return
	}
	r := recorder(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		return
	}
	d := r.timings[stage]
	d.add(elapsed)
	r.timings[stage] = d
}

// FirstToken records the first content/tool/reasoning delta delivered downstream.
// Protocol envelopes, role-only events, heartbeats and non-streaming JSON do not count.
func FirstToken(ctx context.Context) {
	r := recorder(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished || r.firstToken {
		return
	}
	r.firstToken = true
	d := r.timings["first_token"]
	d.add(time.Since(r.started))
	r.timings["first_token"] = d
}

func statusCode(status int) int {
	if status < 100 || status > 599 {
		return 0
	}
	return status
}
func Attempt(ctx context.Context, provider string, status int) {
	r := recorder(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	key := r.labels
	r.mu.Unlock()
	key.Provider = provider
	r.collector.update(key, func(s *Series) { s.Attempts[statusCode(status)]++ })
}

// Finish assigns shared ingress costs and request outcome to the final provider.
// Retry attempt outcomes remain independently attributed to each provider.
func Finish(ctx context.Context, status int) {
	r := recorder(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		return
	}
	r.finished = true
	d := r.timings["total"]
	d.add(time.Since(r.started))
	r.timings["total"] = d
	r.collector.update(r.labels, func(s *Series) {
		s.Requests[statusCode(status)]++
		for name, v := range r.timings {
			d := s.Timings[name]
			d.Count += v.Count
			d.Nanoseconds += v.Nanoseconds
			d.MaxNanoseconds = max(d.MaxNanoseconds, v.MaxNanoseconds)
			for i, n := range v.Buckets {
				d.Buckets[i] += n
			}
			s.Timings[name] = d
		}
	})
}

type transport struct{ base http.RoundTripper }

func (t transport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.base.RoundTrip(req)
	Observe(req.Context(), "upstream_headers", time.Since(start))
	if resp != nil {
		if r := recorder(req.Context()); r != nil {
			r.mu.Lock()
			key := r.labels
			r.mu.Unlock()
			r.collector.update(key, func(s *Series) { s.UpstreamHTTP[statusCode(resp.StatusCode)]++ })
		}
	}
	return resp, err
}
func WrapTransport(ctx context.Context, base http.RoundTripper) http.RoundTripper {
	if recorder(ctx) == nil {
		return base
	}
	if base == nil {
		base = http.DefaultTransport
	}
	return transport{base}
}
