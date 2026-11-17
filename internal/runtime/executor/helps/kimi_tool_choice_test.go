package helps

import (
	"testing"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	"github.com/tidwall/gjson"
)

func TestKimiCodeNamedToolChoicePreservesConstraint(t *testing.T) {
	for _, tc := range []struct{ format, tools, choice, path, want string }{
		{"openai", `[{"type":"function","function":{"name":"lookup","parameters":{"const":9007199254740993}}},{"type":"function","function":{"name":"delete"}}]`, `{"type":"function","function":{"name":"lookup"}}`, "tool_choice", "required"},
		{"openai-response", `[{"type":"function","name":"lookup","parameters":{"const":9007199254740993}},{"type":"function","name":"delete"}]`, `{"type":"function","name":"lookup"}`, "tool_choice", "required"},
		{"claude", `[{"name":"lookup","input_schema":{"const":9007199254740993}},{"name":"delete"}]`, `{"type":"tool","name":"lookup","disable_parallel_tool_use":true}`, "tool_choice.type", "any"},
	} {
		for _, model := range []string{"k3.1-pro", "k3.1-pro-highspeed", "k3.1-pro[1m]", "k3.1-pro-highspeed[1m]"} {
			t.Run(tc.format+"/"+model, func(t *testing.T) {
				body := []byte(`{"tools":` + tc.tools + `,"tool_choice":` + tc.choice + `,"messages":[{"role":"assistant","reasoning_content":"original","tool_calls":[{"id":"old","function":{"name":"delete","arguments":"{}"}}]}],"input":[{"type":"reasoning","encrypted_content":"opaque"}],"reasoning":{"effort":"high"}}`)
				out, err := NormalizeKimiCodeToolChoice(body, model, tc.format)
				if err != nil {
					t.Fatal(err)
				}
				if gjson.GetBytes(out, tc.path).String() != tc.want || gjson.GetBytes(out, "tools.#").Int() != 1 || gjson.GetBytes(out, "tools.0").Raw != gjson.GetBytes(body, "tools.0").Raw {
					t.Fatalf("lost tool constraint/schema: %s", out)
				}
				for _, path := range []string{"messages", "input", "reasoning"} {
					if gjson.GetBytes(out, path).Raw != gjson.GetBytes(body, path).Raw {
						t.Fatalf("changed %s", path)
					}
				}
				if tc.format == "claude" && (!gjson.GetBytes(out, "tool_choice.disable_parallel_tool_use").Bool() || gjson.GetBytes(out, "tool_choice.name").Exists()) {
					t.Fatalf("invalid Messages choice: %s", out)
				}
				again, err := NormalizeKimiCodeToolChoice(out, model, tc.format)
				if err != nil || string(again) != string(out) {
					t.Fatalf("not idempotent: %s %v", again, err)
				}
			})
		}
	}
}

func TestKimiCodeNamedToolChoiceRejectsAmbiguity(t *testing.T) {
	for _, body := range []string{
		`{"tool_choice":{"type":"function","function":{"name":"lookup"}}}`,
		`{"tool_choice":{"type":"function","function":{"name":""}},"tools":[{"type":"function","function":{"name":"lookup"}}]}`,
		`{"tool_choice":{"type":"function","function":{"name":"lookup"}},"tools":[{"type":"function","function":{"name":"other"}}]}`,
		`{"tool_choice":{"type":"function","function":{"name":"lookup"}},"tools":[{"type":"function","function":{"name":"lookup"}},{"type":"function","function":{"name":"lookup"}}]}`,
		`{"tool_choice":{"type":"function","function":{"name":"lookup"}},"tools":[{"type":"builtin_function","function":{"name":"lookup"}}]}`,
		`{"tool_choice":{"type":"function","function":{"name":"lookup"}},"tools":[{"type":"function","function":{"name":"lookup"}}],"messages":[{"role":"system","tools":[]}]}`,
		`{"tool_choice":{"type":"function","function":{"name":"lookup"}},"tools":[{"type":"function","function":{"name":"lookup"}}],"input":[{"type":"tool_search_output"}]}`,
	} {
		out, err := NormalizeKimiCodeToolChoice([]byte(body), "k3.1-pro", "openai")
		f, ok := err.(*failurecontract.Failure)
		if !ok || f.StatusCode() != 400 || f.Scope != failurecontract.ScopeRequest || f.Retryable || string(out) != body {
			t.Fatalf("expected unchanged request-scoped rejection, got %s %v", out, err)
		}
	}
}

func TestKimiCodeToolChoiceLeavesOtherModesUnchanged(t *testing.T) {
	for _, model := range []string{"k3.1-pro", "k3", "kimi-for-coding", "kimi-k3", "other"} {
		for _, choice := range []string{`"auto"`, `"none"`, `"required"`, `{"type":"auto"}`, `{"type":"any"}`, `{"type":"none"}`} {
			body := []byte(`{"tools":[{"type":"function","function":{"name":"lookup"}}],"tool_choice":` + choice + `}`)
			out, err := NormalizeKimiCodeToolChoice(body, model, "openai")
			if err != nil || string(out) != string(body) {
				t.Fatalf("%s changed: %s %v", model, out, err)
			}
		}
		if model == "k3.1-pro" {
			continue
		}
		body := []byte(`{"tool_choice":{"type":"function","function":{"name":"lookup"}}}`)
		out, err := NormalizeKimiCodeToolChoice(body, model, "openai")
		if err != nil || string(out) != string(body) {
			t.Fatalf("other model changed: %s %v", out, err)
		}
	}
}

func TestKimiCodeEmptyToolsRemoveOrphanChoice(t *testing.T) {
	for _, format := range []string{"openai", "openai-response", "claude"} {
		for _, tools := range []string{"", `,"tools":[]`, `,"tools":null`} {
			for _, choice := range []string{`"required"`, `"auto"`, `"none"`, `{"type":"any"}`, `{"type":"auto"}`, `{"type":"none"}`} {
				body := []byte(`{"tool_choice":` + choice + tools + `,"messages":[{"role":"user","content":"hello"}],"reasoning_effort":"high"}`)
				out, err := NormalizeKimiCodeToolChoice(body, "k3.1-pro", format)
				if err != nil || gjson.GetBytes(out, "tool_choice").Exists() || gjson.GetBytes(out, "reasoning_effort").String() != "high" || gjson.GetBytes(out, "messages").Raw != gjson.GetBytes(body, "messages").Raw {
					t.Fatalf("orphan choice: %s %v", out, err)
				}
			}
		}
	}
	body := []byte(`{"tool_choice":"required","messages":[{"role":"system","tools":[{"type":"function","function":{"name":"lookup"}}]}]}`)
	out, err := NormalizeKimiCodeToolChoice(body, "k3.1-pro", "openai")
	if err != nil || string(out) != string(body) {
		t.Fatalf("dynamic-only tools lost: %s %v", out, err)
	}
}
