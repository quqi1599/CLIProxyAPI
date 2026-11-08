package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestKimiRetryQueueBoundAndCancellation(t *testing.T) {
	m := NewManager(nil, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	for range kimiRetryQueueLimit {
		wg.Add(1)
		go func() {
			defer wg.Done()
			admitted, err := m.waitForRetryCooldown(ctx, "kimi-k3.1-pro", time.Hour)
			if !admitted || !errors.Is(err, context.Canceled) {
				t.Errorf("wait result admitted=%v err=%v", admitted, err)
			}
		}()
	}
	deadline := time.After(5 * time.Second)
	for m.kimiRetryWaiters.Load() != kimiRetryQueueLimit {
		select {
		case <-deadline:
			t.Fatal("waiters did not enter queue")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if admitted, err := m.waitForRetryCooldown(context.Background(), "k3.1-pro", time.Hour); admitted || err != nil {
		t.Fatalf("queue limit bypassed: %v %v", admitted, err)
	}
	cancel()
	wg.Wait()
	if m.kimiRetryWaiters.Load() != 0 {
		t.Fatal("cancelled requests leaked queue slots")
	}
	if admitted, err := m.waitForRetryCooldown(context.Background(), "k3.1-pro", time.Millisecond); !admitted || err != nil {
		t.Fatalf("released queue cannot be reused: %v %v", admitted, err)
	}
}

func TestKimiRetryQueueFullPreservesFailure(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(2, time.Second, 0)
	a := kimiQuotaTestAuth("queue-full", "kimi", "synthetic")
	m.auths[a.ID] = a
	m.kimiRetryWaiters.Store(kimiRetryQueueLimit)
	wait := time.Millisecond
	cause := kimiInsufficientQuotaTestFailure()
	failure := cause.(*failurecontract.Failure)
	failure.RetryAfter = &wait
	calls := 0
	runner := managerAttemptRunner[int]{manager: m, runOnce: func(context.Context, []string, cliproxyexecutor.Request, cliproxyexecutor.Options, int) (int, error) {
		calls++
		return 0, cause
	}}
	outcome := runner.run(context.Background(), []string{"kimi"}, cliproxyexecutor.Request{Model: "kimi-k3"}, cliproxyexecutor.Options{}, 1, time.Second)
	if calls != 1 || outcome.returnErr != cause {
		t.Fatalf("queue rejection changed failure or retried: calls=%d err=%v", calls, outcome.returnErr)
	}
}

func TestKimiLongQuotaDoesNotEnterRetryQueue(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(2, time.Second, 0)
	m.auths["quota"] = kimiQuotaTestAuth("quota", "kimi", "synthetic")
	cause := kimiInsufficientQuotaTestFailure().(*failurecontract.Failure)
	wait := 24 * time.Hour
	cause.RetryAfter = &wait
	if _, retry := m.shouldRetryAfterError(cause, 0, []string{"kimi"}, "kimi-k3", time.Second); retry {
		t.Fatal("long quota allowed into short retry queue")
	}
}
