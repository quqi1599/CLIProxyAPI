package helps

import (
	"net/http"

	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// KimiCodeRequiresThinking identifies exact Coding Plan models that cannot turn
// thinking off. Other model families retain their existing tool-choice policy.
func KimiCodeRequiresThinking(model string) bool {
	info := registry.KimiCodeModelInfo(thinking.ParseSuffix(model).ModelName)
	return info != nil && info.Thinking != nil && !info.Thinking.ZeroAllowed
}

// NormalizeKimiCodeToolChoice preserves a named function constraint using one
// callable definition plus required/any. Kimi rejects named choices with thinking
// enabled, but accepts required tool use. Never broaden the choice to other tools.
func NormalizeKimiCodeToolChoice(body []byte, model, format string) ([]byte, error) {
	if !KimiCodeRequiresThinking(model) {
		return body, nil
	}
	choice := gjson.GetBytes(body, "tool_choice")
	choiceType, namePath, toolNamePath := "function", "function.name", "function.name"
	switch format {
	case "claude":
		choiceType, namePath, toolNamePath = "tool", "name", "name"
	case "codex", "openai-response":
		namePath, toolNamePath = "name", "name"
	case "openai", "kimi":
	default:
		return body, nil
	}
	tools := gjson.GetBytes(body, "tools")
	mode := choice.String()
	if choice.IsObject() {
		mode = choice.Get("type").String()
	}
	if (!tools.Exists() || tools.Type == gjson.Null || (tools.IsArray() && len(tools.Array()) == 0)) && !kimiHasDynamicToolDefinitions(body) {
		switch mode {
		case "auto", "none", "required", "any":
			// Some clients retain a forced mode after clearing their tool list.
			// With no available tools, omit the orphan control and allow text.
			return sjson.DeleteBytes(body, "tool_choice")
		}
	}
	if !choice.IsObject() || choice.Get("type").String() != choiceType {
		return body, nil
	}
	name := choice.Get(namePath)
	if name.Type != gjson.String || name.Str == "" {
		return body, kimiToolChoiceFailure()
	}
	// Dynamic definitions in history could leave other tools callable after
	// restricting the root list. Keep that history intact and reject locally.
	if kimiHasDynamicToolDefinitions(body) {
		return body, kimiToolChoiceFailure()
	}
	var selected gjson.Result
	matches := 0
	for _, tool := range tools.Array() {
		if tool.Get(toolNamePath).String() != name.Str {
			continue
		}
		matches++
		kind := tool.Get("type").String()
		if (format == "claude" && (kind == "" || kind == "custom")) || (format != "claude" && kind == "function") {
			selected = tool
		}
	}
	if matches != 1 || !selected.Exists() {
		return body, kimiToolChoiceFailure()
	}
	out, err := sjson.SetRawBytes(body, "tools", []byte("["+selected.Raw+"]"))
	if err != nil {
		return body, err
	}
	if format == "claude" {
		out, err = sjson.SetBytes(out, "tool_choice.type", "any")
		if err == nil {
			out, err = sjson.DeleteBytes(out, "tool_choice.name")
		}
	} else {
		out, err = sjson.SetBytes(out, "tool_choice", "required")
	}
	return out, err
}

func kimiHasDynamicToolDefinitions(body []byte) bool {
	for _, message := range gjson.GetBytes(body, "messages").Array() {
		if message.Get("tools").Exists() {
			return true
		}
	}
	for _, item := range gjson.GetBytes(body, "input").Array() {
		if item.Get("tools").Exists() || item.Get("type").String() == "tool_search_output" {
			return true
		}
	}
	return false
}

func kimiToolChoiceFailure() *failurecontract.Failure {
	return &failurecontract.Failure{
		Kind: failurecontract.InvalidRequest, Scope: failurecontract.ScopeRequest,
		HTTPStatus: http.StatusBadRequest, OuterStatus: http.StatusBadRequest,
		ProviderCode: "kimi_tool_choice_invalid", SemanticCode: "kimi_tool_choice_invalid",
		SemanticType: "invalid_request_error", StreamPhase: failurecontract.StreamPhaseBeforeOutput,
		Retryable:     false,
		PublicMessage: "Kimi named tool_choice requires exactly one matching function in root tools and no dynamic tool definitions in history",
	}
}
