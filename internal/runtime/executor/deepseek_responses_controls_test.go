package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/compat"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/tidwall/gjson"
)

func TestDeepSeekResponsesForcedToolsPreserveSelectionAndDisableThinking(t *testing.T) {
	for _, model := range []string{"deepseek-flash", "deepseek-v4.1-flash", "deepseek-v4-pro"} {
		for _, control := range []string{``, `,"reasoning":{"effort":"none"}`, `,"reasoning":{"effort":"high"}`, `,"thinking":{"type":"disabled"}`} {
			for _, choice := range []string{`"required"`, `{"type":"function","name":"echo"}`} {
				t.Run(model+"/"+control+"/"+choice, func(t *testing.T) {
					body := []byte(fmt.Sprintf(`{"model":%q,"input":"test","tools":[{"type":"function","name":"echo","parameters":{"type":"object"}}],"tool_choice":%s%s}`, model, choice, control))
					e := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
					plan, err := e.prepareOpenAICompatRequest(context.Background(), nil, cliproxyexecutor.Request{Model: model, Payload: body}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}, "https://api.deepseek.com/v1", model, openAICompatProfileForKind("deepseek"), false)
					if err != nil {
						t.Fatal(err)
					}
					if gjson.GetBytes(plan.body, "reasoning.effort").String() != "none" || gjson.GetBytes(plan.body, "thinking").Exists() {
						t.Fatalf("invalid wire controls: %s", plan.body)
					}
					var want, got any
					_ = json.Unmarshal([]byte(choice), &want)
					_ = json.Unmarshal([]byte(gjson.GetBytes(plan.body, "tool_choice").Raw), &got)
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("selection changed: %s", plan.body)
					}
				})
			}
		}
	}
}

func TestDeepSeekAutomaticToolChoiceDoesNotDisableThinking(t *testing.T) {
	for _, choice := range []string{`"auto"`, `"none"`} {
		body := []byte(`{"model":"deepseek-flash","input":"hi","reasoning":{"effort":"high"},"tool_choice":` + choice + `,"tools":[{"type":"function","name":"echo","parameters":{"type":"object"}}]}`)
		e := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
		for _, stream := range []bool{false, true} {
			p, err := e.prepareOpenAICompatRequest(context.Background(), nil, cliproxyexecutor.Request{Model: "deepseek-flash", Payload: body}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}, "https://api.deepseek.com", "deepseek-flash", openAICompatProfileForKind("deepseek"), stream)
			if err != nil {
				t.Fatal(err)
			}
			if gjson.GetBytes(p.body, "reasoning.effort").String() != "high" || gjson.GetBytes(p.body, "tool_choice").Raw != choice {
				t.Fatalf("non-forced mode changed: %s", p.body)
			}
		}
	}
}

func TestDeepSeekCompatibilityDiagnosticSafeCategories(t *testing.T) {
	hook := logtest.NewGlobal()
	t.Cleanup(hook.Reset)
	for _, kind := range []string{"deepseek", "doubao"} {
		hook.Reset()
		logOpenAICompatCompatibilityDiagnostic(context.Background(), openAICompatPayloadDiagnostic{Model: "deepseek-flash", CompatKind: kind}, 400, nil, []byte(`{"error":{"code":"SensitiveContentDetected","message":"private customer material"}}`))
		entry := hook.LastEntry()
		if entry == nil || entry.Data["upstream_error_reason"] != "content_policy" {
			t.Fatalf("missing %s diagnostic: %v", kind, entry)
		}
		if strings.Contains(fmt.Sprint(entry.Data), "private customer") || strings.Contains(entry.Message, "private customer") {
			t.Fatal("raw message leaked")
		}
	}
}

func TestDeepSeekClaudeForcedToolKeepsExplicitOff(t *testing.T) {
	for _, source := range []string{"claude", "openai-response"} {
		for _, stream := range []bool{false, true} {
			for _, control := range []string{``, `,"thinking":{"type":"disabled"}`, `,"thinking":{"type":"enabled","budget_tokens":1024}`} {
				t.Run(fmt.Sprintf("%s/%v/%s", source, stream, control), func(t *testing.T) {
					body := []byte(`{"model":"deepseek-flash","max_tokens":4096,"messages":[{"role":"user","content":"echo"}],"tools":[{"name":"echo","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"echo"}` + control + `}`)
					if source == "openai-response" {
						body = []byte(`{"model":"deepseek-flash","input":"echo","tools":[{"type":"function","name":"echo","parameters":{"type":"object"}}],"tool_choice":{"type":"function","name":"echo"}` + control + `}`)
					}
					auth := &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{"base_url": "https://api.deepseek.com/anthropic", "api_key": "test", "compat_kind": "deepseek"}}
					p, err := NewClaudeExecutor(&config.Config{DisableClaudeCloakMode: true}).prepareClaudeRequest(context.Background(), auth, cliproxyexecutor.Request{Model: "deepseek-flash", Payload: body}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(source)}, "deepseek-flash", stream)
					if err != nil {
						t.Fatal(err)
					}
					if gjson.GetBytes(p.bodyForUpstream, "thinking.type").String() != "disabled" || gjson.GetBytes(p.bodyForUpstream, "thinking.budget_tokens").Exists() {
						t.Fatalf("DeepSeek default-on restored: %s", p.bodyForUpstream)
					}
					if gjson.GetBytes(p.bodyForUpstream, "tool_choice.type").String() != "tool" || gjson.GetBytes(p.bodyForUpstream, "tool_choice.name").String() != "echo" {
						t.Fatalf("tool selection changed: %s", p.bodyForUpstream)
					}
				})
			}
		}
	}
}

func TestDeepSeekResponsesForcedToolPostConfig(t *testing.T) {
	for _, endpoint := range []string{"responses", "compact"} {
		body := []byte(`{"model":"deepseek-flash","input":"echo","reasoning":{"effort":"high"},"tool_choice":"required","tools":[{"type":"function","name":"echo","parameters":{"type":"object"}}]}`)
		out := scrubOpenAICompatPostConfigPayload(body, openAICompatProfileForKind("deepseek"), "deepseek-flash", "https://api.deepseek.com", compat.EndpointKind(endpoint))
		if gjson.GetBytes(out, "reasoning.effort").String() != "none" || gjson.GetBytes(out, "thinking").Exists() || gjson.GetBytes(out, "tool_choice").String() != "required" {
			t.Fatalf("endpoint=%s body=%s", endpoint, out)
		}
		if !slices.Contains(openAICompatDeepSeekPolicyDowngrades(body, out), openAICompatDeepSeekToolChoiceDowngrade) {
			t.Fatal("forced choice downgrade not recorded")
		}
	}
}
