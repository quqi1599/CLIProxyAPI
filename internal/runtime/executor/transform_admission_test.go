package executor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

type executorPreparationGate struct {
	active, acquisitions atomic.Int32
	err                  error
}

func (g *executorPreparationGate) AcquireTransform(context.Context, int64) (func(time.Duration), error) {
	g.acquisitions.Add(1)
	if g.err != nil {
		return nil, g.err
	}
	g.active.Add(1)
	return func(time.Duration) { g.active.Add(-1) }, nil
}

func TestExecutorPreparationReleasesBeforeUpstream(t *testing.T) {
	cases := []struct {
		name, model, body string
		format            translator.Format
		executor          coreauth.ProviderExecutor
	}{
		{"codex", "gpt-5.5", `{"input":[{"role":"user","content":"hello"}]}`, translator.FormatCodex, NewCodexExecutor(&config.Config{})},
		{"claude", "claude-sonnet-4-6", `{"messages":[{"role":"user","content":"hello"}],"max_tokens":32}`, translator.FormatClaude, NewClaudeExecutor(&config.Config{})},
		{"openai", "test-model", `{"messages":[{"role":"user","content":"hello"}]}`, translator.FormatOpenAI, NewOpenAICompatExecutor("test", &config.Config{})},
		{"gemini", "gemini-3.1-pro-preview", `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`, translator.FormatGemini, NewGeminiExecutor(&config.Config{})},
		{"kimi", "kimi-k2.5", `{"messages":[{"role":"user","content":"hello"}]}`, translator.FormatOpenAI, NewKimiExecutor(&config.Config{})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gate := &executorPreparationGate{}
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if gate.active.Load() != 0 {
					t.Error("transform capacity held during upstream IO")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"test rejection"}}`))
			}))
			defer upstream.Close()
			ctx := internalpayload.WithTransformAdmission(context.Background(), gate)
			auth := &coreauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": upstream.URL}}
			req := coreexecutor.Request{Model: tc.model, Payload: []byte(tc.body)}
			_, _ = tc.executor.Execute(ctx, auth, req, coreexecutor.Options{SourceFormat: tc.format})
			if calls.Load() != 1 || gate.acquisitions.Load() == 0 || gate.active.Load() != 0 {
				t.Fatalf("calls=%d acquires=%d active=%d", calls.Load(), gate.acquisitions.Load(), gate.active.Load())
			}
			gate.err = errors.New("test transform saturation")
			_, err := tc.executor.Execute(ctx, auth, req, coreexecutor.Options{SourceFormat: tc.format})
			if !errors.Is(err, gate.err) || calls.Load() != 1 {
				t.Fatalf("rejected transform reached upstream: calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}
