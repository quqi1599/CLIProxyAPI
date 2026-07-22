package helps

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
)

func TestRecordAPIResponseMetadataStoresHeadersWhenRequestLogDisabled(t *testing.T) {
	ctx := logging.WithResponseHeadersHolder(context.Background())
	headers := http.Header{}
	headers.Add("X-Upstream-Request-Id", "upstream-req-1")

	RecordAPIResponseMetadata(ctx, &config.Config{}, http.StatusOK, headers)
	headers.Set("X-Upstream-Request-Id", "mutated")

	got := logging.GetResponseHeaders(ctx)
	if got.Get("X-Upstream-Request-Id") != "upstream-req-1" {
		t.Fatalf("response header = %q, want %q", got.Get("X-Upstream-Request-Id"), "upstream-req-1")
	}
}

func TestAppendAPIResponseChunkKeepsSmallBodyReadable(t *testing.T) {
	t.Setenv("GIN_MODE", "test")
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	cfg := &config.Config{SDKConfig: config.SDKConfig{RequestLog: true}}

	RecordAPIResponseMetadata(ctx, cfg, http.StatusOK, http.Header{"Content-Type": {"text/event-stream"}})
	AppendAPIResponseChunk(ctx, cfg, []byte("event: message"))
	AppendAPIResponseChunk(ctx, cfg, []byte("data: {\"ok\":true}"))

	got := testAPIResponseText(t, ginCtx)
	if !strings.Contains(got, "Status: 200") {
		t.Fatalf("missing status in api response log: %s", got)
	}
	if !strings.Contains(got, "Body:\n[BODY METADATA v1]") {
		t.Fatalf("small stream body metadata missing: %s", got)
	}
	if strings.Contains(got, `{"ok":true}`) {
		t.Fatalf("small response raw body leaked: %s", got)
	}
}

func TestAppendAPIResponseChunkBoundsLargeBodyCapture(t *testing.T) {
	t.Setenv("GIN_MODE", "test")
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	cfg := &config.Config{SDKConfig: config.SDKConfig{RequestLog: true}}

	first := bytes.Repeat([]byte("A"), (32<<10)+128)
	second := bytes.Repeat([]byte("B"), (32<<10)+256)

	RecordAPIResponseMetadata(ctx, cfg, http.StatusOK, nil)
	AppendAPIResponseChunk(ctx, cfg, first)
	AppendAPIResponseChunk(ctx, cfg, second)

	got := testAPIResponseText(t, ginCtx)
	totalBytes := len(first) + len(second)
	if !strings.Contains(got, `"chunks":2`) {
		t.Fatalf("expected logical chunk count in bounded marker, got: %s", got)
	}
	if !strings.Contains(got, `"bytes":`+strconv.Itoa(totalBytes)) {
		t.Fatalf("expected total byte count %d in bounded marker, got: %s", totalBytes, got)
	}
	if strings.Contains(got, strings.Repeat("A", 64)) || strings.Contains(got, strings.Repeat("B", 64)) {
		t.Fatalf("raw head/tail bytes leaked: %s", got)
	}
	if !strings.Contains(got, `"sha256":`) {
		t.Fatalf("expected digest in bounded marker, got: %s", got)
	}
}

func TestAppendAPIResponseChunkDefersAggregateRenderUntilMaterialized(t *testing.T) {
	t.Setenv("GIN_MODE", "test")
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	cfg := &config.Config{SDKConfig: config.SDKConfig{RequestLog: true}}

	RecordAPIResponseMetadata(ctx, cfg, http.StatusOK, nil)
	AppendAPIResponseChunk(ctx, cfg, []byte("event: message"))
	AppendAPIResponseChunk(ctx, cfg, []byte("data: {\"ok\":true}"))

	value, exists := ginCtx.Get(apiResponseKey)
	if !exists {
		t.Fatal("API_RESPONSE payload missing from gin context before materialization")
	}
	rawBefore, ok := value.([]byte)
	if !ok {
		t.Fatalf("API_RESPONSE type = %T, want []byte before materialization", value)
	}
	if strings.Contains(string(rawBefore), "data: {\"ok\":true}") {
		t.Fatalf("expected chunk body to remain deferred before materialization: %s", rawBefore)
	}

	got := string(MaterializeAPIResponse(ginCtx))
	if !strings.Contains(got, "Body:\n[BODY METADATA v1]") || strings.Contains(got, `{"ok":true}`) {
		t.Fatalf("materialized body is not metadata-only: %s", got)
	}
}

func TestRecordAPIRequestAndResponseNeverRetainSecrets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	cfg := &config.Config{SDKConfig: config.SDKConfig{RequestLog: true}}
	secret := "unique-upstream-log-secret"

	RecordAPIRequest(ctx, cfg, UpstreamRequestLog{
		URL:       "https://user:" + secret + "@upstream.example/v1/responses?key=" + secret + "&prompt=" + secret + "#" + secret,
		Method:    http.MethodPost,
		Headers:   http.Header{"Authorization": {"Bearer " + secret}, "X-Api-Key": {secret}, "X-Trace-ID": {"trace-safe"}, "Content-Type": {"application/json"}},
		Body:      []byte(`{"prompt":"` + secret + `"}`),
		Provider:  "test-provider",
		AuthType:  "api_key",
		AuthValue: secret,
	})
	requestValue, exists := ginCtx.Get("API_REQUEST")
	if !exists {
		t.Fatal("API_REQUEST missing")
	}
	requestText, ok := requestValue.([]byte)
	if !ok {
		t.Fatalf("API_REQUEST type = %T, want []byte", requestValue)
	}
	if bytes.Contains(requestText, []byte(secret)) || bytes.Contains(requestText, []byte(`{"prompt":`)) {
		t.Fatalf("API request log leaked raw data: %s", requestText)
	}
	if !bytes.Contains(requestText, []byte("Authorization: "+logging.RedactedHeaderValue)) || !bytes.Contains(requestText, []byte("X-Trace-ID: trace-safe")) || !bytes.Contains(requestText, []byte("Body:\n[BODY METADATA v1]")) {
		t.Fatalf("API request diagnostics missing: %s", requestText)
	}

	RecordAPIResponseMetadata(ctx, cfg, http.StatusTeapot, http.Header{
		"Set-Cookie": {"session=" + secret},
		"X-Trace-ID": {"response-trace-safe"},
	})
	AppendAPIResponseChunk(ctx, cfg, []byte(`{"error":"`+secret+`"}`))
	responseText := MaterializeAPIResponse(ginCtx)
	if bytes.Contains(responseText, []byte(secret)) || bytes.Contains(responseText, []byte(`{"error":`)) {
		t.Fatalf("API response log leaked raw data: %s", responseText)
	}
	if !bytes.Contains(responseText, []byte("Status: 418")) || !bytes.Contains(responseText, []byte("Set-Cookie: "+logging.RedactedHeaderValue)) || !bytes.Contains(responseText, []byte("X-Trace-ID: response-trace-safe")) {
		t.Fatalf("API response diagnostics missing: %s", responseText)
	}
}

func TestAPIWebsocketLoggingNeverRetainsFrameOrErrorSecrets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	cfg := &config.Config{SDKConfig: config.SDKConfig{RequestLog: true}}
	secret := "unique-websocket-log-secret"

	RecordAPIWebsocketRequest(ctx, cfg, UpstreamRequestLog{
		URL:     "wss://upstream.example/v1/responses?access_token=" + secret + "&input=" + secret,
		Headers: http.Header{"Authorization": {"Bearer " + secret}, "X-Trace-ID": {"ws-trace-safe"}},
		Body:    []byte(`{"input":"` + secret + `"}`),
	})
	AppendAPIWebsocketResponse(ctx, cfg, []byte(`{"output":"`+secret+`"}`))
	RecordAPIWebsocketError(ctx, cfg, "read", errors.New("dial failed with "+secret))

	value, exists := ginCtx.Get(apiWebsocketTimelineKey)
	if !exists {
		t.Fatal("API websocket timeline missing")
	}
	timeline, ok := value.([]byte)
	if !ok {
		t.Fatalf("API websocket timeline type = %T, want []byte", value)
	}
	if bytes.Contains(timeline, []byte(secret)) || bytes.Contains(timeline, []byte(`{"input":`)) || bytes.Contains(timeline, []byte(`{"output":`)) {
		t.Fatalf("API websocket timeline leaked raw data: %s", timeline)
	}
	if !bytes.Contains(timeline, []byte("Authorization: "+logging.RedactedHeaderValue)) || !bytes.Contains(timeline, []byte("X-Trace-ID: ws-trace-safe")) || !bytes.Contains(timeline, []byte("Error: upstream websocket failed")) {
		t.Fatalf("API websocket diagnostics missing: %s", timeline)
	}
}

func TestSummarizeErrorBodyReturnsMetadataOnly(t *testing.T) {
	secret := "unique-error-body-secret"
	summary := SummarizeErrorBody("application/json", []byte(`{"error":"`+secret+`"}`))
	if strings.Contains(summary, secret) || strings.Contains(summary, `{"error":`) {
		t.Fatalf("error summary leaked raw body: %s", summary)
	}
	if !strings.HasPrefix(summary, "[BODY METADATA v1]") || !strings.Contains(summary, `"sha256":`) || !strings.Contains(summary, `"content_type":"application/json"`) {
		t.Fatalf("error summary missing metadata: %s", summary)
	}
}

func TestAPILogTempSourcesNeverStoreRawSecrets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	cfg := &config.Config{SDKConfig: config.SDKConfig{RequestLog: true}}
	secret := "unique-api-temp-source-secret"

	requestSource, errRequestSource := logging.NewFileBodySourceInDir(t.TempDir(), "api-request-safe")
	if errRequestSource != nil {
		t.Fatalf("request source: %v", errRequestSource)
	}
	responseSource, errResponseSource := logging.NewFileBodySourceInDir(t.TempDir(), "api-response-safe")
	if errResponseSource != nil {
		t.Fatalf("response source: %v", errResponseSource)
	}
	websocketSource, errWebsocketSource := logging.NewFileBodySourceInDir(t.TempDir(), "api-websocket-safe")
	if errWebsocketSource != nil {
		t.Fatalf("websocket source: %v", errWebsocketSource)
	}
	t.Cleanup(func() {
		_ = requestSource.Cleanup()
		_ = responseSource.Cleanup()
		_ = websocketSource.Cleanup()
	})
	ginCtx.Set(logging.APIRequestSourceContextKey, requestSource)
	ginCtx.Set(logging.APIResponseSourceContextKey, responseSource)
	ginCtx.Set(logging.APIWebsocketTimelineSourceContextKey, websocketSource)

	info := UpstreamRequestLog{
		URL:       "https://user:" + secret + "@upstream.example/v1/responses?key=" + secret + "&prompt=" + secret,
		Method:    http.MethodPost,
		Headers:   http.Header{"Authorization": {"Bearer " + secret}, "Cookie": {"session=" + secret}, "X-Trace-ID": {"safe-trace"}, "Content-Type": {"application/json"}},
		Body:      []byte(`{"prompt":"` + secret + `","tool_output":"private"}`),
		AuthType:  "api_key",
		AuthValue: secret,
	}
	RecordAPIRequest(ctx, cfg, info)
	RecordAPIResponseMetadata(ctx, cfg, http.StatusBadGateway, http.Header{"Set-Cookie": {"session=" + secret}, "X-Trace-ID": {"safe-response-trace"}})
	AppendAPIResponseChunk(ctx, cfg, []byte(`{"reasoning":"`+secret+`","image":"private"}`))
	_ = MaterializeAPIResponse(ginCtx)
	RecordAPIWebsocketRequest(ctx, cfg, info)
	AppendAPIWebsocketResponse(ctx, cfg, []byte(`{"output":"`+secret+`"}`))
	RecordAPIWebsocketError(ctx, cfg, "read", errors.New("upstream failed with "+secret))

	for name, source := range map[string]*logging.FileBodySource{
		"request":   requestSource,
		"response":  responseSource,
		"websocket": websocketSource,
	} {
		raw, errRead := source.Bytes()
		if errRead != nil {
			t.Fatalf("%s source.Bytes: %v", name, errRead)
		}
		if bytes.Contains(raw, []byte(secret)) || bytes.Contains(raw, []byte(`{"prompt":`)) || bytes.Contains(raw, []byte("tool_output")) || bytes.Contains(raw, []byte("reasoning")) || bytes.Contains(raw, []byte("image")) {
			t.Fatalf("%s temp source leaked raw content: %s", name, raw)
		}
		if !bytes.Contains(raw, []byte("[BODY METADATA v1]")) {
			t.Fatalf("%s temp source missing body metadata: %s", name, raw)
		}
	}
}

func testAPIResponseText(t *testing.T, ginCtx *gin.Context) string {
	t.Helper()
	raw := MaterializeAPIResponse(ginCtx)
	if len(raw) == 0 {
		t.Fatal("API_RESPONSE payload missing from gin context")
	}
	return string(raw)
}
