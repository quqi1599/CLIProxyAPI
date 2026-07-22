package handlers

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

type countingRequestReader struct {
	reads int
	data  *strings.Reader
}

func (r *countingRequestReader) Read(p []byte) (int, error) {
	r.reads++
	return r.data.Read(p)
}

func TestIngressAdmissionRejectsBeforeReadingRequestBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewBaseAPIHandlers(nil, nil)
	controller := newAdmissionController(1, 1, time.Second)
	controller.maxWait = time.Millisecond
	handler.admission.Store(controller)
	releaseActive, err := controller.acquire(context.Background(), 1)
	if err != nil {
		t.Fatalf("fill admission capacity: %v", err)
	}
	defer releaseActive()

	reader := &countingRequestReader{data: strings.NewReader(`{"messages":[]}`)}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", reader)
	recorder := httptest.NewRecorder()
	c, engine := gin.CreateTestContext(recorder)
	c.Request = request
	engine.Use(handler.IngressAdmissionMiddleware())
	engine.POST("/v1/chat/completions", func(c *gin.Context) {
		t.Fatal("request reached handler after admission rejection")
	})

	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusServiceUnavailable, recorder.Body.String())
	}
	if reader.reads != 0 {
		t.Fatalf("request body reads = %d, want 0", reader.reads)
	}
	if active, queued := controller.snapshot(); active != 1 || queued != 0 {
		t.Fatalf("admission snapshot = %d/%d, want 1/0", active, queued)
	}
}

func TestPreAuthAdmissionRejectsBeforeBodyReadingMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewBaseAPIHandlers(nil, nil)
	controller := newAdmissionController(1, 1, time.Second)
	handler.admission.Store(controller)
	releaseActive, err := controller.acquire(context.Background(), 1)
	if err != nil {
		t.Fatalf("fill admission capacity: %v", err)
	}
	defer releaseActive()

	reader := &countingRequestReader{data: strings.NewReader(`{"messages":[]}`)}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", reader)
	recorder := httptest.NewRecorder()
	engine := gin.New()
	engine.Use(handler.PreAuthIngressAdmissionMiddleware())
	engine.Use(func(c *gin.Context) {
		_, _ = io.ReadAll(c.Request.Body)
		c.Next()
	})
	engine.POST("/v1/chat/completions", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusServiceUnavailable, recorder.Body.String())
	}
	if reader.reads != 0 {
		t.Fatalf("request body reads = %d, want 0", reader.reads)
	}
}

func TestIngressAdmissionUsesPhysicalDecodedCeilingAndReleases(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const capacity = 8
	middlewares := []struct {
		name string
		use  func(*BaseAPIHandler) gin.HandlerFunc
	}{
		{name: "pre-auth", use: (*BaseAPIHandler).PreAuthIngressAdmissionMiddleware},
		{name: "route", use: (*BaseAPIHandler).IngressAdmissionMiddleware},
	}
	requests := []struct {
		name          string
		contentLength int64
		encoding      string
		cancel        bool
		wantWeight    int
	}{
		{name: "gzip", contentLength: 2, encoding: "gzip", wantWeight: capacity},
		{name: "unknown-length", contentLength: -1, wantWeight: capacity},
		{name: "unknown-length-canceled", contentLength: -1, cancel: true, wantWeight: capacity},
		{name: "known-identity", contentLength: 4 << 20, wantWeight: 3},
	}

	for _, middleware := range middlewares {
		for _, requestCase := range requests {
			t.Run(middleware.name+"/"+requestCase.name, func(t *testing.T) {
				handler := newPayloadBodyLimitTestHandler(payloadBodyLimitObserve, 64, false)
				controller := newAdmissionController(capacity, 1, time.Second)
				handler.admission.Store(controller)
				observedWeight := -1
				observedCanceled := false

				request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
				request.ContentLength = requestCase.contentLength
				if requestCase.encoding != "" {
					request.Header.Set("Content-Encoding", requestCase.encoding)
				}
				var cancel context.CancelFunc
				if requestCase.cancel {
					var requestContext context.Context
					requestContext, cancel = context.WithCancel(request.Context())
					request = request.WithContext(requestContext)
				}

				engine := gin.New()
				engine.Use(middleware.use(handler))
				engine.POST("/v1/chat/completions", func(c *gin.Context) {
					observedWeight, _ = controller.snapshot()
					if cancel != nil {
						cancel()
						observedCanceled = errors.Is(c.Request.Context().Err(), context.Canceled)
					}
					c.Status(http.StatusNoContent)
				})

				response := httptest.NewRecorder()
				engine.ServeHTTP(response, request)
				if cancel != nil {
					cancel()
				}
				if response.Code != http.StatusNoContent {
					t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusNoContent, response.Body.String())
				}
				if observedWeight != requestCase.wantWeight {
					t.Fatalf("active weight = %d, want %d", observedWeight, requestCase.wantWeight)
				}
				if requestCase.cancel && !observedCanceled {
					t.Fatal("request context was not canceled in the handler")
				}
				if active, queued := controller.snapshot(); active != 0 || queued != 0 {
					t.Fatalf("released snapshot = %d/%d, want 0/0", active, queued)
				}
			})
		}
	}
}

func TestIdleResponsesWebsocketsDoNotConsumeGlobalAdmission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const connections = 128
	handler := NewBaseAPIHandlers(nil, nil)
	controller := newAdmissionController(connections, connections, 0)
	handler.admission.Store(controller)

	entered := make(chan struct{}, connections)
	unblock := make(chan struct{})
	done := make(chan struct{}, connections)
	engine := gin.New()
	engine.Use(handler.PreAuthIngressAdmissionMiddleware())
	engine.GET("/v1/responses", func(c *gin.Context) {
		entered <- struct{}{}
		<-unblock
		c.Status(http.StatusNoContent)
	})
	engine.POST("/v1/chat/completions", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	defer func() {
		close(unblock)
		for range connections {
			<-done
		}
	}()

	for range connections {
		go func() {
			request := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
			request.Header.Set("Connection", "upgrade")
			request.Header.Set("Upgrade", "websocket")
			engine.ServeHTTP(httptest.NewRecorder(), request)
			done <- struct{}{}
		}()
	}
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for range connections {
		select {
		case <-entered:
		case <-timer.C:
			t.Fatal("idle websocket requests did not all reach the handler")
		}
	}

	if active, queued := controller.snapshot(); active != 0 || queued != 0 {
		t.Errorf("idle websocket admission = active:%d queued:%d, want 0/0", active, queued)
	}
	if !handler.AdmissionReady() {
		t.Error("idle websocket connections made admission readiness fail")
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Errorf("normal request status with idle websockets = %d, want %d", recorder.Code, http.StatusNoContent)
	}
}

func TestIngressAdmissionUpgradeAndExecutionReuseOneLease(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewBaseAPIHandlers(nil, nil)
	controller := newAdmissionController(16, 2, time.Second)
	handler.admission.Store(controller)
	body := admissionRequestWithMessages(257)
	wantVector, valid := inspectRequestComplexity(body)
	if !valid {
		t.Fatal("test request complexity is invalid")
	}
	wantWeight := complexityAdmissionWeight(wantVector)

	engine := gin.New()
	engine.Use(handler.IngressAdmissionMiddleware())
	engine.POST("/v1/chat/completions", func(c *gin.Context) {
		raw, errRead := ReadRequestBody(c)
		if errRead != nil {
			t.Fatalf("ReadRequestBody() error = %v", errRead)
		}
		ctx := context.WithValue(context.Background(), "gin", c)
		_, releaseExecution, errAcquire := handler.inspectAndAcquireAdmission(ctx, raw, &modelExecutionOptions{})
		if errAcquire != nil {
			t.Fatalf("execution admission error = %v", errAcquire)
		}
		defer releaseExecution()
		if active, queued := controller.snapshot(); active != wantWeight || queued != 0 {
			t.Fatalf("active execution snapshot = %d/%d, want %d/0", active, queued, wantWeight)
		}
		c.Status(http.StatusNoContent)
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusNoContent, recorder.Body.String())
	}
	if active, queued := controller.snapshot(); active != 0 || queued != 0 {
		t.Fatalf("released snapshot = %d/%d, want 0/0", active, queued)
	}
}

func TestIngressAdmissionUpgradeFailsWithoutQueueingBehindItself(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewBaseAPIHandlers(nil, nil)
	controller := newAdmissionController(3, 2, time.Second)
	handler.admission.Store(controller)
	releaseOther, err := controller.acquire(context.Background(), 1)
	if err != nil {
		t.Fatalf("acquire unrelated capacity: %v", err)
	}
	defer releaseOther()
	body := admissionRequestWithMessages(257)

	engine := gin.New()
	engine.Use(handler.IngressAdmissionMiddleware())
	engine.POST("/v1/chat/completions", func(c *gin.Context) {
		if _, errRead := ReadRequestBody(c); errRead == nil {
			t.Fatal("ReadRequestBody() succeeded, want admission upgrade rejection")
		} else {
			WriteRequestBodyError(c, errRead)
		}
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusServiceUnavailable, recorder.Body.String())
	}
	if active, queued := controller.snapshot(); active != 1 || queued != 0 {
		t.Fatalf("post-rejection snapshot = %d/%d, want 1/0", active, queued)
	}
}

func TestMultipartComplexityCachesFileBytesAndParts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	file, err := writer.CreateFormFile("image", "reference.png")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	imageBytes := bytes.Repeat([]byte("x"), 1024)
	if _, errWrite := file.Write(imageBytes); errWrite != nil {
		t.Fatalf("write multipart file: %v", errWrite)
	}
	if errWrite := writer.WriteField("prompt", "edit this"); errWrite != nil {
		t.Fatalf("WriteField: %v", errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("close multipart writer: %v", errClose)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(body.Bytes()))
	request.Header.Set("Content-Type", writer.FormDataContentType())
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = request
	form, errParse := ParseMultipartFormWithLimits(c, int64(body.Len()), 128, int64(len(imageBytes)))
	if errParse != nil {
		t.Fatalf("ParseMultipartFormWithLimits() error = %v", errParse)
	}
	if form != nil {
		defer func() {
			if errRemove := form.RemoveAll(); errRemove != nil {
				t.Errorf("RemoveAll() error = %v", errRemove)
			}
		}()
	}
	value, exists := c.Get(requestComplexityGinKey)
	if !exists {
		t.Fatal("multipart complexity was not cached")
	}
	cached, ok := value.(cachedRequestComplexity)
	if !ok {
		t.Fatalf("cached complexity type = %T", value)
	}
	if !cached.valid || cached.vector.InlineImageBytes != int64(len(imageBytes)) || cached.vector.ContentPartCount != 2 {
		t.Fatalf("multipart complexity = %+v, want image_bytes=%d parts=2", cached.vector, len(imageBytes))
	}
}
