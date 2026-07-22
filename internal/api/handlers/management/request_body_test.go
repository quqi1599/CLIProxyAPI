package management

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usage"
	sdkhandlers "github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestDecodeManagementJSONBodyIsBoundedAndRejectsTrailingValues(t *testing.T) {
	t.Run("known oversize", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/plugins/test", strings.NewReader(`{}`))
		c.Request.ContentLength = maxManagementJSONBodyBytes + 1

		var body map[string]any
		err := decodeManagementJSONBody(c, maxManagementJSONBodyBytes, &body)
		if !sdkhandlers.IsRequestBodyTooLarge(err) {
			t.Fatalf("error = %v, want request-too-large", err)
		}
	})

	t.Run("trailing JSON", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/plugins/test", strings.NewReader(`{} {}`))

		var body map[string]any
		if err := decodeManagementJSONBody(c, maxManagementJSONBodyBytes, &body); err == nil {
			t.Fatal("decodeManagementJSONBody() error = nil, want trailing-value rejection")
		}
	})
}

func TestManagementMultipartUploadsRejectKnownOversizeBeforeParsing(t *testing.T) {
	tests := []struct {
		name   string
		limit  int64
		handle func(*Handler, *gin.Context)
	}{
		{name: "auth files", limit: maxManagementAuthUploadBodyBytes, handle: func(h *Handler, c *gin.Context) { h.UploadAuthFile(c) }},
		{name: "vertex", limit: maxVertexCredentialBodyBytes, handle: func(h *Handler, c *gin.Context) { h.ImportVertexCredential(c) }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := NewHandlerWithoutConfigFilePath(
				&config.Config{AuthDir: t.TempDir()},
				coreauth.NewManager(nil, nil, nil),
			)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/upload", strings.NewReader("body"))
			c.Request.ContentLength = test.limit + 1
			c.Request.Header.Set("Content-Type", "multipart/form-data; boundary=test")

			test.handle(handler, c)

			if recorder.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusRequestEntityTooLarge, recorder.Body.String())
			}
			if got := gjson.GetBytes(recorder.Body.Bytes(), "error").String(); got != "request_too_large" {
				t.Fatalf("error = %q", got)
			}
		})
	}
}

func TestOAuthCallbackRejectsKnownOversizeBeforeStateValidation(t *testing.T) {
	handler := NewHandlerWithoutConfigFilePath(
		&config.Config{AuthDir: t.TempDir()},
		coreauth.NewManager(nil, nil, nil),
	)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/oauth-callback", strings.NewReader(`{}`))
	c.Request.ContentLength = maxManagementJSONBodyBytes + 1

	handler.PostOAuthCallback(c)

	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusRequestEntityTooLarge, recorder.Body.String())
	}
}

func TestManagementJSONHandlersRejectKnownOversize(t *testing.T) {
	handler := &Handler{
		cfg:         &config.Config{},
		authManager: coreauth.NewManager(nil, nil, nil),
		usageStats:  usage.NewRequestStatistics(),
	}
	tests := []struct {
		name   string
		limit  int64
		handle func(*gin.Context)
	}{
		{name: "generic field", limit: maxManagementJSONBodyBytes, handle: func(c *gin.Context) { handler.updateBoolField(c, func(bool) {}) }},
		{name: "config field", limit: maxManagementConfigBodyBytes, handle: handler.PutLogsMaxTotalSizeMB},
		{name: "config list", limit: maxManagementConfigBodyBytes, handle: func(c *gin.Context) { handler.putStringList(c, func([]string) {}, nil) }},
		{name: "auth file", limit: maxManagementJSONBodyBytes, handle: handler.PatchAuthFileStatus},
		{name: "plugin", limit: maxManagementJSONBodyBytes, handle: func(c *gin.Context) {
			c.Params = gin.Params{{Key: "id", Value: "test-plugin"}}
			handler.PatchPluginEnabled(c)
		}},
		{name: "usage", limit: maxManagementJSONBodyBytes, handle: handler.ImportUsageStatistics},
		{name: "quota", limit: maxManagementJSONBodyBytes, handle: handler.ResetQuota},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/test", strings.NewReader(`{}`))
			c.Request.ContentLength = test.limit + 1

			test.handle(c)

			if recorder.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusRequestEntityTooLarge, recorder.Body.String())
			}
			if got := gjson.GetBytes(recorder.Body.Bytes(), "error").String(); got != "request_too_large" {
				t.Fatalf("error = %q, want request_too_large", got)
			}
		})
	}
}
