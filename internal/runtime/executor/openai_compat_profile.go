package executor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const openAICompatAccountQuotaRetryWait = 24 * time.Hour
const openAICompatEmptyUpstreamResponseCode = "empty_upstream_response"
const deepSeekThinkingBudgetMin = 100
const deepSeekThinkingBudgetMax = 32768
const xiaomiMimo25MaxTokens = 131072
const doubaoSeed20MaxTemperature = 1.5
const doubaoSeed20MaxCompletionTokens = 98304
const kimiThinkingTemperature = 1.0
const kimiInstantTemperature = 0.6
const kimiTopP = 0.95

type openAICompatProfile struct {
	Kind                     string
	SupportsResponses        bool
	SupportsNativeResponses  bool
	SupportsStreamUsage      bool
	SupportsParallelToolCall bool
	SupportsReasoning        bool
	SupportsNativeThinking   bool
	PreserveReasoningContent bool
	NormalizeToolHistory     bool
	SupportsMetadata         bool
	SupportsStore            bool
	SystemMessagesAsUser     bool
	DefaultHeaders           map[string]string
}

func genericOpenAICompatProfile() openAICompatProfile {
	return openAICompatProfile{
		SupportsResponses:        true,
		SupportsStreamUsage:      true,
		SupportsParallelToolCall: true,
		SupportsReasoning:        true,
		SupportsMetadata:         true,
		SupportsStore:            true,
	}
}

var openAICompatProfiles = map[string]openAICompatProfile{
	"kimi": {
		Kind:                     "kimi",
		SupportsResponses:        false,
		SupportsStreamUsage:      true,
		SupportsParallelToolCall: false,
		SupportsReasoning:        false,
		PreserveReasoningContent: true,
		NormalizeToolHistory:     true,
		SupportsMetadata:         false,
		SupportsStore:            false,
	},
	"minimax": {
		Kind:                     "minimax",
		SupportsResponses:        false,
		SupportsStreamUsage:      false,
		SupportsParallelToolCall: false,
		SupportsReasoning:        false,
		SupportsMetadata:         false,
		SupportsStore:            false,
		SystemMessagesAsUser:     true,
	},
	"xiaomi": {
		Kind:                     "xiaomi",
		SupportsResponses:        false,
		SupportsStreamUsage:      false,
		SupportsParallelToolCall: false,
		SupportsReasoning:        false,
		SupportsNativeThinking:   true,
		PreserveReasoningContent: true,
		NormalizeToolHistory:     true,
		SupportsMetadata:         false,
		SupportsStore:            false,
	},
	"zhipu": {
		Kind:                     "zhipu",
		SupportsResponses:        false,
		SupportsStreamUsage:      false,
		SupportsParallelToolCall: false,
		SupportsReasoning:        false,
		SupportsMetadata:         false,
		SupportsStore:            false,
	},
	"doubao": {
		Kind:                     "doubao",
		SupportsResponses:        true,
		SupportsNativeResponses:  true,
		SupportsStreamUsage:      false,
		SupportsParallelToolCall: false,
		SupportsReasoning:        false,
		SupportsMetadata:         false,
		SupportsStore:            false,
	},
	"xfyun": {
		Kind:                     "xfyun",
		SupportsResponses:        false,
		SupportsStreamUsage:      false,
		SupportsParallelToolCall: false,
		SupportsReasoning:        false,
		SupportsMetadata:         false,
		SupportsStore:            false,
	},
	"maas": {
		Kind:                     "maas",
		SupportsResponses:        false,
		SupportsStreamUsage:      false,
		SupportsParallelToolCall: false,
		SupportsReasoning:        false,
		SupportsMetadata:         false,
		SupportsStore:            false,
	},
	"langengyun": {
		Kind:                     "langengyun",
		SupportsResponses:        false,
		SupportsStreamUsage:      false,
		SupportsParallelToolCall: false,
		SupportsReasoning:        false,
		SupportsMetadata:         false,
		SupportsStore:            false,
	},
	"newapi": {
		Kind:                     "newapi",
		SupportsResponses:        false,
		SupportsStreamUsage:      false,
		SupportsParallelToolCall: false,
		SupportsReasoning:        false,
		SupportsMetadata:         false,
		SupportsStore:            false,
	},
}

func openAICompatProfileForKind(kind string) openAICompatProfile {
	normalized := config.NormalizeOpenAICompatibilityKind(kind)
	if profile, ok := openAICompatProfiles[normalized]; ok {
		return profile
	}
	profile := genericOpenAICompatProfile()
	profile.Kind = normalized
	return profile
}

func (e *OpenAICompatExecutor) resolveProfile(auth *cliproxyauth.Auth) openAICompatProfile {
	profile := genericOpenAICompatProfile()
	profile.Kind = ""
	compat := e.resolveCompatConfig(auth)
	if compat == nil {
		if auth != nil && auth.Attributes != nil {
			if kind := config.NormalizeOpenAICompatibilityKind(auth.Attributes["compat_kind"]); kind != "" {
				return openAICompatProfileForKind(kind)
			}
			if kind := inferOpenAICompatKindFromBaseURL(auth.Attributes["base_url"]); kind != "" {
				return openAICompatProfileForKind(kind)
			}
		}
		return profile
	}
	resolved := openAICompatProfileForKind(compat.Kind)
	if resolved.Kind == "" && auth != nil && auth.Attributes != nil {
		if kind := inferOpenAICompatKindFromBaseURL(auth.Attributes["base_url"]); kind != "" {
			resolved = openAICompatProfileForKind(kind)
		}
	}
	if len(compat.Headers) > 0 {
		resolved.DefaultHeaders = config.NormalizeHeaders(compat.Headers)
	}
	return resolved
}

func inferOpenAICompatKindFromBaseURL(rawBaseURL string) string {
	baseURL := strings.TrimSpace(rawBaseURL)
	if baseURL == "" {
		return ""
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	switch host {
	case "api.moonshot.ai", "api.moonshot.cn", "api.kimi.com":
		return "kimi"
	case "api.minimax.io", "api.minimaxi.io", "api.minimaxi.com":
		return "minimax"
	case "api.z.ai", "open.bigmodel.cn", "maas-api.lanyun.net":
		return "zhipu"
	case "api.deepseek.com":
		return "deepseek"
	case "api.xiaomimimo.com":
		return "xiaomi"
	case "ark.cn-beijing.volces.com":
		return "doubao"
	default:
		if config.IsXiaomiTokenPlanBaseURLHost(host) {
			return "xiaomi"
		}
		return ""
	}
}

func applyOpenAICompatDefaultHeaders(req *http.Request, profile openAICompatProfile) {
	if req == nil || len(profile.DefaultHeaders) == 0 {
		return
	}
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	for key, value := range profile.DefaultHeaders {
		if req.Header.Get(key) != "" {
			continue
		}
		req.Header.Set(key, value)
	}
}

func scrubOpenAICompatPayload(payload []byte, profile openAICompatProfile) []byte {
	if len(payload) == 0 {
		return payload
	}
	if repaired, ok := helps.RepairInvalidJSONStringEscapes(payload); ok {
		payload = repaired
	}
	if !profile.SupportsStore {
		if updated, err := sjson.DeleteBytes(payload, "store"); err == nil {
			payload = updated
		}
	}
	if !profile.SupportsMetadata {
		if updated, err := sjson.DeleteBytes(payload, "metadata"); err == nil {
			payload = updated
		}
	}
	if !profile.SupportsParallelToolCall {
		if updated, err := sjson.DeleteBytes(payload, "parallel_tool_calls"); err == nil {
			payload = updated
		}
	}
	if !profile.SupportsStreamUsage {
		if updated, err := sjson.DeleteBytes(payload, "stream_options"); err == nil {
			payload = updated
		}
	}
	if !profile.SupportsReasoning {
		if !profile.SupportsNativeThinking {
			for _, path := range []string{"reasoning", "reasoning_effort"} {
				if updated, err := sjson.DeleteBytes(payload, path); err == nil {
					payload = updated
				}
			}
		}
		if !profile.PreserveReasoningContent {
			payload = deleteMessageReasoningContent(payload)
		}
	}
	return payload
}

func scrubOpenAICompatPayloadForModel(payload []byte, profile openAICompatProfile, model string, baseURL string) []byte {
	if repaired, ok := helps.RepairInvalidJSONStringEscapes(payload); ok {
		payload = repaired
	}
	compatKind := config.NormalizeOpenAICompatibilityKind(profile.Kind)
	if compatKind == "kimi" {
		payload = normalizeKimiThinkingConfig(payload, model)
	}
	doubaoDeepSeekEffort, doubaoDeepSeekThinkingDisabled := doubaoDeepSeekReasoningIntent(payload, model)
	payload = scrubOpenAICompatPayload(payload, profile)
	if compatKind == "doubao" {
		payload = applyDoubaoDeepSeekReasoningIntent(payload, model, doubaoDeepSeekEffort, doubaoDeepSeekThinkingDisabled)
	}
	if profile.SystemMessagesAsUser {
		payload = rewriteOpenAICompatSystemMessagesAsUser(payload)
	}
	payload = repairOpenAICompatToolCallHistory(payload)
	payload = sanitizeOpenAICompatToolSchemas(payload)
	payload = scrubDeepSeekThinkingBudgetForCompat(payload, model, baseURL, profile.Kind)
	if profile.NormalizeToolHistory {
		if normalized, err := normalizeOpenAICompatToolMessageLinks(payload, "openai compat executor"); err == nil {
			payload = normalized
		} else {
			log.WithError(err).WithField("compat_kind", config.NormalizeOpenAICompatibilityKind(profile.Kind)).Warn("openai compat executor: failed to normalize tool message history")
		}
	}
	payload = scrubOpenAICompatProviderToolPayload(payload, profile)
	payload = scrubOpenAICompatToolChoice(payload, profile)
	if compatKind == "kimi" {
		payload = scrubKimiPayloadForModel(payload, model)
	}
	if compatKind == "xiaomi" {
		payload = scrubXiaomiPayloadForModel(payload, model)
	}
	if compatKind == "zhipu" {
		payload = scrubZhipuImageURLDataURLs(payload)
	}
	if compatKind == "doubao" {
		payload = scrubDoubaoPayloadForModel(payload, model)
	}
	if requiresDeepSeekToolSchemaCompatibility(model) {
		payload = scrubDeepSeekToolPayload(payload, baseURL)
	}
	return payload
}

func scrubKimiPayloadForModel(payload []byte, model string) []byte {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload
	}
	if !requiresKimiK25K26PayloadCompatibility(payload, model) {
		if requiresKimiForCodingPayloadCompatibility(payload, model) {
			payload = normalizeKimiForCodingTemperature(payload)
		}
		return payload
	}
	payload = normalizeKimiThinkingConfig(payload, model)
	hasOfficialWebSearch := kimiPayloadHasOfficialWebSearch(payload)
	if hasOfficialWebSearch && kimiThinkingEnabled(payload) {
		if updated, err := sjson.SetBytes(payload, "thinking.type", "disabled"); err == nil {
			payload = updated
		}
		if updated, err := sjson.DeleteBytes(payload, "thinking.keep"); err == nil {
			payload = updated
		}
	}
	payload = normalizeKimiFixedSamplingParams(payload)
	payload = normalizeKimiToolChoice(payload, kimiThinkingEnabled(payload) || hasOfficialWebSearch)
	payload = normalizeOpenAICompatToolCallArguments(payload)
	return payload
}

func normalizedKimiModelName(model string) string {
	modelName := strings.ToLower(strings.TrimSpace(thinking.ParseSuffix(model).ModelName))
	if slash := strings.LastIndex(modelName, "/"); slash >= 0 {
		modelName = modelName[slash+1:]
	}
	return strings.TrimPrefix(modelName, "kimi-")
}

func requiresKimiK25K26Compatibility(model string) bool {
	modelName := normalizedKimiModelName(model)
	return modelName == "k2.5" ||
		modelName == "k2.6" ||
		strings.HasPrefix(modelName, "k2.5-") ||
		strings.HasPrefix(modelName, "k2.6-")
}

func requiresKimiK25K26PayloadCompatibility(payload []byte, model string) bool {
	if requiresKimiK25K26Compatibility(model) {
		return true
	}
	payloadModel := strings.TrimSpace(gjson.GetBytes(payload, "model").String())
	return payloadModel != "" && requiresKimiK25K26Compatibility(payloadModel)
}

func requiresKimiForCodingCompatibility(model string) bool {
	modelName := normalizedKimiModelName(model)
	return modelName == "for-coding" || strings.HasPrefix(modelName, "for-coding-")
}

func requiresKimiForCodingPayloadCompatibility(payload []byte, model string) bool {
	if requiresKimiForCodingCompatibility(model) {
		return true
	}
	payloadModel := strings.TrimSpace(gjson.GetBytes(payload, "model").String())
	return payloadModel != "" && requiresKimiForCodingCompatibility(payloadModel)
}

func normalizeKimiThinkingConfig(payload []byte, model string) []byte {
	if len(payload) == 0 || !gjson.ValidBytes(payload) || !requiresKimiK25K26PayloadCompatibility(payload, model) {
		return payload
	}
	thinkingType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "thinking.type").String()))
	if thinkingType == "" {
		thinkingType = kimiThinkingTypeFromReasoning(payload)
	}
	switch thinkingType {
	case "none", "off", "false", "disabled", "disable":
		if updated, err := sjson.SetBytes(payload, "thinking.type", "disabled"); err == nil {
			payload = updated
		}
	case "", "low", "medium", "high", "max", "auto", "adaptive", "true", "enabled", "enable":
		if thinkingType != "" {
			if updated, err := sjson.SetBytes(payload, "thinking.type", "enabled"); err == nil {
				payload = updated
			}
		}
	default:
		if updated, err := sjson.SetBytes(payload, "thinking.type", "enabled"); err == nil {
			payload = updated
		}
	}
	for _, path := range []string{
		"reasoning",
		"reasoning_effort",
		"thinking.reasoning_effort",
		"thinking.budget_tokens",
		"thinking_budget",
	} {
		if updated, err := sjson.DeleteBytes(payload, path); err == nil {
			payload = updated
		}
	}
	if !kimiThinkingEnabled(payload) {
		if updated, err := sjson.DeleteBytes(payload, "thinking.keep"); err == nil {
			payload = updated
		}
		return payload
	}
	if kimiPayloadHasReasoningContent(payload) {
		if updated, err := sjson.SetBytes(payload, "thinking.type", "enabled"); err == nil {
			payload = updated
		}
	}
	if updated, err := sjson.DeleteBytes(payload, "thinking.keep"); err == nil {
		payload = updated
	}
	return payload
}

func kimiThinkingTypeFromReasoning(payload []byte) string {
	for _, path := range []string{"reasoning_effort", "reasoning.effort", "thinking.reasoning_effort"} {
		value := gjson.GetBytes(payload, path)
		if !value.Exists() || value.Type != gjson.String {
			continue
		}
		if effort := strings.ToLower(strings.TrimSpace(value.String())); effort != "" {
			return effort
		}
	}
	return ""
}

func kimiThinkingEnabled(payload []byte) bool {
	thinkingType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "thinking.type").String()))
	switch thinkingType {
	case "none", "off", "false", "disabled", "disable":
		return false
	default:
		return true
	}
}

func kimiPayloadHasReasoningContent(payload []byte) bool {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return false
	}
	for _, msg := range messages.Array() {
		if strings.TrimSpace(msg.Get("role").String()) != "assistant" {
			continue
		}
		if strings.TrimSpace(msg.Get("reasoning_content").String()) != "" {
			return true
		}
	}
	return false
}

func normalizeKimiFixedSamplingParams(payload []byte) []byte {
	temperature := kimiThinkingTemperature
	if !kimiThinkingEnabled(payload) {
		temperature = kimiInstantTemperature
	}
	for path, value := range map[string]any{
		"temperature":       temperature,
		"top_p":             kimiTopP,
		"n":                 1,
		"presence_penalty":  0.0,
		"frequency_penalty": 0.0,
	} {
		if !gjson.GetBytes(payload, path).Exists() {
			continue
		}
		if updated, err := sjson.SetBytes(payload, path, value); err == nil {
			payload = updated
		}
	}
	return payload
}

func normalizeKimiForCodingTemperature(payload []byte) []byte {
	if !gjson.GetBytes(payload, "temperature").Exists() {
		return payload
	}
	if updated, err := sjson.SetBytes(payload, "temperature", kimiThinkingTemperature); err == nil {
		payload = updated
	}
	return payload
}

func normalizeKimiToolChoice(payload []byte, enforceAutoNone bool) []byte {
	toolChoice := gjson.GetBytes(payload, "tool_choice")
	if !toolChoice.Exists() {
		return payload
	}
	if !gjson.GetBytes(payload, "tools").IsArray() {
		if updated, err := sjson.DeleteBytes(payload, "tool_choice"); err == nil {
			payload = updated
		}
		return payload
	}
	if !enforceAutoNone {
		return payload
	}
	if value, ok := kimiToolChoiceAutoOrNone(toolChoice); ok {
		if updated, err := sjson.SetBytes(payload, "tool_choice", value); err == nil {
			payload = updated
		}
		return payload
	}
	if updated, err := sjson.SetBytes(payload, "tool_choice", "auto"); err == nil {
		payload = updated
	}
	return payload
}

func kimiToolChoiceAutoOrNone(toolChoice gjson.Result) (string, bool) {
	switch toolChoice.Type {
	case gjson.String:
		value := strings.ToLower(strings.TrimSpace(toolChoice.String()))
		return value, value == "auto" || value == "none"
	case gjson.JSON:
		value := strings.ToLower(strings.TrimSpace(toolChoice.Get("type").String()))
		return value, value == "auto" || value == "none"
	default:
		return "", false
	}
}

func kimiPayloadHasOfficialWebSearch(payload []byte) bool {
	tools := gjson.GetBytes(payload, "tools")
	if !tools.IsArray() {
		return false
	}
	for _, tool := range tools.Array() {
		if kimiToolLooksLikeOfficialWebSearch(tool) {
			return true
		}
	}
	return false
}

func kimiToolLooksLikeOfficialWebSearch(tool gjson.Result) bool {
	toolType := strings.ToLower(strings.TrimSpace(tool.Get("type").String()))
	toolName := strings.ToLower(strings.TrimSpace(tool.Get("name").String()))
	if toolName == "" {
		toolName = strings.ToLower(strings.TrimSpace(tool.Get("function.name").String()))
	}
	if strings.Contains(toolName, "$web_search") || toolName == "_web_search" || strings.Contains(toolType, "web_search") {
		return true
	}
	if toolType != "function" && strings.Contains(toolName, "web_search") {
		return true
	}
	if strings.Contains(toolType, "builtin") && strings.Contains(strings.ToLower(tool.Raw), "web_search") {
		return true
	}
	return false
}

func scrubDoubaoPayloadForModel(payload []byte, model string) []byte {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload
	}
	payload = scrubDoubaoUnsupportedOpenAIFields(payload, model)
	if !requiresDoubaoSeed20Compatibility(model) {
		return payload
	}
	payload = normalizeDoubaoSeed20Temperature(payload)
	payload = normalizeDoubaoSeed20TokenFields(payload)
	payload = normalizeDoubaoChatContentParts(payload)
	payload = normalizeOpenAICompatToolCallArguments(payload)
	return payload
}

func scrubXiaomiPayloadForModel(payload []byte, model string) []byte {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload
	}
	payload = normalizeXiaomiThinkingConfig(payload)
	payload = normalizeXiaomiThinkingHyperparameters(payload)
	payload = normalizeXiaomiMimo25TokenFields(payload, model)
	payload = normalizeOpenAICompatToolCallArguments(payload)
	payload = scrubXiaomiToolSchemas(payload)
	return payload
}

func normalizeXiaomiMimo25TokenFields(payload []byte, model string) []byte {
	if !requiresXiaomiMimo25TokenClamp(model) {
		return payload
	}
	for _, path := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		value := gjson.GetBytes(payload, path)
		if !value.Exists() {
			continue
		}
		tokens, ok := xiaomiTokenLimitValue(value)
		if !ok {
			if updated, err := sjson.DeleteBytes(payload, path); err == nil {
				payload = updated
			}
			continue
		}
		if tokens < 1 {
			tokens = 1
		} else if tokens > xiaomiMimo25MaxTokens {
			tokens = xiaomiMimo25MaxTokens
		}
		if updated, err := sjson.SetBytes(payload, path, tokens); err == nil {
			payload = updated
		}
	}
	return payload
}

func requiresXiaomiMimo25TokenClamp(model string) bool {
	modelName := strings.ToLower(strings.TrimSpace(thinking.ParseSuffix(model).ModelName))
	if slash := strings.LastIndex(modelName, "/"); slash >= 0 {
		modelName = modelName[slash+1:]
	}
	return modelName == "mimo-v2.5-pro" || strings.HasPrefix(modelName, "mimo-v2.5-pro-")
}

func xiaomiTokenLimitValue(value gjson.Result) (int64, bool) {
	switch value.Type {
	case gjson.Number:
		return value.Int(), true
	case gjson.String:
		parsed, err := strconv.ParseInt(strings.TrimSpace(value.String()), 10, 64)
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

func normalizeXiaomiThinkingConfig(payload []byte) []byte {
	thinkingType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "thinking.type").String()))
	if thinkingType == "" {
		thinkingType = xiaomiThinkingTypeFromReasoning(payload)
	}
	switch thinkingType {
	case "none", "off", "false", "disabled", "disable":
		payload, _ = sjson.SetBytes(payload, "thinking.type", "disabled")
	case "low", "medium", "high", "max", "auto", "adaptive", "true", "enabled", "enable":
		payload, _ = sjson.SetBytes(payload, "thinking.type", "enabled")
	}
	for _, path := range []string{
		"reasoning",
		"reasoning_effort",
		"thinking.reasoning_effort",
		"thinking.budget_tokens",
		"thinking_budget",
	} {
		if updated, err := sjson.DeleteBytes(payload, path); err == nil {
			payload = updated
		}
	}
	return payload
}

func xiaomiThinkingTypeFromReasoning(payload []byte) string {
	for _, path := range []string{"reasoning_effort", "reasoning.effort", "thinking.reasoning_effort"} {
		value := gjson.GetBytes(payload, path)
		if !value.Exists() {
			continue
		}
		if value.Type == gjson.String {
			if effort := strings.ToLower(strings.TrimSpace(value.String())); effort != "" {
				return effort
			}
		}
	}
	return ""
}

func normalizeXiaomiThinkingHyperparameters(payload []byte) []byte {
	if !strings.EqualFold(strings.TrimSpace(gjson.GetBytes(payload, "thinking.type").String()), "enabled") {
		return payload
	}
	if updated, err := sjson.SetBytes(payload, "temperature", 1.0); err == nil {
		payload = updated
	}
	if updated, err := sjson.SetBytes(payload, "top_p", 0.95); err == nil {
		payload = updated
	}
	return payload
}

func requiresDoubaoSeed20Compatibility(model string) bool {
	modelName := strings.ToLower(strings.TrimSpace(thinking.ParseSuffix(model).ModelName))
	return strings.HasPrefix(modelName, "doubao-seed-2.0-")
}

func scrubDoubaoUnsupportedOpenAIFields(payload []byte, model string) []byte {
	unsupportedPaths := []string{
		"user",
		"response_format",
		"store",
		"metadata",
		"parallel_tool_calls",
		"stream_options",
		"service_tier",
		"reasoning_effort",
		"thinking.reasoning_effort",
		"thinking.budget_tokens",
		"thinking_budget",
		"thinking",
		"output_config.effort",
	}
	if !isDoubaoDeepSeekReasoningModel(model) {
		unsupportedPaths = append(unsupportedPaths, "reasoning")
	}
	for _, path := range unsupportedPaths {
		if updated, err := sjson.DeleteBytes(payload, path); err == nil {
			payload = updated
		}
	}
	if oc := gjson.GetBytes(payload, "output_config"); oc.Exists() && oc.IsObject() && len(oc.Map()) == 0 {
		if updated, err := sjson.DeleteBytes(payload, "output_config"); err == nil {
			payload = updated
		}
	}
	payload = deleteMessageReasoningContent(payload)
	return payload
}

func doubaoDeepSeekReasoningIntent(payload []byte, model string) (string, bool) {
	if len(payload) == 0 || !gjson.ValidBytes(payload) || !isDoubaoDeepSeekReasoningModel(model) {
		return "", false
	}
	for _, path := range []string{"reasoning.effort", "reasoning_effort", "thinking.reasoning_effort", "output_config.effort"} {
		value := gjson.GetBytes(payload, path)
		if !value.Exists() || value.Type != gjson.String {
			continue
		}
		normalized, disabled := normalizeDoubaoDeepSeekReasoningEffort(value.String())
		if disabled || normalized != "" {
			return normalized, disabled
		}
	}
	thinkingType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "thinking.type").String()))
	switch thinkingType {
	case "disabled", "none", "off", "false", "disable":
		return "", true
	case "enabled", "adaptive", "auto", "true", "enable":
		return "high", false
	}
	for _, path := range []string{"thinking.budget_tokens", "thinking_budget"} {
		value := gjson.GetBytes(payload, path)
		if !value.Exists() {
			continue
		}
		if budget, ok := deepSeekThinkingBudgetValue(value); ok {
			if budget <= 0 {
				return "", true
			}
			return "high", false
		}
	}
	return "", false
}

func applyDoubaoDeepSeekReasoningIntent(payload []byte, model, effort string, disabled bool) []byte {
	if len(payload) == 0 || !gjson.ValidBytes(payload) || !isDoubaoDeepSeekReasoningModel(model) {
		return payload
	}
	if disabled {
		for _, path := range []string{
			"reasoning",
			"reasoning_effort",
			"thinking",
			"thinking_budget",
			"output_config.effort",
		} {
			if updated, err := sjson.DeleteBytes(payload, path); err == nil {
				payload = updated
			}
		}
		if oc := gjson.GetBytes(payload, "output_config"); oc.Exists() && oc.IsObject() && len(oc.Map()) == 0 {
			if updated, err := sjson.DeleteBytes(payload, "output_config"); err == nil {
				payload = updated
			}
		}
		return payload
	}
	effort, disabled = normalizeDoubaoDeepSeekReasoningEffort(effort)
	if disabled || effort == "" {
		return payload
	}
	updated, err := sjson.SetBytes(payload, "reasoning.effort", effort)
	if err != nil {
		return payload
	}
	return updated
}

func normalizeDoubaoDeepSeekReasoningEffort(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "default":
		return "", false
	case "none", "off", "disabled", "disable", "false":
		return "", true
	case "minimal", "low", "medium", "high":
		return strings.ToLower(strings.TrimSpace(raw)), false
	case "auto", "adaptive", "enabled", "enable", "true", "xhigh", "max":
		return "high", false
	default:
		return "", false
	}
}

func isDoubaoDeepSeekReasoningModel(model string) bool {
	modelName := strings.ToLower(strings.TrimSpace(thinking.ParseSuffix(model).ModelName))
	if slash := strings.LastIndex(modelName, "/"); slash >= 0 {
		modelName = modelName[slash+1:]
	}
	return strings.HasPrefix(modelName, "deepseek-v4") || strings.Contains(modelName, "deepseek-r1")
}

func normalizeDoubaoSeed20Temperature(payload []byte) []byte {
	value := gjson.GetBytes(payload, "temperature")
	if !value.Exists() {
		return payload
	}
	temperature, ok := openAICompatFloatValue(value)
	if !ok {
		if updated, err := sjson.DeleteBytes(payload, "temperature"); err == nil {
			return updated
		}
		return payload
	}
	if temperature < 0 {
		temperature = 0
	} else if temperature > doubaoSeed20MaxTemperature {
		temperature = doubaoSeed20MaxTemperature
	}
	updated, err := sjson.SetBytes(payload, "temperature", temperature)
	if err != nil {
		return payload
	}
	return updated
}

func normalizeDoubaoSeed20TokenFields(payload []byte) []byte {
	token, ok := firstOpenAICompatIntegerValue(payload, "max_completion_tokens", "max_tokens", "max_output_tokens")
	if !ok {
		for _, path := range []string{"max_completion_tokens", "max_tokens", "max_output_tokens"} {
			if updated, err := sjson.DeleteBytes(payload, path); err == nil {
				payload = updated
			}
		}
		return payload
	}
	if token < 1 {
		token = 1
	} else if token > doubaoSeed20MaxCompletionTokens {
		token = doubaoSeed20MaxCompletionTokens
	}
	updated, err := sjson.SetBytes(payload, "max_completion_tokens", token)
	if err == nil {
		payload = updated
	}
	for _, path := range []string{"max_tokens", "max_output_tokens"} {
		if updated, errDelete := sjson.DeleteBytes(payload, path); errDelete == nil {
			payload = updated
		}
	}
	return payload
}

func firstOpenAICompatIntegerValue(payload []byte, paths ...string) (int64, bool) {
	for _, path := range paths {
		value := gjson.GetBytes(payload, path)
		if !value.Exists() {
			continue
		}
		switch value.Type {
		case gjson.Number:
			return value.Int(), true
		case gjson.String:
			parsed, err := strconv.ParseInt(strings.TrimSpace(value.String()), 10, 64)
			if err == nil {
				return parsed, true
			}
		}
	}
	return 0, false
}

func openAICompatFloatValue(value gjson.Result) (float64, bool) {
	switch value.Type {
	case gjson.Number:
		return value.Float(), true
	case gjson.String:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(value.String()), 64)
		if err == nil {
			return parsed, true
		}
		return 0, false
	default:
		return 0, false
	}
}

func normalizeDoubaoChatContentParts(payload []byte) []byte {
	if !gjson.GetBytes(payload, "messages").IsArray() {
		return payload
	}

	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		return payload
	}
	messages, ok := root["messages"].([]any)
	if !ok {
		return payload
	}

	changed := false
	for _, rawMessage := range messages {
		message, okMessage := rawMessage.(map[string]any)
		if !okMessage {
			continue
		}
		parts, okParts := message["content"].([]any)
		if !okParts {
			continue
		}
		for idx, rawPart := range parts {
			normalized, partChanged := normalizeDoubaoChatContentPart(rawPart)
			if !partChanged {
				continue
			}
			parts[idx] = normalized
			changed = true
		}
	}
	if !changed {
		return payload
	}
	root["messages"] = messages
	out, err := json.Marshal(root)
	if err != nil || !gjson.ValidBytes(out) {
		return payload
	}
	return out
}

func normalizeDoubaoChatContentPart(rawPart any) (any, bool) {
	part, ok := rawPart.(map[string]any)
	if !ok {
		return rawPart, false
	}
	partType := strings.ToLower(strings.TrimSpace(compatStringValue(part["type"])))
	switch partType {
	case "input_text":
		text := strings.TrimSpace(compatStringValue(part["text"]))
		if text == "" {
			text = strings.TrimSpace(compatStringValue(part["content"]))
		}
		return map[string]any{"type": "text", "text": text}, true
	case "image_url":
		return normalizeDoubaoMediaPart(part, "image_url", "image_url")
	case "input_image":
		return normalizeDoubaoMediaPart(part, "image_url", "input_image")
	case "video_url":
		return normalizeDoubaoMediaPart(part, "video_url", "video_url")
	case "input_video":
		return normalizeDoubaoMediaPart(part, "video_url", "input_video")
	default:
		return rawPart, false
	}
}

func normalizeDoubaoMediaPart(part map[string]any, targetType string, sourceType string) (any, bool) {
	urlValue := firstDoubaoMediaURL(part, targetType, sourceType)
	if urlValue == "" {
		return part, false
	}
	next := map[string]any{
		"type": targetType,
		targetType: map[string]any{
			"url": urlValue,
		},
	}
	if detail := strings.TrimSpace(compatStringValue(part["detail"])); detail != "" && targetType == "image_url" {
		next[targetType].(map[string]any)["detail"] = detail
	}
	if sourceType != targetType {
		return next, true
	}
	if media, ok := part[targetType].(map[string]any); ok {
		if strings.TrimSpace(compatStringValue(media["url"])) == urlValue {
			return part, false
		}
	}
	return next, true
}

func firstDoubaoMediaURL(part map[string]any, targetType string, sourceType string) string {
	for _, key := range []string{targetType, sourceType, "image_url", "video_url", "url"} {
		raw, ok := part[key]
		if !ok {
			continue
		}
		switch typed := raw.(type) {
		case string:
			if value := strings.TrimSpace(typed); value != "" {
				return value
			}
		case map[string]any:
			for _, nestedKey := range []string{"url", "image_url", "video_url"} {
				if value := strings.TrimSpace(compatStringValue(typed[nestedKey])); value != "" {
					return value
				}
			}
		}
	}
	return ""
}

func normalizeOpenAICompatToolCallArguments(payload []byte) []byte {
	if !gjson.GetBytes(payload, "messages").IsArray() {
		return payload
	}

	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		return payload
	}
	messages, ok := root["messages"].([]any)
	if !ok {
		return payload
	}

	changed := false
	for _, rawMessage := range messages {
		message, okMessage := rawMessage.(map[string]any)
		if !okMessage {
			continue
		}
		toolCalls, okToolCalls := message["tool_calls"].([]any)
		if !okToolCalls {
			continue
		}
		for _, rawToolCall := range toolCalls {
			toolCall, okToolCall := rawToolCall.(map[string]any)
			if !okToolCall {
				continue
			}
			function, okFunction := toolCall["function"].(map[string]any)
			if !okFunction {
				continue
			}
			arguments, changedArguments := normalizeOpenAICompatToolArgumentsValue(function["arguments"])
			if changedArguments {
				function["arguments"] = arguments
				changed = true
			}
		}
	}
	if !changed {
		return payload
	}
	root["messages"] = messages
	out, err := json.Marshal(root)
	if err != nil || !gjson.ValidBytes(out) {
		return payload
	}
	return out
}

func normalizeOpenAICompatToolArgumentsValue(raw any) (string, bool) {
	switch typed := raw.(type) {
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return "{}", true
		}
		if gjson.Valid(trimmed) {
			return typed, false
		}
		if repaired, ok := helps.RepairInvalidJSONStringEscapes([]byte(trimmed)); ok && gjson.ValidBytes(repaired) {
			return string(repaired), true
		}
		return "{}", true
	case nil:
		return "{}", true
	default:
		encoded, err := json.Marshal(typed)
		if err != nil || !gjson.ValidBytes(encoded) {
			return "{}", true
		}
		return string(encoded), true
	}
}

type openAICompatPayloadDiagnostic struct {
	Model             string
	CompatKind        string
	Endpoint          string
	RequestPath       string
	Channel           string
	AuthID            string
	AuthLabel         string
	CompatName        string
	CompatKindSource  string
	CompatMapping     string
	PayloadSize       int
	PayloadFields     []string
	AddedFields       []string
	RemovedFields     []string
	ModifiedFields    []string
	UpstreamRequestID string
}

func newOpenAICompatPayloadDiagnostic(before, after []byte, profile openAICompatProfile, auth *cliproxyauth.Auth, model, endpoint, requestPath string, requestHeaders http.Header, responseHeaders http.Header) openAICompatPayloadDiagnostic {
	beforeFields := openAICompatTopLevelRawFields(before)
	afterFields := openAICompatTopLevelRawFields(after)
	diag := openAICompatPayloadDiagnostic{
		Model:             strings.TrimSpace(model),
		CompatKind:        config.NormalizeOpenAICompatibilityKind(profile.Kind),
		Endpoint:          strings.TrimSpace(endpoint),
		RequestPath:       strings.TrimSpace(requestPath),
		Channel:           firstHeaderValue(requestHeaders, "X-Newapi-Channel-Id", "X-New-Api-Channel-Id", "X-Channel-Id", "Channel-Id"),
		PayloadSize:       len(after),
		PayloadFields:     sortedMapKeys(afterFields),
		AddedFields:       sortedFieldDiff(afterFields, beforeFields),
		RemovedFields:     sortedFieldDiff(beforeFields, afterFields),
		ModifiedFields:    sortedModifiedFields(beforeFields, afterFields),
		UpstreamRequestID: firstHeaderValue(responseHeaders, "X-Tt-Logid", "X-Volc-Request-Id", "X-Request-Id", "X-Request-ID", "X-Requestid", "Request-Id"),
	}
	diag.CompatKindSource = openAICompatKindSource(profile, auth)
	diag.CompatMapping = openAICompatMapping(profile, model)
	if auth != nil {
		diag.AuthID = strings.TrimSpace(auth.ID)
		diag.AuthLabel = strings.TrimSpace(auth.Label)
		if auth.Attributes != nil {
			diag.CompatName = strings.TrimSpace(auth.Attributes["compat_name"])
		}
	}
	return diag
}

func openAICompatKindSource(profile openAICompatProfile, auth *cliproxyauth.Auth) string {
	kind := config.NormalizeOpenAICompatibilityKind(profile.Kind)
	if kind == "" {
		return ""
	}
	if auth != nil && auth.Attributes != nil {
		if attrKind := config.NormalizeOpenAICompatibilityKind(auth.Attributes["compat_kind"]); attrKind == kind {
			return "auth_attribute:compat_kind"
		}
		if inferred := inferOpenAICompatKindFromBaseURL(auth.Attributes["base_url"]); inferred == kind {
			return "base_url_inference"
		}
	}
	return "compat_config"
}

func openAICompatMapping(profile openAICompatProfile, model string) string {
	if config.NormalizeOpenAICompatibilityKind(profile.Kind) != "doubao" {
		return ""
	}
	modelName := strings.ToLower(strings.TrimSpace(thinking.ParseSuffix(model).ModelName))
	if strings.HasPrefix(modelName, "deepseek-v4-pro") || strings.HasPrefix(modelName, "deepseek-v4-flash") {
		return "deepseek_v4_via_doubao_volcengine"
	}
	return ""
}

func (d openAICompatPayloadDiagnostic) relevant() bool {
	return d.CompatKind == "doubao" || len(d.AddedFields) > 0 || len(d.RemovedFields) > 0 || len(d.ModifiedFields) > 0
}

func openAICompatTopLevelRawFields(payload []byte) map[string]string {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return nil
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(payload, &root); err != nil {
		return nil
	}
	fields := make(map[string]string, len(root))
	for key, raw := range root {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		fields[key] = string(raw)
	}
	return fields
}

func sortedMapKeys(values map[string]string) []string {
	if len(values) == 0 {
		return nil
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedFieldDiff(left, right map[string]string) []string {
	if len(left) == 0 {
		return nil
	}
	fields := make([]string, 0)
	for key := range left {
		if _, ok := right[key]; !ok {
			fields = append(fields, key)
		}
	}
	sort.Strings(fields)
	return fields
}

func sortedModifiedFields(before, after map[string]string) []string {
	if len(before) == 0 || len(after) == 0 {
		return nil
	}
	fields := make([]string, 0)
	for key, beforeRaw := range before {
		afterRaw, ok := after[key]
		if !ok || beforeRaw == afterRaw {
			continue
		}
		fields = append(fields, key)
	}
	sort.Strings(fields)
	return fields
}

func firstHeaderValue(headers http.Header, names ...string) string {
	if headers == nil {
		return ""
	}
	for _, name := range names {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

func rewriteOpenAICompatSystemMessagesAsUser(payload []byte) []byte {
	if len(payload) == 0 || !gjson.GetBytes(payload, "messages").IsArray() {
		return payload
	}

	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		return payload
	}
	messages, ok := root["messages"].([]any)
	if !ok || len(messages) == 0 {
		return payload
	}

	changed := false
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			continue
		}
		role := strings.TrimSpace(compatStringValue(message["role"]))
		text := openAICompatTextContent(message["content"])
		if strings.EqualFold(role, "system") {
			message["role"] = "user"
			if text != "" {
				message["content"] = openAICompatSystemInstructionText(openAICompatUnwrapSystemReminder(text))
			}
			changed = true
			continue
		}
		if reminder, okReminder := openAICompatSystemReminderText(text); okReminder {
			message["role"] = "user"
			message["content"] = openAICompatSystemInstructionText(reminder)
			changed = true
		}
	}
	if !changed {
		return payload
	}

	root["messages"] = messages
	out, err := json.Marshal(root)
	if err != nil || !gjson.ValidBytes(out) {
		return payload
	}
	return out
}

func openAICompatSystemInstructionText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	return "System instructions:\n" + text
}

func openAICompatSystemReminderText(text string) (string, bool) {
	unwrapped := openAICompatUnwrapSystemReminder(text)
	return unwrapped, unwrapped != strings.TrimSpace(text) && unwrapped != ""
}

func openAICompatUnwrapSystemReminder(text string) string {
	text = strings.TrimSpace(text)
	const start = "<system-reminder>"
	const end = "</system-reminder>"
	if strings.HasPrefix(text, start) && strings.HasSuffix(text, end) {
		return strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(text, start), end))
	}
	return text
}

func openAICompatTextContent(content any) string {
	switch typed := content.(type) {
	case string:
		return strings.TrimSpace(typed)
	case []any:
		parts := make([]string, 0, len(typed))
		for _, part := range typed {
			if text := openAICompatTextContent(part); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		if text := strings.TrimSpace(compatStringValue(typed["text"])); text != "" {
			return text
		}
		if nested, ok := typed["content"]; ok {
			return openAICompatTextContent(nested)
		}
		return ""
	default:
		return ""
	}
}

func scrubDeepSeekThinkingBudgetForCompat(payload []byte, model string, baseURL string, compatKind string) []byte {
	if len(payload) == 0 || !gjson.ValidBytes(payload) || !requiresDeepSeekThinkingBudgetCompatibility(model, baseURL, compatKind) {
		return payload
	}

	if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(payload, "thinking.type").String()), "disabled") {
		payload = deleteDeepSeekThinkingBudgetPaths(payload)
		return payload
	}

	for _, path := range []string{"thinking_budget", "thinking.budget_tokens"} {
		payload = normalizeDeepSeekThinkingBudgetPath(payload, path)
	}

	effort := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "reasoning_effort").String()))
	normalizedEffort := thinking.NormalizeDeepSeekOfficialReasoningEffort(effort)
	switch normalizedEffort {
	case "none", "disabled", "off":
		payload, _ = sjson.SetBytes(payload, "thinking.type", "disabled")
		payload, _ = sjson.DeleteBytes(payload, "reasoning_effort")
		payload = deleteDeepSeekThinkingBudgetPaths(payload)
	case "high", "max":
		payload, _ = sjson.SetBytes(payload, "reasoning_effort", normalizedEffort)
	}

	return payload
}

func requiresDeepSeekThinkingBudgetCompatibility(model string, baseURL string, compatKind string) bool {
	switch config.NormalizeOpenAICompatibilityKind(compatKind) {
	case "deepseek":
		return true
	case "kimi", "minimax", "xiaomi", "zhipu", "doubao", "xfyun", "maas", "langengyun", "newapi":
		return false
	}
	switch config.InferCompatKindFromBaseURL(baseURL) {
	case "deepseek":
		return true
	case "kimi", "minimax", "xiaomi", "zhipu", "doubao", "xfyun", "maas", "langengyun", "newapi":
		return false
	}
	modelName := strings.ToLower(strings.TrimSpace(model))
	return strings.HasPrefix(modelName, "deepseek-v4") || strings.Contains(modelName, "deepseek-reasoner")
}

func deleteDeepSeekThinkingBudgetPaths(payload []byte) []byte {
	for _, path := range []string{"thinking_budget", "thinking.budget_tokens"} {
		if updated, err := sjson.DeleteBytes(payload, path); err == nil {
			payload = updated
		}
	}
	return payload
}

func normalizeDeepSeekThinkingBudgetPath(payload []byte, path string) []byte {
	value := gjson.GetBytes(payload, path)
	if !value.Exists() {
		return payload
	}

	budget, ok := deepSeekThinkingBudgetValue(value)
	if !ok || budget <= 0 {
		updated, err := sjson.DeleteBytes(payload, path)
		if err != nil {
			return payload
		}
		return updated
	}
	if budget < deepSeekThinkingBudgetMin {
		budget = deepSeekThinkingBudgetMin
	} else if budget > deepSeekThinkingBudgetMax {
		budget = deepSeekThinkingBudgetMax
	}
	updated, err := sjson.SetBytes(payload, path, budget)
	if err != nil {
		return payload
	}
	return updated
}

func deepSeekThinkingBudgetValue(value gjson.Result) (int, bool) {
	switch value.Type {
	case gjson.Number:
		return int(value.Int()), true
	case gjson.String:
		raw := strings.TrimSpace(value.String())
		if raw == "" {
			return 0, false
		}
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

func scrubZhipuImageURLDataURLs(payload []byte) []byte {
	if len(payload) == 0 || !gjson.GetBytes(payload, "messages").IsArray() {
		return payload
	}

	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		return payload
	}
	messages, ok := root["messages"].([]any)
	if !ok {
		return payload
	}

	changed := false
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := message["content"].([]any)
		if !ok {
			continue
		}
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			if !strings.EqualFold(strings.TrimSpace(compatStringValue(part["type"])), "image_url") {
				continue
			}
			imageURL, ok := part["image_url"].(map[string]any)
			if !ok {
				if urlValue := strings.TrimSpace(compatStringValue(part["image_url"])); urlValue != "" {
					if _, data, okData := util.ParseDataURL(urlValue); okData {
						part["image_url"] = map[string]any{"url": data}
						changed = true
					}
				}
				continue
			}
			urlValue := strings.TrimSpace(compatStringValue(imageURL["url"]))
			if urlValue == "" {
				continue
			}
			if _, data, okData := util.ParseDataURL(urlValue); okData {
				imageURL["url"] = data
				changed = true
			}
		}
	}
	if !changed {
		return payload
	}
	out, err := json.Marshal(root)
	if err != nil || !gjson.ValidBytes(out) {
		return payload
	}
	return out
}

func scrubOpenAICompatProviderToolPayload(payload []byte, profile openAICompatProfile) []byte {
	switch config.NormalizeOpenAICompatibilityKind(profile.Kind) {
	case "kimi", "minimax", "xiaomi", "zhipu", "doubao", "xfyun", "maas", "langengyun", "newapi":
		return scrubOpenAICompatFunctionToolPayload(payload, profile)
	default:
		return payload
	}
}

func sanitizeOpenAICompatToolSchemas(payload []byte) []byte {
	if len(payload) == 0 || !gjson.GetBytes(payload, "tools").IsArray() {
		return payload
	}

	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		return payload
	}
	tools, ok := root["tools"].([]any)
	if !ok || len(tools) == 0 {
		return payload
	}

	changed := false
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"input_schema", "parameters", "parametersJsonSchema"} {
			if normalized, okNormalize := normalizeOpenAICompatParameterNode(tool[key]); okNormalize {
				if !jsonValuesEqual(tool[key], normalized) {
					tool[key] = normalized
					changed = true
				}
			}
		}
		function, okFunction := tool["function"].(map[string]any)
		if !okFunction {
			continue
		}
		for _, key := range []string{"parameters", "parametersJsonSchema"} {
			if normalized, okNormalize := normalizeOpenAICompatParameterNode(function[key]); okNormalize {
				if !jsonValuesEqual(function[key], normalized) {
					function[key] = normalized
					changed = true
				}
			}
		}
	}
	if !changed {
		return payload
	}

	root["tools"] = tools
	out, err := json.Marshal(root)
	if err != nil || !gjson.ValidBytes(out) {
		return payload
	}
	return out
}

func scrubOpenAICompatFunctionToolPayload(payload []byte, profile openAICompatProfile) []byte {
	if len(payload) == 0 || !gjson.GetBytes(payload, "tools").IsArray() {
		return payload
	}
	profileKind := config.NormalizeOpenAICompatibilityKind(profile.Kind)

	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		return payload
	}
	tools, ok := root["tools"].([]any)
	if !ok || len(tools) == 0 {
		return payload
	}

	cleanedTools := make([]any, 0, len(tools))
	nameMapping := make(map[string]string)
	changed := false
	for _, rawTool := range tools {
		cleaned, ok := normalizeDeepSeekTool(rawTool, false)
		if !ok {
			cleanedTools = append(cleanedTools, rawTool)
			continue
		}
		if profileKind == "kimi" {
			if function, okFunction := cleaned["function"].(map[string]any); okFunction {
				function["strict"] = false
				if parameters, okParameters := function["parameters"].(map[string]any); okParameters {
					function["parameters"] = normalizeMoonshotSchemaCombiners(parameters)
				}
			}
		}
		if profileKind == "xiaomi" {
			if function, okFunction := cleaned["function"].(map[string]any); okFunction {
				if parameters, okParameters := function["parameters"]; okParameters {
					function["parameters"] = normalizeXiaomiToolSchema(parameters)
				}
			}
		}
		if originalName := openAICompatOriginalFunctionName(rawTool); originalName != "" {
			if normalizedName := openAICompatNormalizedFunctionName(cleaned); normalizedName != "" && normalizedName != originalName {
				nameMapping[originalName] = normalizedName
			}
		}
		cleanedTools = append(cleanedTools, cleaned)
		if !jsonValuesEqual(rawTool, cleaned) {
			changed = true
		}
	}
	if rewriteOpenAICompatFunctionNameReferences(root, nameMapping) {
		changed = true
	}
	if !changed {
		return payload
	}

	root["tools"] = cleanedTools
	out, err := json.Marshal(root)
	if err != nil || !gjson.ValidBytes(out) {
		return payload
	}
	return out
}

func scrubXiaomiToolSchemas(payload []byte) []byte {
	if len(payload) == 0 || !gjson.GetBytes(payload, "tools").IsArray() {
		return payload
	}

	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		return payload
	}
	tools, ok := root["tools"].([]any)
	if !ok || len(tools) == 0 {
		return payload
	}

	changed := false
	for _, rawTool := range tools {
		tool, okTool := rawTool.(map[string]any)
		if !okTool {
			continue
		}
		if _, okStrict := tool["strict"]; okStrict {
			delete(tool, "strict")
			changed = true
		}
		function, okFunction := tool["function"].(map[string]any)
		if !okFunction {
			continue
		}
		if _, okStrict := function["strict"]; okStrict {
			delete(function, "strict")
			changed = true
		}
		parameters, okParameters := function["parameters"]
		if !okParameters {
			continue
		}
		normalized := normalizeXiaomiToolSchema(parameters)
		if !jsonValuesEqual(parameters, normalized) {
			function["parameters"] = normalized
			changed = true
		}
	}
	if !changed {
		return payload
	}
	root["tools"] = tools
	out, err := json.Marshal(root)
	if err != nil || !gjson.ValidBytes(out) {
		return payload
	}
	return out
}

func normalizeXiaomiToolSchema(parameters any) any {
	normalized := normalizeOpenAICompatParameters(parameters)
	return simplifyXiaomiToolSchema(normalized)
}

func simplifyXiaomiToolSchema(node any) any {
	switch typed := node.(type) {
	case map[string]any:
		if branch, ok := firstXiaomiSchemaCombinerBranch(typed); ok {
			return simplifyXiaomiToolSchema(branch)
		}
		out := make(map[string]any, 4)
		schemaType := xiaomiSchemaType(typed)
		out["type"] = schemaType
		if description := strings.TrimSpace(compatStringValue(typed["description"])); description != "" {
			out["description"] = description
		}
		if enumValues := xiaomiScalarArray(typed["enum"]); len(enumValues) > 0 {
			out["enum"] = enumValues
		}
		switch schemaType {
		case "object":
			out["properties"] = simplifyXiaomiToolProperties(typed["properties"])
			if required := normalizeOpenAICompatStringArray(typed["required"]); len(required) > 0 {
				out["required"] = required
			}
		case "array":
			out["items"] = simplifyXiaomiArrayItems(typed["items"])
		}
		return out
	case []any:
		if len(typed) == 0 {
			return map[string]any{"type": "string"}
		}
		return simplifyXiaomiToolSchema(typed[0])
	case string:
		if schemaType, ok := normalizeOpenAICompatSchemaType(typed); ok {
			return openAICompatSchemaForType(schemaType)
		}
		return map[string]any{"type": "string"}
	default:
		if schemaType, ok := normalizeOpenAICompatScalarSchemaType(typed); ok {
			return openAICompatSchemaForType(schemaType)
		}
		return map[string]any{"type": "string"}
	}
}

func firstXiaomiSchemaCombinerBranch(schema map[string]any) (any, bool) {
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		branches, ok := schema[key].([]any)
		if !ok || len(branches) == 0 {
			continue
		}
		for _, branch := range branches {
			branchMap, okBranch := branch.(map[string]any)
			if !okBranch {
				continue
			}
			if schemaType, okType := normalizeOpenAICompatScalarSchemaType(branchMap["type"]); okType && schemaType == "null" {
				continue
			}
			return branch, true
		}
		return branches[0], true
	}
	return nil, false
}

func xiaomiSchemaType(schema map[string]any) string {
	if schemaType, ok := normalizeOpenAICompatScalarSchemaType(schema["type"]); ok && schemaType != "null" {
		return schemaType
	}
	if _, ok := schema["properties"]; ok {
		return "object"
	}
	if _, ok := schema["items"]; ok {
		return "array"
	}
	return "string"
}

func simplifyXiaomiToolProperties(raw any) map[string]any {
	properties, ok := raw.(map[string]any)
	if !ok || len(properties) == 0 {
		return map[string]any{}
	}
	out := make(map[string]any, len(properties))
	for name, value := range properties {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		out[name] = simplifyXiaomiToolSchema(value)
	}
	return out
}

func simplifyXiaomiArrayItems(raw any) any {
	if raw == nil {
		return map[string]any{"type": "string"}
	}
	return simplifyXiaomiToolSchema(raw)
}

func xiaomiScalarArray(raw any) []any {
	values, ok := raw.([]any)
	if !ok || len(values) == 0 {
		return nil
	}
	out := make([]any, 0, len(values))
	for _, value := range values {
		switch typed := value.(type) {
		case string:
			if strings.TrimSpace(typed) != "" {
				out = append(out, typed)
			}
		case float64, bool:
			out = append(out, typed)
		}
	}
	return out
}

func normalizeMoonshotSchemaCombiners(node any) any {
	switch typed := node.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, value := range typed {
			out[key] = normalizeMoonshotSchemaCombiners(value)
		}
		if hasMoonshotSchemaCombiner(out) {
			for _, key := range []string{"type", "properties", "required", "items", "additionalProperties"} {
				delete(out, key)
			}
		}
		return out
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, normalizeMoonshotSchemaCombiners(item))
		}
		return out
	default:
		return node
	}
}

func hasMoonshotSchemaCombiner(schema map[string]any) bool {
	if schema == nil {
		return false
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		if _, ok := schema[key]; ok {
			return true
		}
	}
	return false
}

func scrubOpenAICompatToolChoice(payload []byte, profile openAICompatProfile) []byte {
	if config.NormalizeOpenAICompatibilityKind(profile.Kind) != "zhipu" {
		return payload
	}
	toolChoice := gjson.GetBytes(payload, "tool_choice")
	if !toolChoice.Exists() {
		return payload
	}
	if !gjson.GetBytes(payload, "tools").IsArray() {
		if out, err := sjson.DeleteBytes(payload, "tool_choice"); err == nil {
			return out
		}
		return payload
	}
	if toolChoice.Type == gjson.String && strings.EqualFold(strings.TrimSpace(toolChoice.String()), "auto") {
		return payload
	}
	out, err := sjson.SetBytes(payload, "tool_choice", "auto")
	if err != nil {
		return payload
	}
	return out
}

func repairOpenAICompatToolCallHistory(payload []byte) []byte {
	if len(payload) == 0 || !gjson.GetBytes(payload, "messages").IsArray() {
		return payload
	}

	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		return payload
	}
	messages, ok := root["messages"].([]any)
	if !ok || len(messages) == 0 {
		return payload
	}

	repaired := make([]any, 0, len(messages))
	changed := false
	var pending map[string]bool
	seenToolCallIDs := make(map[string]bool)
	for idx, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			repaired = append(repaired, rawMessage)
			pending = nil
			continue
		}

		role := strings.TrimSpace(compatStringValue(message["role"]))
		if role == "tool" {
			toolCallID := strings.TrimSpace(compatStringValue(message["tool_call_id"]))
			if pending != nil && pending[toolCallID] {
				repaired = append(repaired, message)
				delete(pending, toolCallID)
			} else {
				changed = true
			}
			continue
		}

		pending = nil
		if role != "assistant" {
			if !openAICompatMessageHasContent(message) {
				changed = true
				continue
			}
			repaired = append(repaired, message)
			continue
		}

		toolCalls, ok := message["tool_calls"].([]any)
		if !ok || len(toolCalls) == 0 {
			if !openAICompatMessageHasContent(message) {
				changed = true
				continue
			}
			repaired = append(repaired, message)
			continue
		}

		nextToolResults := openAICompatToolResultIDsInNextMessages(messages, idx)
		keptToolCalls := make([]any, 0, len(toolCalls))
		keptIDs := make(map[string]bool)
		for _, rawToolCall := range toolCalls {
			normalizedToolCall, changedToolCall, okToolCall := normalizeOpenAICompatHistoryToolCall(rawToolCall)
			if !okToolCall {
				changed = true
				continue
			}
			if changedToolCall {
				changed = true
			}
			toolCallID := strings.TrimSpace(openAICompatToolCallID(rawToolCall))
			if toolCallID == "" || !nextToolResults[toolCallID] || keptIDs[toolCallID] || seenToolCallIDs[toolCallID] {
				changed = true
				continue
			}
			keptToolCalls = append(keptToolCalls, normalizedToolCall)
			keptIDs[toolCallID] = true
			seenToolCallIDs[toolCallID] = true
		}

		if len(keptToolCalls) == 0 {
			delete(message, "tool_calls")
			if !openAICompatMessageHasContent(message) {
				changed = true
				continue
			}
			repaired = append(repaired, message)
			continue
		}
		if len(keptToolCalls) != len(toolCalls) {
			message["tool_calls"] = keptToolCalls
		}
		repaired = append(repaired, message)
		pending = keptIDs
	}

	if !changed {
		return payload
	}
	root["messages"] = repaired
	out, err := json.Marshal(root)
	if err != nil || !gjson.ValidBytes(out) {
		return payload
	}
	return out
}

func openAICompatToolResultIDsInNextMessages(messages []any, assistantIdx int) map[string]bool {
	result := make(map[string]bool)
	for idx := assistantIdx + 1; idx < len(messages); idx++ {
		message, ok := messages[idx].(map[string]any)
		if !ok {
			break
		}
		if strings.TrimSpace(compatStringValue(message["role"])) != "tool" {
			break
		}
		if toolCallID := strings.TrimSpace(compatStringValue(message["tool_call_id"])); toolCallID != "" {
			result[toolCallID] = true
		}
	}
	return result
}

func openAICompatToolCallID(rawToolCall any) string {
	toolCall, ok := rawToolCall.(map[string]any)
	if !ok {
		return ""
	}
	return compatStringValue(toolCall["id"])
}

func normalizeOpenAICompatHistoryToolCall(rawToolCall any) (any, bool, bool) {
	toolCall, ok := rawToolCall.(map[string]any)
	if !ok {
		return nil, false, false
	}
	changed := false

	function, ok := toolCall["function"].(map[string]any)
	if !ok {
		function = map[string]any{}
		toolCall["function"] = function
		changed = true
	}

	name, okName := normalizeOpenAICompatFunctionName(compatStringValue(function["name"]))
	if !okName {
		name, okName = normalizeOpenAICompatFunctionName(compatStringValue(toolCall["name"]))
	}
	if !okName {
		return nil, changed, false
	}
	if compatStringValue(function["name"]) != name {
		function["name"] = name
		changed = true
	}
	if strings.TrimSpace(compatStringValue(toolCall["type"])) == "" {
		toolCall["type"] = "function"
		changed = true
	}
	if _, okArguments := function["arguments"].(string); !okArguments {
		if function["arguments"] == nil {
			function["arguments"] = ""
		} else if rawArguments, err := json.Marshal(function["arguments"]); err == nil {
			function["arguments"] = string(rawArguments)
		} else {
			function["arguments"] = ""
		}
		changed = true
	}

	return toolCall, changed, true
}

func openAICompatMessageHasContent(message map[string]any) bool {
	content, ok := message["content"]
	if !ok || content == nil {
		return false
	}
	switch v := content.(type) {
	case string:
		return strings.TrimSpace(v) != ""
	case []any:
		for _, rawPart := range v {
			switch part := rawPart.(type) {
			case string:
				if strings.TrimSpace(part) != "" {
					return true
				}
			case map[string]any:
				if strings.TrimSpace(compatStringValue(part["text"])) != "" {
					return true
				}
				if imageURL, okImageURL := part["image_url"].(map[string]any); okImageURL {
					if strings.TrimSpace(compatStringValue(imageURL["url"])) != "" {
						return true
					}
				} else if strings.TrimSpace(compatStringValue(part["image_url"])) != "" {
					return true
				}
			default:
				if rawPart != nil {
					return true
				}
			}
		}
		return false
	default:
		return true
	}
}

func requiresDeepSeekToolSchemaCompatibility(model string) bool {
	modelName := strings.ToLower(strings.TrimSpace(model))
	return strings.HasPrefix(modelName, "deepseek-v4")
}

func scrubDeepSeekToolPayload(payload []byte, baseURL string) []byte {
	if len(payload) == 0 || !gjson.GetBytes(payload, "tools").IsArray() {
		return payload
	}

	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		return payload
	}
	tools, ok := root["tools"].([]any)
	if !ok || len(tools) == 0 {
		return payload
	}

	keepStrict := deepSeekBaseURLUsesBeta(baseURL) && allDeepSeekFunctionToolsStrict(tools)
	cleanedTools := make([]any, 0, len(tools))
	nameMapping := make(map[string]string)
	changed := false
	for _, rawTool := range tools {
		cleaned, ok := normalizeDeepSeekTool(rawTool, keepStrict)
		if !ok {
			cleanedTools = append(cleanedTools, rawTool)
			continue
		}
		if originalName := openAICompatOriginalFunctionName(rawTool); originalName != "" {
			if normalizedName := openAICompatNormalizedFunctionName(cleaned); normalizedName != "" && normalizedName != originalName {
				nameMapping[originalName] = normalizedName
			}
		}
		cleanedTools = append(cleanedTools, cleaned)
		if !jsonValuesEqual(rawTool, cleaned) {
			changed = true
		}
	}
	if rewriteOpenAICompatFunctionNameReferences(root, nameMapping) {
		changed = true
	}
	if !changed {
		return payload
	}

	root["tools"] = cleanedTools
	out, err := json.Marshal(root)
	if err != nil || !gjson.ValidBytes(out) {
		return payload
	}
	return out
}

func deepSeekBaseURLUsesBeta(baseURL string) bool {
	baseURL = strings.ToLower(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	return strings.HasSuffix(baseURL, "/beta")
}

func allDeepSeekFunctionToolsStrict(tools []any) bool {
	foundFunction := false
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		function := deepSeekFunctionToolNode(tool)
		if function == nil {
			continue
		}
		foundFunction = true
		if strict, _ := function["strict"].(bool); strict {
			continue
		}
		if strict, _ := tool["strict"].(bool); strict {
			continue
		}
		return false
	}
	return foundFunction
}

func normalizeDeepSeekTool(rawTool any, keepStrict bool) (map[string]any, bool) {
	tool, ok := rawTool.(map[string]any)
	if !ok {
		return nil, false
	}

	function := deepSeekFunctionToolNode(tool)
	if function == nil {
		return nil, false
	}

	name, ok := normalizeOpenAICompatFunctionName(compatStringValue(function["name"]))
	if !ok {
		name, ok = normalizeOpenAICompatFunctionName(compatStringValue(tool["name"]))
	}
	if !ok {
		return nil, false
	}

	normalizedFunction := map[string]any{"name": name}
	if description := compatStringValue(function["description"]); strings.TrimSpace(description) != "" {
		normalizedFunction["description"] = description
	} else if fallback := compatStringValue(tool["description"]); strings.TrimSpace(fallback) != "" {
		normalizedFunction["description"] = fallback
	}

	parameters, parametersRaw := deepSeekToolParameters(function, tool)
	if keepStrict {
		normalizedFunction["parameters"] = schemaValueFromString(util.CleanJSONSchemaForOpenAIStructuredOutput(parametersRaw))
		normalizedFunction["strict"] = true
	} else {
		normalizedFunction["parameters"] = parameters
	}

	return map[string]any{
		"type":     "function",
		"function": normalizedFunction,
	}, true
}

func openAICompatOriginalFunctionName(rawTool any) string {
	tool, ok := rawTool.(map[string]any)
	if !ok {
		return ""
	}
	if function, okFunction := tool["function"].(map[string]any); okFunction {
		if name := strings.TrimSpace(compatStringValue(function["name"])); name != "" {
			return name
		}
	}
	return strings.TrimSpace(compatStringValue(tool["name"]))
}

func openAICompatNormalizedFunctionName(tool map[string]any) string {
	if tool == nil {
		return ""
	}
	function, ok := tool["function"].(map[string]any)
	if !ok {
		return ""
	}
	return strings.TrimSpace(compatStringValue(function["name"]))
}

func rewriteOpenAICompatFunctionNameReferences(root map[string]any, mapping map[string]string) bool {
	if len(mapping) == 0 {
		return false
	}
	changed := false
	rename := func(value any) (string, bool) {
		name := strings.TrimSpace(compatStringValue(value))
		if name == "" {
			return "", false
		}
		mapped := mapping[name]
		if mapped == "" || mapped == name {
			return "", false
		}
		return mapped, true
	}

	if toolChoice, ok := root["tool_choice"].(map[string]any); ok {
		if mapped, okMap := rename(toolChoice["name"]); okMap {
			toolChoice["name"] = mapped
			changed = true
		}
		if function, okFunction := toolChoice["function"].(map[string]any); okFunction {
			if mapped, okMap := rename(function["name"]); okMap {
				function["name"] = mapped
				changed = true
			}
		}
	}

	messages, ok := root["messages"].([]any)
	if !ok {
		return changed
	}
	for _, rawMessage := range messages {
		message, okMessage := rawMessage.(map[string]any)
		if !okMessage {
			continue
		}
		toolCalls, okToolCalls := message["tool_calls"].([]any)
		if !okToolCalls {
			continue
		}
		for _, rawToolCall := range toolCalls {
			toolCall, okToolCall := rawToolCall.(map[string]any)
			if !okToolCall {
				continue
			}
			function, okFunction := toolCall["function"].(map[string]any)
			if !okFunction {
				continue
			}
			if mapped, okMap := rename(function["name"]); okMap {
				function["name"] = mapped
				changed = true
			}
		}
	}
	return changed
}

func deepSeekFunctionToolNode(tool map[string]any) map[string]any {
	if function, ok := tool["function"].(map[string]any); ok {
		return function
	}
	if _, hasName := tool["name"]; !hasName {
		return nil
	}
	if _, hasInputSchema := tool["input_schema"]; hasInputSchema {
		return tool
	}
	if _, hasParameters := tool["parameters"]; hasParameters {
		return tool
	}
	if toolType := strings.TrimSpace(compatStringValue(tool["type"])); toolType == "" || toolType == "function" {
		return tool
	}
	return nil
}

func deepSeekToolParameters(function map[string]any, tool map[string]any) (any, string) {
	for _, candidate := range []any{
		function["parameters"],
		function["parametersJsonSchema"],
		tool["parameters"],
		tool["input_schema"],
		tool["parametersJsonSchema"],
	} {
		if candidate == nil {
			continue
		}
		normalized := normalizeOpenAICompatParameters(candidate)
		raw, err := json.Marshal(normalized)
		if err != nil || !gjson.ValidBytes(raw) {
			continue
		}
		return normalized, string(raw)
	}
	defaultSchema := map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
	raw, _ := json.Marshal(defaultSchema)
	return defaultSchema, string(raw)
}

func normalizeOpenAICompatParameters(parameters any) any {
	normalized, ok := normalizeOpenAICompatParameterNode(parameters)
	if !ok {
		return map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		}
	}
	return normalized
}

func normalizeOpenAICompatParameterNode(parameters any) (map[string]any, bool) {
	if raw, ok := parameters.(string); ok {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return nil, false
		}
		if schemaType, okType := normalizeOpenAICompatSchemaType(raw); okType {
			return openAICompatSchemaForType(schemaType), true
		}
		if !gjson.Valid(raw) {
			return nil, false
		}
		var parsed any
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			return nil, false
		}
		return normalizeOpenAICompatSchemaNode(parsed)
	}
	return normalizeOpenAICompatSchemaNode(parameters)
}

func normalizeOpenAICompatSchemaNode(node any) (map[string]any, bool) {
	schema, ok := node.(map[string]any)
	if !ok {
		if schemaType, okType := normalizeOpenAICompatScalarSchemaType(node); okType {
			return openAICompatSchemaForType(schemaType), true
		}
		return nil, false
	}

	out := make(map[string]any, len(schema)+2)
	for key, value := range schema {
		if value == nil {
			continue
		}
		switch key {
		case "properties":
			if properties, okProperties := value.(map[string]any); okProperties {
				cleanedProperties := make(map[string]any, len(properties))
				for propertyName, rawProperty := range properties {
					if propertyName = strings.TrimSpace(propertyName); propertyName == "" {
						continue
					}
					if normalizedProperty, okNormalize := normalizeOpenAICompatSchemaNode(rawProperty); okNormalize {
						cleanedProperties[propertyName] = normalizedProperty
					} else {
						cleanedProperties[propertyName] = map[string]any{"type": "string"}
					}
				}
				out[key] = cleanedProperties
			} else {
				out[key] = map[string]any{}
			}
		case "items":
			switch items := value.(type) {
			case []any:
				cleanedItems := make([]any, 0, len(items))
				for _, rawItem := range items {
					if normalizedItem, okNormalize := normalizeOpenAICompatSchemaNode(rawItem); okNormalize {
						cleanedItems = append(cleanedItems, normalizedItem)
					}
				}
				if len(cleanedItems) > 0 {
					out[key] = cleanedItems
				}
			default:
				if normalizedItem, okNormalize := normalizeOpenAICompatSchemaNode(items); okNormalize {
					out[key] = normalizedItem
				}
			}
		case "additionalProperties":
			switch additionalProperties := value.(type) {
			case bool:
				out[key] = additionalProperties
			default:
				if normalizedAdditional, okNormalize := normalizeOpenAICompatSchemaNode(additionalProperties); okNormalize {
					out[key] = normalizedAdditional
				}
			}
		case "required":
			if required := normalizeOpenAICompatStringArray(value); len(required) > 0 {
				out[key] = required
			}
		case "anyOf", "oneOf", "allOf":
			if branches, okBranches := value.([]any); okBranches {
				cleanedBranches := make([]any, 0, len(branches))
				for _, rawBranch := range branches {
					if normalizedBranch, okNormalize := normalizeOpenAICompatSchemaNode(rawBranch); okNormalize {
						cleanedBranches = append(cleanedBranches, normalizedBranch)
					}
				}
				if len(cleanedBranches) > 0 {
					out[key] = cleanedBranches
				}
			}
		case "type":
			if schemaType, okType := normalizeOpenAICompatScalarSchemaType(value); okType {
				out[key] = schemaType
			}
		default:
			out[key] = value
		}
	}
	schemaType := strings.TrimSpace(compatStringValue(out["type"]))
	if schemaType == "" {
		schemaType = "object"
		out["type"] = schemaType
	}
	if schemaType == "object" {
		if _, okProperties := out["properties"]; !okProperties {
			out["properties"] = map[string]any{}
		}
	}
	if schemaType == "array" {
		if _, okItems := out["items"]; !okItems {
			out["items"] = map[string]any{"type": "string"}
		}
	}
	if _, hasAnyOf := out["anyOf"]; hasAnyOf {
		delete(out, "type")
	}
	if _, hasOneOf := out["oneOf"]; hasOneOf {
		delete(out, "type")
	}
	return out, true
}

func normalizeOpenAICompatScalarSchemaType(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return normalizeOpenAICompatSchemaType(typed)
	case []any:
		for _, item := range typed {
			if str, ok := item.(string); ok {
				if schemaType, okType := normalizeOpenAICompatSchemaType(str); okType && schemaType != "null" {
					return schemaType, true
				}
			}
		}
		return "", false
	default:
		return "", false
	}
}

func normalizeOpenAICompatSchemaType(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "object", "array", "string", "number", "integer", "boolean", "null":
		return strings.ToLower(strings.TrimSpace(raw)), true
	default:
		return "", false
	}
}

func openAICompatSchemaForType(schemaType string) map[string]any {
	schema := map[string]any{"type": schemaType}
	switch schemaType {
	case "object":
		schema["properties"] = map[string]any{}
	case "array":
		schema["items"] = map[string]any{"type": "string"}
	}
	return schema
}

func normalizeOpenAICompatStringArray(value any) []string {
	switch typed := value.(type) {
	case []string:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if item = strings.TrimSpace(item); item != "" {
				out = append(out, item)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if str, ok := item.(string); ok {
				if str = strings.TrimSpace(str); str != "" {
					out = append(out, str)
				}
			}
		}
		return out
	default:
		return nil
	}
}

func schemaValueFromString(raw string) any {
	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		}
	}
	return parsed
}

func compatStringValue(value any) string {
	str, _ := value.(string)
	return str
}

func normalizeOpenAICompatFunctionName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false
	}

	var builder strings.Builder
	builder.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			builder.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			builder.WriteRune(r)
		case r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '_' || r == '-':
			builder.WriteRune(r)
		default:
			builder.WriteByte('_')
		}
	}

	normalized := builder.String()
	if normalized == "" {
		return "", false
	}
	first := normalized[0]
	if !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z') || first == '_') {
		normalized = "_" + normalized
	}
	if len(normalized) > 64 {
		normalized = normalized[:64]
	}
	for len(normalized) < 3 {
		normalized += "_"
	}
	return normalized, true
}

func jsonValuesEqual(left any, right any) bool {
	leftJSON, errLeft := json.Marshal(left)
	rightJSON, errRight := json.Marshal(right)
	return errLeft == nil && errRight == nil && string(leftJSON) == string(rightJSON)
}

func deleteMessageReasoningContent(payload []byte) []byte {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return payload
	}
	messages.ForEach(func(key, value gjson.Result) bool {
		if !value.Get("reasoning_content").Exists() {
			return true
		}
		updated := value.Raw
		if next, err := sjson.Delete(updated, "reasoning_content"); err == nil {
			updated = next
		}
		if nextPayload, err := sjson.SetRawBytes(payload, fmt.Sprintf("messages.%s", key.String()), []byte(updated)); err == nil {
			payload = nextPayload
		}
		return true
	})
	return payload
}

func summarizeOpenAICompatError(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return "empty upstream response"
	}
	jsonBody := openAICompatJSONErrorBody(body)
	if !gjson.ValidBytes(jsonBody) {
		return trimmed
	}
	message := firstNonEmptyJSONValue(jsonBody,
		"error.message",
		"message",
		"msg",
		"error.msg",
		"detail",
		"error.detail",
		"reason",
		"error.reason",
		"error.metadata.message",
		"error.metadata.reason",
		"error.details.0.message",
		"error.details.0.reason",
		"error.details.0.description",
	)
	if message == "" {
		return trimmed
	}
	label := firstNonEmptyJSONValue(jsonBody, "error.type", "type", "error.code", "code", "error.err_code")
	if label == "" {
		return message
	}
	lowerMessage := strings.ToLower(message)
	lowerLabel := strings.ToLower(label)
	if strings.Contains(lowerMessage, lowerLabel) {
		return message
	}
	return label + ": " + message
}

func openAICompatJSONErrorBody(body []byte) []byte {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" || gjson.Valid(trimmed) {
		return []byte(trimmed)
	}
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToLower(line), "data:") {
			continue
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "" || payload == "[DONE]" {
			continue
		}
		if gjson.Valid(payload) {
			return []byte(payload)
		}
	}
	return body
}

func firstNonEmptyJSONValue(body []byte, paths ...string) string {
	for _, path := range paths {
		value := gjson.GetBytes(body, path)
		if !value.Exists() {
			continue
		}
		switch value.Type {
		case gjson.String:
			if trimmed := strings.TrimSpace(value.String()); trimmed != "" {
				return trimmed
			}
		case gjson.Number:
			if raw := strings.TrimSpace(value.Raw); raw != "" {
				return raw
			}
		}
	}
	return ""
}

func openAICompatRetryAfter(headers http.Header, body []byte) *time.Duration {
	now := time.Now()
	if headers != nil {
		if retry := parseOpenAICompatRetryAfterString(headers.Get("Retry-After"), false, now); retry != nil {
			return retry
		}
	}

	candidates := []struct {
		path      string
		timestamp bool
	}{
		{path: "retry_after"},
		{path: "retryAfter"},
		{path: "retry_after_seconds"},
		{path: "retryAfterSeconds"},
		{path: "retry_delay"},
		{path: "retryDelay"},
		{path: "reset_after"},
		{path: "resetAfter"},
		{path: "reset_in"},
		{path: "resetIn"},
		{path: "reset_in_seconds"},
		{path: "resetInSeconds"},
		{path: "cooldown"},
		{path: "cooldown_seconds"},
		{path: "cooldownSeconds"},
		{path: "error.retry_after"},
		{path: "error.retryAfter"},
		{path: "error.retry_after_seconds"},
		{path: "error.retryAfterSeconds"},
		{path: "error.retry_delay"},
		{path: "error.retryDelay"},
		{path: "error.reset_after"},
		{path: "error.resetAfter"},
		{path: "error.reset_in"},
		{path: "error.resetIn"},
		{path: "error.reset_in_seconds"},
		{path: "error.resetInSeconds"},
		{path: "error.cooldown"},
		{path: "error.cooldown_seconds"},
		{path: "error.cooldownSeconds"},
		{path: "error.metadata.retry_after"},
		{path: "error.metadata.retry_after_seconds"},
		{path: "error.metadata.retryDelay"},
		{path: "error.metadata.reset_after"},
		{path: "error.metadata.reset_in_seconds"},
		{path: "retry_at", timestamp: true},
		{path: "retryAt", timestamp: true},
		{path: "reset_at", timestamp: true},
		{path: "resetAt", timestamp: true},
		{path: "error.retry_at", timestamp: true},
		{path: "error.retryAt", timestamp: true},
		{path: "error.reset_at", timestamp: true},
		{path: "error.resetAt", timestamp: true},
		{path: "error.metadata.retry_at", timestamp: true},
		{path: "error.metadata.retryAt", timestamp: true},
		{path: "error.metadata.reset_at", timestamp: true},
		{path: "error.metadata.resetAt", timestamp: true},
	}
	for _, candidate := range candidates {
		value := gjson.GetBytes(body, candidate.path)
		if !value.Exists() {
			continue
		}
		if retry := parseOpenAICompatRetryAfterValue(value, candidate.timestamp, now); retry != nil {
			return retry
		}
	}
	if openAICompatAccountQuotaLikeMessage(strings.ToLower(summarizeOpenAICompatError(body))) {
		duration := openAICompatAccountQuotaRetryWait
		return &duration
	}
	return nil
}

func parseOpenAICompatRetryAfterValue(value gjson.Result, timestamp bool, now time.Time) *time.Duration {
	switch value.Type {
	case gjson.String:
		return parseOpenAICompatRetryAfterString(value.String(), timestamp, now)
	case gjson.Number:
		number := value.Float()
		if number <= 0 {
			return nil
		}
		if timestamp {
			return durationUntilUnix(number, now)
		}
		duration := time.Duration(number * float64(time.Second))
		if duration <= 0 {
			return nil
		}
		return &duration
	default:
		return nil
	}
}

func parseOpenAICompatRetryAfterString(raw string, timestamp bool, now time.Time) *time.Duration {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	if parsed, err := strconv.ParseFloat(trimmed, 64); err == nil {
		if parsed <= 0 {
			return nil
		}
		if timestamp {
			return durationUntilUnix(parsed, now)
		}
		duration := time.Duration(parsed * float64(time.Second))
		if duration <= 0 {
			return nil
		}
		return &duration
	}
	if duration, err := time.ParseDuration(trimmed); err == nil && duration > 0 {
		return &duration
	}
	for _, layout := range []string{time.RFC3339, time.RFC3339Nano, http.TimeFormat} {
		if parsed, err := time.Parse(layout, trimmed); err == nil {
			duration := time.Until(parsed)
			if duration > 0 {
				return &duration
			}
		}
	}
	if timestamp {
		return nil
	}
	if parsed, err := http.ParseTime(trimmed); err == nil {
		duration := parsed.Sub(now)
		if duration > 0 {
			return &duration
		}
	}
	return nil
}

func durationUntilUnix(value float64, now time.Time) *time.Duration {
	if value <= 0 {
		return nil
	}
	var target time.Time
	switch {
	case value >= 1e12:
		target = time.UnixMilli(int64(value))
	case value >= 1e9:
		target = time.Unix(int64(value), 0)
	default:
		return nil
	}
	duration := target.Sub(now)
	if duration <= 0 {
		return nil
	}
	return &duration
}

func normalizeOpenAICompatStatus(code int, message string) int {
	lower := strings.ToLower(strings.TrimSpace(message))
	switch {
	case openAICompatPaymentLikeMessage(lower) && code != http.StatusPaymentRequired && code != http.StatusForbidden:
		return http.StatusPaymentRequired
	case openAICompatQuotaLikeMessage(lower) && code != http.StatusTooManyRequests:
		return http.StatusTooManyRequests
	case openAICompatAvailabilityMessage(lower) && (code == http.StatusBadRequest || code == http.StatusForbidden):
		return http.StatusServiceUnavailable
	default:
		return code
	}
}

func openAICompatPaymentLikeMessage(message string) bool {
	return containsAny(message,
		"payment required",
		"insufficient balance",
		"balance insufficient",
		"account balance insufficient",
		"余额不足",
		"账户余额不足",
		"帐户余额不足",
		"钱包余额不足",
		"充值后重试",
	)
}

func openAICompatQuotaLikeMessage(message string) bool {
	if openAICompatAccountQuotaLikeMessage(message) {
		return true
	}
	return containsAny(message,
		"insufficient_quota",
		"quota exhausted",
		"quota_exhausted",
		"rate limit",
		"rate_limit",
		"too many requests",
		"resource exhausted",
		"额度已用尽",
		"额度不足",
		"频率限制",
	)
}

func openAICompatAccountQuotaLikeMessage(message string) bool {
	return containsAny(message,
		"usage limit",
		"billing cycle",
		"quota will be refreshed",
		"refreshed in the next cycle",
		"quota-upgrade",
		"monthly quota",
	)
}

func openAICompatAvailabilityMessage(message string) bool {
	return containsAny(message,
		"no available key",
		"no available api key",
		"no available channel",
		"channel unavailable",
		"upstream unavailable",
		"provider unavailable",
		"no healthy upstream",
		"no available upstream",
		"无可用 key",
		"无可用key",
		"无可用渠道",
		"渠道不可用",
		"上游不可用",
	)
}

func containsAny(message string, patterns ...string) bool {
	for _, pattern := range patterns {
		if pattern != "" && strings.Contains(message, pattern) {
			return true
		}
	}
	return false
}

func logOpenAICompatUpstreamError(profile openAICompatProfile, auth *cliproxyauth.Auth, routeModel string, statusCode int, retryAfter *time.Duration, contentType string, body []byte) {
	entry := log.WithFields(log.Fields{
		"provider":           profile.KindOrFallback(auth),
		"compat_kind":        profile.Kind,
		"compat_kind_source": openAICompatKindSource(profile, auth),
		"compat_mapping":     openAICompatMapping(profile, routeModel),
		"model":              strings.TrimSpace(routeModel),
		"status":             statusCode,
	})
	if auth != nil {
		if authID := strings.TrimSpace(auth.ID); authID != "" {
			entry = entry.WithField("auth_id", authID)
		}
		if compatName := strings.TrimSpace(auth.Attributes["compat_name"]); compatName != "" {
			entry = entry.WithField("compat_name", compatName)
		}
	}
	if retryAfter != nil {
		entry = entry.WithField("retry_after", retryAfter.String())
	}
	summary := helps.SummarizeErrorBody(contentType, body)
	if strings.TrimSpace(summary) == "" && strings.TrimSpace(string(body)) == "" {
		summary = "empty upstream response"
	}
	entry.Warnf("openai compat upstream error: %s", summary)
}

func newOpenAICompatStatusErr(profile openAICompatProfile, auth *cliproxyauth.Auth, routeModel string, statusCode int, headers http.Header, contentType string, body []byte) statusErr {
	retryAfter := openAICompatRetryAfter(headers, body)
	logOpenAICompatUpstreamError(profile, auth, routeModel, statusCode, retryAfter, contentType, body)
	jsonBody := openAICompatJSONErrorBody(body)
	message := summarizeOpenAICompatError(body)
	errorCode := firstNonEmptyJSONValue(jsonBody, "error.code", "code", "error.type", "type", "error.err_code")
	if errorCode == "" && strings.TrimSpace(string(body)) == "" {
		errorCode = openAICompatEmptyUpstreamResponseCode
	}
	return statusErr{
		code:               normalizeOpenAICompatStatus(statusCode, message),
		providerStatusCode: statusCode,
		msg:                message,
		errorCode:          errorCode,
		retryAfter:         retryAfter,
	}
}

func (p openAICompatProfile) KindOrFallback(auth *cliproxyauth.Auth) string {
	if p.Kind != "" {
		return p.Kind
	}
	if auth != nil {
		if auth.Attributes != nil {
			if providerKey := strings.TrimSpace(auth.Attributes["provider_key"]); providerKey != "" {
				return providerKey
			}
		}
		if provider := strings.TrimSpace(auth.Provider); provider != "" {
			return provider
		}
	}
	return "openai-compatibility"
}
