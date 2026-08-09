package auth

import (
	"context"
	"errors"
	"net/http"
	"time"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
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
	manager                      *Manager
	runOnce                      managerAttemptRunFunc[T]
	fallback                     managerAttemptFallbackFunc[T]
	recovery                     managerAttemptRecoveryFunc[T]
	configureGPTFirstEventPolicy bool
}

func (runner managerAttemptRunner[T]) run(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, maxRetryCredentials int, maxWait time.Duration) managerAttemptOutcome[T] {
	var lastErr error
	probeWaitUsed := false
attempts:
	for attempt := 0; ; {
		trace := requestAttemptTraceFromContext(ctx)
		if trace != nil {
			trace.beginGPTRound(attempt + 1)
		}
		result, errRun := runner.runOnce(ctx, providers, req, opts, maxRetryCredentials)
		if errRun == nil {
			recordManagerAttemptSuccess(ctx)
			return managerAttemptOutcome[T]{result: result, success: true}
		}
		lastErr = errRun
		if trace != nil && isGPTZeroEligibleSelectionError(errRun) {
			if gptRoute, configured := trace.gptRouteValue(); configured && gptRoute {
				if probeWaitUsed {
					break attempts
				}
				waitState, errWait := runner.manager.waitForZeroEligibleProbe(ctx, req.Model, zeroEligibleProbeWaitForTrace(trace))
				if errWait != nil {
					return managerAttemptOutcome[T]{returnErr: errWait, finalErr: errWait}
				}
				switch waitState {
				case zeroEligibleProbeWaitCompleted:
					// Waiting for another request's probe does not consume a GPT
					// upstream round. Re-select once with the same round budget.
					probeWaitUsed = true
					continue
				case zeroEligibleProbeWaitTimedOut, zeroEligibleProbeWaitRejected:
					break attempts
				}
			}
		}
		wait, shouldRetry := runner.manager.shouldRetryAfterError(errRun, attempt, providers, req.Model, maxWait)
		if trace != nil {
			if gptRoute, configured := trace.gptRouteValue(); configured && gptRoute {
				if isGPTLargeToolHistoryResponsesRequest(providers, req.Model, opts) {
					wait, shouldRetry = 0, false
				} else {
					var cooldownErr *modelCooldownError
					if !errors.As(errRun, &cooldownErr) || !shouldRetry {
						wait, shouldRetry = shouldRetryGPTRound(errRun, attempt, providers, req.Model, trace)
						wait, shouldRetry = preserveCanonicalGPTRoundRetryAfter(errRun, wait, shouldRetry, maxWait)
					}
				}
			}
		}
		if !shouldRetry {
			break
		}
		if trace != nil && isRetryableEmptyUpstreamResponseError(errRun) {
			trace.recordEmptyResponseRetry()
		}
		wait = runner.manager.effectiveRetryWait(errRun, wait)
		if errWait := waitForCooldown(ctx, wait); errWait != nil {
			return managerAttemptOutcome[T]{returnErr: errWait, finalErr: errWait}
		}
		attempt++
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

func zeroEligibleProbeWaitForTrace(trace *requestAttemptTrace) time.Duration {
	wait := gptZeroEligibleProbeMinWait
	if trace != nil {
		if policy, configured := trace.gptFirstEventPolicyValue(); configured && policy.EnforcedTimeoutMs > 0 {
			policyWait := time.Duration(policy.EnforcedTimeoutMs)*time.Millisecond + gptZeroEligibleProbeWaitGrace
			if policyWait > wait {
				wait = policyWait
			}
		}
	}
	if wait > gptZeroEligibleProbeMaxWait {
		return gptZeroEligibleProbeMaxWait
	}
	return wait
}

func isGPTZeroEligibleSelectionError(err error) bool {
	if err == nil {
		return false
	}
	var cooldownErr *modelCooldownError
	if errors.As(err, &cooldownErr) && cooldownErr != nil {
		return true
	}
	var authErr *Error
	if !errors.As(err, &authErr) || authErr == nil {
		return false
	}
	switch authErr.Code {
	case "auth_not_found", "auth_unavailable":
		return true
	default:
		return false
	}
}

func preserveCanonicalGPTRoundRetryAfter(err error, wait time.Duration, shouldRetry bool, maxWait time.Duration) (time.Duration, bool) {
	if !shouldRetry {
		return wait, false
	}
	typed, ok := failurecontract.As(err)
	if !ok || typed.RetryAfter == nil {
		return wait, true
	}
	if _, controlled := controlledFailureScope(string(typed.Scope)); !controlled {
		return wait, true
	}
	retryAfter := *typed.RetryAfter
	if retryAfter < 0 || (retryAfter > 0 && (maxWait <= 0 || retryAfter > maxWait)) {
		return 0, false
	}
	return retryAfter, true
}

func recordManagerAttemptSuccess(ctx context.Context) {
	if trace := requestAttemptTraceFromContext(ctx); trace != nil {
		trace.recordFinalStatus(http.StatusOK)
	}
}

func runManagerAttemptOperation[T any](ctx context.Context, manager *Manager, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, runner managerAttemptRunner[T]) (T, error) {
	ctx = manager.translatorContext(ctx)
	ctx, trace := ensureRequestAttemptTrace(ctx)
	defer manager.releaseZeroEligibleProbe(ctx, req.Model)
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
	gptRoute := isGPTRetryRoute(providers, req.Model)
	trace.configureGPTRoute(gptRoute)

	requestRetry, maxRetryCredentials, maxWait := manager.retrySettings()
	if gptRoute {
		if isGPTLargeToolHistoryResponsesRequest(providers, req.Model, opts) {
			trace.configureBudget(gptLargeToolHistoryMaxRetryCredentials+1, gptLargeToolHistoryMaxRetryCredentials)
		} else if runner.configureGPTFirstEventPolicy {
			policy := trace.configureGPTFirstEventPolicy(manager.selectGPTFirstEventPolicy(req.Model))
			trace.configureBudget(policy.MaxChannels*policy.MaxRounds, policy.MaxChannels*policy.MaxRounds-1)
		} else {
			trace.configureBudget(gptImmediateFailoverMaxChannels*gptImmediateFailoverMaxRounds, gptImmediateFailoverMaxChannels*gptImmediateFailoverMaxRounds-1)
		}
	} else {
		trace.configureBudget(requestRetry+1, maxRetryCredentials)
	}
	outcome = runner.run(ctx, providers, req, opts, maxRetryCredentials, maxWait)
	outcome.returnErr = normalizeTerminalManagerError(ctx, outcome.returnErr)
	outcome.finalErr = normalizeTerminalManagerError(ctx, outcome.finalErr)
	return outcome.result, outcome.returnErr
}

func normalizeTerminalManagerError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return newCallerRequestFailure(ctx.Err())
	}
	if _, ok := failurecontract.As(err); ok {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		status := http.StatusBadGateway
		if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		return &failurecontract.Failure{
			Kind:          failurecontract.TransportError,
			Scope:         failurecontract.ScopeProvider,
			HTTPStatus:    status,
			OuterStatus:   status,
			ProviderCode:  "upstream_transport_error",
			SemanticCode:  "upstream_transport_error",
			SemanticType:  "server_error",
			StreamPhase:   failurecontract.StreamPhaseUnknown,
			Retryable:     true,
			Cause:         err,
			PublicMessage: "upstream transport failed",
		}
	}
	status := statusCodeFromError(err)
	if status > 0 && !(status == http.StatusInternalServerError && errorCodeFromError(err) == "") {
		return err
	}
	classified := failurecontract.Classify(err)
	if classified == nil {
		classified = &failurecontract.Failure{}
	}
	provider500 := status == http.StatusInternalServerError && errorCodeFromError(err) == ""
	if provider500 {
		classified.Kind = failurecontract.ProviderUnavailable
		classified.Scope = failurecontract.ScopeProvider
		classified.ProviderCode = "upstream_http_500"
		classified.SemanticCode = "upstream_http_500"
		classified.SemanticType = "server_error"
		classified.Retryable = true
	} else {
		if classified.Kind == "" {
			classified.Kind = failurecontract.InternalTransformError
		}
		if classified.Scope == "" {
			classified.Scope = failurecontract.ScopeRequest
		}
		classified.Retryable = false
	}
	classified.HTTPStatus = http.StatusInternalServerError
	if classified.OuterStatus <= 0 {
		classified.OuterStatus = http.StatusInternalServerError
	}
	if classified.SemanticCode == "" {
		classified.SemanticCode = "internal_execution_error"
	}
	if classified.ProviderCode == "" {
		classified.ProviderCode = classified.SemanticCode
	}
	classified.Cause = err
	if provider500 {
		classified.PublicMessage = "upstream request failed"
	} else {
		classified.PublicMessage = "internal execution error"
	}
	return classified
}

func newCallerRequestFailure(err error) error {
	if err == nil {
		return nil
	}
	return &failurecontract.Failure{
		Kind:          failurecontract.Cancelled,
		Scope:         failurecontract.ScopeRequest,
		HTTPStatus:    499,
		OuterStatus:   499,
		ProviderCode:  "request_canceled",
		SemanticCode:  "request_canceled",
		SemanticType:  "canceled",
		StreamPhase:   failurecontract.StreamPhaseUnknown,
		Retryable:     false,
		Cause:         err,
		PublicMessage: "request canceled by client",
	}
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
		manager:                      m,
		runOnce:                      m.executeStreamMixedOnce,
		configureGPTFirstEventPolicy: true,
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
