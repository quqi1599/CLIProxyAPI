package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	failurecontract "github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestExecutionErrorMessageIncludesTypedRetryAfter(t *testing.T) {
	retryAfter := 1500 * time.Millisecond
	errMsg := executionErrorMessage(&failurecontract.Failure{
		Kind:          failurecontract.ProviderUnavailable,
		Scope:         failurecontract.ScopeModel,
		HTTPStatus:    http.StatusServiceUnavailable,
		SemanticCode:  "compaction_route_unavailable",
		RetryAfter:    &retryAfter,
		Retryable:     true,
		PublicMessage: "compaction_route_unavailable: compatible route is cooling",
	})

	if errMsg.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("StatusCode = %d, want 503", errMsg.StatusCode)
	}
	if got := errMsg.Addon.Get("Retry-After"); got != "2" {
		t.Fatalf("Retry-After = %q, want 2", got)
	}

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	NewBaseAPIHandlers(nil, nil).WriteErrorResponse(c, errMsg)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("response status = %d, want 503", recorder.Code)
	}
	if got := recorder.Header().Get("Retry-After"); got != "2" {
		t.Fatalf("response Retry-After = %q, want 2", got)
	}
	var payload ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.Error.Code != "compaction_route_unavailable" {
		t.Fatalf("response error code = %q, want compaction_route_unavailable", payload.Error.Code)
	}
}

func TestWriteErrorResponse_OnlyRetryAfterAllowedByDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	handler := NewBaseAPIHandlers(nil, nil)
	handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      errors.New("rate limit"),
		Addon: http.Header{
			"Retry-After":   {"30"},
			"Authorization": {"Bearer secret"},
			"Cookie":        {"session=secret"},
			"X-Request-Id":  {"req-1"},
		},
	})

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusTooManyRequests)
	}
	if got := recorder.Header().Get("Retry-After"); got != "30" {
		t.Fatalf("Retry-After = %q, want 30", got)
	}
	for _, key := range []string{"Authorization", "Cookie", "X-Request-Id"} {
		if got := recorder.Header().Get(key); got != "" {
			t.Fatalf("%s leaked while passthrough is disabled: %q", key, got)
		}
	}
}

func TestWriteErrorResponse_AddonHeadersEnabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Writer.Header().Set("X-Request-Id", "old-value")

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{PassthroughHeaders: true}, nil)
	handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      errors.New("rate limit"),
		Addon: http.Header{
			"Retry-After":  {"30"},
			"X-Request-Id": {"new-1", "new-2"},
		},
	})

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusTooManyRequests)
	}
	if got := recorder.Header().Get("Retry-After"); got != "30" {
		t.Fatalf("Retry-After = %q, want %q", got, "30")
	}
	if got := recorder.Header().Values("X-Request-Id"); !reflect.DeepEqual(got, []string{"new-1", "new-2"}) {
		t.Fatalf("X-Request-Id = %#v, want %#v", got, []string{"new-1", "new-2"})
	}
}

func TestWriteErrorResponse_NormalizesMiniMaxInputNewSensitiveStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	handler := NewBaseAPIHandlers(nil, nil)
	handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
		StatusCode: http.StatusInternalServerError,
		Error:      errors.New("status_code=500, input new_sensitive (1026)"),
	})

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}

	var payload ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Error.Message != UserFacingContentSafetyMessage("input") {
		t.Fatalf("message = %q, want %q", payload.Error.Message, UserFacingContentSafetyMessage("input"))
	}
	if payload.Error.Type != "invalid_request_error" {
		t.Fatalf("type = %q, want invalid_request_error", payload.Error.Type)
	}
	if payload.Error.Code != contentPolicyViolationErrorCode {
		t.Fatalf("code = %q, want %q", payload.Error.Code, contentPolicyViolationErrorCode)
	}
}

func TestWriteErrorResponse_NormalizesContentSafety1301Status(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	handler := NewBaseAPIHandlers(nil, nil)
	handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
		StatusCode: http.StatusBadGateway,
		Error:      errors.New("status_code=502, claude executor: upstream returned error event: [1301] content violates safety policy"),
	})

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}

	var payload ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Error.Message != UserFacingContentSafetyMessage("input") {
		t.Fatalf("message = %q, want %q", payload.Error.Message, UserFacingContentSafetyMessage("input"))
	}
	if payload.Error.Type != "invalid_request_error" {
		t.Fatalf("type = %q, want invalid_request_error", payload.Error.Type)
	}
	if payload.Error.Code != contentPolicyViolationErrorCode {
		t.Fatalf("code = %q, want %q", payload.Error.Code, contentPolicyViolationErrorCode)
	}
}

func TestWriteErrorResponse_NormalizesCapturedKnownUserErrorBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name string
		body string
		code string
	}{
		{
			name: "content safety hidden behind api error",
			body: `{"error":{"message":"[1301] content violates safety policy","code":"1301"}}`,
			code: contentPolicyViolationErrorCode,
		},
		{
			name: "aliyun data inspection hidden behind api error",
			body: `{"error":{"message":"Input data may contain inappropriate content.","code":"data_inspection_failed"}}`,
			code: contentPolicyViolationErrorCode,
		},
		{
			name: "unsupported request shape hidden behind api error",
			body: `{"error":{"message":"request_feature_unsupported: large_claude_tool_history cannot be safely routed through MiniMax compatibility","code":"request_feature_unsupported"}}`,
			code: requestFeatureUnsupportedErrorCode,
		},
		{
			name: "invalid parameter hidden behind api error",
			body: `{"error":{"message":"InvalidParameter: reasoning_effort xhigh is not supported","code":"InvalidParameter"}}`,
			code: "invalid_request_parameters",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			c.Set("API_RESPONSE", []byte(tc.body))

			handler := NewBaseAPIHandlers(nil, nil)
			handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
				StatusCode: http.StatusInternalServerError,
				Error:      errors.New("status_code=500, api_error"),
			})

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
			}
			var payload ErrorResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if payload.Error.Code != tc.code {
				t.Fatalf("code = %q, want %q", payload.Error.Code, tc.code)
			}
			if payload.Error.Type != "invalid_request_error" {
				t.Fatalf("type = %q, want invalid_request_error", payload.Error.Type)
			}
		})
	}
}

func TestWriteErrorResponse_NormalizesGenericChineseContentSafety(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	handler := NewBaseAPIHandlers(nil, nil)
	handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
		StatusCode: http.StatusBadRequest,
		Error:      errors.New("status_code=400, 有敏感内容，请勿重复请求"),
	})

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}

	var payload ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Error.Message != UserFacingContentSafetyMessage("input") {
		t.Fatalf("message = %q, want %q", payload.Error.Message, UserFacingContentSafetyMessage("input"))
	}
	if payload.Error.Type != "invalid_request_error" {
		t.Fatalf("type = %q, want invalid_request_error", payload.Error.Type)
	}
	if payload.Error.Code != contentPolicyViolationErrorCode {
		t.Fatalf("code = %q, want %q", payload.Error.Code, contentPolicyViolationErrorCode)
	}
}

func TestWriteErrorResponse_NormalizesRequestFeatureUnsupportedStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	handler := NewBaseAPIHandlers(nil, nil)
	handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
		StatusCode: http.StatusInternalServerError,
		Error:      errors.New("status_code=500, request_feature_unsupported: large_claude_tool_history cannot be safely routed through MiniMax/Step Anthropic compatibility"),
	})

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}

	var payload ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Error.Message != UserFacingRequestFeatureUnsupportedMessage() {
		t.Fatalf("message = %q, want %q", payload.Error.Message, UserFacingRequestFeatureUnsupportedMessage())
	}
	if payload.Error.Type != requestFeatureUnsupportedErrorType {
		t.Fatalf("type = %q, want %q", payload.Error.Type, requestFeatureUnsupportedErrorType)
	}
	if payload.Error.Code != requestFeatureUnsupportedErrorCode {
		t.Fatalf("code = %q, want %q", payload.Error.Code, requestFeatureUnsupportedErrorCode)
	}
}

func TestEnrichAuthSelectionError_DefaultsTo503WithConciseMessage(t *testing.T) {
	in := &coreauth.Error{Code: "auth_not_found", Message: "no auth available"}
	out := enrichAuthSelectionError(in, []string{"claude"}, "claude-sonnet-4-6")

	var got *coreauth.Error
	if !errors.As(out, &got) || got == nil {
		t.Fatalf("expected coreauth.Error, got %T", out)
	}
	if got.StatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", got.StatusCode(), http.StatusServiceUnavailable)
	}
	if got.Message != "requested route is temporarily unavailable" {
		t.Fatalf("message = %q, want %q", got.Message, "requested route is temporarily unavailable")
	}
}

func TestEnrichAuthSelectionError_PreservesExplicitStatus(t *testing.T) {
	in := &coreauth.Error{Code: "auth_unavailable", Message: "no auth available", HTTPStatus: http.StatusTooManyRequests}
	out := enrichAuthSelectionError(in, []string{"gemini"}, "gemini-2.5-pro")

	var got *coreauth.Error
	if !errors.As(out, &got) || got == nil {
		t.Fatalf("expected coreauth.Error, got %T", out)
	}
	if got.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", got.StatusCode(), http.StatusTooManyRequests)
	}
}

func TestEnrichAuthSelectionError_IgnoresOtherErrors(t *testing.T) {
	in := errors.New("boom")
	out := enrichAuthSelectionError(in, []string{"claude"}, "claude-sonnet-4-6")
	if out != in {
		t.Fatalf("expected original error to be returned unchanged")
	}
}

func TestBuildErrorResponseBody_NormalizesContextWindowPlainText(t *testing.T) {
	body := BuildErrorResponseBody(http.StatusBadRequest, "bad_request_error: invalid params, context window exceeds limit (2013)")

	var payload ErrorResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Error.Message != UserFacingContextWindowMessage() {
		t.Fatalf("message = %q, want %q", payload.Error.Message, UserFacingContextWindowMessage())
	}
	if payload.Error.Type != contextWindowExceededErrorType {
		t.Fatalf("type = %q, want %q", payload.Error.Type, contextWindowExceededErrorType)
	}
	if payload.Error.Code != contextWindowExceededErrorCode {
		t.Fatalf("code = %q, want %q", payload.Error.Code, contextWindowExceededErrorCode)
	}
}

func TestBuildErrorResponseBody_NormalizesMiniMaxInputNewSensitive(t *testing.T) {
	body := BuildErrorResponseBody(http.StatusInternalServerError, "status_code=500, input new_sensitive (1026)")

	var payload ErrorResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Error.Message != UserFacingContentSafetyMessage("input") {
		t.Fatalf("message = %q, want %q", payload.Error.Message, UserFacingContentSafetyMessage("input"))
	}
	if payload.Error.Type != "invalid_request_error" {
		t.Fatalf("type = %q, want invalid_request_error", payload.Error.Type)
	}
	if payload.Error.Code != contentPolicyViolationErrorCode {
		t.Fatalf("code = %q, want %q", payload.Error.Code, contentPolicyViolationErrorCode)
	}
}

func TestBuildErrorResponseBody_NormalizesMiniMaxInputImageNewSensitive(t *testing.T) {
	body := BuildErrorResponseBody(http.StatusInternalServerError, "status_code=500, input new_sensitive, messages[2]'s content[1] image is sensitive, please check your input (1026)")

	var payload ErrorResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Error.Message != UserFacingContentSafetyMessage("input_image") {
		t.Fatalf("message = %q, want %q", payload.Error.Message, UserFacingContentSafetyMessage("input_image"))
	}
	if payload.Error.Type != "invalid_request_error" {
		t.Fatalf("type = %q, want invalid_request_error", payload.Error.Type)
	}
	if payload.Error.Code != contentPolicyViolationErrorCode {
		t.Fatalf("code = %q, want %q", payload.Error.Code, contentPolicyViolationErrorCode)
	}
}

func TestBuildErrorResponseBody_NormalizesContentSafety1301(t *testing.T) {
	body := BuildErrorResponseBody(http.StatusBadGateway, `{"error":{"message":"[1301] content violates safety policy","code":"1301"}}`)

	var payload ErrorResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Error.Message != UserFacingContentSafetyMessage("input") {
		t.Fatalf("message = %q, want %q", payload.Error.Message, UserFacingContentSafetyMessage("input"))
	}
	if payload.Error.Type != "invalid_request_error" {
		t.Fatalf("type = %q, want invalid_request_error", payload.Error.Type)
	}
	if payload.Error.Code != contentPolicyViolationErrorCode {
		t.Fatalf("code = %q, want %q", payload.Error.Code, contentPolicyViolationErrorCode)
	}
}

func TestBuildErrorResponseBody_NormalizesImageGenerationSafetyRefusal(t *testing.T) {
	errText := "upstream returned text instead of image output: 抱歉，我不能帮助生成涉及对未成年人施暴、伤害细节或带血腥结果的画面。"

	if got := NormalizeContentSafetyStatus(http.StatusBadGateway, errText); got != http.StatusBadRequest {
		t.Fatalf("normalized status = %d, want %d", got, http.StatusBadRequest)
	}

	body := BuildErrorResponseBody(http.StatusBadGateway, errText)
	var payload ErrorResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Error.Message != UserFacingContentSafetyMessage("input") {
		t.Fatalf("message = %q, want %q", payload.Error.Message, UserFacingContentSafetyMessage("input"))
	}
	if payload.Error.Type != "invalid_request_error" {
		t.Fatalf("type = %q, want invalid_request_error", payload.Error.Type)
	}
	if payload.Error.Code != contentPolicyViolationErrorCode {
		t.Fatalf("code = %q, want %q", payload.Error.Code, contentPolicyViolationErrorCode)
	}
}

func TestBuildErrorResponseBody_NormalizesRequestFeatureUnsupportedJSON(t *testing.T) {
	body := BuildErrorResponseBody(http.StatusBadRequest, `{"error":{"message":"request_feature_unsupported: minimax anthropic compatibility does not support server tool type \"web_search\"","type":"invalid_request_error","code":"request_feature_unsupported"}}`)

	var payload ErrorResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Error.Message != UserFacingRequestFeatureUnsupportedMessage() {
		t.Fatalf("message = %q, want %q", payload.Error.Message, UserFacingRequestFeatureUnsupportedMessage())
	}
	if payload.Error.Type != requestFeatureUnsupportedErrorType {
		t.Fatalf("type = %q, want %q", payload.Error.Type, requestFeatureUnsupportedErrorType)
	}
	if payload.Error.Code != requestFeatureUnsupportedErrorCode {
		t.Fatalf("code = %q, want %q", payload.Error.Code, requestFeatureUnsupportedErrorCode)
	}
}

func TestBuildErrorResponseBody_NormalizesDeepSeekOfficialImageRequestFeatureUnsupported(t *testing.T) {
	body := BuildErrorResponseBody(http.StatusBadRequest, `{"error":{"message":"request_feature_unsupported: deepseek_official_image_input. 当前 DeepSeek 官方 OpenAI Chat 路由不支持 image_url 图片内容","type":"invalid_request_error","code":"request_feature_unsupported"}}`)

	var payload ErrorResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Error.Message != userFacingDeepSeekOfficialImageInputMessage() {
		t.Fatalf("message = %q, want %q", payload.Error.Message, userFacingDeepSeekOfficialImageInputMessage())
	}
	if payload.Error.Type != requestFeatureUnsupportedErrorType {
		t.Fatalf("type = %q, want %q", payload.Error.Type, requestFeatureUnsupportedErrorType)
	}
	if payload.Error.Code != requestFeatureUnsupportedErrorCode {
		t.Fatalf("code = %q, want %q", payload.Error.Code, requestFeatureUnsupportedErrorCode)
	}
}

func TestBuildErrorResponseBody_NormalizesDeepSeekChatJSONSchemaRequestFeatureUnsupported(t *testing.T) {
	body := BuildErrorResponseBody(http.StatusBadRequest, `{"error":{"message":"request_feature_unsupported: deepseek_chat_json_schema. DeepSeek 官方 Chat Completions 不支持 json_schema","type":"invalid_request_error","code":"request_feature_unsupported"}}`)

	var payload ErrorResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Error.Message != userFacingDeepSeekChatJSONSchemaMessage() {
		t.Fatalf("message = %q, want %q", payload.Error.Message, userFacingDeepSeekChatJSONSchemaMessage())
	}
	for _, marker := range []string{"Codex", "结构化 JSON 输出", "原生 GPT 模型"} {
		if !strings.Contains(payload.Error.Message, marker) {
			t.Fatalf("message = %q, want marker %q", payload.Error.Message, marker)
		}
	}
	for _, internalTerm := range []string{"request_feature_unsupported", "deepseek_chat_json_schema", "CPA", "json_schema"} {
		if strings.Contains(payload.Error.Message, internalTerm) {
			t.Fatalf("message = %q, contains internal term %q", payload.Error.Message, internalTerm)
		}
	}
	if payload.Error.Code != requestFeatureUnsupportedErrorCode {
		t.Fatalf("code = %q, want %q", payload.Error.Code, requestFeatureUnsupportedErrorCode)
	}
}

func TestBuildErrorResponseBody_ExplainsDeepSeekResponsesToolsInCustomerLanguage(t *testing.T) {
	errText := `{"error":{"message":"request_feature_unsupported: deepseek_responses_unsupported_tools. DeepSeek V4 Pro 当前可用的 Responses 工具是：函数工具(function)、网页搜索(web_search)和补丁应用(apply_patch)。当前请求还包含不支持的：工具命名空间(namespace), 自定义工具(custom:shell)。CPA 不会静默删除或改写这些工具。","type":"invalid_request_error","code":"request_feature_unsupported"}}`
	body := BuildErrorResponseBody(http.StatusBadRequest, errText)

	var payload ErrorResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Error.Message != userFacingDeepSeekResponsesNonFunctionToolsMessage(errText) {
		t.Fatalf("message = %q, want %q", payload.Error.Message, userFacingDeepSeekResponsesNonFunctionToolsMessage(errText))
	}
	for _, marker := range []string{"Codex", "工具分组", "自定义工具", "已支持函数调用、联网搜索和补丁应用", "原生 GPT 模型"} {
		if !strings.Contains(payload.Error.Message, marker) {
			t.Fatalf("message = %q, want marker %q", payload.Error.Message, marker)
		}
	}
	for _, internalTerm := range []string{"request_feature_unsupported", "deepseek_responses_unsupported_tools", "namespace", "web_search", "apply_patch", "CPA", "Responses", "function"} {
		if strings.Contains(payload.Error.Message, internalTerm) {
			t.Fatalf("message = %q, contains internal term %q", payload.Error.Message, internalTerm)
		}
	}
	if payload.Error.Type != requestFeatureUnsupportedErrorType {
		t.Fatalf("type = %q, want %q", payload.Error.Type, requestFeatureUnsupportedErrorType)
	}
	if payload.Error.Code != requestFeatureUnsupportedErrorCode {
		t.Fatalf("code = %q, want %q", payload.Error.Code, requestFeatureUnsupportedErrorCode)
	}
}

func TestBuildErrorResponseBody_ExplainsDeepSeekCompatibilityFamilies(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		marker string
	}{
		{name: "file input", input: `{"error":{"message":"request_feature_unsupported: deepseek_official_file_input. unsupported","code":"request_feature_unsupported"}}`, marker: "文件输入"},
		{name: "responses state", input: `{"error":{"message":"request_feature_unsupported: deepseek_responses_state. unsupported","code":"request_feature_unsupported"}}`, marker: "不保存服务端会话状态"},
		{name: "fim route", input: `{"error":{"message":"request_feature_unsupported: deepseek_fim_requires_openai_compat. unsupported","code":"request_feature_unsupported"}}`, marker: "不能走 Anthropic API"},
		{name: "fim thinking", input: `{"error":{"message":"request_feature_unsupported: deepseek_fim_non_thinking_only. unsupported","code":"request_feature_unsupported"}}`, marker: "仅支持非思考模式"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := BuildErrorResponseBody(http.StatusBadRequest, test.input)
			var payload ErrorResponse
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !strings.Contains(payload.Error.Message, test.marker) {
				t.Fatalf("message = %q, want marker %q", payload.Error.Message, test.marker)
			}
			if strings.Contains(payload.Error.Message, "request_feature_unsupported") || strings.Contains(payload.Error.Message, "deepseek_") {
				t.Fatalf("message leaked internal marker: %q", payload.Error.Message)
			}
		})
	}
}

func TestBuildErrorResponseBody_NormalizesMiMoV25ProImageRequestFeatureUnsupported(t *testing.T) {
	body := BuildErrorResponseBody(http.StatusBadRequest, `{"error":{"message":"request_feature_unsupported: mimo_v2_5_pro_image_input. mimo-v2.5-pro does not support image input","type":"invalid_request_error","code":"request_feature_unsupported"}}`)

	var payload ErrorResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Error.Message != userFacingMiMoV25ProImageInputMessage() {
		t.Fatalf("message = %q, want %q", payload.Error.Message, userFacingMiMoV25ProImageInputMessage())
	}
	if payload.Error.Type != requestFeatureUnsupportedErrorType {
		t.Fatalf("type = %q, want %q", payload.Error.Type, requestFeatureUnsupportedErrorType)
	}
	if payload.Error.Code != requestFeatureUnsupportedErrorCode {
		t.Fatalf("code = %q, want %q", payload.Error.Code, requestFeatureUnsupportedErrorCode)
	}
}

func TestBuildErrorResponseBody_NormalizesOpenAICompatToolHistoryRequestFeatureUnsupported(t *testing.T) {
	body := BuildErrorResponseBody(http.StatusBadRequest, `{"error":{"message":"request_feature_unsupported: large_openai_tool_history. 历史工具调用过多","type":"invalid_request_error","code":"request_feature_unsupported"}}`)

	var payload ErrorResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Error.Message != userFacingOpenAICompatToolHistoryMessage() {
		t.Fatalf("message = %q, want %q", payload.Error.Message, userFacingOpenAICompatToolHistoryMessage())
	}
	if payload.Error.Type != requestFeatureUnsupportedErrorType {
		t.Fatalf("type = %q, want %q", payload.Error.Type, requestFeatureUnsupportedErrorType)
	}
	if payload.Error.Code != requestFeatureUnsupportedErrorCode {
		t.Fatalf("code = %q, want %q", payload.Error.Code, requestFeatureUnsupportedErrorCode)
	}
}

func TestBuildErrorResponseBody_NormalizesWorkBuddyDeepSeekTutorials(t *testing.T) {
	tests := []struct {
		name    string
		code    string
		message string
	}{
		{"complex tools", "workbuddy_deepseek_akool_complex_tools", userFacingWorkBuddyDeepSeekComplexToolsMessage()},
		{"tool history", "workbuddy_deepseek_tool_history_too_large", userFacingWorkBuddyDeepSeekToolHistoryMessage()},
		{"attachment", "workbuddy_deepseek_attachment_input", userFacingWorkBuddyDeepSeekAttachmentMessage()},
		{"content format", "workbuddy_deepseek_content_format", userFacingWorkBuddyDeepSeekContentFormatMessage()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := BuildErrorResponseBody(http.StatusBadRequest, `{"error":{"message":"request_feature_unsupported: `+test.code+`. unsupported","type":"invalid_request_error","code":"request_feature_unsupported"}}`)
			var payload ErrorResponse
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if payload.Error.Message != test.message {
				t.Fatalf("message = %q, want %q", payload.Error.Message, test.message)
			}
			for _, internal := range []string{"request_feature_unsupported", "workbuddy_deepseek_", "CPA", "reasoning_content", "tool_result"} {
				if strings.Contains(payload.Error.Message, internal) {
					t.Fatalf("message leaked internal marker %q: %s", internal, payload.Error.Message)
				}
			}
		})
	}
}

func TestBuildErrorResponseBody_NormalizesGenericClientHints(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		errText string
		code    string
		errType string
	}{
		{
			name:    "invalid parameter",
			status:  http.StatusBadRequest,
			errText: "InvalidParameter: reasoning_effort xhigh is not supported",
			code:    "invalid_request_parameters",
			errType: "invalid_request_error",
		},
		{
			name:    "quota",
			status:  http.StatusTooManyRequests,
			errText: "AccountQuotaExceeded",
			code:    "rate_limit_exceeded",
			errType: "rate_limit_error",
		},
		{
			name:    "upstream",
			status:  http.StatusBadGateway,
			errText: `{"error":{"message":"Upstream request failed","type":"server_error","code":"upstream_error"}}`,
			code:    "upstream_error",
			errType: "server_error",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := BuildErrorResponseBody(tc.status, tc.errText)

			var payload ErrorResponse
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if payload.Error.Code != tc.code {
				t.Fatalf("code = %q, want %q", payload.Error.Code, tc.code)
			}
			if payload.Error.Type != tc.errType {
				t.Fatalf("type = %q, want %q", payload.Error.Type, tc.errType)
			}
			if payload.Error.Message == "" || payload.Error.Message == tc.errText {
				t.Fatalf("message was not normalized: %q", payload.Error.Message)
			}
		})
	}
}

func TestBuildOpenAIResponsesStreamErrorChunk_NormalizesMiniMaxInputNewSensitive(t *testing.T) {
	body := BuildOpenAIResponsesStreamErrorChunk(http.StatusInternalServerError, `{"error":{"message":"input new_sensitive (1026)","code":"1026"}}`, 7)

	var payload openAIResponsesStreamErrorChunk
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Type != "error" {
		t.Fatalf("type = %q, want error", payload.Type)
	}
	if payload.SequenceNumber != 7 {
		t.Fatalf("sequence_number = %d, want 7", payload.SequenceNumber)
	}
	if payload.Message != UserFacingContentSafetyMessage("input") {
		t.Fatalf("message = %q, want %q", payload.Message, UserFacingContentSafetyMessage("input"))
	}
	if payload.Code != contentPolicyViolationErrorCode {
		t.Fatalf("code = %q, want %q", payload.Code, contentPolicyViolationErrorCode)
	}
}

func TestBuildOpenAIResponsesStreamErrorChunk_NormalizesRequestFeatureUnsupported(t *testing.T) {
	body := BuildOpenAIResponsesStreamErrorChunk(http.StatusInternalServerError, `{"error":{"message":"request_feature_unsupported: large_claude_tool_history cannot be safely routed through MiniMax/Step Anthropic compatibility","code":"request_feature_unsupported"}}`, 9)

	var payload openAIResponsesStreamErrorChunk
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Type != "error" {
		t.Fatalf("type = %q, want error", payload.Type)
	}
	if payload.SequenceNumber != 9 {
		t.Fatalf("sequence_number = %d, want 9", payload.SequenceNumber)
	}
	if payload.Message != UserFacingRequestFeatureUnsupportedMessage() {
		t.Fatalf("message = %q, want %q", payload.Message, UserFacingRequestFeatureUnsupportedMessage())
	}
	if payload.Code != requestFeatureUnsupportedErrorCode {
		t.Fatalf("code = %q, want %q", payload.Code, requestFeatureUnsupportedErrorCode)
	}
}

func TestBuildOpenAIResponsesStreamErrorChunk_NormalizesContentSafety1301(t *testing.T) {
	body := BuildOpenAIResponsesStreamErrorChunk(http.StatusBadGateway, `{"error":{"message":"[1301] content violates safety policy","code":"1301"}}`, 11)

	var payload openAIResponsesStreamErrorChunk
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Type != "error" {
		t.Fatalf("type = %q, want error", payload.Type)
	}
	if payload.SequenceNumber != 11 {
		t.Fatalf("sequence_number = %d, want 11", payload.SequenceNumber)
	}
	if payload.Message != UserFacingContentSafetyMessage("input") {
		t.Fatalf("message = %q, want %q", payload.Message, UserFacingContentSafetyMessage("input"))
	}
	if payload.Code != contentPolicyViolationErrorCode {
		t.Fatalf("code = %q, want %q", payload.Code, contentPolicyViolationErrorCode)
	}
}

func TestBuildErrorResponseBody_NormalizesContextWindowJSON(t *testing.T) {
	body := BuildErrorResponseBody(http.StatusBadRequest, `{"error":{"message":"invalid params, context window exceeds limit (2013)","type":"bad_request_error"}}`)

	var payload ErrorResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Error.Message != UserFacingContextWindowMessage() {
		t.Fatalf("message = %q, want %q", payload.Error.Message, UserFacingContextWindowMessage())
	}
	if payload.Error.Type != contextWindowExceededErrorType {
		t.Fatalf("type = %q, want %q", payload.Error.Type, contextWindowExceededErrorType)
	}
	if payload.Error.Code != contextWindowExceededErrorCode {
		t.Fatalf("code = %q, want %q", payload.Error.Code, contextWindowExceededErrorCode)
	}
}

func TestIsContextWindowExceededError_DoesNotMatchBare2013(t *testing.T) {
	if IsContextWindowExceededError(http.StatusBadRequest, "provider error (2013)") {
		t.Fatal("bare 2013 should not match context-window classification")
	}
}
