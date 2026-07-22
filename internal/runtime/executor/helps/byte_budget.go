package helps

import (
	"context"
	"errors"
	"sync"
)

const ProviderWebsocketAggregateByteBudget int64 = 128 << 20

var ErrByteBudgetOversize = errors.New("byte budget request exceeds capacity")

var sharedProviderWebsocketByteBudget = NewByteBudget(ProviderWebsocketAggregateByteBudget)

// ByteBudget bounds aggregate bytes held by concurrent owners.
type ByteBudget struct {
	mu      sync.Mutex
	limit   int64
	inUse   int64
	changed chan struct{}
}

// ByteLease owns bytes in a ByteBudget until Release is called.
type ByteLease struct {
	mu       sync.Mutex
	budget   *ByteBudget
	weight   int64
	released bool
}

// NewByteBudget creates a byte budget with the provided capacity.
func NewByteBudget(limit int64) *ByteBudget {
	if limit < 0 {
		limit = 0
	}
	return &ByteBudget{limit: limit, changed: make(chan struct{})}
}

// SharedProviderWebsocketByteBudget is shared by Codex and xAI upstream WebSockets.
func SharedProviderWebsocketByteBudget() *ByteBudget {
	return sharedProviderWebsocketByteBudget
}

// Acquire waits without an internal timeout until bytes are available or ctx is canceled.
func (b *ByteBudget) Acquire(ctx context.Context, weight int64) (*ByteLease, error) {
	if b == nil {
		return nil, ErrByteBudgetOversize
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if weight < 0 {
		return nil, ErrByteBudgetOversize
	}
	if weight == 0 {
		return &ByteLease{}, nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		b.mu.Lock()
		if weight > b.limit {
			b.mu.Unlock()
			return nil, ErrByteBudgetOversize
		}
		if b.limit-b.inUse >= weight {
			b.inUse += weight
			b.mu.Unlock()
			return &ByteLease{budget: b, weight: weight}, nil
		}
		changed := b.changed
		b.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

// Shrink reduces a lease to the actual retained byte count.
func (l *ByteLease) Shrink(weight int64) error {
	if l == nil {
		return nil
	}
	if weight < 0 {
		return ErrByteBudgetOversize
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released || l.budget == nil {
		return nil
	}
	if weight > l.weight {
		return ErrByteBudgetOversize
	}
	if weight == l.weight {
		return nil
	}

	l.budget.release(l.weight - weight)
	l.weight = weight
	return nil
}

// Release returns the lease bytes exactly once.
func (l *ByteLease) Release() {
	if l == nil {
		return
	}

	l.mu.Lock()
	if l.released {
		l.mu.Unlock()
		return
	}
	l.released = true
	budget := l.budget
	weight := l.weight
	l.budget = nil
	l.weight = 0
	l.mu.Unlock()

	if budget != nil && weight > 0 {
		budget.release(weight)
	}
}

// InUse returns the bytes currently held by active leases.
func (b *ByteBudget) InUse() int64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inUse
}

func (b *ByteBudget) release(weight int64) {
	if b == nil || weight <= 0 {
		return
	}
	b.mu.Lock()
	b.inUse -= weight
	if b.inUse < 0 {
		b.inUse = 0
	}
	close(b.changed)
	b.changed = make(chan struct{})
	b.mu.Unlock()
}
