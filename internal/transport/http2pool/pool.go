package http2pool

import (
	"net/http"
	"sync"

	"golang.org/x/net/http2"
)

const DefaultMaxConnsPerHost = 8

type ClientConn interface {
	CanTakeNewRequest() bool
	Close() error
	RoundTrip(*http.Request) (*http.Response, error)
	State() http2.ClientConnState
}

type DialFunc func(host, addr string) (ClientConn, error)

type Pool struct {
	mu              sync.Mutex
	hosts           map[string]*hostPool
	dial            DialFunc
	maxConnsPerHost int
}

type hostPool struct {
	conns    []ClientConn
	creating int
	cond     *sync.Cond
}

func New(maxConnsPerHost int, dial DialFunc) *Pool {
	if maxConnsPerHost <= 0 {
		maxConnsPerHost = DefaultMaxConnsPerHost
	}
	return &Pool{
		hosts:           make(map[string]*hostPool),
		dial:            dial,
		maxConnsPerHost: maxConnsPerHost,
	}
}

func (p *Pool) Get(host, addr string) (ClientConn, error) {
	p.mu.Lock()
	for {
		pool := p.hostPoolLocked(host)
		p.pruneClosedLocked(host, pool)
		bestUsable, bestUsableLoad, bestAny := bestConnection(pool.conns)

		if bestUsable != nil && (bestUsableLoad == 0 || len(pool.conns)+pool.creating >= p.maxConnsPerHost) {
			p.mu.Unlock()
			return bestUsable, nil
		}

		if len(pool.conns)+pool.creating < p.maxConnsPerHost {
			pool.creating++
			p.mu.Unlock()

			conn, err := p.dial(host, addr)

			p.mu.Lock()
			pool = p.hostPoolLocked(host)
			pool.creating--
			if err == nil && conn != nil {
				pool.conns = append(pool.conns, conn)
			}
			pool.cond.Broadcast()
			p.cleanupHostLocked(host, pool)
			if err != nil {
				bestUsable, _, bestAny = bestConnection(pool.conns)
				p.mu.Unlock()
				if bestUsable != nil {
					return bestUsable, nil
				}
				if bestAny != nil {
					return bestAny, nil
				}
				return nil, err
			}
			p.mu.Unlock()
			return conn, nil
		}

		if bestUsable != nil {
			p.mu.Unlock()
			return bestUsable, nil
		}
		if bestAny != nil {
			p.mu.Unlock()
			return bestAny, nil
		}
		if pool.creating > 0 {
			pool.cond.Wait()
			continue
		}
	}
}

func (p *Pool) Forget(host string, target ClientConn) {
	if target == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	pool, ok := p.hosts[host]
	if !ok || pool == nil {
		return
	}

	for i, conn := range pool.conns {
		if conn != target {
			continue
		}
		_ = conn.Close()
		pool.conns = append(pool.conns[:i], pool.conns[i+1:]...)
		break
	}
	p.cleanupHostLocked(host, pool)
	pool.cond.Broadcast()
}

func (p *Pool) hostPoolLocked(host string) *hostPool {
	if pool, ok := p.hosts[host]; ok && pool != nil {
		return pool
	}
	pool := &hostPool{}
	pool.cond = sync.NewCond(&p.mu)
	p.hosts[host] = pool
	return pool
}

func (p *Pool) pruneClosedLocked(host string, pool *hostPool) {
	if pool == nil || len(pool.conns) == 0 {
		return
	}

	filtered := pool.conns[:0]
	for _, conn := range pool.conns {
		state := conn.State()
		if state.Closed || state.Closing {
			_ = conn.Close()
			continue
		}
		filtered = append(filtered, conn)
	}
	pool.conns = filtered
	p.cleanupHostLocked(host, pool)
}

func (p *Pool) cleanupHostLocked(host string, pool *hostPool) {
	if pool == nil {
		return
	}
	if len(pool.conns) == 0 && pool.creating == 0 {
		delete(p.hosts, host)
	}
}

func bestConnection(conns []ClientConn) (ClientConn, int, ClientConn) {
	var (
		bestUsable     ClientConn
		bestAny        ClientConn
		bestUsableLoad int
		bestAnyLoad    int
	)

	for _, conn := range conns {
		state := conn.State()
		load := state.StreamsActive + state.StreamsReserved + state.StreamsPending

		if bestAny == nil || load < bestAnyLoad {
			bestAny = conn
			bestAnyLoad = load
		}
		if !conn.CanTakeNewRequest() {
			continue
		}
		if bestUsable == nil || load < bestUsableLoad {
			bestUsable = conn
			bestUsableLoad = load
		}
	}

	return bestUsable, bestUsableLoad, bestAny
}
