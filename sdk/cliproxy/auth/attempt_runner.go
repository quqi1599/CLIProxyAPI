package auth

import (
	"context"
	"errors"
	"net/http"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

type managerAttemptRunFunc[T any] func(context.Context, []string, cliproxyexecutor.Request, cliproxyexecutor.Options, int) (T, error)

type managerAttemptFallbackFunc[T any] func(context.Context, []string, cliproxyexecutor.Request, cliproxyexecutor.Options, error) (T, bool, error)

type managerAttemptRecoveryFunc[T any] func(error) (T, error, error, bool)

type managerAttemptOutcome[T any] struct {
	result    T
	returnErr error
	finalErr  error
	success   bool
}

type managerAttemptRunner[T any] struct {
	manager  *Manager
	runOnce  managerAttemptRunFunc[T]
	fallback managerAttemptFallbackFunc[T]
	recovery managerAttemptRecoveryFunc[T]
}

func (runner managerAttemptRunner[T]) run(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, maxRetryCredentials int, maxWait time.Duration) managerAttemptOutcome[T] {
	var lastErr error
	for attempt := 0; ; attempt++ {
		result, errRun := runner.runOnce(ctx, providers, req, opts, maxRetryCredentials)
		if errRun == nil {
			recordManagerAttemptSuccess(ctx)
			return managerAttemptOutcome[T]{result: result, success: true}
		}
		lastErr = errRun
		wait, shouldRetry := runner.manager.shouldRetryAfterError(errRun, attempt, providers, req.Model, maxWait)
		if !shouldRetry {
			break
		}
		wait = runner.manager.effectiveRetryWait(errRun, wait)
		if errWait := waitForCooldown(ctx, wait); errWait != nil {
			return managerAttemptOutcome[T]{returnErr: errWait, finalErr: errWait}
		}
	}

	if lastErr == nil {
		lastErr = &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	if runner.fallback != nil {
		result, ok, errFallback := runner.fallback(ctx, providers, req, opts, lastErr)
		if errFallback != nil {
			return managerAttemptOutcome[T]{returnErr: errFallback, finalErr: errFallback}
		}
		if ok {
			recordManagerAttemptSuccess(ctx)
			return managerAttemptOutcome[T]{result: result, success: true}
		}
	}
	if runner.recovery != nil {
		result, returnErr, finalErr, handled := runner.recovery(lastErr)
		if handled {
			return managerAttemptOutcome[T]{result: result, returnErr: returnErr, finalErr: finalErr}
		}
	}
	return managerAttemptOutcome[T]{returnErr: lastErr, finalErr: lastErr}
}

func recordManagerAttemptSuccess(ctx context.Context) {
	if trace := requestAttemptTraceFromContext(ctx); trace != nil {
		trace.recordFinalStatus(http.StatusOK)
	}
}

func runManagerAttemptOperation[T any](ctx context.Context, manager *Manager, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, runner managerAttemptRunner[T]) (T, error) {
	ctx = manager.translatorContext(ctx)
	ctx, trace := ensureRequestAttemptTrace(ctx)
	outcome := managerAttemptOutcome[T]{}
	defer func() {
		coreusage.PublishRequestFinal(ctx, coreusage.RequestFinal{
			RequestID:    trace.requestIDValue(),
			FinalSuccess: outcome.success,
			AttemptCount: trace.attemptCount(),
			CompletedAt:  time.Now(),
		})
		logRequestExecutionSummary(ctx, trace, outcome.success, outcome.finalErr)
	}()

	if errPreflight := rejectMiMoV25ProImageInput(req, opts); errPreflight != nil {
		outcome.returnErr = errPreflight
		outcome.finalErr = errPreflight
		return outcome.result, outcome.returnErr
	}
	providers = manager.normalizeProviders(providers)
	if len(providers) == 0 {
		outcome.returnErr = &Error{Code: "provider_not_found", Message: "no provider supplied"}
		outcome.finalErr = outcome.returnErr
		return outcome.result, outcome.returnErr
	}

	requestRetry, maxRetryCredentials, maxWait := manager.retrySettings()
	trace.configureBudget(requestRetry+1, maxRetryCredentials)
	outcome = runner.run(ctx, providers, req, opts, maxRetryCredentials, maxWait)
	return outcome.result, outcome.returnErr
}

func (m *Manager) runExecuteAttempts(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	runner := managerAttemptRunner[cliproxyexecutor.Response]{
		manager: m,
		runOnce: m.executeMixedOnce,
		fallback: func(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, lastErr error) (cliproxyexecutor.Response, bool, error) {
			if !hasAntigravityProvider(providers) || !shouldAttemptAntigravityCreditsFallback(m, lastErr, providers) {
				return cliproxyexecutor.Response{}, false, nil
			}
			return m.tryAntigravityCreditsExecute(ctx, req, opts)
		},
	}
	return runManagerAttemptOperation(ctx, m, providers, req, opts, runner)
}

func (m *Manager) runCountAttempts(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	runner := managerAttemptRunner[cliproxyexecutor.Response]{
		manager: m,
		runOnce: m.executeCountMixedOnce,
	}
	return runManagerAttemptOperation(ctx, m, providers, req, opts, runner)
}

func (m *Manager) runStreamAttempts(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	runner := managerAttemptRunner[*cliproxyexecutor.StreamResult]{
		manager: m,
		runOnce: m.executeStreamMixedOnce,
		fallback: func(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, lastErr error) (*cliproxyexecutor.StreamResult, bool, error) {
			if !hasAntigravityProvider(providers) || !shouldAttemptAntigravityCreditsFallback(m, lastErr, providers) {
				return nil, false, nil
			}
			return m.tryAntigravityCreditsExecuteStream(ctx, req, opts)
		},
		recovery: func(lastErr error) (*cliproxyexecutor.StreamResult, error, error, bool) {
			var bootstrapErr *streamBootstrapError
			if !errors.As(lastErr, &bootstrapErr) || bootstrapErr == nil {
				return nil, nil, nil, false
			}
			return streamErrorResult(bootstrapErr.Headers(), bootstrapErr.cause), nil, bootstrapErr.cause, true
		},
	}
	return runManagerAttemptOperation(ctx, m, providers, req, opts, runner)
}
