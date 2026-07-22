package handlers

import (
	"bytes"
	"context"
	"testing"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestAmplificationPolicyControlsProductionRequestContext(t *testing.T) {
	from := sdktranslator.Format("handler-amplification-policy-input")
	to := sdktranslator.Format("handler-amplification-policy-output")
	translationRegistry := sdktranslator.NewRegistry()
	translationRegistry.Register(from, to, func(_ string, _ []byte, _ bool) []byte {
		return bytes.Repeat([]byte("x"), 300<<10)
	}, sdktranslator.ResponseTransform{})

	input := []byte(`{}`)
	for _, test := range []struct {
		mode        string
		wantFailure bool
	}{
		{mode: "observe"},
		{mode: "enforce", wantFailure: true},
	} {
		t.Run(test.mode, func(t *testing.T) {
			cfg := &config.SDKConfig{}
			cfg.RequestGuards.Amplification.Mode = test.mode
			handler := NewBaseAPIHandlers(cfg, nil)
			snapshot := handler.AmplificationGuardSnapshot()
			if !snapshot.Configured || snapshot.Mode != test.mode {
				t.Fatalf("amplification snapshot = %+v, want configured %s", snapshot, test.mode)
			}
			requestContext := sdktranslator.ContextWithRegistry(context.Background(), translationRegistry)
			ctx, release, errAdmission := handler.inspectAndAcquireAdmission(requestContext, input, nil)
			if errAdmission != nil {
				t.Fatalf("inspectAndAcquireAdmission() error = %v", errAdmission)
			}
			defer release()

			mode, configured := internalpayload.AmplificationModeFromContext(ctx)
			if !configured || string(mode) != test.mode {
				t.Fatalf("request amplification mode = %q configured=%t, want %q true", mode, configured, test.mode)
			}
			output, errTranslate := helps.TranslateRequestGuarded(
				ctx,
				"legacy.translate.handler_policy",
				from,
				to,
				"",
				input,
				false,
				internalpayload.AmplificationOverride{},
			)
			if test.wantFailure {
				if output != nil {
					t.Fatalf("enforce mode retained %d rejected bytes", len(output))
				}
				typed, ok := failurecontract.As(errTranslate)
				if !ok || typed.Kind != failurecontract.InvalidRequest || typed.ProviderCode != "request_transform_expansion_exceeded" {
					t.Fatalf("enforce failure = %#v", typed)
				}
			} else if errTranslate != nil || len(output) != 300<<10 {
				t.Fatalf("observe translation = %d bytes, error = %v", len(output), errTranslate)
			}

			report, ok := internalpayload.TransformReportFromContext(ctx)
			if !ok || len(report.Stages) != 1 || !report.Stages[0].Amplification.Exceeded {
				t.Fatalf("transform report = %#v", report)
			}
		})
	}
}

func TestAmplificationPolicyLeavesUnconfiguredSDKContextExplicit(t *testing.T) {
	handler := NewBaseAPIHandlers(&config.SDKConfig{}, nil)
	if snapshot := handler.AmplificationGuardSnapshot(); snapshot.Configured || snapshot.Mode != "enforce" {
		t.Fatalf("unconfigured amplification snapshot = %+v, want effective enforce", snapshot)
	}
	ctx, release, errAdmission := handler.inspectAndAcquireAdmission(context.Background(), []byte(`{}`), nil)
	if errAdmission != nil {
		t.Fatalf("inspectAndAcquireAdmission() error = %v", errAdmission)
	}
	defer release()
	if mode, configured := internalpayload.AmplificationModeFromContext(ctx); configured {
		t.Fatalf("unconfigured SDK request mode = %q, want no context override", mode)
	}
}

func TestAmplificationPolicyHotReloads(t *testing.T) {
	cfg := &config.SDKConfig{}
	cfg.RequestGuards.Amplification.Mode = "observe"
	handler := NewBaseAPIHandlers(cfg, nil)

	next := &config.SDKConfig{}
	next.RequestGuards.Amplification.Mode = "enforce"
	handler.UpdateClients(next)
	if snapshot := handler.AmplificationGuardSnapshot(); !snapshot.Configured || snapshot.Mode != "enforce" {
		t.Fatalf("reloaded amplification snapshot = %+v, want configured enforce", snapshot)
	}
}
