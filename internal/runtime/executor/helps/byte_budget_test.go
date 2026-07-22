package helps

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestByteBudgetConcurrentSaturationAndRelease(t *testing.T) {
	budget := NewByteBudget(8)
	start := make(chan struct{})
	errCh := make(chan error, 64)
	var workers sync.WaitGroup
	for range 64 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			lease, err := budget.Acquire(context.Background(), 1)
			if err != nil {
				errCh <- err
				return
			}
			if got := budget.InUse(); got > 8 {
				errCh <- errors.New("byte budget exceeded capacity")
			}
			lease.Release()
		}()
	}
	close(start)
	workers.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	if got := budget.InUse(); got != 0 {
		t.Fatalf("bytes in use = %d, want 0", got)
	}
}

func TestByteBudgetCanceledAcquireDoesNotLeak(t *testing.T) {
	budget := NewByteBudget(10)
	first, err := budget.Acquire(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		close(started)
		_, errAcquire := budget.Acquire(ctx, 1)
		result <- errAcquire
	}()
	<-started
	cancel()
	if errAcquire := <-result; !errors.Is(errAcquire, context.Canceled) {
		t.Fatalf("acquire error = %v, want context canceled", errAcquire)
	}
	if got := budget.InUse(); got != 10 {
		t.Fatalf("bytes in use after cancellation = %d, want 10", got)
	}
	first.Release()
	if got := budget.InUse(); got != 0 {
		t.Fatalf("bytes in use after release = %d, want 0", got)
	}
}

func TestByteBudgetRejectsOversize(t *testing.T) {
	budget := NewByteBudget(10)
	lease, err := budget.Acquire(context.Background(), 11)
	if lease != nil || !errors.Is(err, ErrByteBudgetOversize) {
		t.Fatalf("oversize acquire = (%v, %v), want nil oversize error", lease, err)
	}
	if got := budget.InUse(); got != 0 {
		t.Fatalf("bytes in use = %d, want 0", got)
	}
}

func TestByteLeaseShrinkAndIdempotentRelease(t *testing.T) {
	budget := NewByteBudget(10)
	lease, err := budget.Acquire(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if errShrink := lease.Shrink(4); errShrink != nil {
		t.Fatal(errShrink)
	}
	if got := budget.InUse(); got != 4 {
		t.Fatalf("bytes in use after shrink = %d, want 4", got)
	}
	if errGrow := lease.Shrink(5); !errors.Is(errGrow, ErrByteBudgetOversize) {
		t.Fatalf("grow error = %v, want oversize", errGrow)
	}
	lease.Release()
	lease.Release()
	if got := budget.InUse(); got != 0 {
		t.Fatalf("bytes in use after release = %d, want 0", got)
	}
}
