package handlers

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
)

// AwaitStreamBootstrap waits for the first payload or terminal error.
// A clean completion is reported only after both channels close, so a buffered
// terminal error cannot race with data-channel closure.
func AwaitStreamBootstrap(ctx context.Context, data <-chan []byte, errs <-chan *interfaces.ErrorMessage) (chunk []byte, errMsg *interfaces.ErrorMessage, done bool, err error) {
	var ctxDone <-chan struct{}
	if ctx != nil {
		ctxDone = ctx.Done()
	}

	for data != nil || errs != nil {
		select {
		case <-ctxDone:
			return nil, nil, false, ctx.Err()
		case chunk, ok := <-data:
			if !ok {
				data = nil
				continue
			}
			return chunk, nil, false, nil
		case errMsg, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if errMsg != nil {
				return nil, errMsg, false, nil
			}
		}
	}

	return nil, nil, true, nil
}
