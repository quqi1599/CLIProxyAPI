package helps

import (
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestKimiNativeAuthEndpointAndCredentialBoundary(t *testing.T) {
	for _, tc := range []struct {
		base, model, key, source, alt string
		want                          bool
	}{
		{"https://api.kimi.com/coding", "k3.1-pro", "key", "openai", "", true},
		{"https://api.kimi.ai/coding/v1/", "k3.1-pro-highspeed[1m](low)", "key", "openai-response", "", true},
		{"https://gateway.test/coding", "k3.1-pro", "key", "openai", "", false},
		{"https://api.moonshot.cn/v1", "k3.1-pro", "key", "openai", "", false},
		{"http://api.kimi.com/coding", "k3.1-pro", "key", "openai", "", false},
		{"https://api.kimi.com/coding/other", "k3.1-pro", "key", "openai", "", false},
		{"https://api.kimi.com/coding?route=other", "k3.1-pro", "key", "openai", "", false},
		{"https://user@api.kimi.com/coding", "k3.1-pro", "key", "openai", "", false},
		{"https://api.kimi.com/coding", "k3.1-pro", "", "openai", "", false},
		{"https://api.kimi.com/coding", "k3.1-pro", "key", "claude", "", false},
		{"https://api.kimi.com/coding", "k3.1-pro", "key", "openai-response", "responses/compact", false},
		{"https://api.kimi.com/coding", "kimi-k3", "key", "openai", "", false},
		{"https://api.kimi.com/coding", "kimi-for-coding-highspeed", "key", "openai", "", false},
		{"https://api.kimi.com/coding", "k3.1-pro-unknown", "key", "openai", "", false},
	} {
		t.Run(tc.base+"/"+tc.model+"/"+tc.source+"/"+tc.alt+"/"+tc.key, func(t *testing.T) {
			auth := &cliproxyauth.Auth{ID: "same-id", Provider: "claude", ProxyURL: "http://proxy.test:8080", Attributes: map[string]string{"base_url": tc.base, "api_key": tc.key, "header:X-Test": "value"}, Metadata: map[string]any{"access_token": "oauth-token"}}
			adapted := KimiCodeNativeOpenAIAuth(auth, cliproxyexecutor.Request{Model: tc.model}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(tc.source), Alt: tc.alt})
			if (adapted != nil) != tc.want {
				t.Fatalf("native adaptation=%t want=%t", adapted != nil, tc.want)
			}
			if adapted == nil {
				return
			}
			if adapted == auth || adapted.ID != auth.ID || adapted.Provider != auth.Provider || adapted.ProxyURL != auth.ProxyURL || adapted.Attributes["api_key"] != tc.key || adapted.Attributes["header:X-Test"] != "value" {
				t.Fatal("credential identity, proxy or configured headers changed")
			}
			if auth.Attributes["base_url"] != tc.base || auth.Attributes["compat_kind"] != "" {
				t.Fatal("stored credential mutated")
			}
		})
	}
}

func TestNormalizeKimiChatSamplingBoundaries(t *testing.T) {
	const official = "https://api.kimi.ai/coding/v1"
	const raw = `{"temperature":0,"top_p":1,"presence_penalty":-0.5,"frequency_penalty":0.5,"reasoning_effort":"high","n":2,"messages":[{"role":"user","content":"temperature top_p"}],"tools":[{"const":9007199254740993}]}`
	const adapted = `{"reasoning_effort":"high","n":2,"messages":[{"role":"user","content":"temperature top_p"}],"tools":[{"const":9007199254740993}]}`
	for _, tc := range []struct{ model, base, input, want string }{
		{"k3.1-pro[1m]", official, raw, adapted},
		{"k3.1-pro-highspeed", official, raw, adapted},
		{"k3.1-pro", "https://other.invalid/coding/v1", raw, raw},
		{"k3.1-pro", "https://api.kimi.ai/v1", raw, raw},
		{"kimi-for-coding", official, raw, raw},
		{"k3.1-pro", official, `{"temperature":2.1,"top_p":-0.1}`, `{"temperature":2.1,"top_p":-0.1}`},
		{"k3.1-pro", official, `{"presence_penalty":-2.1,"frequency_penalty":2.1}`, `{"presence_penalty":-2.1,"frequency_penalty":2.1}`},
		{"k3.1-pro", official, `{"presence_penalty":"0.5","frequency_penalty":true}`, `{"presence_penalty":"0.5","frequency_penalty":true}`},
		{"k3.1-pro", official, `{"temperature":"0","top_p":true}`, `{"temperature":"0","top_p":true}`},
		{"k3.1-pro", official, `{"temperature":1,"top_p":1.1}`, `{"top_p":1.1}`},
	} {
		t.Run(tc.model+"/"+tc.base+"/"+tc.input, func(t *testing.T) {
			got, err := NormalizeKimiChatSampling([]byte(tc.input), tc.model, tc.base)
			if err != nil || string(got) != tc.want {
				t.Fatalf("got %s, err %v; want %s", got, err, tc.want)
			}
		})
	}
}

func TestKimiVideoNativeAuthScope(t *testing.T) {
	const media = `{"messages":[{"role":"user","content":[{"type":"video_url","video_url":{"url":"ms://file"}}]}]}`
	const text = `{"messages":[{"role":"user","content":"video_url"}]}`
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "synthetic", "base_url": "https://api.kimi.com/coding"}}
	for _, tc := range []struct {
		model, body string
		want        bool
	}{
		{"k3[1m](high)", media, true},
		{"k3", text, false},
		{"k3-256k", media, false},
		{"kimi-for-coding", media, false},
	} {
		got := KimiCodeNativeOpenAIAuth(auth, cliproxyexecutor.Request{Model: tc.model, Payload: []byte(tc.body)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
		if (got != nil) != tc.want {
			t.Fatalf("%s adaptation %v", tc.model, got != nil)
		}
	}
}
