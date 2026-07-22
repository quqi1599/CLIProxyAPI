package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
)

func TestAwaitStreamBootstrapNeverTreatsBufferedErrorAsCleanClose(t *testing.T) {
	const iterations = 1000
	for i := 0; i < iterations; i++ {
		data := make(chan []byte)
		close(data)
		errs := make(chan *interfaces.ErrorMessage, 1)
		errs <- &interfaces.ErrorMessage{StatusCode: 502, Error: errors.New("upstream protocol error")}
		close(errs)

		chunk, errMsg, done, err := AwaitStreamBootstrap(context.Background(), data, errs)
		if err != nil {
			t.Fatalf("iteration %d: bootstrap error = %v", i, err)
		}
		if done || errMsg == nil || len(chunk) != 0 {
			t.Fatalf("iteration %d: chunk=%q err=%v done=%v, want terminal error", i, chunk, errMsg, done)
		}
	}
}
