package executor

import (
	"context"
	"fmt"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	claudechat "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/claude/openai/chat-completions"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestPrepareClaudeRequestPreservesScalarToolEnums(t *testing.T) {
	const schema = `{"type":"object","properties":{"n":{"type":"integer","enum":[7]},"flag":{"type":"boolean","enum":[true,false]},"nothing":{"type":"null","enum":[null]},"label":{"type":"string","enum":["7"]}},"required":["n"]}`
	check := func(t *testing.T, stage string, body []byte) {
		t.Helper()
		for _, want := range []struct {
			name string
			typ  string
			enum string
		}{
			{"n", "integer", "[7]"},
			{"flag", "boolean", "[true,false]"},
			{"nothing", "null", "[null]"},
			{"label", "string", `["7"]`},
		} {
			path := "tools.0.input_schema.properties." + want.name
			if got := gjson.GetBytes(body, path+".type").String(); got != want.typ {
				t.Fatalf("%s %s type = %q, want %q: %s", stage, want.name, got, want.typ, body)
			}
			if got := gjson.GetBytes(body, path+".enum").Raw; got != want.enum {
				t.Fatalf("%s %s enum = %s, want %s: %s", stage, want.name, got, want.enum, body)
			}
		}
	}
	auth := &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{
		"api_key": "offline-fixture", "base_url": "https://api.anthropic.com",
	}}
	for _, field := range []string{"parameters", "parametersJsonSchema"} {
		payload := []byte(fmt.Sprintf(`{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"Call fixture"}],"tools":[{"type":"function","function":{"name":"fixture","%s":%s}}]}`, field, schema))
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream-%t", field, stream), func(t *testing.T) {
				check(t, "translator", claudechat.ConvertOpenAIRequestToClaude("claude-sonnet-4-5", payload, stream))
				executor := NewClaudeExecutor(&config.Config{DisableClaudeCloakMode: true})
				plan, err := executor.prepareClaudeRequest(context.Background(), auth, cliproxyexecutor.Request{
					Model: "claude-sonnet-4-5", Payload: payload,
				}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")}, "claude-sonnet-4-5", stream)
				if err != nil {
					t.Fatal(err)
				}
				check(t, "upstream request plan", plan.bodyForUpstream)
			})
		}
	}
	t.Run("native-claude-control", func(t *testing.T) {
		payload := []byte(fmt.Sprintf(`{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"Call fixture"}],"tools":[{"name":"fixture","input_schema":%s}]}`, schema))
		executor := NewClaudeExecutor(&config.Config{DisableClaudeCloakMode: true})
		plan, err := executor.prepareClaudeRequest(context.Background(), auth, cliproxyexecutor.Request{
			Model: "claude-sonnet-4-5", Payload: payload,
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}, "claude-sonnet-4-5", false)
		if err != nil {
			t.Fatal(err)
		}
		check(t, "native upstream request plan", plan.bodyForUpstream)
	})
}
