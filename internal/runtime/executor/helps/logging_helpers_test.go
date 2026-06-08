package helps

import (
	"bytes"
	"context"
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
	if !strings.Contains(got, "Body:\nevent: message\ndata: {\"ok\":true}") {
		t.Fatalf("small stream body was not preserved readably: %s", got)
	}
	if strings.Contains(got, "truncated upstream response body") {
		t.Fatalf("small response should not be truncated: %s", got)
	}
}

func TestAppendAPIResponseChunkBoundsLargeBodyCapture(t *testing.T) {
	t.Setenv("GIN_MODE", "test")
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	cfg := &config.Config{SDKConfig: config.SDKConfig{RequestLog: true}}

	first := bytes.Repeat([]byte("A"), apiResponseHeadLimit+128)
	second := bytes.Repeat([]byte("B"), apiResponseTailLimit+256)

	RecordAPIResponseMetadata(ctx, cfg, http.StatusOK, nil)
	AppendAPIResponseChunk(ctx, cfg, first)
	AppendAPIResponseChunk(ctx, cfg, second)

	got := testAPIResponseText(t, ginCtx)
	totalBytes := len(first) + len(second) + 2 // separator between non-SSE chunks
	if !strings.Contains(got, "truncated upstream response body") {
		t.Fatalf("expected bounded capture marker, got: %s", got)
	}
	if !strings.Contains(got, "chunks_count=2") {
		t.Fatalf("expected logical chunk count in bounded marker, got: %s", got)
	}
	if !strings.Contains(got, "bytes="+strconv.Itoa(totalBytes)) {
		t.Fatalf("expected total byte count %d in bounded marker, got: %s", totalBytes, got)
	}
	if !strings.Contains(got, strings.Repeat("A", 64)) {
		t.Fatalf("expected head bytes to be preserved, got: %s", got)
	}
	if !strings.Contains(got, strings.Repeat("B", 64)) {
		t.Fatalf("expected tail bytes to be preserved, got: %s", got)
	}
	if !strings.Contains(got, "sha256=") {
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
	if !strings.Contains(got, "Body:\nevent: message\ndata: {\"ok\":true}") {
		t.Fatalf("materialized body missing deferred chunks: %s", got)
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
