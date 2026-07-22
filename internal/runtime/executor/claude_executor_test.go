package executor

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	xxHash64 "github.com/pierrec/xxHash/xxHash64"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/provideridentity"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func resetClaudeDeviceProfileCache() {
	helps.ResetClaudeDeviceProfileCache()
}

func TestPrepareClaudeRequest_StreamAndNonStreamShareCommonFixture(t *testing.T) {
	executor := NewClaudeExecutor(&config.Config{DisableClaudeCloakMode: true})
	auth := &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{
		"api_key":  "test-key",
		"base_url": "https://api.anthropic.com",
	}}
	req := cliproxyexecutor.Request{
		Model: "claude-3-5-sonnet-20241022",
		Payload: []byte(`{
			"model":"client-alias",
			"max_tokens":128,
			"betas":["prompt-caching-2024-07-31"],
			"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],
			"tools":[{"name":"skill:pet_animals","description":"pets","input_schema":{"type":"object"}}]
		}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}

	nonStream, err := executor.prepareClaudeRequest(context.Background(), auth, req, opts, req.Model, false)
	if err != nil {
		t.Fatalf("prepareClaudeRequest(non-stream) error = %v", err)
	}
	stream, err := executor.prepareClaudeRequest(context.Background(), auth, req, opts, req.Model, true)
	if err != nil {
		t.Fatalf("prepareClaudeRequest(stream) error = %v", err)
	}

	if nonStream.upstreamStream {
		t.Fatal("non-stream plan unexpectedly requests an upstream stream")
	}
	if !stream.upstreamStream {
		t.Fatal("stream plan does not request an upstream stream")
	}
	if !bytes.Equal(nonStream.bodyForTranslation, stream.bodyForTranslation) {
		t.Fatalf("translation bodies differ:\nnon-stream: %s\nstream: %s", nonStream.bodyForTranslation, stream.bodyForTranslation)
	}
	if !bytes.Equal(nonStream.bodyForUpstream, stream.bodyForUpstream) {
		t.Fatalf("upstream bodies differ:\nnon-stream: %s\nstream: %s", nonStream.bodyForUpstream, stream.bodyForUpstream)
	}
	wantIdentity := provideridentity.Identity{CanonicalProvider: "claude", ExecutorKey: "claude", Source: provideridentity.SourceDefault, BaseHost: "api.anthropic.com"}
	if nonStream.providerIdentity != wantIdentity || stream.providerIdentity != wantIdentity {
		t.Fatalf("provider identities differ: non-stream=%+v stream=%+v want=%+v", nonStream.providerIdentity, stream.providerIdentity, wantIdentity)
	}
	if got := nonStream.extraBetas; len(got) != 1 || got[0] != "prompt-caching-2024-07-31" {
		t.Fatalf("extraBetas = %v, want prompt-caching beta", got)
	}
	if !strings.EqualFold(strings.Join(nonStream.extraBetas, ","), strings.Join(stream.extraBetas, ",")) {
		t.Fatalf("beta header intent differs: non-stream=%v stream=%v", nonStream.extraBetas, stream.extraBetas)
	}
	if got := gjson.GetBytes(nonStream.bodyForUpstream, "model").String(); got != req.Model {
		t.Fatalf("upstream model = %q, want %q", got, req.Model)
	}
	if gjson.GetBytes(nonStream.bodyForUpstream, "betas").Exists() {
		t.Fatalf("upstream body still contains betas: %s", nonStream.bodyForUpstream)
	}
	if got := gjson.GetBytes(nonStream.bodyForUpstream, "tools.0.name").String(); strings.Contains(got, ":") {
		t.Fatalf("upstream tool name was not sanitized: %q", got)
	}
}

func malformedClaudeTreeSignatureForClaudeExecutorTest() string {
	return base64.StdEncoding.EncodeToString([]byte{0x12, 0xFF, 0xFE, 0xFD})
}

func newClaudeHeaderTestRequest(t *testing.T, incoming http.Header) *http.Request {
	t.Helper()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginReq := httptest.NewRequest(http.MethodPost, "http://localhost/v1/messages", nil)
	ginReq.Header = incoming.Clone()
	ginCtx.Request = ginReq

	req := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	return req.WithContext(context.WithValue(req.Context(), "gin", ginCtx))
}

func assertClaudeFingerprint(t *testing.T, headers http.Header, userAgent, pkgVersion, runtimeVersion, osName, arch string) {
	t.Helper()

	if got := headers.Get("User-Agent"); got != userAgent {
		t.Fatalf("User-Agent = %q, want %q", got, userAgent)
	}
	if got := headers.Get("X-Stainless-Package-Version"); got != pkgVersion {
		t.Fatalf("X-Stainless-Package-Version = %q, want %q", got, pkgVersion)
	}
	if got := headers.Get("X-Stainless-Runtime-Version"); got != runtimeVersion {
		t.Fatalf("X-Stainless-Runtime-Version = %q, want %q", got, runtimeVersion)
	}
	if got := headers.Get("X-Stainless-Os"); got != osName {
		t.Fatalf("X-Stainless-Os = %q, want %q", got, osName)
	}
	if got := headers.Get("X-Stainless-Arch"); got != arch {
		t.Fatalf("X-Stainless-Arch = %q, want %q", got, arch)
	}
}

func TestApplyClaudeHeaders_UsesConfiguredBaselineFingerprint(t *testing.T) {
	resetClaudeDeviceProfileCache()
	stabilize := true

	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.70 (external, cli)",
			PackageVersion:         "0.80.0",
			RuntimeVersion:         "v24.5.0",
			OS:                     "MacOS",
			Arch:                   "arm64",
			Timeout:                "900",
			StabilizeDeviceProfile: &stabilize,
		},
	}
	auth := &cliproxyauth.Auth{
		ID: "auth-baseline",
		Attributes: map[string]string{
			"api_key":                            "key-baseline",
			"header:User-Agent":                  "evil-client/9.9",
			"header:X-Stainless-Os":              "Linux",
			"header:X-Stainless-Arch":            "x64",
			"header:X-Stainless-Package-Version": "9.9.9",
		},
	}
	incoming := http.Header{
		"User-Agent":                  []string{"curl/8.7.1"},
		"X-Stainless-Package-Version": []string{"0.10.0"},
		"X-Stainless-Runtime-Version": []string{"v18.0.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	}

	req := newClaudeHeaderTestRequest(t, incoming)
	applyClaudeHeaders(req, auth, "key-baseline", false, nil, cfg)

	assertClaudeFingerprint(t, req.Header, "evil-client/9.9", "9.9.9", "v24.5.0", "Linux", "x64")
	if got := req.Header.Get("X-Stainless-Timeout"); got != "900" {
		t.Fatalf("X-Stainless-Timeout = %q, want %q", got, "900")
	}
}

func TestApplyClaudeHeaders_TracksHighestClaudeCLIFingerprint(t *testing.T) {
	resetClaudeDeviceProfileCache()
	stabilize := true

	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.60 (external, cli)",
			PackageVersion:         "0.70.0",
			RuntimeVersion:         "v22.0.0",
			OS:                     "MacOS",
			Arch:                   "arm64",
			StabilizeDeviceProfile: &stabilize,
		},
	}
	auth := &cliproxyauth.Auth{
		ID: "auth-upgrade",
		Attributes: map[string]string{
			"api_key": "key-upgrade",
		},
	}

	firstReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.62 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.74.0"},
		"X-Stainless-Runtime-Version": []string{"v24.3.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(firstReq, auth, "key-upgrade", false, nil, cfg)
	assertClaudeFingerprint(t, firstReq.Header, "claude-cli/2.1.62 (external, cli)", "0.74.0", "v24.3.0", "MacOS", "arm64")

	thirdPartyReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"lobe-chat/1.0"},
		"X-Stainless-Package-Version": []string{"0.10.0"},
		"X-Stainless-Runtime-Version": []string{"v18.0.0"},
		"X-Stainless-Os":              []string{"Windows"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(thirdPartyReq, auth, "key-upgrade", false, nil, cfg)
	assertClaudeFingerprint(t, thirdPartyReq.Header, "claude-cli/2.1.62 (external, cli)", "0.74.0", "v24.3.0", "MacOS", "arm64")

	higherReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.63 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.75.0"},
		"X-Stainless-Runtime-Version": []string{"v24.4.0"},
		"X-Stainless-Os":              []string{"MacOS"},
		"X-Stainless-Arch":            []string{"arm64"},
	})
	applyClaudeHeaders(higherReq, auth, "key-upgrade", false, nil, cfg)
	assertClaudeFingerprint(t, higherReq.Header, "claude-cli/2.1.63 (external, cli)", "0.75.0", "v24.4.0", "MacOS", "arm64")

	lowerReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.61 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.73.0"},
		"X-Stainless-Runtime-Version": []string{"v24.2.0"},
		"X-Stainless-Os":              []string{"Windows"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(lowerReq, auth, "key-upgrade", false, nil, cfg)
	assertClaudeFingerprint(t, lowerReq.Header, "claude-cli/2.1.63 (external, cli)", "0.75.0", "v24.4.0", "MacOS", "arm64")
}

func TestApplyClaudeHeaders_DoesNotDowngradeConfiguredBaselineOnFirstClaudeClient(t *testing.T) {
	resetClaudeDeviceProfileCache()
	stabilize := true

	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.70 (external, cli)",
			PackageVersion:         "0.80.0",
			RuntimeVersion:         "v24.5.0",
			OS:                     "MacOS",
			Arch:                   "arm64",
			StabilizeDeviceProfile: &stabilize,
		},
	}
	auth := &cliproxyauth.Auth{
		ID: "auth-baseline-floor",
		Attributes: map[string]string{
			"api_key": "key-baseline-floor",
		},
	}

	olderClaudeReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.62 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.74.0"},
		"X-Stainless-Runtime-Version": []string{"v24.3.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(olderClaudeReq, auth, "key-baseline-floor", false, nil, cfg)
	assertClaudeFingerprint(t, olderClaudeReq.Header, "claude-cli/2.1.70 (external, cli)", "0.80.0", "v24.5.0", "MacOS", "arm64")

	newerClaudeReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.71 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.81.0"},
		"X-Stainless-Runtime-Version": []string{"v24.6.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(newerClaudeReq, auth, "key-baseline-floor", false, nil, cfg)
	assertClaudeFingerprint(t, newerClaudeReq.Header, "claude-cli/2.1.71 (external, cli)", "0.81.0", "v24.6.0", "MacOS", "arm64")
}

func TestApplyClaudeHeaders_UpgradesCachedSoftwareFingerprintWhenBaselineAdvances(t *testing.T) {
	resetClaudeDeviceProfileCache()
	stabilize := true

	oldCfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.70 (external, cli)",
			PackageVersion:         "0.80.0",
			RuntimeVersion:         "v24.5.0",
			OS:                     "MacOS",
			Arch:                   "arm64",
			StabilizeDeviceProfile: &stabilize,
		},
	}
	newCfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.77 (external, cli)",
			PackageVersion:         "0.87.0",
			RuntimeVersion:         "v24.8.0",
			OS:                     "MacOS",
			Arch:                   "arm64",
			StabilizeDeviceProfile: &stabilize,
		},
	}
	auth := &cliproxyauth.Auth{
		ID: "auth-baseline-reload",
		Attributes: map[string]string{
			"api_key": "key-baseline-reload",
		},
	}

	officialReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.71 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.81.0"},
		"X-Stainless-Runtime-Version": []string{"v24.6.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(officialReq, auth, "key-baseline-reload", false, nil, oldCfg)
	assertClaudeFingerprint(t, officialReq.Header, "claude-cli/2.1.71 (external, cli)", "0.81.0", "v24.6.0", "MacOS", "arm64")

	thirdPartyReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"curl/8.7.1"},
		"X-Stainless-Package-Version": []string{"0.10.0"},
		"X-Stainless-Runtime-Version": []string{"v18.0.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(thirdPartyReq, auth, "key-baseline-reload", false, nil, newCfg)
	assertClaudeFingerprint(t, thirdPartyReq.Header, "claude-cli/2.1.77 (external, cli)", "0.87.0", "v24.8.0", "MacOS", "arm64")
}

func TestValidateMiniMaxToolResultAdjacencyRejectsIncompleteSequence(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"messages": [
			{
				"role": "assistant",
				"content": [
					{"type":"tool_use","id":"call_1","name":"read","input":{}},
					{"type":"tool_use","id":"call_2","name":"glob","input":{}},
					{"type":"tool_use","id":"call_3","name":"grep","input":{}}
				]
			},
			{
				"role": "user",
				"content": [
					{"type":"tool_result","tool_use_id":"call_1","content":"ok"}
				]
			},
			{
				"role": "user",
				"content": [
					{"type":"text","text":"continue"}
				]
			}
		]
	}`)

	err := validateMiniMaxToolResultAdjacency(body)
	if err == nil {
		t.Fatal("expected invalid MiniMax tool sequence error")
	}
	statusProvider, ok := err.(interface{ StatusCode() int })
	if !ok || statusProvider.StatusCode() != http.StatusBadRequest {
		t.Fatalf("expected bad request status error, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "tool_result must immediately follow tool_use") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateMiniMaxToolResultAdjacencyAcceptsCompletedSequence(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"messages": [
			{
				"role": "assistant",
				"content": [
					{"type":"tool_use","id":"call_1","name":"read","input":{}},
					{"type":"tool_use","id":"call_2","name":"glob","input":{}},
					{"type":"tool_use","id":"call_3","name":"grep","input":{}}
				]
			},
			{
				"role": "user",
				"content": [
					{"type":"tool_result","tool_use_id":"call_1","content":"ok"}
				]
			},
			{
				"role": "user",
				"content": [
					{"type":"tool_result","tool_use_id":"call_2","content":"ok"}
				]
			},
			{
				"role": "user",
				"content": [
					{"type":"tool_result","tool_use_id":"call_3","content":"ok"}
				]
			},
			{
				"role": "user",
				"content": [
					{"type":"text","text":"continue"}
				]
			}
		]
	}`)

	if err := validateMiniMaxToolResultAdjacency(body); err != nil {
		t.Fatalf("expected completed MiniMax tool sequence to pass, got %v", err)
	}
}

func TestValidateMiniMaxToolResultAdjacencyAcceptsOpenAIToolCycleAfterTranslation(t *testing.T) {
	t.Parallel()

	input := []byte(`{
		"model": "claude-sonnet-4-6",
		"messages": [
			{"role":"system","content":"Decide whether to continue."},
			{
				"role":"assistant",
				"content":"Analysis: no reply is needed.",
				"tool_calls":[{"id":"call_no_reply","type":"function","function":{"name":"no_reply","arguments":"{}"}}]
			},
			{"role":"tool","tool_call_id":"call_no_reply","content":"Wait for the next message."},
			{"role":"user","content":"New message arrived."}
		],
		"tools":[
			{"type":"function","function":{"name":"no_reply","parameters":{"type":"object","properties":{}}}}
		]
	}`)

	body := sdktranslator.TranslateRequest(sdktranslator.FromString("openai"), sdktranslator.FromString("claude"), "MiniMax-M2.7-highspeed", input, true)
	var err error
	body, err = repairClaudeToolUseHistory(body, "test")
	if err != nil {
		t.Fatalf("repairClaudeToolUseHistory() error = %v", err)
	}
	body = ensureCacheControl(body)
	body = enforceCacheControlLimit(body, 4)
	body = normalizeCacheControlTTL(body)

	if err := validateMiniMaxToolResultAdjacency(body); err != nil {
		t.Fatalf("expected translated OpenAI tool cycle to pass, got %v\nbody: %s", err, body)
	}
}

func TestClaudeProviderIdentityUsesSharedAttributeInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		auth    *cliproxyauth.Auth
		baseURL string
		want    provideridentity.Identity
	}{
		{
			name: "canonical attribute",
			auth: &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{
				"provider_key":    "configured-route",
				"provider_family": "openai-compatibility",
				"compat_name":     "Configured Route",
				"compat_kind":     "MiniMax",
			}},
			baseURL: "https://proxy.example.com/anthropic",
			want: provideridentity.Identity{
				CanonicalProvider: "minimax",
				ExecutorKey:       "configured-route",
				ProviderFamily:    "openai-compatibility",
				CompatName:        "Configured Route",
				Kind:              "minimax",
				Source:            provideridentity.SourceAttribute,
				BaseHost:          "proxy.example.com",
			},
		},
		{
			name: "legacy attribute",
			auth: &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{
				"compat-kind": "MiniMax",
			}},
			baseURL: "https://proxy.example.com/anthropic",
			want:    provideridentity.Identity{CanonicalProvider: "minimax", ExecutorKey: "claude", Kind: "minimax", Source: provideridentity.SourceAttribute, BaseHost: "proxy.example.com"},
		},
		{
			name:    "base URL inference",
			auth:    &cliproxyauth.Auth{Provider: "claude"},
			baseURL: "https://api.deepseek.com/anthropic",
			want:    provideridentity.Identity{CanonicalProvider: "deepseek", ExecutorKey: "claude", Kind: "deepseek", Source: provideridentity.SourceBaseURL, BaseHost: "api.deepseek.com"},
		},
		{
			name:    "known provider root host",
			auth:    &cliproxyauth.Auth{Provider: "claude"},
			baseURL: "https://api.deepseek.com",
			want:    provideridentity.Identity{CanonicalProvider: "deepseek", ExecutorKey: "claude", Kind: "deepseek", Source: provideridentity.SourceBaseURL, BaseHost: "api.deepseek.com"},
		},
		{
			name: "stale URL-derived attribute",
			auth: &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{
				"compat_kind":                        "minimax",
				provideridentity.KindSourceAttribute: string(provideridentity.SourceBaseURL),
				"base_url":                           "https://api.deepseek.com/anthropic",
			}},
			baseURL: "https://api.deepseek.com/anthropic",
			want:    provideridentity.Identity{CanonicalProvider: "deepseek", ExecutorKey: "claude", Kind: "deepseek", Source: provideridentity.SourceBaseURL, BaseHost: "api.deepseek.com"},
		},
		{
			name: "explicit attribute source",
			auth: &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{
				"compat_kind":                        "minimax",
				provideridentity.KindSourceAttribute: string(provideridentity.SourceCompatConfig),
				"base_url":                           "https://api.deepseek.com/anthropic",
			}},
			baseURL: "https://api.deepseek.com/anthropic",
			want:    provideridentity.Identity{CanonicalProvider: "minimax", ExecutorKey: "claude", Kind: "minimax", Source: provideridentity.SourceCompatConfig, BaseHost: "api.deepseek.com"},
		},
		{
			name:    "native Claude",
			auth:    &cliproxyauth.Auth{Provider: "claude"},
			baseURL: "https://api.anthropic.com",
			want:    provideridentity.Identity{CanonicalProvider: "claude", ExecutorKey: "claude", Source: provideridentity.SourceDefault, BaseHost: "api.anthropic.com"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := claudeProviderIdentity(tt.auth, tt.baseURL); got != tt.want {
				t.Fatalf("claudeProviderIdentity() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestRepairMiniMaxToolResultAdjacencySplitsMixedUserContent(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"messages": [
			{
				"role": "assistant",
				"content": [
					{"type":"tool_use","id":"call_1","name":"read","input":{}}
				]
			},
			{
				"role": "user",
				"content": [
					{"type":"text","text":"new user content"},
					{"type":"tool_result","tool_use_id":"call_1","content":"ok"}
				]
			}
		]
	}`)

	out, repairs, err := repairMiniMaxToolResultAdjacency(body)
	if err != nil {
		t.Fatalf("repairMiniMaxToolResultAdjacency() error = %v", err)
	}
	if repairs != 1 {
		t.Fatalf("repairs = %d, want 1", repairs)
	}
	if err := validateMiniMaxToolResultAdjacency(out); err != nil {
		t.Fatalf("expected repaired sequence to pass, got %v\nbody: %s", err, out)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	if len(msgs) != 3 {
		t.Fatalf("messages length = %d, want 3: %s", len(msgs), gjson.GetBytes(out, "messages").Raw)
	}
	if got := msgs[1].Get("content.0.type").String(); got != "tool_result" {
		t.Fatalf("message 1 content type = %q, want tool_result: %s", got, msgs[1].Raw)
	}
	if got := msgs[2].Get("content.0.type").String(); got != "text" {
		t.Fatalf("message 2 content type = %q, want text: %s", got, msgs[2].Raw)
	}
}

func TestRepairClaudeToolAdjacencyForDeepSeekCompat(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"messages": [
			{
				"role": "assistant",
				"content": [
					{"type":"tool_use","id":"browser_back","name":"browser_back","input":{}}
				]
			},
			{
				"role": "user",
				"content": [
					{"type":"text","text":"next user instruction"},
					{"type":"tool_result","tool_use_id":"browser_back","content":"ok"}
				]
			}
		]
	}`)

	out, err := repairMiniMaxClaudeToolAdjacencyForCompat("deepseek", body)
	if err != nil {
		t.Fatalf("repairMiniMaxClaudeToolAdjacencyForCompat() error = %v", err)
	}
	if err := validateMiniMaxToolResultAdjacency(out); err != nil {
		t.Fatalf("expected repaired DeepSeek sequence to pass, got %v\nbody: %s", err, out)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	if len(msgs) != 3 {
		t.Fatalf("messages length = %d, want 3: %s", len(msgs), gjson.GetBytes(out, "messages").Raw)
	}
	if got := msgs[1].Get("content.0.type").String(); got != "tool_result" {
		t.Fatalf("message 1 content type = %q, want tool_result: %s", got, msgs[1].Raw)
	}
	if got := msgs[2].Get("content.0.type").String(); got != "text" {
		t.Fatalf("message 2 content type = %q, want text: %s", got, msgs[2].Raw)
	}
}

func TestNormalizeClaudeEmptyToolResults(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{
			name:    "missing content",
			content: `{"type":"tool_result","tool_use_id":"call_1"}`,
		},
		{
			name:    "null content",
			content: `{"type":"tool_result","tool_use_id":"call_1","content":null}`,
		},
		{
			name:    "empty string content",
			content: `{"type":"tool_result","tool_use_id":"call_1","content":""}`,
		},
		{
			name:    "blank string content",
			content: `{"type":"tool_result","tool_use_id":"call_1","content":"   "}`,
		},
		{
			name:    "empty array content",
			content: `{"type":"tool_result","tool_use_id":"call_1","content":[]}`,
		},
		{
			name:    "blank text block content",
			content: `{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"   "}]} `,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := []byte(`{"messages":[{"role":"user","content":[` + tt.content + `]}]}`)
			out, repairs, err := normalizeClaudeEmptyToolResults(input)
			if err != nil {
				t.Fatalf("normalizeClaudeEmptyToolResults() error = %v", err)
			}
			if repairs != 1 {
				t.Fatalf("normalizeClaudeEmptyToolResults() repairs = %d, want 1", repairs)
			}
			got := gjson.GetBytes(out, "messages.0.content.0.content")
			if !got.IsArray() {
				t.Fatalf("tool_result content should be an array, got %s", got.Raw)
			}
			if got.Get("0.type").String() != "text" {
				t.Fatalf("tool_result content[0].type = %q, want %q", got.Get("0.type").String(), "text")
			}
			if got.Get("0.text").String() != "No output." {
				t.Fatalf("tool_result content[0].text = %q, want No output.", got.Get("0.text").String())
			}
		})
	}
}

func TestNormalizeClaudeEmptyToolResultsPreservesNonTextContent(t *testing.T) {
	input := []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]}]}`)

	out, repairs, err := normalizeClaudeEmptyToolResults(input)
	if err != nil {
		t.Fatalf("normalizeClaudeEmptyToolResults() error = %v", err)
	}
	if repairs != 0 {
		t.Fatalf("normalizeClaudeEmptyToolResults() repairs = %d, want 0", repairs)
	}
	if string(out) != string(input) {
		t.Fatalf("non-text content should be preserved:\n got: %s\nwant: %s", out, input)
	}
}

func TestRepairMiniMaxToolResultAdjacencyMovesAssistantToolUseLast(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"messages": [
			{
				"role": "assistant",
				"content": [
					{"type":"text","text":"before"},
					{"type":"tool_use","id":"call_1","name":"read","input":{}},
					{"type":"text","text":"after"}
				]
			},
			{
				"role": "user",
				"content": [
					{"type":"tool_result","tool_use_id":"call_1","content":"ok"}
				]
			}
		]
	}`)

	out, repairs, err := repairMiniMaxToolResultAdjacency(body)
	if err != nil {
		t.Fatalf("repairMiniMaxToolResultAdjacency() error = %v", err)
	}
	if repairs != 1 {
		t.Fatalf("repairs = %d, want 1", repairs)
	}
	content := gjson.GetBytes(out, "messages.0.content").Array()
	if got := content[len(content)-1].Get("type").String(); got != "tool_use" {
		t.Fatalf("last assistant content type = %q, want tool_use: %s", got, gjson.GetBytes(out, "messages.0.content").Raw)
	}
	if err := validateMiniMaxToolResultAdjacency(out); err != nil {
		t.Fatalf("expected repaired sequence to pass, got %v\nbody: %s", err, out)
	}
}

func TestRepairClaudeHistoryThenMiniMaxAdjacencyHandlesMixedToolResults(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"messages": [
			{
				"role": "assistant",
				"content": [
					{"type":"tool_use","id":"call_1","name":"read","input":{}},
					{"type":"tool_use","id":"call_2","name":"grep","input":{}}
				]
			},
			{
				"role": "user",
				"content": [
					{"type":"text","text":"continue with this instruction"},
					{"type":"tool_result","tool_use_id":"call_2","content":"grep ok"},
					{"type":"tool_result","tool_use_id":"call_1","content":"read ok"}
				]
			}
		]
	}`)

	repaired, err := repairClaudeToolUseHistory(body, "test")
	if err != nil {
		t.Fatalf("repairClaudeToolUseHistory() error = %v", err)
	}
	out, repairs, err := repairMiniMaxToolResultAdjacency(repaired)
	if err != nil {
		t.Fatalf("repairMiniMaxToolResultAdjacency() error = %v", err)
	}
	if repairs != 1 {
		t.Fatalf("repairs = %d, want 1", repairs)
	}
	if err := validateMiniMaxToolResultAdjacency(out); err != nil {
		t.Fatalf("expected repaired sequence to pass, got %v\nbody: %s", err, out)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	if len(msgs) != 3 {
		t.Fatalf("messages length = %d, want 3: %s", len(msgs), gjson.GetBytes(out, "messages").Raw)
	}
	if got := msgs[1].Get("content.0.tool_use_id").String(); got != "call_1" {
		t.Fatalf("first split tool_result = %q, want call_1: %s", got, msgs[1].Raw)
	}
	if got := msgs[1].Get("content.1.tool_use_id").String(); got != "call_2" {
		t.Fatalf("second split tool_result = %q, want call_2: %s", got, msgs[1].Raw)
	}
	if got := msgs[2].Get("content.0.type").String(); got != "text" {
		t.Fatalf("following user content type = %q, want text: %s", got, msgs[2].Raw)
	}
}

func TestRepairMiniMaxToolResultAdjacencyPreservesPendingAcrossPureToolResultMessage(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"messages": [
			{
				"role": "assistant",
				"content": [
					{"type":"tool_use","id":"call_1","name":"read","input":{}},
					{"type":"tool_use","id":"call_2","name":"grep","input":{}},
					{"type":"tool_use","id":"call_3","name":"glob","input":{}}
				]
			},
			{
				"role": "user",
				"content": [
					{"type":"tool_result","tool_use_id":"call_1","content":"read ok"},
					{"type":"tool_result","tool_use_id":"call_2","content":"grep ok"}
				]
			},
			{
				"role": "user",
				"content": [
					{"type":"text","text":"continue after tool outputs"},
					{"type":"tool_result","tool_use_id":"call_3","content":"glob ok"}
				]
			}
		]
	}`)

	out, repairs, err := repairMiniMaxToolResultAdjacency(body)
	if err != nil {
		t.Fatalf("repairMiniMaxToolResultAdjacency() error = %v", err)
	}
	if repairs != 1 {
		t.Fatalf("repairs = %d, want 1", repairs)
	}
	if err := validateMiniMaxToolResultAdjacency(out); err != nil {
		t.Fatalf("expected repaired sequence to pass, got %v\nbody: %s", err, out)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	if len(msgs) != 4 {
		t.Fatalf("messages length = %d, want 4: %s", len(msgs), gjson.GetBytes(out, "messages").Raw)
	}
	if got := msgs[2].Get("content.0.tool_use_id").String(); got != "call_3" {
		t.Fatalf("split tool_result = %q, want call_3: %s", got, msgs[2].Raw)
	}
	if got := msgs[3].Get("content.0.type").String(); got != "text" {
		t.Fatalf("trailing user content type = %q, want text: %s", got, msgs[3].Raw)
	}
}

func TestRepairMiniMaxClaudeToolAdjacencyForCompatWithLogSkipsWhenNoToolResultMarker(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"messages": [
			{
				"role": "assistant",
				"content": [
					{"type":"text","text":"before"},
					{"type":"tool_use","id":"call_1","name":"read","input":{}},
					{"type":"text","text":"after"}
				]
			}
		]
	}`)
	meta := compatRepairLogMeta{compatKind: "minimax"}

	out, err := repairMiniMaxClaudeToolAdjacencyForCompatWithLog(context.Background(), body, meta)
	if err != nil {
		t.Fatalf("repairMiniMaxClaudeToolAdjacencyForCompatWithLog() error = %v", err)
	}
	if string(out) != string(body) {
		t.Fatalf("body changed without tool_result marker:\n got: %s\nwant: %s", out, body)
	}
}

func TestRepairClaudeToolUseHistoryWithCompatLogDropsOrphanToolResultWithoutToolUseMarker(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"messages": [
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"call_1","content":"orphan"},
				{"type":"text","text":"continue"}
			]}
		]
	}`)
	meta := compatRepairLogMeta{compatKind: "minimax"}

	out, err := repairClaudeToolUseHistoryWithCompatLog(context.Background(), body, meta)
	if err != nil {
		t.Fatalf("repairClaudeToolUseHistoryWithCompatLog() error = %v", err)
	}
	if strings.Contains(string(out), `"tool_use_id":"call_1"`) {
		t.Fatalf("orphan tool_result should be dropped: %s", out)
	}
	if !strings.Contains(string(out), `"text":"continue"`) {
		t.Fatalf("non-tool user content should be preserved: %s", out)
	}
}

func TestApplyClaudeHeaders_LearnsOfficialFingerprintAfterCustomBaselineFallback(t *testing.T) {
	resetClaudeDeviceProfileCache()
	stabilize := true

	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "my-gateway/1.0",
			PackageVersion:         "custom-pkg",
			RuntimeVersion:         "custom-runtime",
			OS:                     "MacOS",
			Arch:                   "arm64",
			StabilizeDeviceProfile: &stabilize,
		},
	}
	auth := &cliproxyauth.Auth{
		ID: "auth-custom-baseline-learning",
		Attributes: map[string]string{
			"api_key": "key-custom-baseline-learning",
		},
	}

	thirdPartyReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"curl/8.7.1"},
		"X-Stainless-Package-Version": []string{"0.10.0"},
		"X-Stainless-Runtime-Version": []string{"v18.0.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(thirdPartyReq, auth, "key-custom-baseline-learning", false, nil, cfg)
	assertClaudeFingerprint(t, thirdPartyReq.Header, "my-gateway/1.0", "custom-pkg", "custom-runtime", "MacOS", "arm64")

	officialReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.77 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.87.0"},
		"X-Stainless-Runtime-Version": []string{"v24.8.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(officialReq, auth, "key-custom-baseline-learning", false, nil, cfg)
	assertClaudeFingerprint(t, officialReq.Header, "claude-cli/2.1.77 (external, cli)", "0.87.0", "v24.8.0", "MacOS", "arm64")

	postLearningThirdPartyReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"curl/8.7.1"},
		"X-Stainless-Package-Version": []string{"0.10.0"},
		"X-Stainless-Runtime-Version": []string{"v18.0.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(postLearningThirdPartyReq, auth, "key-custom-baseline-learning", false, nil, cfg)
	assertClaudeFingerprint(t, postLearningThirdPartyReq.Header, "claude-cli/2.1.77 (external, cli)", "0.87.0", "v24.8.0", "MacOS", "arm64")
}

func TestResolveClaudeDeviceProfile_RechecksCacheBeforeStoringCandidate(t *testing.T) {
	resetClaudeDeviceProfileCache()
	stabilize := true

	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.60 (external, cli)",
			PackageVersion:         "0.70.0",
			RuntimeVersion:         "v22.0.0",
			OS:                     "MacOS",
			Arch:                   "arm64",
			StabilizeDeviceProfile: &stabilize,
		},
	}
	auth := &cliproxyauth.Auth{
		ID: "auth-racy-upgrade",
		Attributes: map[string]string{
			"api_key": "key-racy-upgrade",
		},
	}

	lowPaused := make(chan struct{})
	releaseLow := make(chan struct{})
	var pauseOnce sync.Once
	var releaseOnce sync.Once

	helps.ClaudeDeviceProfileBeforeCandidateStore = func(candidate helps.ClaudeDeviceProfile) {
		if candidate.UserAgent != "claude-cli/2.1.62 (external, cli)" {
			return
		}
		pauseOnce.Do(func() { close(lowPaused) })
		<-releaseLow
	}
	t.Cleanup(func() {
		helps.ClaudeDeviceProfileBeforeCandidateStore = nil
		releaseOnce.Do(func() { close(releaseLow) })
	})

	lowResultCh := make(chan helps.ClaudeDeviceProfile, 1)
	go func() {
		lowResultCh <- helps.ResolveClaudeDeviceProfile(auth, "key-racy-upgrade", http.Header{
			"User-Agent":                  []string{"claude-cli/2.1.62 (external, cli)"},
			"X-Stainless-Package-Version": []string{"0.74.0"},
			"X-Stainless-Runtime-Version": []string{"v24.3.0"},
			"X-Stainless-Os":              []string{"Linux"},
			"X-Stainless-Arch":            []string{"x64"},
		}, cfg)
	}()

	select {
	case <-lowPaused:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for lower candidate to pause before storing")
	}

	highResult := helps.ResolveClaudeDeviceProfile(auth, "key-racy-upgrade", http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.63 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.75.0"},
		"X-Stainless-Runtime-Version": []string{"v24.4.0"},
		"X-Stainless-Os":              []string{"MacOS"},
		"X-Stainless-Arch":            []string{"arm64"},
	}, cfg)
	releaseOnce.Do(func() { close(releaseLow) })

	select {
	case lowResult := <-lowResultCh:
		if lowResult.UserAgent != "claude-cli/2.1.63 (external, cli)" {
			t.Fatalf("lowResult.UserAgent = %q, want %q", lowResult.UserAgent, "claude-cli/2.1.63 (external, cli)")
		}
		if lowResult.PackageVersion != "0.75.0" {
			t.Fatalf("lowResult.PackageVersion = %q, want %q", lowResult.PackageVersion, "0.75.0")
		}
		if lowResult.OS != "MacOS" || lowResult.Arch != "arm64" {
			t.Fatalf("lowResult platform = %s/%s, want %s/%s", lowResult.OS, lowResult.Arch, "MacOS", "arm64")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for lower candidate result")
	}

	if highResult.UserAgent != "claude-cli/2.1.63 (external, cli)" {
		t.Fatalf("highResult.UserAgent = %q, want %q", highResult.UserAgent, "claude-cli/2.1.63 (external, cli)")
	}
	if highResult.OS != "MacOS" || highResult.Arch != "arm64" {
		t.Fatalf("highResult platform = %s/%s, want %s/%s", highResult.OS, highResult.Arch, "MacOS", "arm64")
	}

	cached := helps.ResolveClaudeDeviceProfile(auth, "key-racy-upgrade", http.Header{
		"User-Agent": []string{"curl/8.7.1"},
	}, cfg)
	if cached.UserAgent != "claude-cli/2.1.63 (external, cli)" {
		t.Fatalf("cached.UserAgent = %q, want %q", cached.UserAgent, "claude-cli/2.1.63 (external, cli)")
	}
	if cached.PackageVersion != "0.75.0" {
		t.Fatalf("cached.PackageVersion = %q, want %q", cached.PackageVersion, "0.75.0")
	}
	if cached.OS != "MacOS" || cached.Arch != "arm64" {
		t.Fatalf("cached platform = %s/%s, want %s/%s", cached.OS, cached.Arch, "MacOS", "arm64")
	}
}

func TestApplyClaudeHeaders_ThirdPartyBaselineThenOfficialUpgradeKeepsPinnedPlatform(t *testing.T) {
	resetClaudeDeviceProfileCache()
	stabilize := true

	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.70 (external, cli)",
			PackageVersion:         "0.80.0",
			RuntimeVersion:         "v24.5.0",
			OS:                     "MacOS",
			Arch:                   "arm64",
			StabilizeDeviceProfile: &stabilize,
		},
	}
	auth := &cliproxyauth.Auth{
		ID: "auth-third-party-then-official",
		Attributes: map[string]string{
			"api_key": "key-third-party-then-official",
		},
	}

	thirdPartyReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"curl/8.7.1"},
		"X-Stainless-Package-Version": []string{"0.10.0"},
		"X-Stainless-Runtime-Version": []string{"v18.0.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(thirdPartyReq, auth, "key-third-party-then-official", false, nil, cfg)
	assertClaudeFingerprint(t, thirdPartyReq.Header, "claude-cli/2.1.70 (external, cli)", "0.80.0", "v24.5.0", "MacOS", "arm64")

	officialReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.77 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.87.0"},
		"X-Stainless-Runtime-Version": []string{"v24.8.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(officialReq, auth, "key-third-party-then-official", false, nil, cfg)
	assertClaudeFingerprint(t, officialReq.Header, "claude-cli/2.1.77 (external, cli)", "0.87.0", "v24.8.0", "MacOS", "arm64")
}

func TestApplyClaudeHeaders_DisableDeviceProfileStabilization(t *testing.T) {
	resetClaudeDeviceProfileCache()

	stabilize := false
	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.60 (external, cli)",
			PackageVersion:         "0.70.0",
			RuntimeVersion:         "v22.0.0",
			OS:                     "MacOS",
			Arch:                   "arm64",
			StabilizeDeviceProfile: &stabilize,
		},
	}
	auth := &cliproxyauth.Auth{
		ID: "auth-disable-stability",
		Attributes: map[string]string{
			"api_key": "key-disable-stability",
		},
	}

	firstReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.62 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.74.0"},
		"X-Stainless-Runtime-Version": []string{"v24.3.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(firstReq, auth, "key-disable-stability", false, nil, cfg)
	assertClaudeFingerprint(t, firstReq.Header, "claude-cli/2.1.62 (external, cli)", "0.74.0", "v24.3.0", "Linux", "x64")

	thirdPartyReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"lobe-chat/1.0"},
		"X-Stainless-Package-Version": []string{"0.10.0"},
		"X-Stainless-Runtime-Version": []string{"v18.0.0"},
		"X-Stainless-Os":              []string{"Windows"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(thirdPartyReq, auth, "key-disable-stability", false, nil, cfg)
	assertClaudeFingerprint(t, thirdPartyReq.Header, "claude-cli/2.1.60 (external, cli)", "0.10.0", "v18.0.0", "Windows", "x64")

	lowerReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.61 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.73.0"},
		"X-Stainless-Runtime-Version": []string{"v24.2.0"},
		"X-Stainless-Os":              []string{"Windows"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(lowerReq, auth, "key-disable-stability", false, nil, cfg)
	assertClaudeFingerprint(t, lowerReq.Header, "claude-cli/2.1.61 (external, cli)", "0.73.0", "v24.2.0", "Windows", "x64")
}

func TestApplyClaudeHeaders_LegacyModePreservesConfiguredUserAgentOverrideForClaudeClients(t *testing.T) {
	resetClaudeDeviceProfileCache()

	stabilize := false
	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.60 (external, cli)",
			PackageVersion:         "0.70.0",
			RuntimeVersion:         "v22.0.0",
			StabilizeDeviceProfile: &stabilize,
		},
	}
	auth := &cliproxyauth.Auth{
		ID: "auth-legacy-ua-override",
		Attributes: map[string]string{
			"api_key":           "key-legacy-ua-override",
			"header:User-Agent": "config-ua/1.0",
		},
	}

	req := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.62 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.74.0"},
		"X-Stainless-Runtime-Version": []string{"v24.3.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(req, auth, "key-legacy-ua-override", false, nil, cfg)

	assertClaudeFingerprint(t, req.Header, "config-ua/1.0", "0.74.0", "v24.3.0", "Linux", "x64")
}

func TestApplyClaudeHeaders_LegacyModeFallsBackToRuntimeOSArchWhenMissing(t *testing.T) {
	resetClaudeDeviceProfileCache()

	stabilize := false
	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.60 (external, cli)",
			PackageVersion:         "0.70.0",
			RuntimeVersion:         "v22.0.0",
			OS:                     "MacOS",
			Arch:                   "arm64",
			StabilizeDeviceProfile: &stabilize,
		},
	}
	auth := &cliproxyauth.Auth{
		ID: "auth-legacy-runtime-os-arch",
		Attributes: map[string]string{
			"api_key": "key-legacy-runtime-os-arch",
		},
	}

	req := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent": []string{"curl/8.7.1"},
	})
	applyClaudeHeaders(req, auth, "key-legacy-runtime-os-arch", false, nil, cfg)

	assertClaudeFingerprint(t, req.Header, "claude-cli/2.1.60 (external, cli)", "0.70.0", "v22.0.0", helps.MapStainlessOS(), helps.MapStainlessArch())
}

func TestApplyClaudeHeaders_UnsetStabilizationAlsoUsesLegacyRuntimeOSArchFallback(t *testing.T) {
	resetClaudeDeviceProfileCache()

	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:      "claude-cli/2.1.60 (external, cli)",
			PackageVersion: "0.70.0",
			RuntimeVersion: "v22.0.0",
			OS:             "MacOS",
			Arch:           "arm64",
		},
	}
	auth := &cliproxyauth.Auth{
		ID: "auth-unset-runtime-os-arch",
		Attributes: map[string]string{
			"api_key": "key-unset-runtime-os-arch",
		},
	}

	req := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent": []string{"curl/8.7.1"},
	})
	applyClaudeHeaders(req, auth, "key-unset-runtime-os-arch", false, nil, cfg)

	assertClaudeFingerprint(t, req.Header, "claude-cli/2.1.60 (external, cli)", "0.70.0", "v22.0.0", helps.MapStainlessOS(), helps.MapStainlessArch())
}

func TestClaudeDeviceProfileStabilizationEnabled_DefaultFalse(t *testing.T) {
	if helps.ClaudeDeviceProfileStabilizationEnabled(nil) {
		t.Fatal("expected nil config to default to disabled stabilization")
	}
	if helps.ClaudeDeviceProfileStabilizationEnabled(&config.Config{}) {
		t.Fatal("expected unset stabilize-device-profile to default to disabled stabilization")
	}
}

func TestSanitizeClaudeWebSearchDomains(t *testing.T) {
	// Mirrors the litellm payload from issue #2681: a non-empty allowed_domains
	// alongside an empty blocked_domains, which Anthropic rejects as ambiguous.
	input := []byte(`{"tools":[{"type":"web_search_20250305","name":"web_search","allowed_domains":["anthropic.com"],"blocked_domains":[],"max_uses":8}]}`)
	out := sanitizeClaudeWebSearchDomains(input)

	if gjson.GetBytes(out, "tools.0.blocked_domains").Exists() {
		t.Fatalf("empty blocked_domains should be removed: %s", string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.allowed_domains").Array(); len(got) != 1 || got[0].String() != "anthropic.com" {
		t.Fatalf("non-empty allowed_domains should be preserved: %s", string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.max_uses").Int(); got != 8 {
		t.Fatalf("max_uses should be preserved: got %d", got)
	}
}

func TestSanitizeClaudeWebSearchDomains_LeavesNonBuiltinAndNonEmpty(t *testing.T) {
	// Empty arrays on non-web_search tools must be left untouched.
	input := []byte(`{"tools":[{"type":"custom","name":"x","blocked_domains":[]},{"type":"web_search_20250305","name":"web_search","blocked_domains":["evil.com"]}]}`)
	out := sanitizeClaudeWebSearchDomains(input)

	if !gjson.GetBytes(out, "tools.0.blocked_domains").Exists() {
		t.Fatalf("non-web_search tool fields should be untouched: %s", string(out))
	}
	if got := gjson.GetBytes(out, "tools.1.blocked_domains").Array(); len(got) != 1 || got[0].String() != "evil.com" {
		t.Fatalf("non-empty blocked_domains should be preserved: %s", string(out))
	}
}

func TestGeminiToAntigravity_RequestTypeDetectsGoogleSearchAnywhere(t *testing.T) {
	t.Run("googleSearch at index 1 sets web_search", func(t *testing.T) {
		input := []byte(`{"model":"gemini-3-flash","request":{"tools":[{"functionDeclarations":[{"name":"f"}]},{"googleSearch":{}}]}}`)
		out := geminiToAntigravity("gemini-3-flash", input, "")
		if got := gjson.GetBytes(out, "requestType").String(); got != "web_search" {
			t.Fatalf("requestType = %q, want %q", got, "web_search")
		}
	})

	t.Run("no googleSearch keeps agent", func(t *testing.T) {
		input := []byte(`{"model":"gemini-3-flash","request":{"tools":[{"functionDeclarations":[{"name":"f"}]}]}}`)
		out := geminiToAntigravity("gemini-3-flash", input, "")
		if got := gjson.GetBytes(out, "requestType").String(); got != "agent" {
			t.Fatalf("requestType = %q, want %q", got, "agent")
		}
	})
}

func TestClaudeExecutor_ExecuteStripsOpenAIEncryptedThinkingBeforeUpstream(t *testing.T) {
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{
		"messages": [
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"codex reasoning","signature":"gAAAAABopenai-encrypted-content"},
				{"type":"text","text":"Answer"}
			]},
			{"role":"user","content":[{"type":"text","text":"next"}]}
		]
	}`)

	ctx, releaseReport := retainExecutorTransformReport(context.Background(), len(payload))
	_, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(seenBody) == 0 {
		t.Fatal("expected request body to be captured")
	}
	if strings.Contains(string(seenBody), "gAAAAABopenai-encrypted-content") || strings.Contains(string(seenBody), "codex reasoning") {
		t.Fatalf("invalid thinking block was forwarded: %s", string(seenBody))
	}
	content := gjson.GetBytes(seenBody, "messages.0.content").Array()
	if len(content) != 1 {
		t.Fatalf("messages.0.content length = %d, want 1: %s", len(content), string(seenBody))
	}
	if got := content[0].Get("text").String(); got != "Answer" {
		t.Fatalf("remaining content text = %q, want Answer", got)
	}
	assertExecutorRequestTransformReport(t, ctx, releaseReport, claudeFinalSanitizeTransformStage, len(seenBody))
}

func TestClaudeExecutor_ExecuteStripsForeignToolUseSignaturesBeforeUpstream(t *testing.T) {
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{
		"messages": [
			{"role":"assistant","content":[
				{
					"type":"tool_use",
					"id":"toolu_1",
					"name":"lookup",
					"input":{"q":"x"},
					"signature":"skip_thought_signature_validator",
					"thought_signature":"skip_thought_signature_validator",
					"extra_content":{"google":{"thought_signature":"skip_thought_signature_validator"}}
				}
			]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}
		]
	}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(seenBody) == 0 {
		t.Fatal("expected request body to be captured")
	}
	toolUse := gjson.GetBytes(seenBody, "messages.0.content.0")
	if !toolUse.Get("type").Exists() || toolUse.Get("type").String() != "tool_use" {
		t.Fatalf("tool_use block was not preserved: %s", string(seenBody))
	}
	for _, path := range []string{"signature", "thought_signature", "extra_content"} {
		if toolUse.Get(path).Exists() {
			t.Fatalf("foreign tool_use signature field %s was forwarded: %s", path, string(seenBody))
		}
	}
}

func TestShouldSanitizeClaudeMessagesForUpstream_OnlyClaudeFamily(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{model: "claude-sonnet-4-5", want: true},
		{model: "claude-3-5-sonnet-20241022", want: true},
		{model: "kimi-k2.5", want: false},
		{model: "mimo-v2", want: false},
		{model: "gemini-3.5-flash", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			got := shouldSanitizeClaudeMessagesForUpstream(tc.model)
			if got != tc.want {
				t.Errorf("shouldSanitizeClaudeMessagesForUpstream(%q) = %v, want %v", tc.model, got, tc.want)
			}
		})
	}
}

func TestSanitizeClaudeMessagesForClaudeUpstream_BypassesUnknownModelSignatureMatrix(t *testing.T) {
	rawSignature := "skip_thought_signature_validator"
	body := []byte(`{
		"model": "kimi-k2.5",
		"messages": [
			{
				"role": "assistant",
				"content": [
					{"type": "thinking", "thinking": "keep", "signature": "` + rawSignature + `"},
					{"type": "text", "text": "hello"},
					{"type": "tool_use", "id": "call_123", "name": "get_weather", "input": {}, "signature": "` + rawSignature + `"}
				]
			}
		]
	}`)

	output := sanitizeClaudeMessagesForClaudeUpstreamWithDebug(context.Background(), body, "kimi-k2.5")
	parts := gjson.GetBytes(output, "messages.0.content").Array()
	if len(parts) != 3 {
		t.Fatalf("content length = %d, want 3 when sanitizer is bypassed: %s", len(parts), output)
	}
	if got := parts[0].Get("signature").String(); got != rawSignature {
		t.Fatalf("thinking signature = %q, want preserved %q", got, rawSignature)
	}
	if got := parts[2].Get("signature").String(); got != rawSignature {
		t.Fatalf("tool_use signature = %q, want preserved %q", got, rawSignature)
	}
}

func TestClaudeExecutor_ExecuteBypassesSignatureSanitizerForUnknownModel(t *testing.T) {
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"mimo-v2","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{
		"messages": [
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"keep reasoning","signature":""},
				{"type":"text","text":"Answer"}
			]},
			{"role":"user","content":[{"type":"text","text":"next"}]}
		]
	}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "mimo-v2",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(seenBody) == 0 {
		t.Fatal("expected request body to be captured")
	}
	if !strings.Contains(string(seenBody), "keep reasoning") {
		t.Fatalf("unknown-model thinking block should bypass Claude sanitizer: %s", string(seenBody))
	}
}

func TestClaudeExecutor_ExecuteStripsMalformedEPrefixThinkingBeforeUpstream(t *testing.T) {
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	malformedSignature := malformedClaudeTreeSignatureForClaudeExecutorTest()
	payload := []byte(`{
		"messages": [
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"bad reasoning","signature":"` + malformedSignature + `"},
				{"type":"text","text":"Answer"}
			]},
			{"role":"user","content":[{"type":"text","text":"next"}]}
		]
	}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(seenBody) == 0 {
		t.Fatal("expected request body to be captured")
	}
	if strings.Contains(string(seenBody), malformedSignature) || strings.Contains(string(seenBody), "bad reasoning") {
		t.Fatalf("malformed E-prefix thinking block was forwarded: %s", string(seenBody))
	}
	content := gjson.GetBytes(seenBody, "messages.0.content").Array()
	if len(content) != 1 {
		t.Fatalf("messages.0.content length = %d, want 1: %s", len(content), string(seenBody))
	}
	if got := content[0].Get("text").String(); got != "Answer" {
		t.Fatalf("remaining content text = %q, want Answer", got)
	}
}

func TestClaudeExecutor_ExecuteStripsInvalidBase64ThinkingBeforeUpstream(t *testing.T) {
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{
		"messages": [
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"bad reasoning","signature":"E!!!invalid!!!"},
				{"type":"text","text":"Answer"}
			]},
			{"role":"user","content":[{"type":"text","text":"next"}]}
		]
	}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(seenBody) == 0 {
		t.Fatal("expected request body to be captured")
	}
	if strings.Contains(string(seenBody), "E!!!invalid!!!") || strings.Contains(string(seenBody), "bad reasoning") {
		t.Fatalf("invalid-base64 thinking block was forwarded: %s", string(seenBody))
	}
	content := gjson.GetBytes(seenBody, "messages.0.content").Array()
	if len(content) != 1 {
		t.Fatalf("messages.0.content length = %d, want 1: %s", len(content), string(seenBody))
	}
}

func TestClaudeExecutor_ExecuteStripsEmptySignatureEmptyTextThinking(t *testing.T) {
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{
		"messages": [
			{"role":"assistant","content":[
				{"type":"thinking","text":"","signature":""},
				{"type":"text","text":"Answer"}
			]},
			{"role":"user","content":[{"type":"text","text":"next"}]}
		]
	}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(seenBody) == 0 {
		t.Fatal("expected request body to be captured")
	}
	content := gjson.GetBytes(seenBody, "messages.0.content").Array()
	if len(content) != 1 {
		t.Fatalf("messages.0.content length = %d, want 1: %s", len(content), string(seenBody))
	}
	if got := content[0].Get("type").String(); got != "text" {
		t.Fatalf("remaining content type = %q, want text: %s", got, string(seenBody))
	}
	if got := content[0].Get("text").String(); got != "Answer" {
		t.Fatalf("remaining content text = %q, want Answer: %s", got, string(seenBody))
	}
}

func TestClaudeExecutor_ExecuteStreamStripsOpenAIEncryptedThinkingBeforeUpstream(t *testing.T) {
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{
		"messages": [
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"codex reasoning","signature":"gAAAAABopenai-encrypted-content"},
				{"type":"text","text":"Answer"}
			]},
			{"role":"user","content":[{"type":"text","text":"next"}]}
		]
	}`)

	ctx, releaseReport := retainExecutorTransformReport(context.Background(), len(payload))
	result, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected chunk error: %v", chunk.Err)
		}
	}
	assertExecutorRequestTransformReport(t, ctx, releaseReport, claudeFinalSanitizeTransformStage, len(seenBody))
	if len(seenBody) == 0 {
		t.Fatal("expected request body to be captured")
	}
	if strings.Contains(string(seenBody), "gAAAAABopenai-encrypted-content") || strings.Contains(string(seenBody), "codex reasoning") {
		t.Fatalf("invalid thinking block was forwarded: %s", string(seenBody))
	}
}

func TestClaudeExecutor_CountTokensStripsOpenAIEncryptedThinkingBeforeUpstream(t *testing.T) {
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":42}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{
		"messages": [
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"codex reasoning","signature":"gAAAAABopenai-encrypted-content"},
				{"type":"text","text":"Answer"}
			]},
			{"role":"user","content":[{"type":"text","text":"next"}]}
		]
	}`)

	_, err := executor.CountTokens(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("CountTokens() error = %v", err)
	}
	if len(seenBody) == 0 {
		t.Fatal("expected request body to be captured")
	}
	if strings.Contains(string(seenBody), "gAAAAABopenai-encrypted-content") || strings.Contains(string(seenBody), "codex reasoning") {
		t.Fatalf("invalid thinking block was forwarded: %s", string(seenBody))
	}
}

func TestClaudeExecutor_ReusesUserIDAcrossModelsWhenCacheEnabled(t *testing.T) {
	var userIDs []string
	var requestModels []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		userID := gjson.GetBytes(body, "metadata.user_id").String()
		model := gjson.GetBytes(body, "model").String()
		userIDs = append(userIDs, userID)
		requestModels = append(requestModels, model)
		t.Logf("HTTP Server received request: model=%s, user_id=%s, url=%s", model, userID, r.URL.String())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	t.Logf("End-to-end test: Fake HTTP server started at %s", server.URL)

	cacheEnabled := true
	executor := NewClaudeExecutor(&config.Config{
		ClaudeKey: []config.ClaudeKey{
			{
				APIKey:  "key-123",
				BaseURL: server.URL,
				Cloak: &config.CloakConfig{
					CacheUserID: &cacheEnabled,
				},
			},
		},
	})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}

	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	models := []string{"claude-3-5-sonnet", "claude-3-5-haiku"}
	for _, model := range models {
		t.Logf("Sending request for model: %s", model)
		modelPayload, _ := sjson.SetBytes(payload, "model", model)
		if _, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
			Model:   model,
			Payload: modelPayload,
		}, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FromString("claude"),
		}); err != nil {
			t.Fatalf("Execute(%s) error: %v", model, err)
		}
	}

	if len(userIDs) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(userIDs))
	}
	if userIDs[0] == "" || userIDs[1] == "" {
		t.Fatal("expected user_id to be populated")
	}
	t.Logf("user_id[0] (model=%s): %s", requestModels[0], userIDs[0])
	t.Logf("user_id[1] (model=%s): %s", requestModels[1], userIDs[1])
	if userIDs[0] != userIDs[1] {
		t.Fatalf("expected user_id to be reused across models, got %q and %q", userIDs[0], userIDs[1])
	}
	if !helps.IsValidUserID(userIDs[0]) {
		t.Fatalf("user_id %q is not valid", userIDs[0])
	}
	t.Logf("✓ End-to-end test passed: Same user_id (%s) was used for both models", userIDs[0])
}

func TestClaudeExecutor_GeneratesNewUserIDByDefault(t *testing.T) {
	var userIDs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		userIDs = append(userIDs, gjson.GetBytes(body, "metadata.user_id").String())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}

	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	for i := 0; i < 2; i++ {
		if _, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
			Model:   "claude-3-5-sonnet",
			Payload: payload,
		}, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FromString("claude"),
		}); err != nil {
			t.Fatalf("Execute call %d error: %v", i, err)
		}
	}

	if len(userIDs) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(userIDs))
	}
	if userIDs[0] == "" || userIDs[1] == "" {
		t.Fatal("expected user_id to be populated")
	}
	if userIDs[0] == userIDs[1] {
		t.Fatalf("expected user_id to change when caching is not enabled, got identical values %q", userIDs[0])
	}
	if !helps.IsValidUserID(userIDs[0]) || !helps.IsValidUserID(userIDs[1]) {
		t.Fatalf("user_ids should be valid, got %q and %q", userIDs[0], userIDs[1])
	}
}

func TestClaudeExecutorMovesSystemRoleMessagesToTopLevelSystem(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"MiniMax-M2.7-highspeed","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{
		ClaudeKey: []config.ClaudeKey{{
			APIKey:                  "key-123",
			BaseURL:                 server.URL,
			RebuildMidSystemMessage: true,
		}},
	})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{
		"model":"MiniMax-M2.7-highspeed",
		"max_tokens":16,
		"messages":[
			{"role":"system","content":"You are concise."},
			{"role":"user","content":"Reply with OK only."}
		]
	}`)

	if _, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "MiniMax-M2.7-highspeed",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	}); err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	if gjson.GetBytes(gotBody, `messages.#(role=="system")`).Exists() {
		t.Fatalf("system role should not reach Claude-compatible upstream: %s", string(gotBody))
	}
	foundSystemText := false
	for _, block := range gjson.GetBytes(gotBody, "system").Array() {
		if block.Get("text").String() == "You are concise." {
			foundSystemText = true
			break
		}
	}
	if strings.Contains(gjson.GetBytes(gotBody, "messages.0.content").String(), "You are concise.") {
		foundSystemText = true
	}
	if !foundSystemText {
		t.Fatalf("moved system text was not preserved: %s", string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "messages.0.role").String(); got != "user" {
		t.Fatalf("messages.0.role = %q, want user: %s", got, string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "messages.0.content").String(); !strings.Contains(got, "Reply with OK only.") {
		t.Fatalf("messages.0.content = %q, want user content: %s", got, string(gotBody))
	}
}

func TestNormalizeClaudeSystemRoleMessagesAppendsExistingSystem(t *testing.T) {
	payload := []byte(`{
		"system":[{"type":"text","text":"Existing"}],
		"messages":[
			{"role":"system","content":[{"type":"text","text":"Moved","cache_control":{"type":"ephemeral"}}]},
			{"role":"user","content":"hi"}
		]
	}`)

	out := normalizeClaudeSystemRoleMessages(payload)

	if gjson.GetBytes(out, `messages.#(role=="system")`).Exists() {
		t.Fatalf("system role should be removed from messages: %s", string(out))
	}
	if got := gjson.GetBytes(out, "system.0.text").String(); got != "Existing" {
		t.Fatalf("system.0.text = %q, want Existing: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "system.1.text").String(); got != "Moved" {
		t.Fatalf("system.1.text = %q, want Moved: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "system.1.cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("moved cache_control.type = %q, want ephemeral: %s", got, string(out))
	}
}

func TestClaudeExecutor_ExecuteOpenAINonStreamRejectsEmptyClaudeStream(t *testing.T) {
	_, err := executeOpenAIChatCompletionThroughClaude(t, "")
	if err == nil {
		t.Fatal("Execute error = nil, want empty stream error")
	}
	assertStatusErr(t, err, http.StatusBadGateway)
	if !strings.Contains(err.Error(), "empty stream response") {
		t.Fatalf("Execute error = %q, want empty stream response", err.Error())
	}
}

func TestClaudeExecutor_ExecuteOpenAINonStreamRejectsClaudeErrorEvent(t *testing.T) {
	body := `data: {"type":"error","error":{"type":"overloaded_error","message":"upstream overloaded"}}` + "\n"
	_, err := executeOpenAIChatCompletionThroughClaude(t, body)
	if err == nil {
		t.Fatal("Execute error = nil, want upstream error event")
	}
	assertStatusErr(t, err, http.StatusBadGateway)
	if !strings.Contains(err.Error(), "upstream overloaded") {
		t.Fatalf("Execute error = %q, want upstream overloaded", err.Error())
	}
}

func TestClaudeExecutor_ExecuteOpenAINonStreamRejectsIncompleteClaudeStream(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_123","model":"claude-3-5-sonnet-20241022"}}`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	_, err := executeOpenAIChatCompletionThroughClaude(t, body)
	if err == nil {
		t.Fatal("Execute error = nil, want incomplete stream error")
	}
	assertStatusErr(t, err, http.StatusBadGateway)
	if !strings.Contains(err.Error(), "ended before message completion") {
		t.Fatalf("Execute error = %q, want incomplete stream error", err.Error())
	}
}

func TestClaudeExecutor_ExecuteOpenAINonStreamConvertsValidClaudeStream(t *testing.T) {
	body := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_123","model":"claude-3-5-sonnet-20241022"}}`,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":2,"output_tokens":1}}`,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	resp, err := executeOpenAIChatCompletionThroughClaude(t, body)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "id").String(); got != "msg_123" {
		t.Fatalf("response id = %q, want msg_123; payload=%s", got, string(resp.Payload))
	}
	if got := gjson.GetBytes(resp.Payload, "model").String(); got != "claude-3-5-sonnet-20241022" {
		t.Fatalf("response model = %q, want claude-3-5-sonnet-20241022", got)
	}
	if got := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); got != "ok" {
		t.Fatalf("response content = %q, want ok", got)
	}
	if got := gjson.GetBytes(resp.Payload, "usage.total_tokens").Int(); got != 3 {
		t.Fatalf("usage.total_tokens = %d, want 3", got)
	}
}

func executeOpenAIChatCompletionThroughClaude(t *testing.T, upstreamBody string) (cliproxyexecutor.Response, error) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"model":"claude-3-5-sonnet-20241022","messages":[{"role":"user","content":"hi"}]}`)

	return executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
	})
}

func assertStatusErr(t *testing.T, err error, want int) {
	t.Helper()

	status, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("error %T does not expose StatusCode", err)
	}
	if got := status.StatusCode(); got != want {
		t.Fatalf("StatusCode() = %d, want %d", got, want)
	}
}

func TestSanitizeClaudeToolNamesForUpstream_RewritesAndRestores(t *testing.T) {
	input := []byte(`{
		"tools":[
			{"name":"skill:pet_animals","input_schema":{"type":"object"}},
			{"type":"web_search_20250305","name":"web_search"}
		],
		"tool_choice":{"type":"tool","name":"skill:pet_animals"},
		"messages":[{"role":"assistant","content":[
			{"type":"tool_use","name":"skill:pet_animals","id":"toolu_1","input":{}},
			{"type":"tool_reference","tool_name":"skill:pet_animals"},
			{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"tool_reference","tool_name":"skill:pet_animals"}]}
		]}]
	}`)

	out, mapping := sanitizeClaudeToolNamesForUpstream(input)
	if mapping == nil {
		t.Fatal("expected invalid tool name to be sanitized")
	}
	for _, path := range []string{
		"tools.0.name",
		"tool_choice.name",
		"messages.0.content.0.name",
		"messages.0.content.1.tool_name",
		"messages.0.content.2.content.0.tool_name",
	} {
		if got := gjson.GetBytes(out, path).String(); got != "skill_pet_animals" {
			t.Fatalf("%s = %q, want %q", path, got, "skill_pet_animals")
		}
	}
	if got := gjson.GetBytes(out, "tools.1.name").String(); got != "web_search" {
		t.Fatalf("built-in tool name = %q, want web_search", got)
	}

	response := restoreClaudeToolNamesFromResponse([]byte(`{"content":[
		{"type":"tool_use","name":"skill_pet_animals","id":"toolu_2","input":{}},
		{"type":"tool_reference","tool_name":"skill_pet_animals"},
		{"type":"tool_result","tool_use_id":"toolu_2","content":[{"type":"tool_reference","tool_name":"skill_pet_animals"}]}
	]}`), mapping)
	for _, path := range []string{
		"content.0.name",
		"content.1.tool_name",
		"content.2.content.0.tool_name",
	} {
		if got := gjson.GetBytes(response, path).String(); got != "skill:pet_animals" {
			t.Fatalf("%s = %q, want %q", path, got, "skill:pet_animals")
		}
	}

	line := []byte(`data: {"type":"content_block_start","content_block":{"type":"tool_use","name":"skill_pet_animals","id":"toolu_3"},"index":0}`)
	restoredLine := restoreClaudeToolNamesFromStreamLine(line, mapping)
	payload := bytes.TrimSpace(restoredLine)
	if bytes.HasPrefix(payload, []byte("data:")) {
		payload = bytes.TrimSpace(payload[len("data:"):])
	}
	if got := gjson.GetBytes(payload, "content_block.name").String(); got != "skill:pet_animals" {
		t.Fatalf("stream content_block.name = %q, want %q", got, "skill:pet_animals")
	}
}

func TestDowngradeClaudeToolSearchForCompat(t *testing.T) {
	payload := []byte(`{
		"tools":[
			{"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"},
			{"name":"mcp__files__read","description":"Read files","defer_loading":true,"input_schema":{"type":"object"}}
		],
		"messages":[
			{"role":"assistant","content":[
				{"type":"server_tool_use","id":"srvtoolu_1","name":"tool_search_tool_regex","input":{"query":"read"}},
				{"type":"tool_search_tool_result","tool_use_id":"srvtoolu_1","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":"mcp__files__read"}]}},
				{"type":"tool_reference","tool_name":"mcp__files__read"},
				{"type":"tool_use","id":"toolu_1","name":"mcp__files__read","input":{"path":"README.md"}}
			]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"tool_reference","tool_name":"mcp__files__read"},{"type":"text","text":"ok"}]}]}
		]
	}`)

	out := downgradeClaudeToolSearchForCompat("https://api.kimi.com/coding", payload)

	if got := len(gjson.GetBytes(out, "tools").Array()); got != 1 {
		t.Fatalf("tools length = %d, want 1: %s", got, string(out))
	}
	if gjson.GetBytes(out, "tools.0.defer_loading").Exists() {
		t.Fatalf("defer_loading should be removed: %s", string(out))
	}
	for _, partType := range []string{"server_tool_use", "tool_search_tool_result", "tool_reference"} {
		if gjson.GetBytes(out, `messages.0.content.#(type=="`+partType+`")`).Exists() {
			t.Fatalf("%s should be downgraded away: %s", partType, string(out))
		}
	}
	if got := gjson.GetBytes(out, `messages.0.content.#(type=="tool_use").name`).String(); got != "mcp__files__read" {
		t.Fatalf("tool_use name = %q, want mcp__files__read: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "messages.1.content.0.content.0.type").String(); got != "text" {
		t.Fatalf("nested tool_reference should become text, got %q: %s", got, string(out))
	}
}

func TestDowngradeClaudeUnsupportedServerToolsForMiniMax(t *testing.T) {
	payload := []byte(`{
		"model":"MiniMax-M2.7",
		"tools":[
			{"type":"web_search_20250305","name":"web_search","max_uses":8},
			{"name":"read_file","description":"Read files","input_schema":{"type":"object"}}
		],
		"tool_choice":{"type":"tool","name":"web_search"},
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"search"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},
				{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}},
				{"type":"video","source":{"type":"url","url":"https://example.com/demo.mp4"}},
				{"type":"mcp_tool_result","content":[{"type":"text","text":"mcp ok"}]}
			]},
			{"role":"assistant","content":[
				{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"current date"}},
				{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[]}
			]}
		]
	}`)

	out := downgradeClaudeToolSearchForCompat("https://api.minimax.io/anthropic", payload)

	if got := len(gjson.GetBytes(out, "tools").Array()); got != 1 {
		t.Fatalf("tools length = %d, want 1: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "read_file" {
		t.Fatalf("remaining tool = %q, want read_file: %s", got, string(out))
	}
	if gjson.GetBytes(out, "tool_choice").Exists() {
		t.Fatalf("tool_choice for removed server tool should be removed: %s", string(out))
	}
	userContent := gjson.GetBytes(out, "messages.0.content").Array()
	if hasClaudePartType(userContent, "image") || hasClaudePartType(userContent, "image_url") ||
		hasClaudePartType(userContent, "video") || hasClaudePartType(userContent, "mcp_tool_result") {
		t.Fatalf("MiniMax unsupported content block remained: %s", string(out))
	}
	if !hasClaudeText(userContent, "search") || !hasClaudeText(userContent, "mcp ok") {
		t.Fatalf("MiniMax compatible text should be preserved: %s", string(out))
	}
	for _, partType := range []string{"server_tool_use", "web_search_tool_result"} {
		if gjson.GetBytes(out, `messages.1.content.#(type=="`+partType+`")`).Exists() {
			t.Fatalf("%s should be downgraded away: %s", partType, string(out))
		}
	}
	if err := validateClaudeUpstreamPayload("https://api.minimax.io/anthropic", out); err != nil {
		t.Fatalf("downgraded MiniMax payload should pass validation: %v", err)
	}
}

func TestDowngradeClaudeUnsupportedBlocksForMiniMaxM3KeepsImageAndVideo(t *testing.T) {
	t.Parallel()

	payload := []byte(`{
		"model":"MiniMax-M3-highspeed",
		"tools":[
			{"type":"web_search_20250305","name":"web_search","max_uses":8},
			{"name":"read_file","description":"Read files","input_schema":{"type":"object"}}
		],
		"tool_choice":{"type":"tool","name":"web_search"},
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"inspect"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},
				{"type":"video","source":{"type":"url","url":"https://example.com/demo.mp4"}},
				{"type":"image_url","image_url":{"url":"data:image/png;base64,BBBB"}},
				{"type":"video_url","video_url":{"url":"https://example.com/raw.mp4"}},
				{"type":"document","source":{"type":"base64","media_type":"text/plain","data":"QkJCQg=="}},
				{"type":"mcp_tool_result","content":[{"type":"text","text":"mcp ok"}]}
			]},
			{"role":"assistant","content":[
				{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"current date"}},
				{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[]},
				{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"README.md"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","content":[
					{"type":"text","text":"file ok"},
					{"type":"image","source":{"type":"base64","media_type":"image/png","data":"CCCC"}},
					{"type":"video","source":{"type":"url","url":"https://example.com/tool.mp4"}},
					{"type":"document","source":{"type":"base64","media_type":"text/plain","data":"RERERA=="}}
				]}
			]}
		]
	}`)

	out := downgradeClaudeToolSearchForCompat("https://api.minimax.io/anthropic", payload)

	userContent := gjson.GetBytes(out, "messages.0.content").Array()
	if !hasClaudePartType(userContent, "image") || !hasClaudePartType(userContent, "video") {
		t.Fatalf("MiniMax-M3 image/video blocks should be preserved: %s", string(out))
	}
	for _, partType := range []string{"image_url", "video_url", "document", "mcp_tool_result"} {
		if hasClaudePartType(userContent, partType) {
			t.Fatalf("MiniMax-M3 unsupported %s block remained: %s", partType, string(out))
		}
	}
	if !hasClaudeText(userContent, "inspect") || !hasClaudeText(userContent, "mcp ok") {
		t.Fatalf("MiniMax-M3 compatible text should be preserved: %s", string(out))
	}

	assistantContent := gjson.GetBytes(out, "messages.1.content").Array()
	for _, partType := range []string{"server_tool_use", "web_search_tool_result"} {
		if hasClaudePartType(assistantContent, partType) {
			t.Fatalf("MiniMax-M3 server tool block should be downgraded: %s", string(out))
		}
	}
	if !hasClaudePartType(assistantContent, "tool_use") {
		t.Fatalf("MiniMax-M3 custom tool_use should be preserved: %s", string(out))
	}

	toolResultContent := gjson.GetBytes(out, "messages.2.content.0.content").Array()
	if !hasClaudePartType(toolResultContent, "image") || !hasClaudePartType(toolResultContent, "video") {
		t.Fatalf("MiniMax-M3 nested image/video blocks should be preserved: %s", string(out))
	}
	if hasClaudePartType(toolResultContent, "document") || !hasClaudeText(toolResultContent, "file ok") {
		t.Fatalf("MiniMax-M3 nested tool_result content not downgraded correctly: %s", string(out))
	}
	if err := validateClaudeUpstreamPayload("https://api.minimax.io/anthropic", out); err != nil {
		t.Fatalf("downgraded MiniMax-M3 payload should pass validation: %v", err)
	}
}

func TestDowngradeClaudeUnsupportedBlocksForXiaomiMimoV25KeepsImages(t *testing.T) {
	t.Parallel()

	payload := []byte(`{
		"model":"mimo-v2.5",
		"tools":[
			{"type":"web_search_20250305","name":"web_search","max_uses":8},
			{"name":"read_file","description":"Read files","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}
		],
		"tool_choice":{"type":"tool","name":"web_search"},
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"search"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},
				{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}},
				{"type":"mcp_tool_result","content":[{"type":"text","text":"mcp ok"}]}
			]},
			{"role":"assistant","content":[
				{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"current date"}},
				{"type":"code_execution_tool_result","tool_use_id":"srvtoolu_1","content":[{"type":"text","text":"code ok"}]},
				{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"README.md"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","content":[
					{"type":"text","text":"file ok"},
					{"type":"image","source":{"type":"base64","media_type":"image/png","data":"BBBB"}}
				]}
			]}
		]
	}`)

	out := downgradeClaudeToolSearchForCompat("https://token-plan-cn.xiaomimimo.com/anthropic", payload)

	if got := len(gjson.GetBytes(out, "tools").Array()); got != 1 {
		t.Fatalf("tools length = %d, want 1: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "read_file" {
		t.Fatalf("remaining tool = %q, want read_file: %s", got, string(out))
	}
	if gjson.GetBytes(out, "tool_choice").Exists() {
		t.Fatalf("tool_choice for removed server tool should be removed: %s", string(out))
	}

	userContent := gjson.GetBytes(out, "messages.0.content").Array()
	if !hasClaudePartType(userContent, "image") {
		t.Fatalf("mimo-v2.5 image block should be preserved: %s", string(out))
	}
	if hasClaudePartType(userContent, "image_url") || hasClaudePartType(userContent, "mcp_tool_result") {
		t.Fatalf("Xiaomi unsupported content block remained: %s", string(out))
	}
	if !hasClaudeText(userContent, "search") || !hasClaudeText(userContent, "mcp ok") {
		t.Fatalf("Xiaomi compatible text should be preserved: %s", string(out))
	}

	assistantContent := gjson.GetBytes(out, "messages.1.content").Array()
	for _, partType := range []string{"server_tool_use", "code_execution_tool_result"} {
		if hasClaudePartType(assistantContent, partType) {
			t.Fatalf("%s should be downgraded away: %s", partType, string(out))
		}
	}
	if !hasClaudePartType(assistantContent, "tool_use") {
		t.Fatalf("custom tool_use should be preserved: %s", string(out))
	}

	toolResultContent := gjson.GetBytes(out, "messages.2.content.0.content").Array()
	if !hasClaudePartType(toolResultContent, "image") {
		t.Fatalf("mimo-v2.5 image inside tool_result should be preserved: %s", string(out))
	}
	if !hasClaudeText(toolResultContent, "file ok") {
		t.Fatalf("tool_result text should be preserved: %s", string(out))
	}
	if err := validateClaudeUpstreamPayload("https://token-plan-cn.xiaomimimo.com/anthropic", out); err != nil {
		t.Fatalf("downgraded Xiaomi payload should pass validation: %v", err)
	}
}

func TestXiaomiClaudeImagesAreEnabledOnlyForMimoV25(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		compatKind string
		model      string
		partType   string
		want       bool
	}{
		{name: "mimo v2.5 image", compatKind: "xiaomi", model: "mimo-v2.5", partType: "image", want: true},
		{name: "mimo v2.5 image url", compatKind: "xiaomi", model: "mimo-v2.5", partType: "image_url", want: false},
		{name: "mimo v2.5 pro image", compatKind: "xiaomi", model: "mimo-v2.5-pro", partType: "image", want: false},
		{name: "other xiaomi model image", compatKind: "xiaomi", model: "mimo-v2.4", partType: "image", want: false},
		{name: "other compat image", compatKind: "deepseek", model: "mimo-v2.5", partType: "image", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := supportsXiaomiMimoV25ClaudeMultimodalPart(tt.compatKind, tt.model, tt.partType); got != tt.want {
				t.Fatalf("supportsXiaomiMimoV25ClaudeMultimodalPart(%q, %q, %q) = %v, want %v", tt.compatKind, tt.model, tt.partType, got, tt.want)
			}
		})
	}
}

func TestDowngradeClaudeUnsupportedBlocksForDoubaoKeepsImages(t *testing.T) {
	t.Parallel()

	payload := []byte(`{
		"tools":[
			{"type":"web_search_20250305","name":"web_search","max_uses":8},
			{"name":"read_file","description":"Read files","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}
		],
		"tool_choice":{"type":"tool","name":"web_search"},
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"inspect"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},
				{"type":"image_url","image_url":{"url":"data:image/png;base64,BBBB"}},
				{"type":"document","source":{"type":"base64","media_type":"text/plain","data":"aGVsbG8="}},
				{"type":"mcp_tool_result","content":[{"type":"text","text":"mcp ok"}]}
			]},
			{"role":"assistant","content":[
				{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"current date"}},
				{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[]},
				{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"README.md"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","content":[
					{"type":"text","text":"file ok"},
					{"type":"document","source":{"type":"base64","media_type":"text/plain","data":"ZG9j"}}
				]}
			]}
		]
	}`)

	out := downgradeClaudeToolSearchForCompat("https://ark.cn-beijing.volces.com/api/coding", payload)

	if got := len(gjson.GetBytes(out, "tools").Array()); got != 1 {
		t.Fatalf("tools length = %d, want 1: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "read_file" {
		t.Fatalf("remaining tool = %q, want read_file: %s", got, string(out))
	}
	if gjson.GetBytes(out, "tool_choice").Exists() {
		t.Fatalf("tool_choice for removed server tool should be removed: %s", string(out))
	}

	userContent := gjson.GetBytes(out, "messages.0.content").Array()
	if !hasClaudePartType(userContent, "image") {
		t.Fatalf("Doubao image block should be preserved: %s", string(out))
	}
	for _, partType := range []string{"image_url", "document", "mcp_tool_result"} {
		if hasClaudePartType(userContent, partType) {
			t.Fatalf("Doubao unsupported %s block remained: %s", partType, string(out))
		}
	}
	if !hasClaudeText(userContent, "inspect") || !hasClaudeText(userContent, "mcp ok") {
		t.Fatalf("Doubao compatible text should be preserved: %s", string(out))
	}

	assistantContent := gjson.GetBytes(out, "messages.1.content").Array()
	for _, partType := range []string{"server_tool_use", "web_search_tool_result"} {
		if hasClaudePartType(assistantContent, partType) {
			t.Fatalf("Doubao unsupported %s block remained: %s", partType, string(out))
		}
	}
	toolResultContent := gjson.GetBytes(out, "messages.2.content.0.content").Array()
	if hasClaudePartType(toolResultContent, "document") || !hasClaudeText(toolResultContent, "file ok") {
		t.Fatalf("Doubao nested tool_result content not downgraded correctly: %s", string(out))
	}
	if err := validateClaudeUpstreamPayload("https://ark.cn-beijing.volces.com/api/coding", out); err != nil {
		t.Fatalf("downgraded Doubao payload should pass validation: %v", err)
	}
}

func TestApplyMiniMaxStreamingThinkingDefaultForCompat(t *testing.T) {
	t.Parallel()

	payload := []byte(`{"model":"MiniMax-M2.7","messages":[{"role":"user","content":"hi"}]}`)
	out := applyMiniMaxStreamingThinkingDefaultForCompat("minimax", payload, true)
	if got := gjson.GetBytes(out, "thinking.type").String(); got != "disabled" {
		t.Fatalf("thinking.type = %q, want disabled: %s", got, string(out))
	}

	explicit := []byte(`{"model":"MiniMax-M2.7","thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hi"}]}`)
	out = applyMiniMaxStreamingThinkingDefaultForCompat("minimax", explicit, true)
	if got := gjson.GetBytes(out, "thinking.type").String(); got != "enabled" {
		t.Fatalf("explicit thinking.type = %q, want enabled: %s", got, string(out))
	}

	forcedToolChoice := []byte(`{"tool_choice":{"type":"tool","name":"read"},"messages":[{"role":"user","content":"hi"}]}`)
	out = applyMiniMaxStreamingThinkingDefaultForCompat("minimax", forcedToolChoice, true)
	if gjson.GetBytes(out, "thinking").Exists() {
		t.Fatalf("forced tool_choice should not receive implicit thinking: %s", string(out))
	}

	nonStream := applyMiniMaxStreamingThinkingDefaultForCompat("minimax", payload, false)
	if gjson.GetBytes(nonStream, "thinking").Exists() {
		t.Fatalf("non-stream MiniMax request should not be changed: %s", string(nonStream))
	}
}

func TestDowngradeClaudeToolSearchForCompatRepairsInvalidStringEscapes(t *testing.T) {
	t.Parallel()

	payload := []byte(`{
		"messages":[{"role":"user","content":[{"type":"text","text":"- **归档**：\archive/20260516 and *破甲**\ufeff\v**"}]}]
	}`)

	out := downgradeClaudeToolSearchForCompat("https://api.minimax.io/anthropic", payload)

	if !gjson.ValidBytes(out) {
		t.Fatalf("repaired Claude compat payload should be valid JSON: %s", string(out))
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.text").String(); !strings.Contains(got, `\archive/20260516`) || !strings.Contains(got, `\v`) {
		t.Fatalf("literal backslash text not preserved, got %q payload=%s", got, string(out))
	}
}

func TestDowngradeClaudeUnsupportedBlocksForStep(t *testing.T) {
	t.Parallel()

	payload := []byte(`{
		"tools":[
			{"type":"web_search_20250305","name":"web_search","max_uses":8},
			{"name":"read_file","description":"Read files","input_schema":{"type":"object"}}
		],
		"tool_choice":{"type":"tool","name":"web_search"},
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"search"},
				{"type":"mcp_tool_result","content":[{"type":"text","text":"mcp ok"}]}
			]},
			{"role":"assistant","content":[
				{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"current date"}},
				{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[]}
			]}
		]
	}`)

	out := downgradeClaudeToolSearchForCompat("https://api.stepfun.com/step_plan", payload)

	if got := len(gjson.GetBytes(out, "tools").Array()); got != 1 {
		t.Fatalf("tools length = %d, want 1: %s", got, string(out))
	}
	if gjson.GetBytes(out, "tool_choice").Exists() {
		t.Fatalf("tool_choice for removed server tool should be removed: %s", string(out))
	}
	userContent := gjson.GetBytes(out, "messages.0.content").Array()
	if hasClaudePartType(userContent, "mcp_tool_result") {
		t.Fatalf("Step unsupported content block remained: %s", string(out))
	}
	if !hasClaudeText(userContent, "search") || !hasClaudeText(userContent, "mcp ok") {
		t.Fatalf("Step compatible text should be preserved: %s", string(out))
	}
}

func TestSanitizeClaudeHTTPRequestToolNames_DisablesImplicitMiniMaxStreamingThinking(t *testing.T) {
	t.Parallel()

	payload := `{"stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "https://api.minimax.io/anthropic/v1/messages?beta=true", strings.NewReader(payload))

	if _, err := sanitizeClaudeHTTPRequestToolNames(req); err != nil {
		t.Fatalf("sanitizeClaudeHTTPRequestToolNames() error = %v", err)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if got := gjson.GetBytes(body, "thinking.type").String(); got != "disabled" {
		t.Fatalf("thinking.type = %q, want disabled: %s", got, string(body))
	}
}

func TestSanitizeClaudeHTTPRequestToolNames_DowngradesXiaomiAnthropicBody(t *testing.T) {
	t.Parallel()

	payload := `{"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}},{"type":"mcp_tool_result","content":[{"type":"text","text":"mcp ok"}]}]}]}`
	req := httptest.NewRequest(http.MethodPost, "https://api.xiaomimimo.com/anthropic/v1/messages?beta=true", strings.NewReader(payload))

	if _, err := sanitizeClaudeHTTPRequestToolNames(req); err != nil {
		t.Fatalf("sanitizeClaudeHTTPRequestToolNames() error = %v", err)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	content := gjson.GetBytes(body, "messages.0.content").Array()
	if hasClaudePartType(content, "image_url") || hasClaudePartType(content, "mcp_tool_result") {
		t.Fatalf("Xiaomi direct HttpRequest body should remove unsupported blocks: %s", string(body))
	}
	if !hasClaudeText(content, "hi") || !hasClaudeText(content, "mcp ok") {
		t.Fatalf("text should be preserved: %s", string(body))
	}
}

func TestDowngradeClaudeToolSearchForCompatSkipsOfficialAnthropic(t *testing.T) {
	payload := []byte(`{"tools":[{"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"}]}`)
	out := downgradeClaudeToolSearchForCompat("https://api.anthropic.com", payload)
	if !bytes.Equal(out, payload) {
		t.Fatalf("official Anthropic payload should not be changed: %s", string(out))
	}
}

func TestFilterClaudeBetasForCompatDropsToolSearch(t *testing.T) {
	out := filterClaudeBetasForCompat("claude-code-20250219, tool-search-2025-11-19,tool_search_tool_regex_20251119,oauth-2025-04-20,provider-beta-2099-01-01")
	for _, dropped := range []string{"claude-code", "tool-search", "tool_search", "oauth"} {
		if strings.Contains(out, dropped) {
			t.Fatalf("%s beta should be removed for compat endpoints, got %q", dropped, out)
		}
	}
	if !strings.Contains(out, "provider-beta-2099-01-01") {
		t.Fatalf("unknown provider beta should be preserved, got %q", out)
	}
}

func TestSanitizeClaudeToolNamesForUpstream_AvoidsCollisions(t *testing.T) {
	input := []byte(`{"tools":[{"name":"skill_pet_animals"},{"name":"skill:pet_animals"}]}`)

	out, mapping := sanitizeClaudeToolNamesForUpstream(input)
	if mapping == nil {
		t.Fatal("expected invalid tool name to be sanitized")
	}
	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "skill_pet_animals" {
		t.Fatalf("valid tool name changed to %q", got)
	}
	sanitized := gjson.GetBytes(out, "tools.1.name").String()
	if sanitized == "skill_pet_animals" {
		t.Fatal("sanitized invalid tool name collided with an existing valid tool")
	}
	if !strings.HasPrefix(sanitized, "skill_pet_animals_") {
		t.Fatalf("sanitized collision name = %q, want skill_pet_animals_<hash>", sanitized)
	}
	if !isValidClaudeToolName(sanitized) {
		t.Fatalf("sanitized collision name %q is not Anthropic-compatible", sanitized)
	}

	response := restoreClaudeToolNamesFromResponse([]byte(fmt.Sprintf(`{"content":[{"type":"tool_use","name":%q,"id":"toolu_1","input":{}}]}`, sanitized)), mapping)
	if got := gjson.GetBytes(response, "content.0.name").String(); got != "skill:pet_animals" {
		t.Fatalf("restored tool name = %q, want %q", got, "skill:pet_animals")
	}
}

func TestClaudeExecutor_Execute_SanitizesInvalidToolNamesForAPIKeyUpstream(t *testing.T) {
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		name := gjson.GetBytes(body, "tools.0.name").String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"msg_1","type":"message","model":"claude-3-5-sonnet-20241022","role":"assistant","content":[{"type":"tool_use","name":%q,"id":"toolu_1","input":{}}],"usage":{"input_tokens":1,"output_tokens":1}}`, name)
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{
		"tools":[{"name":"skill:pet_animals","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"tool","name":"skill:pet_animals"},
		"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]
	}`)

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if got := gjson.GetBytes(seenBody, "tools.0.name").String(); got != "skill_pet_animals" {
		t.Fatalf("upstream tools.0.name = %q, want %q", got, "skill_pet_animals")
	}
	if got := gjson.GetBytes(seenBody, "tool_choice.name").String(); got != "skill_pet_animals" {
		t.Fatalf("upstream tool_choice.name = %q, want %q", got, "skill_pet_animals")
	}
	if got := gjson.GetBytes(resp.Payload, "content.0.name").String(); got != "skill:pet_animals" {
		t.Fatalf("downstream content.0.name = %q, want original name", got)
	}
}

func TestClaudeExecutor_Execute_DropsUnansweredToolUseHistory(t *testing.T) {
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet-20241022","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{
		"messages":[
			{"role":"user","content":[{"type":"text","text":"start"}]},
			{"role":"assistant","content":[
				{"type":"text","text":"will use tools"},
				{"type":"tool_use","id":"call_01_vp6YvKjZbis7ayYMkHUTn76a","name":"read_file","input":{}},
				{"type":"tool_use","id":"call_02_HV14reYsv1LuKOdMKLX3dKMM","name":"glob","input":{}}
			]},
			{"role":"user","content":[{"type":"text","text":"continue without tool results"}]}
		]
	}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if len(seenBody) == 0 {
		t.Fatal("expected request body to be captured")
	}
	if strings.Contains(string(seenBody), "call_01_vp6YvKjZbis7ayYMkHUTn76a") {
		t.Fatalf("upstream body still has unanswered call_01 tool_use: %s", seenBody)
	}
	if strings.Contains(string(seenBody), "call_02_HV14reYsv1LuKOdMKLX3dKMM") {
		t.Fatalf("upstream body still has unanswered call_02 tool_use: %s", seenBody)
	}
	if !strings.Contains(string(seenBody), "will use tools") {
		t.Fatalf("upstream body should keep original assistant text: %s", seenBody)
	}
	if !strings.Contains(string(seenBody), "continue without tool results") {
		t.Fatalf("upstream body should keep original user text: %s", seenBody)
	}
}

func TestClaudeExecutor_HttpRequest_SanitizesDirectMessagesToolNames(t *testing.T) {
	var seenBody []byte
	var encodedResponse bytes.Buffer
	gzipWriter := gzip.NewWriter(&encodedResponse)
	_, _ = gzipWriter.Write([]byte(`data: {"type":"content_block_start","content_block":{"type":"tool_use","name":"skill_pet_animals","id":"toolu_1"},"index":0}` + "\n\n"))
	_ = gzipWriter.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(encodedResponse.Len()))
		w.Header().Set("ETag", `"encoded"`)
		w.Header().Set("Digest", "sha-256=encoded")
		_, _ = w.Write(encodedResponse.Bytes())
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key": "key-123",
	}}
	payload := []byte(`{
		"tools":[{"name":"skill:pet_animals","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"tool","name":"skill:pet_animals"},
		"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],
		"stream":true
	}`)
	req, errReq := http.NewRequest(http.MethodPost, server.URL+"/v1/messages?beta=true", bytes.NewReader(payload))
	if errReq != nil {
		t.Fatalf("new request: %v", errReq)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := executor.HttpRequest(context.Background(), auth, req)
	if err != nil {
		t.Fatalf("HttpRequest error: %v", err)
	}
	for _, name := range []string{"Content-Encoding", "Content-Length", "ETag", "Digest"} {
		if got := resp.Header.Get(name); got != "" {
			t.Fatalf("response %s = %q, want empty after decoding", name, got)
		}
	}
	if resp.ContentLength != -1 {
		t.Fatalf("response ContentLength = %d, want -1", resp.ContentLength)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			t.Fatalf("response body close error: %v", errClose)
		}
	}()
	data, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		t.Fatalf("read response body: %v", errRead)
	}

	if got := gjson.GetBytes(seenBody, "tools.0.name").String(); got != "skill_pet_animals" {
		t.Fatalf("upstream tools.0.name = %q, want %q", got, "skill_pet_animals")
	}
	if got := gjson.GetBytes(seenBody, "tool_choice.name").String(); got != "skill_pet_animals" {
		t.Fatalf("upstream tool_choice.name = %q, want %q", got, "skill_pet_animals")
	}
	if !bytes.Contains(data, []byte(`"name":"skill:pet_animals"`)) {
		t.Fatalf("downstream stream did not restore tool name: %s", string(data))
	}
}

func TestClaudeExecutor_HttpRequest_BoundsAndRestoresCompressedNonStreamResponse(t *testing.T) {
	responseBody := []byte(`{"content":[{"type":"tool_use","name":"skill_pet_animals","id":"toolu_1","input":{}}]}`)
	var encodedResponse bytes.Buffer
	gzipWriter := gzip.NewWriter(&encodedResponse)
	_, _ = gzipWriter.Write(responseBody)
	_ = gzipWriter.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("ETag", `"encoded"`)
		w.Header().Set("Content-MD5", "encoded")
		_, _ = w.Write(encodedResponse.Bytes())
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	payload := []byte(`{
		"tools":[{"name":"skill:pet_animals","input_schema":{"type":"object"}}],
		"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]
	}`)
	req, errReq := http.NewRequest(http.MethodPost, server.URL+"/v1/messages?beta=true", bytes.NewReader(payload))
	if errReq != nil {
		t.Fatalf("new request: %v", errReq)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := executor.HttpRequest(context.Background(), &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "key-123"}}, req)
	if err != nil {
		t.Fatalf("HttpRequest error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		t.Fatalf("read response body: %v", errRead)
	}
	if !bytes.Contains(data, []byte(`"name":"skill:pet_animals"`)) {
		t.Fatalf("downstream response did not restore tool name: %s", data)
	}
	if resp.ContentLength != int64(len(data)) || resp.Header.Get("Content-Length") != strconv.Itoa(len(data)) {
		t.Fatalf("decoded content length = %d/%q, want %d", resp.ContentLength, resp.Header.Get("Content-Length"), len(data))
	}
	for _, name := range []string{"Content-Encoding", "ETag", "Content-MD5"} {
		if got := resp.Header.Get(name); got != "" {
			t.Fatalf("response %s = %q, want empty after decoding", name, got)
		}
	}
}

func TestNormalizeCacheControlTTL_DowngradesLaterOneHourBlocks(t *testing.T) {
	payload := []byte(`{
		"tools": [{"name":"t1","cache_control":{"type":"ephemeral","ttl":"1h"}}],
		"system": [{"type":"text","text":"s1","cache_control":{"type":"ephemeral"}}],
		"messages": [{"role":"user","content":[{"type":"text","text":"u1","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]
	}`)

	out := normalizeCacheControlTTL(payload)

	if got := gjson.GetBytes(out, "tools.0.cache_control.ttl").String(); got != "1h" {
		t.Fatalf("tools.0.cache_control.ttl = %q, want %q", got, "1h")
	}
	if gjson.GetBytes(out, "messages.0.content.0.cache_control.ttl").Exists() {
		t.Fatalf("messages.0.content.0.cache_control.ttl should be removed after a default-5m block")
	}
}

func TestNormalizeCacheControlTTL_PreservesOriginalBytesWhenNoChange(t *testing.T) {
	// Payload where no TTL normalization is needed (all blocks use 1h with no
	// preceding 5m block). The text intentionally contains HTML chars (<, >, &)
	// that json.Marshal would escape to \u003c etc., altering byte identity.
	payload := []byte(`{"tools":[{"name":"t1","cache_control":{"type":"ephemeral","ttl":"1h"}}],"system":[{"type":"text","text":"<system-reminder>foo & bar</system-reminder>","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)

	out := normalizeCacheControlTTL(payload)

	if !bytes.Equal(out, payload) {
		t.Fatalf("normalizeCacheControlTTL altered bytes when no change was needed.\noriginal: %s\ngot:      %s", payload, out)
	}
}

func TestNormalizeCacheControlTTL_PreservesKeyOrderWhenModified(t *testing.T) {
	payload := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"u1","cache_control":{"type":"ephemeral","ttl":"1h"}}]}],"tools":[{"name":"t1","cache_control":{"type":"ephemeral"}}],"system":[{"type":"text","text":"s1","cache_control":{"type":"ephemeral"}}]}`)

	out := normalizeCacheControlTTL(payload)

	if gjson.GetBytes(out, "messages.0.content.0.cache_control.ttl").Exists() {
		t.Fatalf("messages.0.content.0.cache_control.ttl should be removed after a default-5m block")
	}

	outStr := string(out)
	idxModel := strings.Index(outStr, `"model"`)
	idxMessages := strings.Index(outStr, `"messages"`)
	idxTools := strings.Index(outStr, `"tools"`)
	idxSystem := strings.Index(outStr, `"system"`)
	if idxModel == -1 || idxMessages == -1 || idxTools == -1 || idxSystem == -1 {
		t.Fatalf("failed to locate top-level keys in output: %s", outStr)
	}
	if !(idxModel < idxMessages && idxMessages < idxTools && idxTools < idxSystem) {
		t.Fatalf("top-level key order changed:\noriginal: %s\ngot:      %s", payload, out)
	}
}

func TestEnforceCacheControlLimit_StripsNonLastToolBeforeMessages(t *testing.T) {
	payload := []byte(`{
		"tools": [
			{"name":"t1","cache_control":{"type":"ephemeral"}},
			{"name":"t2","cache_control":{"type":"ephemeral"}}
		],
		"system": [{"type":"text","text":"s1","cache_control":{"type":"ephemeral"}}],
		"messages": [
			{"role":"user","content":[{"type":"text","text":"u1","cache_control":{"type":"ephemeral"}}]},
			{"role":"user","content":[{"type":"text","text":"u2","cache_control":{"type":"ephemeral"}}]}
		]
	}`)

	out := enforceCacheControlLimit(payload, 4)

	if got := countCacheControls(out); got != 4 {
		t.Fatalf("cache_control count = %d, want 4", got)
	}
	if gjson.GetBytes(out, "tools.0.cache_control").Exists() {
		t.Fatalf("tools.0.cache_control should be removed first (non-last tool)")
	}
	if !gjson.GetBytes(out, "tools.1.cache_control").Exists() {
		t.Fatalf("tools.1.cache_control (last tool) should be preserved")
	}
	if !gjson.GetBytes(out, "messages.0.content.0.cache_control").Exists() || !gjson.GetBytes(out, "messages.1.content.0.cache_control").Exists() {
		t.Fatalf("message cache_control blocks should be preserved when non-last tool removal is enough")
	}
}

func TestEnforceCacheControlLimit_PreservesKeyOrderWhenModified(t *testing.T) {
	payload := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"u1","cache_control":{"type":"ephemeral"}},{"type":"text","text":"u2","cache_control":{"type":"ephemeral"}}]}],"tools":[{"name":"t1","cache_control":{"type":"ephemeral"}},{"name":"t2","cache_control":{"type":"ephemeral"}}],"system":[{"type":"text","text":"s1","cache_control":{"type":"ephemeral"}}]}`)

	out := enforceCacheControlLimit(payload, 4)

	if got := countCacheControls(out); got != 4 {
		t.Fatalf("cache_control count = %d, want 4", got)
	}
	if gjson.GetBytes(out, "tools.0.cache_control").Exists() {
		t.Fatalf("tools.0.cache_control should be removed first (non-last tool)")
	}

	outStr := string(out)
	idxModel := strings.Index(outStr, `"model"`)
	idxMessages := strings.Index(outStr, `"messages"`)
	idxTools := strings.Index(outStr, `"tools"`)
	idxSystem := strings.Index(outStr, `"system"`)
	if idxModel == -1 || idxMessages == -1 || idxTools == -1 || idxSystem == -1 {
		t.Fatalf("failed to locate top-level keys in output: %s", outStr)
	}
	if !(idxModel < idxMessages && idxMessages < idxTools && idxTools < idxSystem) {
		t.Fatalf("top-level key order changed:\noriginal: %s\ngot:      %s", payload, out)
	}
}

func TestEnforceCacheControlLimit_ToolOnlyPayloadStillRespectsLimit(t *testing.T) {
	payload := []byte(`{
		"tools": [
			{"name":"t1","cache_control":{"type":"ephemeral"}},
			{"name":"t2","cache_control":{"type":"ephemeral"}},
			{"name":"t3","cache_control":{"type":"ephemeral"}},
			{"name":"t4","cache_control":{"type":"ephemeral"}},
			{"name":"t5","cache_control":{"type":"ephemeral"}}
		]
	}`)

	out := enforceCacheControlLimit(payload, 4)

	if got := countCacheControls(out); got != 4 {
		t.Fatalf("cache_control count = %d, want 4", got)
	}
	if gjson.GetBytes(out, "tools.0.cache_control").Exists() {
		t.Fatalf("tools.0.cache_control should be removed to satisfy max=4")
	}
	if !gjson.GetBytes(out, "tools.4.cache_control").Exists() {
		t.Fatalf("last tool cache_control should be preserved when possible")
	}
}

func TestClaudeExecutor_CountTokens_AppliesCacheControlGuards(t *testing.T) {
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":42}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}

	payload := []byte(`{
		"tools": [
			{"name":"t1","cache_control":{"type":"ephemeral","ttl":"1h"}},
			{"name":"t2","cache_control":{"type":"ephemeral"}}
		],
		"system": [
			{"type":"text","text":"s1","cache_control":{"type":"ephemeral","ttl":"1h"}},
			{"type":"text","text":"s2","cache_control":{"type":"ephemeral","ttl":"1h"}}
		],
		"messages": [
			{"role":"user","content":[{"type":"text","text":"u1","cache_control":{"type":"ephemeral","ttl":"1h"}}]},
			{"role":"user","content":[{"type":"text","text":"u2","cache_control":{"type":"ephemeral","ttl":"1h"}}]}
		]
	}`)

	_, err := executor.CountTokens(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-haiku-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("CountTokens error: %v", err)
	}

	if len(seenBody) == 0 {
		t.Fatal("expected count_tokens request body to be captured")
	}
	if got := countCacheControls(seenBody); got > 4 {
		t.Fatalf("count_tokens body has %d cache_control blocks, want <= 4", got)
	}
	if hasTTLOrderingViolation(seenBody) {
		t.Fatalf("count_tokens body still has ttl ordering violations: %s", string(seenBody))
	}
}

func TestClaudeExecutor_ExecuteSanitizesSignaturesBeforeUpstream(t *testing.T) {
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-sonnet-4-5","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}

	payload := []byte(`{
		"model": "claude-sonnet-4-5",
		"max_tokens": 16,
		"messages": [
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"drop this","signature":""},
				{"type":"text","text":"I will run git status."},
				{"type":"tool_use","id":"Bash-1","name":"Bash","input":{"command":"git status"},"signature":"bad","thoughtSignature":"bad2","model":"claude-opus-4-1"}
			]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"Bash-1","content":"ok"}]}
		]
	}`)

	if _, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-5",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
		Stream:       false,
	}); err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	parts := gjson.GetBytes(seenBody, "messages.0.content").Array()
	if len(parts) != 2 {
		t.Fatalf("messages.0.content length = %d, want 2; body=%s", len(parts), seenBody)
	}
	if parts[0].Get("type").String() != "text" {
		t.Fatalf("first remaining part = %s, want text", parts[0].Raw)
	}
	toolUse := parts[1]
	if toolUse.Get("type").String() != "tool_use" {
		t.Fatalf("second remaining part = %s, want tool_use", toolUse.Raw)
	}
	for _, path := range []string{"signature", "thoughtSignature", "model"} {
		if toolUse.Get(path).Exists() {
			t.Fatalf("tool_use.%s should be removed before upstream: %s", path, seenBody)
		}
	}
}

func hasTTLOrderingViolation(payload []byte) bool {
	seen5m := false
	violates := false

	checkCC := func(cc gjson.Result) {
		if !cc.Exists() || violates {
			return
		}
		ttl := cc.Get("ttl").String()
		if ttl != "1h" {
			seen5m = true
			return
		}
		if seen5m {
			violates = true
		}
	}

	tools := gjson.GetBytes(payload, "tools")
	if tools.IsArray() {
		tools.ForEach(func(_, tool gjson.Result) bool {
			checkCC(tool.Get("cache_control"))
			return !violates
		})
	}

	system := gjson.GetBytes(payload, "system")
	if system.IsArray() {
		system.ForEach(func(_, item gjson.Result) bool {
			checkCC(item.Get("cache_control"))
			return !violates
		})
	}

	messages := gjson.GetBytes(payload, "messages")
	if messages.IsArray() {
		messages.ForEach(func(_, msg gjson.Result) bool {
			content := msg.Get("content")
			if content.IsArray() {
				content.ForEach(func(_, item gjson.Result) bool {
					checkCC(item.Get("cache_control"))
					return !violates
				})
			}
			return !violates
		})
	}

	return violates
}

func TestClaudeExecutor_Execute_InvalidGzipErrorBodyReturnsTypedProtocolFailure(t *testing.T) {
	testClaudeExecutorInvalidCompressedErrorBody(t, func(executor *ClaudeExecutor, auth *cliproxyauth.Auth, payload []byte) error {
		_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
			Model:   "claude-3-5-sonnet-20241022",
			Payload: payload,
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
		return err
	})
}

func TestClaudeExecutor_ExecuteStream_InvalidGzipErrorBodyReturnsTypedProtocolFailure(t *testing.T) {
	testClaudeExecutorInvalidCompressedErrorBody(t, func(executor *ClaudeExecutor, auth *cliproxyauth.Auth, payload []byte) error {
		_, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
			Model:   "claude-3-5-sonnet-20241022",
			Payload: payload,
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
		return err
	})
}

func TestClaudeExecutor_CountTokens_InvalidGzipErrorBodyReturnsTypedProtocolFailure(t *testing.T) {
	testClaudeExecutorInvalidCompressedErrorBody(t, func(executor *ClaudeExecutor, auth *cliproxyauth.Auth, payload []byte) error {
		_, err := executor.CountTokens(context.Background(), auth, cliproxyexecutor.Request{
			Model:   "claude-3-5-sonnet-20241022",
			Payload: payload,
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
		return err
	})
}

func testClaudeExecutorInvalidCompressedErrorBody(
	t *testing.T,
	invoke func(executor *ClaudeExecutor, auth *cliproxyauth.Auth, payload []byte) error,
) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("not-a-valid-gzip-stream"))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	err := invoke(executor, auth, payload)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	typed, ok := failurecontract.As(err)
	if !ok {
		t.Fatalf("error = %T %v, want typed failure", err, err)
	}
	if typed.Kind != failurecontract.UpstreamProtocolError || typed.Scope != failurecontract.ScopeProvider {
		t.Fatalf("failure = %q/%q, want upstream_protocol_error/provider", typed.Kind, typed.Scope)
	}
	if typed.HTTPStatus != http.StatusBadGateway || typed.ProviderCode != "upstream_response_decode_failed" || typed.Retryable {
		t.Fatalf("failure metadata = status:%d code:%q retryable:%t", typed.HTTPStatus, typed.ProviderCode, typed.Retryable)
	}
}

func TestEnsureModelMaxTokens_UsesRegisteredMaxCompletionTokens(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	clientID := "test-claude-max-completion-tokens-client"
	modelID := "test-claude-max-completion-tokens-model"
	reg.RegisterClient(clientID, "claude", []*registry.ModelInfo{{
		ID:                  modelID,
		Type:                "claude",
		OwnedBy:             "anthropic",
		Object:              "model",
		Created:             time.Now().Unix(),
		MaxCompletionTokens: 4096,
		UserDefined:         true,
	}})
	defer reg.UnregisterClient(clientID)

	input := []byte(`{"model":"test-claude-max-completion-tokens-model","messages":[{"role":"user","content":"hi"}]}`)
	out := ensureModelMaxTokens(input, modelID)

	if got := gjson.GetBytes(out, "max_tokens").Int(); got != 4096 {
		t.Fatalf("max_tokens = %d, want %d", got, 4096)
	}
}

func TestEnsureModelMaxTokens_DefaultsMissingValue(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	clientID := "test-claude-default-max-tokens-client"
	modelID := "test-claude-default-max-tokens-model"
	reg.RegisterClient(clientID, "claude", []*registry.ModelInfo{{
		ID:          modelID,
		Type:        "claude",
		OwnedBy:     "anthropic",
		Object:      "model",
		Created:     time.Now().Unix(),
		UserDefined: true,
	}})
	defer reg.UnregisterClient(clientID)

	input := []byte(`{"model":"test-claude-default-max-tokens-model","messages":[{"role":"user","content":"hi"}]}`)
	out := ensureModelMaxTokens(input, modelID)

	if got := gjson.GetBytes(out, "max_tokens").Int(); got != defaultModelMaxTokens {
		t.Fatalf("max_tokens = %d, want %d", got, defaultModelMaxTokens)
	}
}

func TestEnsureModelMaxTokens_PreservesExplicitValue(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	clientID := "test-claude-preserve-max-tokens-client"
	modelID := "test-claude-preserve-max-tokens-model"
	reg.RegisterClient(clientID, "claude", []*registry.ModelInfo{{
		ID:                  modelID,
		Type:                "claude",
		OwnedBy:             "anthropic",
		Object:              "model",
		Created:             time.Now().Unix(),
		MaxCompletionTokens: 4096,
		UserDefined:         true,
	}})
	defer reg.UnregisterClient(clientID)

	input := []byte(`{"model":"test-claude-preserve-max-tokens-model","max_tokens":2048,"messages":[{"role":"user","content":"hi"}]}`)
	out := ensureModelMaxTokens(input, modelID)

	if got := gjson.GetBytes(out, "max_tokens").Int(); got != 2048 {
		t.Fatalf("max_tokens = %d, want %d", got, 2048)
	}
}

func TestEnsureModelMaxTokens_SkipsUnregisteredModel(t *testing.T) {
	input := []byte(`{"model":"test-claude-unregistered-model","messages":[{"role":"user","content":"hi"}]}`)
	out := ensureModelMaxTokens(input, "test-claude-unregistered-model")

	if gjson.GetBytes(out, "max_tokens").Exists() {
		t.Fatalf("max_tokens should remain unset, got %s", gjson.GetBytes(out, "max_tokens").Raw)
	}
}

// TestClaudeExecutor_ExecuteStream_SetsIdentityAcceptEncoding verifies that streaming
// requests use Accept-Encoding: identity so the upstream cannot respond with a
// compressed SSE body that would silently break the line scanner.
func TestClaudeExecutor_ExecuteStream_SetsIdentityAcceptEncoding(t *testing.T) {
	var gotEncoding, gotAccept string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEncoding = r.Header.Get("Accept-Encoding")
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected chunk error: %v", chunk.Err)
		}
	}

	if gotEncoding != "identity" {
		t.Errorf("Accept-Encoding = %q, want %q", gotEncoding, "identity")
	}
	if gotAccept != "text/event-stream" {
		t.Errorf("Accept = %q, want %q", gotAccept, "text/event-stream")
	}
}

func TestClaudeExecutor_ExecuteStream_SharedConsumerRestoresToolNames(t *testing.T) {
	tests := []struct {
		name            string
		payload         []byte
		format          sdktranslator.Format
		usageField      string
		totalUsageField string
	}{
		{
			name: "direct",
			payload: []byte(`{
				"max_tokens":128,
				"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],
				"tools":[{"name":"skill:pet_animals","input_schema":{"type":"object"}}]
			}`),
			format:     sdktranslator.FromString("claude"),
			usageField: `"input_tokens":2`,
		},
		{
			name: "translated",
			payload: []byte(`{
				"max_tokens":128,
				"messages":[{"role":"user","content":"hi"}],
				"tools":[{"type":"function","function":{"name":"skill:pet_animals","parameters":{"type":"object"}}}]
			}`),
			format:          sdktranslator.FromString("openai"),
			usageField:      `"prompt_tokens":2`,
			totalUsageField: `"total_tokens":3`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstreamToolName := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				toolName := gjson.GetBytes(body, "tools.0.name").String()
				upstreamToolName <- toolName
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, strings.Join([]string{
					`event: message_start`,
					`data: {"type":"message_start","message":{"id":"msg_123","model":"claude-3-5-sonnet-20241022","usage":{"input_tokens":2,"output_tokens":0}}}`,
					``,
					`event: content_block_start`,
					`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":%q,"input":{}}}`,
					``,
					`event: content_block_delta`,
					`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
					``,
					`event: content_block_stop`,
					`data: {"type":"content_block_stop","index":0}`,
					``,
					`event: message_delta`,
					`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`,
					``,
				}, "\n")+"\n", toolName)
			}))
			defer server.Close()

			executor := NewClaudeExecutor(&config.Config{DisableClaudeCloakMode: true})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{
				"api_key":  "key-123",
				"base_url": server.URL,
			}}
			result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
				Model:   "claude-3-5-sonnet-20241022",
				Payload: tt.payload,
			}, cliproxyexecutor.Options{SourceFormat: tt.format})
			if err != nil {
				t.Fatalf("ExecuteStream error: %v", err)
			}

			var combined strings.Builder
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					t.Fatalf("chunk error: %v", chunk.Err)
				}
				combined.Write(chunk.Payload)
			}
			if got := <-upstreamToolName; got != "skill_pet_animals" {
				t.Fatalf("upstream tool name = %q, want skill_pet_animals", got)
			}
			if !strings.Contains(combined.String(), `"name":"skill:pet_animals"`) {
				t.Fatalf("downstream stream did not restore tool name: %s", combined.String())
			}
			if !strings.Contains(combined.String(), tt.usageField) {
				t.Fatalf("downstream stream missing usage field %s: %s", tt.usageField, combined.String())
			}
			if tt.totalUsageField != "" && !strings.Contains(combined.String(), tt.totalUsageField) {
				t.Fatalf("downstream stream did not merge cross-event usage field %s: %s", tt.totalUsageField, combined.String())
			}
		})
	}
}

func TestClaudeExecutor_ExecuteStream_PatchesQianfanStartUsageForProgress(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			`event: message_start`,
			`data: {"type":"message_start","message":{"id":"as_123","type":"message","role":"assistant","model":"qianfan-code-latest","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}`,
			`event: message_delta`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":42,"output_tokens":1}}`,
			`event: message_stop`,
			`data: {"type":"message_stop"}`,
			``,
		}, "\n")))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":     "key-123",
		"base_url":    server.URL,
		"compat_kind": "qianfan",
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"tools":[{"name":"read_file","description":"Read a file","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}]}`)

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "qianfan-code-latest",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var combined strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("chunk error: %v", chunk.Err)
		}
		combined.Write(chunk.Payload)
	}
	startPayload := findClaudeSSEPayload(t, combined.String(), "message_start")
	if got := gjson.GetBytes(startPayload, "message.usage.input_tokens").Int(); got <= 0 {
		t.Fatalf("message_start input_tokens = %d, want patched positive value; payload=%s", got, string(startPayload))
	}
	deltaPayload := findClaudeSSEPayload(t, combined.String(), "message_delta")
	if got := gjson.GetBytes(deltaPayload, "usage.input_tokens").Int(); got != 42 {
		t.Fatalf("message_delta input_tokens = %d, want upstream value 42; payload=%s", got, string(deltaPayload))
	}
}

func TestPatchClaudeMessageStartUsageForProgressKeepsExistingUsage(t *testing.T) {
	line := []byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":99,"output_tokens":0}}}`)
	out := patchClaudeMessageStartUsageForProgress(line, 1234)
	payload, ok := sseDataPayload(out)
	if !ok {
		t.Fatal("expected patched line to remain an SSE data line")
	}
	if got := gjson.GetBytes(payload, "message.usage.input_tokens").Int(); got != 99 {
		t.Fatalf("input_tokens = %d, want existing value 99", got)
	}
}

func TestNormalizeClaudeStringMessageSSELineConvertsTopLevelMessageToError(t *testing.T) {
	line := []byte(`data: {"type":"error","message":"upstream failed"}`)
	out := normalizeClaudeStringMessageSSELine(line)
	payload, ok := sseDataPayload(out)
	if !ok {
		t.Fatal("expected normalized line to remain an SSE data line")
	}
	if gjson.GetBytes(payload, "message").Exists() {
		t.Fatalf("top-level message was not removed: %s", string(payload))
	}
	if got := gjson.GetBytes(payload, "type").String(); got != "error" {
		t.Fatalf("type = %q, want error; payload=%s", got, string(payload))
	}
	if got := gjson.GetBytes(payload, "error.type").String(); got != "api_error" {
		t.Fatalf("error.type = %q, want api_error; payload=%s", got, string(payload))
	}
	if got := gjson.GetBytes(payload, "error.message").String(); got != "upstream failed" {
		t.Fatalf("error.message = %q, want upstream failed; payload=%s", got, string(payload))
	}
}

func TestNormalizeClaudeStringMessageSSELineKeepsMessageStartObject(t *testing.T) {
	line := []byte(`data: {"type":"message_start","message":{"id":"msg_1","model":"qwen3.7-plus"}}`)
	out := normalizeClaudeStringMessageSSELine(line)
	if string(out) != string(line) {
		t.Fatalf("line changed unexpectedly:\ngot  %s\nwant %s", string(out), string(line))
	}
}

func findClaudeSSEPayload(t *testing.T, stream, eventType string) []byte {
	t.Helper()
	for _, line := range strings.Split(stream, "\n") {
		payload, ok := sseDataPayload([]byte(line))
		if !ok || len(payload) == 0 || !gjson.ValidBytes(payload) {
			continue
		}
		if gjson.GetBytes(payload, "type").String() == eventType {
			return payload
		}
	}
	t.Fatalf("stream did not contain event type %q: %s", eventType, stream)
	return nil
}

// TestClaudeExecutor_Execute_SetsCompressedAcceptEncoding verifies that non-streaming
// requests keep the full accept-encoding to allow response compression (which
// decodeResponseBody handles correctly).
func TestClaudeExecutor_Execute_SetsCompressedAcceptEncoding(t *testing.T) {
	var gotEncoding, gotAccept string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEncoding = r.Header.Get("Accept-Encoding")
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet-20241022","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	if gotEncoding != "gzip, deflate, br, zstd" {
		t.Errorf("Accept-Encoding = %q, want %q", gotEncoding, "gzip, deflate, br, zstd")
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q, want %q", gotAccept, "application/json")
	}
}

// TestClaudeExecutor_ExecuteStream_GzipSuccessBodyDecoded verifies that a streaming
// HTTP 200 response with Content-Encoding: gzip is correctly decompressed before
// the line scanner runs, so SSE chunks are not silently dropped.
func TestClaudeExecutor_ExecuteStream_GzipSuccessBodyDecoded(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte("data: {\"type\":\"message_stop\"}\n"))
	_ = gz.Close()
	compressedBody := buf.Bytes()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(len(compressedBody)))
		w.Header().Set("ETag", `"encoded"`)
		w.Header().Set("Content-MD5", "encoded")
		w.Header().Set("Digest", "sha-256=encoded")
		_, _ = w.Write(compressedBody)
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	for _, name := range []string{"Content-Encoding", "Content-Length", "ETag", "Content-MD5", "Digest"} {
		if got := result.Headers.Get(name); got != "" {
			t.Fatalf("stream response %s = %q, want empty after decoding", name, got)
		}
	}

	var combined strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("chunk error: %v", chunk.Err)
		}
		combined.Write(chunk.Payload)
	}

	if combined.Len() == 0 {
		t.Fatal("expected at least one chunk from gzip-encoded SSE body, got none (body was not decompressed)")
	}
	if !strings.Contains(combined.String(), "message_stop") {
		t.Errorf("expected SSE content in chunks, got: %q", combined.String())
	}
}

func TestClaudeExecutor_ExecuteStream_CancelClosesUpstream(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		format  sdktranslator.Format
		want    []byte
	}{
		{
			name:    "direct",
			payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
			format:  sdktranslator.FromString("claude"),
			want:    []byte(`"type":"message_start"`),
		},
		{
			name:    "translated",
			payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
			format:  sdktranslator.FromString("openai"),
			want:    []byte(`"object":"chat.completion.chunk"`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requestCancelled := make(chan struct{})
			releaseServer := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_123\",\"model\":\"claude-3-5-sonnet-20241022\"}}\n\n"))
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				select {
				case <-r.Context().Done():
					close(requestCancelled)
				case <-releaseServer:
				}
			}))
			defer func() {
				close(releaseServer)
				server.Close()
			}()

			executor := NewClaudeExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{
				"api_key":  "key-123",
				"base_url": server.URL,
			}}
			result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
				Model:   "claude-3-5-sonnet-20241022",
				Payload: tt.payload,
			}, cliproxyexecutor.Options{SourceFormat: tt.format})
			if err != nil {
				t.Fatalf("ExecuteStream error: %v", err)
			}

			select {
			case chunk := <-result.Chunks:
				if chunk.Err != nil || !bytes.Contains(chunk.Payload, tt.want) {
					t.Fatalf("first chunk = %q, error = %v", chunk.Payload, chunk.Err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for first stream chunk")
			}
			result.Close()
			result.Close()

			select {
			case <-requestCancelled:
			case <-time.After(2 * time.Second):
				t.Fatal("upstream request was not cancelled")
			}
			select {
			case _, ok := <-result.Chunks:
				if ok {
					t.Fatal("stream channel remained open after cancellation")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("stream channel did not close after cancellation")
			}
		})
	}
}

func TestClaudeExecutor_ExecuteStream_TranslatedReadErrorEmittedOnce(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write([]byte("not-gzip"))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	errorChunks := 0
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			errorChunks++
		}
	}
	if errorChunks != 1 {
		t.Fatalf("error chunks = %d, want 1", errorChunks)
	}
}

func TestRewriteClaudeSSEEventPreservesLineEndings(t *testing.T) {
	event := []byte("data: one\nid: two\r\nretry: three\rdata: four")
	got := rewriteClaudeSSEEvent(event, func(line []byte) []byte {
		return append([]byte("rewritten:"), line...)
	})
	want := []byte("rewritten:data: one\nrewritten:id: two\r\nrewritten:retry: three\rrewritten:data: four")
	if !bytes.Equal(got, want) {
		t.Fatalf("rewritten event = %q, want %q", got, want)
	}
}

// TestClaudeExecutor_ExecuteStream_GzipNoContentEncodingHeader verifies the full
// pipeline: when the upstream returns a gzip-compressed SSE body WITHOUT setting
// Content-Encoding (a misbehaving upstream), the shared bounded reader still
// decompresses it, so chunks reach the caller.
func TestClaudeExecutor_ExecuteStream_GzipNoContentEncodingHeader(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte("data: {\"type\":\"message_stop\"}\n"))
	_ = gz.Close()
	compressedBody := buf.Bytes()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Intentionally omit Content-Encoding to simulate misbehaving upstream.
		_, _ = w.Write(compressedBody)
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var combined strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("chunk error: %v", chunk.Err)
		}
		combined.Write(chunk.Payload)
	}

	if combined.Len() == 0 {
		t.Fatal("expected chunks from gzip body without Content-Encoding header, got none (magic-byte sniff failed)")
	}
	if !strings.Contains(combined.String(), "message_stop") {
		t.Errorf("unexpected chunk content: %q", combined.String())
	}
}

// TestClaudeExecutor_Execute_GzipErrorBodyNoContentEncodingHeader verifies that the
// error path decodes gzip for classification without exposing the upstream body.
func TestClaudeExecutor_Execute_GzipErrorBodyNoContentEncodingHeader(t *testing.T) {
	const secret = "gzip-error-body-sentinel"
	const errJSON = `{"type":"error","error":{"type":"invalid_request_error","message":"` + secret + `"}}`

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte(errJSON))
	_ = gz.Close()
	compressedBody := buf.Bytes()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Intentionally omit Content-Encoding to simulate misbehaving upstream.
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(compressedBody)
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err == nil {
		t.Fatal("expected an error for 400 response, got nil")
	}
	assertStatusErr(t, err, http.StatusBadRequest)
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), `"message"`) {
		t.Fatalf("error exposed decompressed upstream body: %q", err.Error())
	}
	for _, want := range []string{"error_type=invalid_request_error", `"bytes":`, `"sha256":`, `"content_type":"application/json"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("safe error metadata = %q, want %q", err.Error(), want)
		}
	}
}

// TestClaudeExecutor_ExecuteStream_GzipErrorBodyNoContentEncodingHeader verifies
// the same safe classification behavior for the streaming executor.
func TestClaudeExecutor_ExecuteStream_GzipErrorBodyNoContentEncodingHeader(t *testing.T) {
	const secret = "gzip-stream-error-body-sentinel"
	const errJSON = `{"type":"error","error":{"type":"invalid_request_error","message":"` + secret + `"}}`

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte(errJSON))
	_ = gz.Close()
	compressedBody := buf.Bytes()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Intentionally omit Content-Encoding to simulate misbehaving upstream.
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(compressedBody)
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	_, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err == nil {
		t.Fatal("expected an error for 400 response, got nil")
	}
	assertStatusErr(t, err, http.StatusBadRequest)
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), `"message"`) {
		t.Fatalf("error exposed decompressed upstream body: %q", err.Error())
	}
	for _, want := range []string{"error_type=invalid_request_error", `"bytes":`, `"sha256":`, `"content_type":"application/json"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("safe error metadata = %q, want %q", err.Error(), want)
		}
	}
}

// TestClaudeExecutor_ExecuteStream_AcceptEncodingOverrideCannotBypassIdentity verifies that the
// streaming executor enforces Accept-Encoding: identity regardless of auth.Attributes override.
func TestClaudeExecutor_ExecuteStream_AcceptEncodingOverrideCannotBypassIdentity(t *testing.T) {
	var gotEncoding string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEncoding = r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":                "key-123",
		"base_url":               server.URL,
		"header:Accept-Encoding": "gzip, deflate, br, zstd",
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected chunk error: %v", chunk.Err)
		}
	}

	if gotEncoding != "identity" {
		t.Errorf("Accept-Encoding = %q; stream path must enforce identity regardless of auth.Attributes override", gotEncoding)
	}
}

func expectedClaudeCodeStaticPrompt() string {
	return strings.Join([]string{
		helps.ClaudeCodeIntro,
		helps.ClaudeCodeSystem,
		helps.ClaudeCodeDoingTasks,
		helps.ClaudeCodeToneAndStyle,
		helps.ClaudeCodeOutputEfficiency,
	}, "\n\n")
}

func expectedForwardedSystemReminder(text string) string {
	return fmt.Sprintf(`<system-reminder>
As you answer the user's questions, you can use the following context from the system:
%s

IMPORTANT: this context may or may not be relevant to your tasks. You should not respond to this context unless it is highly relevant to your task.
</system-reminder>
`, text)
}

// Test case 1: String system prompt is preserved by forwarding it to the first user message
func TestCheckSystemInstructionsWithMode_StringSystemPreserved(t *testing.T) {
	payload := []byte(`{"system":"You are a helpful assistant.","messages":[{"role":"user","content":"hi"}]}`)

	out := checkSystemInstructionsWithMode(payload, false)

	system := gjson.GetBytes(out, "system")
	if !system.IsArray() {
		t.Fatalf("system should be an array, got %s", system.Type)
	}

	blocks := system.Array()
	if len(blocks) != 3 {
		t.Fatalf("expected 3 system blocks, got %d", len(blocks))
	}

	if !strings.HasPrefix(blocks[0].Get("text").String(), "x-anthropic-billing-header:") {
		t.Fatalf("blocks[0] should be billing header, got %q", blocks[0].Get("text").String())
	}
	if blocks[1].Get("text").String() != "You are Claude Code, Anthropic's official CLI for Claude." {
		t.Fatalf("blocks[1] should be agent block, got %q", blocks[1].Get("text").String())
	}
	if blocks[2].Get("text").String() != expectedClaudeCodeStaticPrompt() {
		t.Fatalf("blocks[2] should be static Claude Code prompt, got %q", blocks[2].Get("text").String())
	}
	if blocks[2].Get("cache_control").Exists() {
		t.Fatalf("blocks[2] should not have cache_control, got %s", blocks[2].Get("cache_control").Raw)
	}

	if got := gjson.GetBytes(out, "messages.0.content").String(); got != expectedForwardedSystemReminder("You are a helpful assistant.")+"hi" {
		t.Fatalf("messages[0].content should include forwarded system prompt, got %q", got)
	}
}

// Test case 2: Strict mode keeps only the injected Claude Code system blocks
func TestCheckSystemInstructionsWithMode_StringSystemStrict(t *testing.T) {
	payload := []byte(`{"system":"You are a helpful assistant.","messages":[{"role":"user","content":"hi"}]}`)

	out := checkSystemInstructionsWithMode(payload, true)

	blocks := gjson.GetBytes(out, "system").Array()
	if len(blocks) != 3 {
		t.Fatalf("strict mode should produce 3 injected blocks, got %d", len(blocks))
	}
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != "hi" {
		t.Fatalf("strict mode should not forward system prompt into messages, got %q", got)
	}
}

// Test case 3: Empty string system prompt does not alter the first user message
func TestCheckSystemInstructionsWithMode_EmptyStringSystemIgnored(t *testing.T) {
	payload := []byte(`{"system":"","messages":[{"role":"user","content":"hi"}]}`)

	out := checkSystemInstructionsWithMode(payload, false)

	blocks := gjson.GetBytes(out, "system").Array()
	if len(blocks) != 3 {
		t.Fatalf("empty string system should still produce 3 injected blocks, got %d", len(blocks))
	}
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != "hi" {
		t.Fatalf("empty string system should not alter messages, got %q", got)
	}
}

// Test case 4: Array system prompt is forwarded to the first user message
func TestCheckSystemInstructionsWithMode_ArraySystemStillWorks(t *testing.T) {
	payload := []byte(`{"system":[{"type":"text","text":"Be concise."}],"messages":[{"role":"user","content":"hi"}]}`)

	out := checkSystemInstructionsWithMode(payload, false)

	blocks := gjson.GetBytes(out, "system").Array()
	if len(blocks) != 3 {
		t.Fatalf("expected 3 system blocks, got %d", len(blocks))
	}
	if blocks[2].Get("text").String() != expectedClaudeCodeStaticPrompt() {
		t.Fatalf("blocks[2] should be static Claude Code prompt, got %q", blocks[2].Get("text").String())
	}
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != expectedForwardedSystemReminder("Be concise.")+"hi" {
		t.Fatalf("messages[0].content should include forwarded array system prompt, got %q", got)
	}
}

// Test case 5: Special characters in string system prompt survive forwarding
func TestCheckSystemInstructionsWithMode_StringWithSpecialChars(t *testing.T) {
	payload := []byte(`{"system":"Use <xml> tags & \"quotes\" in output.","messages":[{"role":"user","content":"hi"}]}`)

	out := checkSystemInstructionsWithMode(payload, false)

	blocks := gjson.GetBytes(out, "system").Array()
	if len(blocks) != 3 {
		t.Fatalf("expected 3 system blocks, got %d", len(blocks))
	}
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != expectedForwardedSystemReminder(`Use <xml> tags & "quotes" in output.`)+"hi" {
		t.Fatalf("forwarded system prompt text mangled, got %q", got)
	}
}

func TestClaudeExecutor_ExperimentalCCHSigningDisabledByDefaultKeepsLegacyHeader(t *testing.T) {
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(seenBody) == 0 {
		t.Fatal("expected request body to be captured")
	}

	billingHeader := gjson.GetBytes(seenBody, "system.0.text").String()
	if !strings.HasPrefix(billingHeader, "x-anthropic-billing-header:") {
		t.Fatalf("system.0.text = %q, want billing header", billingHeader)
	}
	if strings.Contains(billingHeader, "cch=00000;") {
		t.Fatalf("legacy mode should not forward cch placeholder, got %q", billingHeader)
	}
}

func TestClaudeExecutor_ExperimentalCCHSigningOptInSignsFinalBody(t *testing.T) {
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{
		ClaudeKey: []config.ClaudeKey{{
			APIKey:                 "key-123",
			BaseURL:                server.URL,
			ExperimentalCCHSigning: true,
		}},
	})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	const messageText = "please keep literal cch=00000 in this message"
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"please keep literal cch=00000 in this message"}]}]}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(seenBody) == 0 {
		t.Fatal("expected request body to be captured")
	}
	if got := gjson.GetBytes(seenBody, "messages.0.content.0.text").String(); got != messageText {
		t.Fatalf("message text = %q, want %q", got, messageText)
	}

	billingPattern := regexp.MustCompile(`(x-anthropic-billing-header:[^"]*?\bcch=)([0-9a-f]{5})(;)`)
	match := billingPattern.FindSubmatch(seenBody)
	if match == nil {
		t.Fatalf("expected signed billing header in body: %s", string(seenBody))
	}
	actualCCH := string(match[2])
	unsignedBody := billingPattern.ReplaceAll(seenBody, []byte(`${1}00000${3}`))
	wantCCH := fmt.Sprintf("%05x", xxHash64.Checksum(unsignedBody, 0x6E52736AC806831E)&0xFFFFF)
	if actualCCH != wantCCH {
		t.Fatalf("cch = %q, want %q\nbody: %s", actualCCH, wantCCH, string(seenBody))
	}
}

func TestClaudeExecutor_RebuildMidSystemMessageDisabledByDefault(t *testing.T) {
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{
		ClaudeKey: []config.ClaudeKey{{
			APIKey:  "key-123",
			BaseURL: server.URL,
		}},
	})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"system":[{"type":"text","text":"Top rule","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]},{"role":"system","content":"Mid rule"},{"role":"user","content":[{"type":"text","text":"continue"}]}]}`)
	ctx := contextWithGinHeaders(map[string]string{"User-Agent": "claude-cli/2.1.153 (external, cli)"})

	_, errExecute := executor.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if len(seenBody) == 0 {
		t.Fatal("expected request body to be captured")
	}
	if got := gjson.GetBytes(seenBody, "system.0.text").String(); got != "Top rule" {
		t.Fatalf("system.0.text = %q, want top-level system preserved", got)
	}
	if got := gjson.GetBytes(seenBody, `messages.#(role=="system").content`).String(); got != "Mid rule" {
		t.Fatalf("mid system message = %q, want original message preserved", got)
	}
}

func TestClaudeExecutor_RebuildMidSystemMessageOptInMovesSystemMessages(t *testing.T) {
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{
		ClaudeKey: []config.ClaudeKey{{
			APIKey:                  "key-123",
			BaseURL:                 server.URL,
			RebuildMidSystemMessage: true,
		}},
	})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"system":"Top rule","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]},{"role":"system","content":"Mid string rule"},{"role":"assistant","content":[{"type":"text","text":"ok"}]},{"role":"system","content":[{"type":"text","text":"Mid array rule","cache_control":{"type":"ephemeral"}}]},{"role":"user","content":[{"type":"text","text":"continue"}]}]}`)
	ctx := contextWithGinHeaders(map[string]string{"User-Agent": "claude-cli/2.1.153 (external, cli)"})

	_, errExecute := executor.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if len(seenBody) == 0 {
		t.Fatal("expected request body to be captured")
	}

	system := gjson.GetBytes(seenBody, "system").Array()
	if len(system) != 3 {
		t.Fatalf("system has %d items, want 3: %s", len(system), gjson.GetBytes(seenBody, "system").Raw)
	}
	wantTexts := []string{"Top rule", "Mid string rule", "Mid array rule"}
	for i, want := range wantTexts {
		if got := system[i].Get("text").String(); got != want {
			t.Fatalf("system[%d].text = %q, want %q", i, got, want)
		}
	}
	if got := gjson.GetBytes(seenBody, "system.2.cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("system.2.cache_control.type = %q, want ephemeral", got)
	}
	if gjson.GetBytes(seenBody, `messages.#(role=="system")`).Exists() {
		t.Fatalf("messages should not contain system role after rebuild: %s", gjson.GetBytes(seenBody, "messages").Raw)
	}
	if got := gjson.GetBytes(seenBody, "messages.#").Int(); got != 3 {
		t.Fatalf("messages count = %d, want 3", got)
	}
}

func TestApplyCloaking_PreservesConfiguredStrictModeAndSensitiveWordsWhenModeOmitted(t *testing.T) {
	cfg := &config.Config{
		ClaudeKey: []config.ClaudeKey{{
			APIKey: "key-123",
			Cloak: &config.CloakConfig{
				StrictMode:     true,
				SensitiveWords: []string{"proxy"},
			},
		}},
	}
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "key-123"}}
	payload := []byte(`{"system":"proxy rules","messages":[{"role":"user","content":[{"type":"text","text":"proxy access"}]}]}`)

	out, errCloaking := applyCloaking(context.Background(), cfg, auth, payload, "claude-3-5-sonnet-20241022", "key-123")
	if errCloaking != nil {
		t.Fatalf("applyCloaking() error = %v", errCloaking)
	}

	blocks := gjson.GetBytes(out, "system").Array()
	if len(blocks) != 3 {
		t.Fatalf("expected strict mode to keep the 3 injected Claude Code system blocks, got %d", len(blocks))
	}
	if got := gjson.GetBytes(out, "messages.0.content.#").Int(); got != 1 {
		t.Fatalf("strict mode should not prepend a forwarded system reminder block, got %d content blocks", got)
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.text").String(); !strings.Contains(got, "\u200B") {
		t.Fatalf("expected configured sensitive word obfuscation to apply, got %q", got)
	}
}

func TestNormalizeClaudeTemperatureForThinking_AdaptiveCoercesToOne(t *testing.T) {
	payload := []byte(`{"temperature":0,"thinking":{"type":"adaptive"},"output_config":{"effort":"max"}}`)
	out := normalizeClaudeTemperatureForThinking(payload)

	if got := gjson.GetBytes(out, "temperature").Float(); got != 1 {
		t.Fatalf("temperature = %v, want 1", got)
	}
}

func TestNormalizeClaudeTemperatureForThinking_EnabledCoercesToOne(t *testing.T) {
	payload := []byte(`{"temperature":0.2,"thinking":{"type":"enabled","budget_tokens":2048}}`)
	out := normalizeClaudeTemperatureForThinking(payload)

	if got := gjson.GetBytes(out, "temperature").Float(); got != 1 {
		t.Fatalf("temperature = %v, want 1", got)
	}
}

func TestNormalizeClaudeTemperatureForThinking_RemovesTopPAndTopK(t *testing.T) {
	payload := []byte(`{"temperature":0.2,"top_p":0.9,"top_k":40,"thinking":{"type":"adaptive"}}`)
	out := normalizeClaudeTemperatureForThinking(payload)

	if got := gjson.GetBytes(out, "temperature").Float(); got != 1 {
		t.Fatalf("temperature = %v, want 1", got)
	}
	if gjson.GetBytes(out, "top_p").Exists() {
		t.Fatal("top_p should be removed when thinking is active")
	}
	if gjson.GetBytes(out, "top_k").Exists() {
		t.Fatal("top_k should be removed when thinking is active")
	}
}

func TestNormalizeClaudeTemperatureForThinking_NoThinkingLeavesTemperatureAlone(t *testing.T) {
	payload := []byte(`{"temperature":0,"top_p":0.9,"top_k":40,"messages":[{"role":"user","content":"hi"}]}`)
	out := normalizeClaudeTemperatureForThinking(payload)

	if got := gjson.GetBytes(out, "temperature").Float(); got != 0 {
		t.Fatalf("temperature = %v, want 0", got)
	}
	if got := gjson.GetBytes(out, "top_p").Float(); got != 0.9 {
		t.Fatalf("top_p = %v, want 0.9", got)
	}
	if got := gjson.GetBytes(out, "top_k").Int(); got != 40 {
		t.Fatalf("top_k = %v, want 40", got)
	}
}

func TestNormalizeClaudeTemperatureForThinking_AfterForcedToolChoiceKeepsOriginalTemperature(t *testing.T) {
	payload := []byte(`{"temperature":0,"thinking":{"type":"adaptive"},"output_config":{"effort":"max"},"tool_choice":{"type":"any"}}`)
	out := disableThinkingIfToolChoiceForced(payload)
	out = normalizeClaudeTemperatureForThinking(out)

	if gjson.GetBytes(out, "thinking").Exists() {
		t.Fatalf("thinking should be removed when tool_choice forces tool use")
	}
	if got := gjson.GetBytes(out, "temperature").Float(); got != 0 {
		t.Fatalf("temperature = %v, want 0", got)
	}
}

func TestRemapOAuthToolNames_TitleCase_NoReverseNeeded(t *testing.T) {
	body := []byte(`{"tools":[{"name":"Bash","description":"Run shell commands","input_schema":{"type":"object","properties":{"cmd":{"type":"string"}}}}],"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	out, reverseMap := remapOAuthToolNames(body)
	if len(reverseMap) != 0 {
		t.Fatalf("reverseMap = %v, want empty", reverseMap)
	}
	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "Bash" {
		t.Fatalf("tools.0.name = %q, want %q", got, "Bash")
	}

	resp := []byte(`{"content":[{"type":"tool_use","id":"toolu_01","name":"Bash","input":{"cmd":"ls"}}]}`)
	reversed := reverseRemapOAuthToolNames(resp, reverseMap)
	if got := gjson.GetBytes(reversed, "content.0.name").String(); got != "Bash" {
		t.Fatalf("content.0.name = %q, want %q", got, "Bash")
	}
}

func TestRemapOAuthToolNames_Lowercase_ReverseApplied(t *testing.T) {
	body := []byte(`{"tools":[{"name":"bash","description":"Run shell commands","input_schema":{"type":"object","properties":{"cmd":{"type":"string"}}}}],"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	out, reverseMap := remapOAuthToolNames(body)
	if reverseMap["Bash"] != "bash" {
		t.Fatalf("reverseMap = %v, want entry Bash->bash", reverseMap)
	}
	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "Bash" {
		t.Fatalf("tools.0.name = %q, want %q", got, "Bash")
	}

	resp := []byte(`{"content":[{"type":"tool_use","id":"toolu_01","name":"Bash","input":{"cmd":"ls"}}]}`)
	reversed := reverseRemapOAuthToolNames(resp, reverseMap)
	if got := gjson.GetBytes(reversed, "content.0.name").String(); got != "bash" {
		t.Fatalf("content.0.name = %q, want %q", got, "bash")
	}
}

func TestValidateClaudeUpstreamPayload_MiniMaxRejectsStructuredOutputFormat(t *testing.T) {
	t.Parallel()

	payload := []byte(`{
		"model":"MiniMax-M2.5",
		"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],
		"output_config":{
			"format":{
				"type":"json_schema",
				"json_schema":{"name":"result","schema":{"type":"object"}}
			}
		}
	}`)

	err := validateClaudeUpstreamPayload("https://api.minimaxi.io/anthropic", payload)
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	se, ok := err.(statusErr)
	if !ok {
		t.Fatalf("error type = %T, want statusErr", err)
	}
	if se.StatusCode() != http.StatusBadRequest {
		t.Fatalf("status code = %d, want %d", se.StatusCode(), http.StatusBadRequest)
	}
	if !strings.Contains(err.Error(), "request_feature_unsupported:") {
		t.Fatalf("error = %q, want request_feature_unsupported prefix", err.Error())
	}
}

func TestDowngradeClaudeStructuredOutputForCompat_NonAnthropicRemovesFormatAndInjectsSchema(t *testing.T) {
	t.Parallel()

	payload := []byte(`{
		"model":"MiniMax-M2.5",
		"system":"Be concise.",
		"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],
		"output_config":{
			"format":{
				"type":"json_schema",
				"json_schema":{"name":"result","schema":{"type":"object","properties":{"ok":{"type":"boolean"}}}}
			}
		}
	}`)

	out := downgradeClaudeStructuredOutputForCompat("https://api.minimax.io/anthropic", payload)
	if gjson.GetBytes(out, "output_config.format").Exists() {
		t.Fatalf("output_config.format should be removed, got %s", string(out))
	}
	system := gjson.GetBytes(out, "system").String()
	if !strings.Contains(system, "Structured output compatibility mode") {
		t.Fatalf("system prompt missing compatibility instruction: %q", system)
	}
	if !strings.Contains(system, `"json_schema"`) || !strings.Contains(system, `"ok"`) {
		t.Fatalf("system prompt missing schema: %q", system)
	}
	if err := validateClaudeUpstreamPayload("https://api.minimax.io/anthropic", out); err != nil {
		t.Fatalf("downgraded payload should pass MiniMax validation, got %v", err)
	}
}

func TestDowngradeClaudeStructuredOutputForCompat_OfficialAnthropicPreservesFormat(t *testing.T) {
	t.Parallel()

	payload := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],
		"output_config":{
			"format":{"type":"json_schema","json_schema":{"name":"result","schema":{"type":"object"}}}
		}
	}`)

	out := downgradeClaudeStructuredOutputForCompat("https://api.anthropic.com", payload)
	if !gjson.GetBytes(out, "output_config.format").Exists() {
		t.Fatalf("official Anthropic payload should preserve output_config.format, got %s", string(out))
	}
}

func TestDowngradeClaudeStructuredOutputForCompat_AppendsArraySystemBlock(t *testing.T) {
	t.Parallel()

	payload := []byte(`{
		"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],
		"system":[{"type":"text","text":"Existing system."}],
		"output_config":{"format":{"type":"json_object"}}
	}`)

	out := downgradeClaudeStructuredOutputForCompat("https://api.moonshot.cn/anthropic", payload)
	system := gjson.GetBytes(out, "system")
	if got := len(system.Array()); got != 2 {
		t.Fatalf("system block count = %d, want 2; payload=%s", got, string(out))
	}
	if got := gjson.GetBytes(out, "system.1.type").String(); got != "text" {
		t.Fatalf("system.1.type = %q, want text", got)
	}
	if got := gjson.GetBytes(out, "system.1.text").String(); !strings.Contains(got, "json_object") {
		t.Fatalf("system.1.text missing format: %q", got)
	}
}

func TestDowngradeClaudeToolSearchForCompatKind_DeepSeekRemovesUnsupportedBlocks(t *testing.T) {
	t.Parallel()

	payload := []byte(`{
		"model":"deepseek-v4-pro",
		"tools":[
			{"type":"web_search_20250305","name":"web_search"},
			{"name":"read_file","description":"Read file","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}
		],
		"tool_choice":{"type":"tool","name":"web_search"},
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"hi"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},
				{"type":"image_url","image_url":{"url":"data:image/png;base64,BBBB"}},
				{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"CCCC"}},
				{"type":"search_result","content":[{"type":"text","text":"search ok"}]},
				{"type":"mcp_tool_result","content":[{"type":"text","text":"mcp ok"}]}
			]},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"plan"},
				{"type":"redacted_thinking","data":"secret"},
				{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"README.md"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","content":[
					{"type":"text","text":"tool ok"},
					{"type":"image","source":{"type":"base64","media_type":"image/png","data":"DDDD"}}
				]}
			]}
		]
	}`)

	out := downgradeClaudeToolSearchForCompatKind("deepseek", "https://api.deepseek.com/anthropic", payload)

	if got := len(gjson.GetBytes(out, "tools").Array()); got != 1 {
		t.Fatalf("tools count = %d, want 1: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "read_file" {
		t.Fatalf("kept tool name = %q, want read_file: %s", got, string(out))
	}
	if gjson.GetBytes(out, "tool_choice").Exists() {
		t.Fatalf("tool_choice for removed server tool should be removed: %s", string(out))
	}

	userContent := gjson.GetBytes(out, "messages.0.content").Array()
	if hasClaudePartType(userContent, "image") || hasClaudePartType(userContent, "image_url") || hasClaudePartType(userContent, "document") || hasClaudePartType(userContent, "search_result") || hasClaudePartType(userContent, "mcp_tool_result") {
		t.Fatalf("DeepSeek unsupported user content block remained: %s", string(out))
	}
	for _, wantText := range []string{"hi", "search ok", "mcp ok"} {
		if !hasClaudeText(userContent, wantText) {
			t.Fatalf("expected text %q in downgraded content: %s", wantText, string(out))
		}
	}

	assistantContent := gjson.GetBytes(out, "messages.1.content").Array()
	if hasClaudePartType(assistantContent, "redacted_thinking") {
		t.Fatalf("redacted_thinking should be removed for DeepSeek: %s", string(out))
	}
	if !hasClaudePartType(assistantContent, "thinking") || !hasClaudePartType(assistantContent, "tool_use") {
		t.Fatalf("supported thinking/tool_use blocks should be preserved: %s", string(out))
	}

	toolResultContent := gjson.GetBytes(out, "messages.2.content.0.content").Array()
	if hasClaudePartType(toolResultContent, "image") {
		t.Fatalf("unsupported image inside tool_result should be removed: %s", string(out))
	}
	if !hasClaudeText(toolResultContent, "tool ok") {
		t.Fatalf("tool_result text should be preserved: %s", string(out))
	}
}

func TestDowngradeClaudeToolSearchForCompatKind_DeepSeekSanitizesToolSchema(t *testing.T) {
	t.Parallel()

	payload := []byte(`{
		"model":"deepseek-v4-flash",
		"tools":[{
			"name":"browser_back",
			"description":"Navigate back in the browser",
			"input_schema":{
				"type":"object",
				"properties":{
					"sessions":{"type":"array","items":null}
				},
				"required":null
			}
		}],
		"messages":[{"role":"user","content":[{"type":"text","text":"go back"}]}]
	}`)

	out := downgradeClaudeToolSearchForCompatKind("deepseek", "https://api.deepseek.com/anthropic", payload)

	if gjson.GetBytes(out, "tools.0.input_schema.required").Exists() {
		t.Fatalf("required=null should be removed for DeepSeek: %s", string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.input_schema.properties.sessions.items.type").String(); got == "" {
		t.Fatalf("array items should be filled for DeepSeek: %s", string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.input_schema.additionalProperties"); !got.Exists() || got.Bool() {
		t.Fatalf("object schema should include additionalProperties=false for DeepSeek: %s", string(out))
	}
}

func TestDeepSeekClaudeCompatNormalizesThinkingBudgetByModelName(t *testing.T) {
	t.Parallel()

	payload := []byte(`{
		"model":"deepseek-v4-pro",
		"thinking":{"type":"enabled","budget_tokens":50},
		"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]
	}`)

	out := scrubDeepSeekThinkingBudgetForCompat(payload, "deepseek-v4-pro", "https://tokenrai.com", "")

	if got := gjson.GetBytes(out, "thinking.budget_tokens").Int(); got != 100 {
		t.Fatalf("thinking.budget_tokens = %d, want 100: %s", got, string(out))
	}
}

func TestDoubaoClaudeDeepSeekThinkingClampsUnsupportedEffort(t *testing.T) {
	t.Parallel()

	payload := []byte(`{
		"model":"deepseek-v4-pro",
		"reasoning_effort":"xhigh",
		"thinking":{"type":"enabled","budget_tokens":99999},
		"output_config":{"effort":"max"},
		"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]
	}`)

	out := scrubDoubaoClaudeDeepSeekThinkingForCompat(payload, "deepseek-v4-pro", "doubao")

	if got := gjson.GetBytes(out, "thinking.type").String(); got != "adaptive" {
		t.Fatalf("thinking.type = %q, want adaptive: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "output_config.effort").String(); got != "high" {
		t.Fatalf("output_config.effort = %q, want high: %s", got, string(out))
	}
	for _, path := range []string{
		"reasoning",
		"reasoning_effort",
		"thinking.reasoning_effort",
		"thinking.budget_tokens",
		"thinking_budget",
	} {
		if gjson.GetBytes(out, path).Exists() {
			t.Fatalf("%s should be removed for doubao Claude DeepSeek compat: %s", path, string(out))
		}
	}
}

func TestSanitizeClaudeHTTPRequestToolNames_NormalizesDoubaoDeepSeekThinking(t *testing.T) {
	t.Parallel()

	payload := `{"model":"deepseek-v4-pro","reasoning_effort":"xhigh","thinking":{"type":"enabled","budget_tokens":"50"},"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "https://ark.cn-beijing.volces.com/api/v3/v1/messages?beta=true", strings.NewReader(payload))

	if _, err := sanitizeClaudeHTTPRequestToolNames(req); err != nil {
		t.Fatalf("sanitizeClaudeHTTPRequestToolNames() error = %v", err)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if got := gjson.GetBytes(body, "thinking.type").String(); got != "adaptive" {
		t.Fatalf("thinking.type = %q, want adaptive: %s", got, string(body))
	}
	if got := gjson.GetBytes(body, "output_config.effort").String(); got != "high" {
		t.Fatalf("output_config.effort = %q, want high: %s", got, string(body))
	}
	if gjson.GetBytes(body, "reasoning_effort").Exists() {
		t.Fatalf("reasoning_effort should be removed: %s", string(body))
	}
	if gjson.GetBytes(body, "thinking.budget_tokens").Exists() {
		t.Fatalf("thinking.budget_tokens should be removed: %s", string(body))
	}
}

func TestSanitizeClaudeHTTPRequestToolNames_DowngradesDeepSeekAnthropicBody(t *testing.T) {
	t.Parallel()

	payload := `{"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]}`
	req := httptest.NewRequest(http.MethodPost, "https://api.deepseek.com/anthropic/v1/messages?beta=true", strings.NewReader(payload))

	if _, err := sanitizeClaudeHTTPRequestToolNames(req); err != nil {
		t.Fatalf("sanitizeClaudeHTTPRequestToolNames() error = %v", err)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if hasClaudePartType(gjson.GetBytes(body, "messages.0.content").Array(), "image") {
		t.Fatalf("DeepSeek direct HttpRequest body should remove image blocks: %s", string(body))
	}
	if !hasClaudeText(gjson.GetBytes(body, "messages.0.content").Array(), "hi") {
		t.Fatalf("text should be preserved: %s", string(body))
	}
}

func TestSanitizeClaudeHTTPRequestToolNames_NormalizesDeepSeekThinkingBudget(t *testing.T) {
	t.Parallel()

	payload := `{"model":"deepseek-v4-pro","thinking":{"type":"enabled","budget_tokens":"50"},"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "https://api.deepseek.com/anthropic/v1/messages?beta=true", strings.NewReader(payload))

	if _, err := sanitizeClaudeHTTPRequestToolNames(req); err != nil {
		t.Fatalf("sanitizeClaudeHTTPRequestToolNames() error = %v", err)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if got := gjson.GetBytes(body, "thinking.budget_tokens").Int(); got != 100 {
		t.Fatalf("thinking.budget_tokens = %d, want 100: %s", got, string(body))
	}
}

func hasClaudePartType(parts []gjson.Result, partType string) bool {
	for _, part := range parts {
		if part.Get("type").String() == partType {
			return true
		}
	}
	return false
}

func hasClaudeText(parts []gjson.Result, text string) bool {
	for _, part := range parts {
		if part.Get("type").String() == "text" && part.Get("text").String() == text {
			return true
		}
	}
	return false
}

func TestValidateClaudeUpstreamPayload_NonMiniMaxAllowsStructuredOutputFormat(t *testing.T) {
	t.Parallel()

	payload := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],
		"output_config":{
			"format":{
				"type":"json_schema",
				"json_schema":{"name":"result","schema":{"type":"object"}}
			}
		}
	}`)

	if err := validateClaudeUpstreamPayload("https://api.anthropic.com", payload); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

func TestValidateClaudeUpstreamPayload_MiniMaxRejectsServerTool(t *testing.T) {
	t.Parallel()

	payload := []byte(`{
		"model":"MiniMax-M2.5",
		"messages":[{"role":"user","content":[{"type":"text","text":"search"}]}],
		"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":8}]
	}`)

	err := validateClaudeUpstreamPayload("https://api.minimax.io/anthropic", payload)
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	se, ok := err.(statusErr)
	if !ok {
		t.Fatalf("error type = %T, want statusErr", err)
	}
	if se.StatusCode() != http.StatusBadRequest {
		t.Fatalf("status code = %d, want %d", se.StatusCode(), http.StatusBadRequest)
	}
	if !strings.Contains(err.Error(), "request_feature_unsupported:") {
		t.Fatalf("error = %q, want request_feature_unsupported prefix", err.Error())
	}
}

func TestValidateClaudeUpstreamPayload_MiniMaxAllowsCustomTypedToolWithSchema(t *testing.T) {
	t.Parallel()

	payload := []byte(`{
		"model":"MiniMax-M2.5",
		"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],
		"tools":[{"type":"custom","name":"lookup","input_schema":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}]
	}`)

	if err := validateClaudeUpstreamPayload("https://api.minimax.io/anthropic", payload); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

// TestRemapOAuthToolNames_MixedCase_OnlyRenamedToolsReversed is the regression
// test for a case where a single request contains both a TitleCase tool (which
// must pass through unchanged) and a lowercase tool that we forward-rename.
// Before the fix, triggering ANY forward rename caused the reverse pass to
// lowercase every TitleCase tool in the response using a global reverse map,
// corrupting tool names the client originally sent in TitleCase.
func TestRemapOAuthToolNames_MixedCase_OnlyRenamedToolsReversed(t *testing.T) {
	body := []byte(`{"tools":[` +
		`{"name":"Bash","input_schema":{"type":"object","properties":{"cmd":{"type":"string"}}}},` +
		`{"name":"glob","input_schema":{"type":"object","properties":{"filePattern":{"type":"string"}}}}` +
		`]}`)

	out, reverseMap := remapOAuthToolNames(body)

	// Forward: TitleCase `Bash` is not a forward-map key, must pass through.
	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "Bash" {
		t.Fatalf("tools.0.name = %q, want %q (TitleCase tool must not be renamed)", got, "Bash")
	}
	// Forward: `glob` is a forward-map key, upstream sees `Glob`.
	if got := gjson.GetBytes(out, "tools.1.name").String(); got != "Glob" {
		t.Fatalf("tools.1.name = %q, want %q", got, "Glob")
	}

	// Reverse map records ONLY the rename that happened.
	if len(reverseMap) != 1 || reverseMap["Glob"] != "glob" {
		t.Fatalf("reverseMap = %v, want {Glob:glob}", reverseMap)
	}

	// Upstream responds with a `Bash` tool_use. Since we never renamed `Bash`,
	// reverseRemap MUST leave it alone.
	bashResp := []byte(`{"content":[{"type":"tool_use","id":"toolu_01","name":"Bash","input":{"cmd":"ls"}}]}`)
	reversed := reverseRemapOAuthToolNames(bashResp, reverseMap)
	if got := gjson.GetBytes(reversed, "content.0.name").String(); got != "Bash" {
		t.Fatalf("content.0.name = %q, want %q (Bash must be preserved; was never forward-renamed)", got, "Bash")
	}

	// Upstream responds with a `Glob` tool_use. Since we renamed `glob`→`Glob`,
	// reverseRemap MUST restore the original `glob`.
	globResp := []byte(`{"content":[{"type":"tool_use","id":"toolu_02","name":"Glob","input":{"filePattern":"**/*.go"}}]}`)
	reversed = reverseRemapOAuthToolNames(globResp, reverseMap)
	if got := gjson.GetBytes(reversed, "content.0.name").String(); got != "glob" {
		t.Fatalf("content.0.name = %q, want %q (Glob must be restored to client's original `glob`)", got, "glob")
	}
}

// TestReverseRemapOAuthToolNamesFromStreamLine_HonorsPerRequestMap guards the
// SSE streaming code path against the same mixed-case bug.
func TestReverseRemapOAuthToolNamesFromStreamLine_HonorsPerRequestMap(t *testing.T) {
	reverseMap := map[string]string{"Glob": "glob"}

	// Bash block was never renamed, must pass through as-is.
	bashLine := []byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_01","name":"Bash","input":{}}}`)
	out := reverseRemapOAuthToolNamesFromStreamLine(bashLine, reverseMap)
	if !bytes.Contains(out, []byte(`"name":"Bash"`)) {
		t.Fatalf("Bash should be preserved, got: %s", string(out))
	}
	if bytes.Contains(out, []byte(`"name":"bash"`)) {
		t.Fatalf("Bash must not be lowercased, got: %s", string(out))
	}

	// Glob block IS in the reverseMap, must be restored to `glob`.
	globLine := []byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_02","name":"Glob","input":{}}}`)
	out = reverseRemapOAuthToolNamesFromStreamLine(globLine, reverseMap)
	if !bytes.Contains(out, []byte(`"name":"glob"`)) {
		t.Fatalf("Glob should be restored to glob, got: %s", string(out))
	}
}

func TestRemapOAuthToolNamesLargePreservesOrderAndUnknownFields(t *testing.T) {
	for _, messageCount := range []int{16, 64, 256, 1024} {
		t.Run(strconv.Itoa(messageCount), func(t *testing.T) {
			input := buildLargeClaudeOAuthToolNamePayload(messageCount)
			original := bytes.Clone(input)

			out, reverseMap := remapOAuthToolNames(input)

			if !bytes.Equal(input, original) {
				t.Fatal("remapOAuthToolNames mutated its input")
			}
			for upstream, client := range map[string]string{"Bash": "bash", "Glob": "glob", "Grep": "grep"} {
				if got := reverseMap[upstream]; got != client {
					t.Fatalf("reverseMap[%q] = %q, want %q", upstream, got, client)
				}
			}
			tools := gjson.GetBytes(out, "tools").Array()
			if len(tools) != messageCount+1 {
				t.Fatalf("tools length = %d, want %d", len(tools), messageCount+1)
			}
			if got := tools[messageCount].Get("name").String(); got != "web_search" {
				t.Fatalf("built-in tool name = %q, want web_search", got)
			}
			messages := gjson.GetBytes(out, "messages").Array()
			if len(messages) != messageCount {
				t.Fatalf("messages length = %d, want %d", len(messages), messageCount)
			}
			for _, index := range []int{0, messageCount / 2, messageCount - 1} {
				if got := tools[index].Get("name").String(); got != "Bash" {
					t.Fatalf("tools[%d].name = %q, want Bash", index, got)
				}
				message := messages[index]
				for path, want := range map[string]string{
					"content.0.name":                "Bash",
					"content.1.tool_name":           "Glob",
					"content.2.content.0.tool_name": "Grep",
					"content.2.content.1.text":      "keep",
				} {
					if got := message.Get(path).String(); got != want {
						t.Fatalf("messages[%d].%s = %q, want %q", index, path, got, want)
					}
				}
				if got := message.Get("marker").Int(); got != int64(index) || !message.Get("unknown.keep").Bool() {
					t.Fatalf("messages[%d] lost marker or unknown field: %s", index, message.Raw)
				}
			}
			outText := string(out)
			if before, toolsKey, choice, messagesKey, after := strings.Index(outText, `"before"`), strings.Index(outText, `"tools"`), strings.Index(outText, `"tool_choice"`), strings.Index(outText, `"messages"`), strings.Index(outText, `"after"`); !(before < toolsKey && toolsKey < choice && choice < messagesKey && messagesKey < after) {
				t.Fatalf("root field order changed")
			}

			response := buildLargeClaudeOAuthToolNameResponse(messageCount)
			restored := reverseRemapOAuthToolNames(response, reverseMap)
			content := gjson.GetBytes(restored, "content").Array()
			if len(content) != messageCount*2 {
				t.Fatalf("response content length = %d, want %d", len(content), messageCount*2)
			}
			for _, index := range []int{0, messageCount / 2, messageCount - 1} {
				base := index * 2
				if got := content[base].Get("name").String(); got != "bash" {
					t.Fatalf("content[%d].name = %q, want bash", base, got)
				}
				if got := content[base+1].Get("tool_name").String(); got != "glob" {
					t.Fatalf("content[%d].tool_name = %q, want glob", base+1, got)
				}
				if !content[base].Get("unknown.keep").Bool() || !content[base+1].Get("unknown.keep").Bool() {
					t.Fatalf("response content near %d lost unknown fields", base)
				}
			}
		})
	}
}

func buildLargeClaudeOAuthToolNamePayload(messageCount int) []byte {
	var payload strings.Builder
	payload.Grow(messageCount * 520)
	payload.WriteString(`{"before":1,"tools":[`)
	for index := 0; index < messageCount; index++ {
		if index > 0 {
			payload.WriteByte(',')
		}
		payload.WriteString(`{"name":"bash","marker":`)
		payload.WriteString(strconv.Itoa(index))
		payload.WriteString(`,"input_schema":{"type":"object"},"unknown":{"keep":true}}`)
	}
	payload.WriteString(`,{"type":"web_search_20250305","name":"web_search","unknown":{"keep":true}}],"tool_choice":{"type":"tool","name":"bash","unknown":{"keep":true}},"messages":[`)
	for index := 0; index < messageCount; index++ {
		if index > 0 {
			payload.WriteByte(',')
		}
		marker := strconv.Itoa(index)
		payload.WriteString(`{"role":"assistant","marker":`)
		payload.WriteString(marker)
		payload.WriteString(`,"content":[{"type":"tool_use","name":"bash","marker":`)
		payload.WriteString(marker)
		payload.WriteString(`,"unknown":{"keep":true}},{"type":"tool_reference","tool_name":"glob","marker":`)
		payload.WriteString(marker)
		payload.WriteString(`,"unknown":{"keep":true}},{"type":"tool_result","tool_use_id":"call_`)
		payload.WriteString(marker)
		payload.WriteString(`","marker":`)
		payload.WriteString(marker)
		payload.WriteString(`,"content":[{"type":"tool_reference","tool_name":"grep","marker":`)
		payload.WriteString(marker)
		payload.WriteString(`,"unknown":{"keep":true}},{"type":"text","text":"keep"}],"unknown":{"keep":true}}],"unknown":{"keep":true}}`)
	}
	payload.WriteString(`],"after":2}`)
	return []byte(payload.String())
}

func buildLargeClaudeOAuthToolNameResponse(messageCount int) []byte {
	var payload strings.Builder
	payload.Grow(messageCount * 180)
	payload.WriteString(`{"before":1,"content":[`)
	for index := 0; index < messageCount; index++ {
		if index > 0 {
			payload.WriteByte(',')
		}
		marker := strconv.Itoa(index)
		payload.WriteString(`{"type":"tool_use","name":"Bash","marker":`)
		payload.WriteString(marker)
		payload.WriteString(`,"unknown":{"keep":true}},{"type":"tool_reference","tool_name":"Glob","marker":`)
		payload.WriteString(marker)
		payload.WriteString(`,"unknown":{"keep":true}}`)
	}
	payload.WriteString(`],"after":2}`)
	return []byte(payload.String())
}

func TestNormalizeClaudeEmptyToolResultsLargePreservesOrderAndInput(t *testing.T) {
	const parts = 1024
	input := buildLargeClaudeEmptyToolResultsPayload(parts)
	original := append([]byte(nil), input...)

	out, repairs, err := normalizeClaudeEmptyToolResults(input)
	if err != nil {
		t.Fatalf("normalizeClaudeEmptyToolResults() error = %v", err)
	}
	if repairs != parts {
		t.Fatalf("repairs = %d, want %d", repairs, parts)
	}
	if !bytes.Equal(input, original) {
		t.Fatal("normalizeClaudeEmptyToolResults mutated its input")
	}
	content := gjson.GetBytes(out, "messages.0.content").Array()
	if len(content) != parts {
		t.Fatalf("content length = %d, want %d", len(content), parts)
	}
	for _, index := range []int{0, parts / 2, parts - 1} {
		if got := content[index].Get("marker").Int(); got != int64(index) {
			t.Fatalf("content[%d].marker = %d, want %d", index, got, index)
		}
		if !content[index].Get("unknown.keep").Bool() {
			t.Fatalf("content[%d] lost unknown field: %s", index, content[index].Raw)
		}
	}
	outText := string(out)
	if before, messages, after := strings.Index(outText, `"before"`), strings.Index(outText, `"messages"`), strings.Index(outText, `"after"`); !(before < messages && messages < after) {
		t.Fatalf("root field order changed: %s", out)
	}
	first := content[0].Raw
	if marker, toolContent, unknown := strings.Index(first, `"marker"`), strings.Index(first, `"content"`), strings.Index(first, `"unknown"`); !(marker < toolContent && toolContent < unknown) {
		t.Fatalf("content field order changed: %s", first)
	}
}

func BenchmarkPayloadGrowthClaudeEmptyToolResults(b *testing.B) {
	for _, parts := range []int{128, 512, 2048} {
		b.Run(strconv.Itoa(parts), func(b *testing.B) {
			input := buildLargeClaudeEmptyToolResultsPayload(parts)
			b.ReportAllocs()
			b.SetBytes(int64(len(input)))
			for range b.N {
				_, repairs, err := normalizeClaudeEmptyToolResults(input)
				if err != nil || repairs != parts {
					b.Fatalf("repairs = %d, err = %v", repairs, err)
				}
			}
		})
	}
}

func buildLargeClaudeEmptyToolResultsPayload(parts int) []byte {
	var payload strings.Builder
	payload.Grow(parts * 120)
	payload.WriteString(`{"before":1,"messages":[{"role":"user","content":[`)
	for index := 0; index < parts; index++ {
		if index > 0 {
			payload.WriteByte(',')
		}
		payload.WriteString(`{"type":"tool_result","tool_use_id":"call_`)
		payload.WriteString(strconv.Itoa(index))
		payload.WriteString(`","marker":`)
		payload.WriteString(strconv.Itoa(index))
		payload.WriteString(`,"content":[],"unknown":{"keep":true}}`)
	}
	payload.WriteString(`]}],"after":2}`)
	return []byte(payload.String())
}

func TestEnforceCacheControlLimitLargePreservesPriorityAndInput(t *testing.T) {
	const (
		systemBlocks  = 400
		toolBlocks    = 300
		messageBlocks = 324
	)
	input := buildLargeClaudeCacheControlPayload(systemBlocks, toolBlocks, messageBlocks)
	original := append([]byte(nil), input...)

	out := enforceCacheControlLimit(input, 4)
	if !bytes.Equal(input, original) {
		t.Fatal("enforceCacheControlLimit mutated its input")
	}
	if got := countCacheControls(out); got != 4 {
		t.Fatalf("cache controls = %d, want 4", got)
	}
	if gjson.GetBytes(out, "system.0.cache_control").Exists() || !gjson.GetBytes(out, "system.399.cache_control").Exists() {
		t.Fatalf("system deletion priority changed: %s", gjson.GetBytes(out, "system").Raw)
	}
	if gjson.GetBytes(out, "tools.0.cache_control").Exists() || !gjson.GetBytes(out, "tools.299.cache_control").Exists() {
		t.Fatalf("tool deletion priority changed: %s", gjson.GetBytes(out, "tools").Raw)
	}
	if gjson.GetBytes(out, "messages.0.content.321.cache_control").Exists() || !gjson.GetBytes(out, "messages.0.content.322.cache_control").Exists() {
		t.Fatalf("message deletion priority changed near cutoff: %s", gjson.GetBytes(out, "messages.0.content").Raw)
	}
	if got := gjson.GetBytes(out, "messages.0.content.323.unknown.marker").Int(); got != 323 {
		t.Fatalf("unknown field marker = %d, want 323", got)
	}
	outText := string(out)
	if before, tools, system, messages, after := strings.Index(outText, `"before"`), strings.Index(outText, `"tools"`), strings.Index(outText, `"system"`), strings.Index(outText, `"messages"`), strings.Index(outText, `"after"`); !(before < tools && tools < system && system < messages && messages < after) {
		t.Fatalf("root field order changed")
	}
}

func BenchmarkPayloadGrowthClaudeCacheControlLimit1024(b *testing.B) {
	input := buildLargeClaudeCacheControlPayload(400, 300, 324)
	b.ReportAllocs()
	b.SetBytes(int64(len(input)))
	var out []byte
	for range b.N {
		out = enforceCacheControlLimit(input, 4)
	}
	if got := countCacheControls(out); got != 4 {
		b.Fatalf("cache controls = %d, want 4", got)
	}
}

func buildLargeClaudeCacheControlPayload(systemBlocks, toolBlocks, messageBlocks int) []byte {
	var payload strings.Builder
	payload.Grow((systemBlocks + toolBlocks + messageBlocks) * 110)
	payload.WriteString(`{"before":1,"tools":[`)
	for index := 0; index < toolBlocks; index++ {
		if index > 0 {
			payload.WriteByte(',')
		}
		payload.WriteString(`{"name":"tool_`)
		payload.WriteString(strconv.Itoa(index))
		payload.WriteString(`","unknown":{"marker":`)
		payload.WriteString(strconv.Itoa(index))
		payload.WriteString(`},"cache_control":{"type":"ephemeral","ttl":"1h"}}`)
	}
	payload.WriteString(`],"system":[`)
	for index := 0; index < systemBlocks; index++ {
		if index > 0 {
			payload.WriteByte(',')
		}
		payload.WriteString(`{"type":"text","text":"system","unknown":{"marker":`)
		payload.WriteString(strconv.Itoa(index))
		payload.WriteString(`},"cache_control":{"type":"ephemeral","ttl":"1h"}}`)
	}
	payload.WriteString(`],"messages":[{"role":"user","content":[`)
	for index := 0; index < messageBlocks; index++ {
		if index > 0 {
			payload.WriteByte(',')
		}
		payload.WriteString(`{"type":"text","text":"message","unknown":{"marker":`)
		payload.WriteString(strconv.Itoa(index))
		payload.WriteString(`},"cache_control":{"type":"ephemeral","ttl":"1h"}}`)
	}
	payload.WriteString(`]}],"after":2}`)
	return []byte(payload.String())
}
