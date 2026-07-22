package handlers

import (
	"bytes"
	"compress/gzip"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestPayloadBodyLimitObserveAllowsKnownContentLengthAndRecordsWouldReject(t *testing.T) {
	resetPayloadBodyLimitMetricsForTest()
	payload := bytes.Repeat([]byte("x"), 96)
	handlerCalls := atomic.Int32{}
	handler := newPayloadBodyLimitTestHandler(payloadBodyLimitObserve, 64, true)
	engine := gin.New()
	engine.Use(handler.PreAuthIngressAdmissionMiddleware())
	engine.POST("/v1/payload-policy", func(c *gin.Context) {
		handlerCalls.Add(1)
		body, errRead := ReadRequestBody(c)
		if errRead != nil {
			WriteRequestBodyError(c, errRead)
			return
		}
		c.String(http.StatusOK, strconv.Itoa(len(body)))
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/payload-policy", bytes.NewReader(payload))
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Body.String() != "96" {
		t.Fatalf("observe response = %d %q, want 200 and body length", response.Code, response.Body.String())
	}
	if handlerCalls.Load() != 1 {
		t.Fatalf("handler calls = %d, want 1", handlerCalls.Load())
	}
	snapshot := handler.PayloadBodyLimitSnapshot()
	if !snapshot.Configured || snapshot.Mode != payloadBodyLimitObserve {
		t.Fatalf("snapshot policy = %+v, want configured observe", snapshot)
	}
	if snapshot.Total.Requests != 1 || snapshot.Total.WouldReject != 1 || snapshot.Total.Rejected != 0 {
		t.Fatalf("snapshot totals = %+v, want one would-reject observation", snapshot.Total)
	}
	if snapshot.Total.MaxWireBytes != int64(len(payload)) || snapshot.Total.MaxDecodedBytes != int64(len(payload)) {
		t.Fatalf("snapshot maxima = wire %d decoded %d, want %d", snapshot.Total.MaxWireBytes, snapshot.Total.MaxDecodedBytes, len(payload))
	}
}

func TestPayloadBodyLimitEnforceRejectsKnownContentLengthBeforeHandler(t *testing.T) {
	resetPayloadBodyLimitMetricsForTest()
	handlerCalls := atomic.Int32{}
	handler := newPayloadBodyLimitTestHandler(payloadBodyLimitEnforce, 64, true)
	engine := gin.New()
	engine.Use(handler.PreAuthIngressAdmissionMiddleware())
	engine.POST("/v1/payload-policy", func(c *gin.Context) {
		handlerCalls.Add(1)
		c.Status(http.StatusNoContent)
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/payload-policy", bytes.NewReader(bytes.Repeat([]byte("x"), 96)))
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)

	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", response.Code)
	}
	if handlerCalls.Load() != 0 {
		t.Fatalf("handler calls = %d, want 0", handlerCalls.Load())
	}
	snapshot := handler.PayloadBodyLimitSnapshot()
	if snapshot.Total.Requests != 1 || snapshot.Total.WouldReject != 1 || snapshot.Total.Rejected != 1 {
		t.Fatalf("snapshot totals = %+v, want one enforced rejection", snapshot.Total)
	}
}

func TestPayloadBodyLimitObserveEmergencyCeilingRejectsKnownContentLength(t *testing.T) {
	resetPayloadBodyLimitMetricsForTest()
	handlerCalls := atomic.Int32{}
	handler := newPayloadBodyLimitTestHandler(payloadBodyLimitObserve, 64, true)
	engine := gin.New()
	engine.Use(handler.PreAuthIngressAdmissionMiddleware())
	engine.POST("/v1/payload-policy", func(c *gin.Context) {
		handlerCalls.Add(1)
		c.Status(http.StatusNoContent)
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/payload-policy", bytes.NewReader([]byte("x")))
	request.ContentLength = EmergencyPayloadBodyBytes + 1
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)

	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", response.Code)
	}
	if handlerCalls.Load() != 0 {
		t.Fatalf("handler calls = %d, want 0", handlerCalls.Load())
	}
	snapshot := handler.PayloadBodyLimitSnapshot()
	if snapshot.Total.EmergencyRejected != 1 || snapshot.Total.Rejected != 1 {
		t.Fatalf("snapshot totals = %+v, want one emergency rejection", snapshot.Total)
	}
}

func TestPayloadBodyLimitCompressedObserveAndEnforce(t *testing.T) {
	payload := []byte(`{"input":"` + string(bytes.Repeat([]byte("a"), 256)) + `"}`)
	compressed := gzipPayloadBody(t, payload)
	if len(compressed) >= 64 {
		t.Fatalf("compressed fixture = %d bytes, want below soft wire limit", len(compressed))
	}

	for _, test := range []struct {
		name       string
		mode       string
		wantStatus int
		wantBody   bool
	}{
		{name: "observe", mode: payloadBodyLimitObserve, wantStatus: http.StatusOK, wantBody: true},
		{name: "enforce", mode: payloadBodyLimitEnforce, wantStatus: http.StatusRequestEntityTooLarge},
	} {
		t.Run(test.name, func(t *testing.T) {
			resetPayloadBodyLimitMetricsForTest()
			handler := newPayloadBodyLimitTestHandler(test.mode, 64, true)
			engine := gin.New()
			engine.Use(handler.PreAuthIngressAdmissionMiddleware())
			engine.POST("/v1/payload-policy", func(c *gin.Context) {
				body, errRead := ReadRequestBody(c)
				if errRead != nil {
					WriteRequestBodyError(c, errRead)
					return
				}
				c.Data(http.StatusOK, "application/json", body)
			})

			request := httptest.NewRequest(http.MethodPost, "/v1/payload-policy", bytes.NewReader(compressed))
			request.Header.Set("Content-Encoding", "gzip")
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, request)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.wantStatus, response.Body.String())
			}
			if test.wantBody && !bytes.Equal(response.Body.Bytes(), payload) {
				t.Fatalf("decoded body mismatch: got %d bytes, want %d", response.Body.Len(), len(payload))
			}
			snapshot := handler.PayloadBodyLimitSnapshot()
			if snapshot.Total.Requests != 1 || snapshot.Total.WouldReject != 1 {
				t.Fatalf("snapshot totals = %+v, want one would-reject", snapshot.Total)
			}
			wantRejected := uint64(0)
			if test.mode == payloadBodyLimitEnforce {
				wantRejected = 1
			}
			if snapshot.Total.Rejected != wantRejected {
				t.Fatalf("rejected = %d, want %d", snapshot.Total.Rejected, wantRejected)
			}
		})
	}
}

func TestExplicitReadRequestBodyWithLimitsIgnoresObservePolicy(t *testing.T) {
	resetPayloadBodyLimitMetricsForTest()
	handler := newPayloadBodyLimitTestHandler(payloadBodyLimitObserve, 64, false)
	engine := gin.New()
	engine.Use(handler.PreAuthIngressAdmissionMiddleware())
	engine.POST("/v1/payload-policy", func(c *gin.Context) {
		_, errRead := ReadRequestBodyWithLimits(c, 32, 32)
		if errRead != nil {
			WriteRequestBodyError(c, errRead)
			return
		}
		c.Status(http.StatusNoContent)
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/payload-policy", bytes.NewReader(bytes.Repeat([]byte("x"), 48)))
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want explicit hard-limit 413", response.Code)
	}
	if snapshot := handler.PayloadBodyLimitSnapshot(); snapshot.Total.Requests != 0 {
		t.Fatalf("explicit low-level read recorded policy metrics: %+v", snapshot.Total)
	}
}

func TestPayloadBodyLimitMultipartAndWebsocketDefaults(t *testing.T) {
	resetPayloadBodyLimitMetricsForTest()
	cfg := &config.SDKConfig{}
	cfg.RequestGuards.PayloadBodyLimit.Mode = payloadBodyLimitObserve
	cfg.RequestGuards.PayloadBodyLimit.MultipartBytes = 64
	handler := NewBaseAPIHandlers(cfg, nil)
	engine := gin.New()
	engine.Use(handler.PreAuthIngressAdmissionMiddleware())
	engine.POST("/v1/images/edits", func(c *gin.Context) {
		form, errParse := ParseMultipartFormWithPolicy(c, 32, 16, 128)
		if form != nil {
			defer form.RemoveAll()
		}
		if errParse != nil {
			WriteRequestBodyError(c, errParse)
			return
		}
		c.Status(http.StatusNoContent)
	})
	engine.GET("/v1/responses", func(c *gin.Context) {
		physicalBytes, softBytes := WebsocketPayloadBodyLimits(c, 32<<20)
		c.String(http.StatusOK, "%d/%d", physicalBytes, softBytes)
	})

	var multipartBody bytes.Buffer
	writer := multipart.NewWriter(&multipartBody)
	part, errPart := writer.CreateFormFile("image", "image.png")
	if errPart != nil {
		t.Fatalf("CreateFormFile() error = %v", errPart)
	}
	if _, errWrite := io.Copy(part, bytes.NewReader(bytes.Repeat([]byte("i"), 96))); errWrite != nil {
		t.Fatalf("write multipart fixture: %v", errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("close multipart writer: %v", errClose)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(multipartBody.Bytes()))
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("multipart observe status = %d, want 204: %s", response.Code, response.Body.String())
	}

	wsRequest := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	wsRequest.Header.Set("Upgrade", "websocket")
	wsResponse := httptest.NewRecorder()
	engine.ServeHTTP(wsResponse, wsRequest)
	if wsResponse.Code != http.StatusOK || wsResponse.Body.String() != "33554432/67108864" {
		t.Fatalf("websocket limits = %d %q, want transport 32MiB/policy 64MiB", wsResponse.Code, wsResponse.Body.String())
	}

	snapshot := handler.PayloadBodyLimitSnapshot()
	if snapshot.MultipartBytes != 64 || snapshot.WebsocketBytes != defaultWebsocketPayloadBodyBytes {
		t.Fatalf("snapshot limits = %+v", snapshot)
	}
	if snapshot.Total.WouldReject != 1 || snapshot.Total.Rejected != 0 {
		t.Fatalf("multipart metrics = %+v, want observe would-reject", snapshot.Total)
	}
}

func TestWebsocketPayloadBodyLimitsClampToTransportAndExposePhysicalCeiling(t *testing.T) {
	resetPayloadBodyLimitMetricsForTest()
	handler := newPayloadBodyLimitTestHandler(payloadBodyLimitObserve, 64, false)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	c.Request.Header.Set("Upgrade", "websocket")
	c.Set(payloadBodyLimitGinKey, handler.payloadBodyLimit.Load())

	physicalBytes, softBytes := WebsocketPayloadBodyLimits(c, 128<<20)
	if physicalBytes != 128<<20 || softBytes != defaultWebsocketPayloadBodyBytes {
		t.Fatalf("websocket limits = %d/%d, want 128MiB/64MiB", physicalBytes, softBytes)
	}
	RecordWebsocketPayloadBodyWithLimit(c, 96<<20, physicalBytes, false)

	metric := handler.PayloadBodyLimitSnapshot().Kinds[string(payloadBodyKindWebsocket)]
	if metric.PhysicalCeilingBytes != 128<<20 || metric.Requests != 1 || metric.WouldReject != 1 || metric.Rejected != 0 {
		t.Fatalf("websocket kind metric = %+v", metric)
	}
}

func TestPayloadBodyLimitTotalSizeBucketsAreCumulative(t *testing.T) {
	resetPayloadBodyLimitMetricsForTest()
	handler := newPayloadBodyLimitTestHandler(payloadBodyLimitObserve, 64, false)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/payload-policy", nil)
	c.Set(payloadBodyLimitGinKey, handler.payloadBodyLimit.Load())
	decision := payloadBodyLimitDecisionForContext(c, payloadBodyKindJSON, 32, 32)

	for _, sizeBytes := range []int64{
		64 << 10,
		(64 << 10) + 1,
		(256 << 10) + 1,
		(1 << 20) + 1,
		(4 << 20) + 1,
		(16 << 20) + 1,
		(64 << 20) + 1,
	} {
		recordPayloadBodyLimit(c, decision, sizeBytes, sizeBytes, false)
	}

	snapshot := handler.PayloadBodyLimitSnapshot()
	wire := snapshot.Total.WireSizeBuckets
	decoded := snapshot.Total.DecodedSizeBuckets
	if wire.Samples != 7 || wire.LessThanOrEqual64KiB != 1 || wire.LessThanOrEqual256KiB != 2 ||
		wire.LessThanOrEqual1MiB != 3 || wire.LessThanOrEqual4MiB != 4 || wire.LessThanOrEqual16MiB != 5 ||
		wire.LessThanOrEqual64MiB != 6 || wire.Overflow != 1 {
		t.Fatalf("wire size buckets = %+v", wire)
	}
	if decoded != wire {
		t.Fatalf("decoded size buckets = %+v, want %+v", decoded, wire)
	}
	kind, exists := snapshot.Kinds[string(payloadBodyKindJSON)]
	if !exists || kind.WireSizeBuckets != wire || kind.DecodedSizeBuckets != decoded {
		t.Fatalf("JSON kind buckets = %+v, want total buckets", kind)
	}
	for _, emptyKind := range []payloadBodyKind{payloadBodyKindMultipart, payloadBodyKindWebsocket, payloadBodyKindOther} {
		if empty := snapshot.Kinds[string(emptyKind)]; empty.Requests != 0 || empty.WireSizeBuckets.Samples != 0 || empty.DecodedSizeBuckets.Samples != 0 {
			t.Fatalf("empty %s kind metrics = %+v", emptyKind, empty)
		}
	}
	if len(snapshot.Endpoints) != 1 {
		t.Fatalf("endpoint metrics = %d entries, want 1", len(snapshot.Endpoints))
	}
}

func TestPayloadBodyLimitKindHistogramsHaveFixedCardinality(t *testing.T) {
	resetPayloadBodyLimitMetricsForTest()
	handler := newPayloadBodyLimitTestHandler(payloadBodyLimitObserve, 64, false)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/payload-policy", nil)
	c.Set(payloadBodyLimitGinKey, handler.payloadBodyLimit.Load())

	for _, kind := range payloadBodyMetricKinds {
		decision := payloadBodyLimitDecisionForContext(c, kind, 32, 32)
		recordPayloadBodyLimit(c, decision, int64(len(kind)), int64(len(kind))+1, false)
	}
	unknownDecision := payloadBodyLimitDecisionForContext(c, "unexpected", 32, 32)
	recordPayloadBodyLimit(c, unknownDecision, 10, 11, false)

	snapshot := handler.PayloadBodyLimitSnapshot()
	if len(snapshot.Kinds) != len(payloadBodyMetricKinds) {
		t.Fatalf("kind metrics = %d entries, want %d: %+v", len(snapshot.Kinds), len(payloadBodyMetricKinds), snapshot.Kinds)
	}
	for _, kind := range payloadBodyMetricKinds {
		metric, exists := snapshot.Kinds[string(kind)]
		if !exists {
			t.Fatalf("missing %s kind metrics: %+v", kind, snapshot.Kinds)
		}
		wantRequests := uint64(1)
		if kind == payloadBodyKindOther {
			wantRequests = 2
		}
		if metric.Requests != wantRequests || metric.WireSizeBuckets.Samples != wantRequests || metric.DecodedSizeBuckets.Samples != wantRequests {
			t.Fatalf("%s kind metrics = %+v, want %d samples", kind, metric, wantRequests)
		}
	}
}

func TestPayloadBodyLimitMalformedCompressedBodyRecordsUnknownDecodedSize(t *testing.T) {
	resetPayloadBodyLimitMetricsForTest()
	handler := newPayloadBodyLimitTestHandler(payloadBodyLimitObserve, 64, false)
	engine := gin.New()
	engine.Use(handler.PreAuthIngressAdmissionMiddleware())
	engine.POST("/v1/payload-policy", func(c *gin.Context) {
		_, errRead := ReadRequestBody(c)
		if errRead != nil {
			c.Status(http.StatusBadRequest)
			return
		}
		c.Status(http.StatusNoContent)
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/payload-policy", strings.NewReader("not-a-gzip-stream"))
	request.Header.Set("Content-Encoding", "gzip")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}

	snapshot := handler.PayloadBodyLimitSnapshot()
	decoded := snapshot.Total.DecodedSizeBuckets
	if decoded.Unknown != 1 || decoded.Samples != 0 || decoded.LessThanOrEqual1MiB != 0 {
		t.Fatalf("decoded buckets = %+v, want one unknown and no numeric samples", decoded)
	}
	jsonKind := snapshot.Kinds[string(payloadBodyKindJSON)].DecodedSizeBuckets
	if jsonKind != decoded {
		t.Fatalf("JSON decoded buckets = %+v, want %+v", jsonKind, decoded)
	}
	if snapshot.Total.WireSizeBuckets.Samples != 1 {
		t.Fatalf("wire buckets = %+v, want one known sample", snapshot.Total.WireSizeBuckets)
	}
}

func newPayloadBodyLimitTestHandler(mode string, jsonBytes int64, admission bool) *BaseAPIHandler {
	cfg := &config.SDKConfig{}
	cfg.RequestGuards.PayloadBodyLimit.Mode = mode
	cfg.RequestGuards.PayloadBodyLimit.JSONBytes = jsonBytes
	cfg.RequestGuards.GlobalAdmission.Enabled = admission
	return NewBaseAPIHandlers(cfg, nil)
}

func gzipPayloadBody(t *testing.T, payload []byte) []byte {
	t.Helper()
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, errWrite := writer.Write(payload); errWrite != nil {
		t.Fatalf("gzip write: %v", errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("gzip close: %v", errClose)
	}
	return compressed.Bytes()
}
