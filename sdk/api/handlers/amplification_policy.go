package handlers

import (
	"context"
	"strings"

	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

type amplificationGuardPolicy struct {
	mode internalpayload.AmplificationMode
}

// AmplificationGuardSnapshot exposes the effective request-transform policy.
// Configured is false for programmatic SDK callers that retain the explicit
// Enforce API default.
type AmplificationGuardSnapshot struct {
	Configured bool   `json:"configured"`
	Mode       string `json:"mode"`
}

func amplificationGuardPolicyFromConfig(cfg *config.SDKConfig) *amplificationGuardPolicy {
	if cfg == nil {
		return nil
	}
	mode := internalpayload.AmplificationMode(strings.ToLower(strings.TrimSpace(cfg.RequestGuards.Amplification.Mode)))
	if mode == "" {
		// Programmatic SDK configurations preserve the explicit Enforce API
		// behavior until they opt into a request-scoped policy. File loaders
		// populate observe mode before unmarshalling.
		return nil
	}
	if mode != internalpayload.AmplificationModeEnforce {
		mode = internalpayload.AmplificationModeObserve
	}
	return &amplificationGuardPolicy{mode: mode}
}

func (h *BaseAPIHandler) updateAmplificationGuardPolicy(cfg *config.SDKConfig) {
	if h == nil {
		return
	}
	h.amplification.Store(amplificationGuardPolicyFromConfig(cfg))
}

func (h *BaseAPIHandler) withAmplificationGuardMode(ctx context.Context) context.Context {
	if h == nil {
		return ctx
	}
	policy := h.amplification.Load()
	if policy == nil {
		return ctx
	}
	return internalpayload.WithAmplificationMode(ctx, policy.mode)
}

func (h *BaseAPIHandler) amplificationGuardMode() (internalpayload.AmplificationMode, bool) {
	if h == nil {
		return "", false
	}
	policy := h.amplification.Load()
	if policy == nil {
		return "", false
	}
	return policy.mode, true
}

// AmplificationGuardSnapshot returns the effective request-transform mode.
func (h *BaseAPIHandler) AmplificationGuardSnapshot() AmplificationGuardSnapshot {
	mode, configured := h.amplificationGuardMode()
	if !configured {
		mode = internalpayload.AmplificationModeEnforce
	}
	return AmplificationGuardSnapshot{Configured: configured, Mode: string(mode)}
}
