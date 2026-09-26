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
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
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
	executionController := newAdmissionController(1, 1, time.Second)
	readController := newAdmissionController(1, 1, time.Second)
	readController.maxWait = time.Millisecond
	handler.admission.Store(executionController)
	handler.readAdmission.Store(readController)
	releaseActive, err := readController.acquire(context.Background(), 1)
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
	if active, queued := readController.snapshot(); active != 1 || queued != 0 {
		t.Fatalf("admission snapshot = %d/%d, want 1/0", active, queued)
	}
}

func TestAdmissionSnapshotSeparatesReadAndExecution(t *testing.T) {
	cfg := &config.SDKConfig{}
	cfg.RequestGuards.GlobalAdmission.Enabled = true
	cfg.RequestGuards.GlobalAdmission.Capacity = 8
	handler := NewBaseAPIHandlers(cfg, nil)
	execution := handler.admission.Load()
	read := handler.readAdmission.Load()
	if execution == nil || read == nil || execution == read {
		t.Fatal("expected independent execution and read admission controllers")
	}
	releaseExecution, err := execution.acquire(context.Background(), 3)
	if err != nil {
		t.Fatalf("acquire execution capacity: %v", err)
	}
	defer releaseExecution()
	releaseRead, err := read.acquire(context.Background(), 8)
	if err != nil {
		t.Fatalf("acquire read capacity: %v", err)
	}
	defer releaseRead()
	snapshot := handler.AdmissionSnapshot()
	if snapshot.ActiveWeight != 3 || snapshot.ReadActiveWeight != 8 {
		t.Fatalf("snapshot = execution:%d read:%d, want 3/8", snapshot.ActiveWeight, snapshot.ReadActiveWeight)
	}
}

func TestReadAdmissionIsNotReacquiredAfterBodyRead(t *testing.T) {
	handler := NewBaseAPIHandlers(nil, nil)
	execution := newAdmissionController(8, 1, time.Second)
	read := newAdmissionController(8, 1, time.Second)
	handler.admission.Store(execution)
	handler.readAdmission.Store(read)
	engine := gin.New()
	engine.Use(handler.PreAuthIngressAdmissionMiddleware())
	engine.Use(func(c *gin.Context) {
		if _, errRead := ReadRequestBody(c); errRead != nil {
			t.Fatalf("ReadRequestBody() error = %v", errRead)
		}
		c.Next()
	})
	engine.Use(handler.IngressAdmissionMiddleware())
	engine.POST("/v1/chat/completions", func(c *gin.Context) {
		if active, queued := read.snapshot(); active != 0 || queued != 0 {
			t.Fatalf("read admission after body read = %d/%d, want 0/0", active, queued)
		}
		if active, queued := execution.snapshot(); active != 0 || queued != 0 {
			t.Fatalf("execution admission before execution = %d/%d, want 0/0", active, queued)
		}
		c.Status(http.StatusNoContent)
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{"messages":[]}`)))
	request.ContentLength = -1
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusNoContent, response.Body.String())
	}
}

func TestPreAuthAdmissionRejectsBeforeBodyReadingMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewBaseAPIHandlers(nil, nil)
	controller := newAdmissionController(1, 1, time.Second)
	handler.readAdmission.Store(controller)
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
		wantExecution int
		wantRead      int
	}{
		{name: "gzip", contentLength: 2, encoding: "gzip", wantExecution: 0, wantRead: capacity},
		{name: "unknown-length", contentLength: -1, wantExecution: 0, wantRead: capacity},
		{name: "unknown-length-canceled", contentLength: -1, cancel: true, wantExecution: 0, wantRead: capacity},
		{name: "known-identity", contentLength: 4 << 20, wantExecution: 0, wantRead: 0},
	}

	for _, middleware := range middlewares {
		for _, requestCase := range requests {
			t.Run(middleware.name+"/"+requestCase.name, func(t *testing.T) {
				handler := newPayloadBodyLimitTestHandler(payloadBodyLimitObserve, 64, false)
				executionController := newAdmissionController(capacity, 1, time.Second)
				readController := newAdmissionController(capacity, 1, time.Second)
				handler.admission.Store(executionController)
				handler.readAdmission.Store(readController)
				observedExecutionWeight := -1
				observedReadWeight := -1
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
					observedExecutionWeight, _ = executionController.snapshot()
					observedReadWeight, _ = readController.snapshot()
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
				if observedExecutionWeight != requestCase.wantExecution {
					t.Fatalf("execution active weight = %d, want %d", observedExecutionWeight, requestCase.wantExecution)
				}
				if observedReadWeight != requestCase.wantRead {
					t.Fatalf("read active weight = %d, want %d", observedReadWeight, requestCase.wantRead)
				}
				if requestCase.cancel && !observedCanceled {
					t.Fatal("request context was not canceled in the handler")
				}
				if active, queued := executionController.snapshot(); active != 0 || queued != 0 {
					t.Fatalf("released execution snapshot = %d/%d, want 0/0", active, queued)
				}
				if active, queued := readController.snapshot(); active != 0 || queued != 0 {
					t.Fatalf("released read snapshot = %d/%d, want 0/0", active, queued)
				}
			})
		}
	}
}

func TestPreAuthAdmissionReweightsUnknownLengthAfterParsing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const capacity = 8
	handler := newPayloadBodyLimitTestHandler(payloadBodyLimitObserve, 64, false)
	executionController := newAdmissionController(capacity, 1, time.Second)
	readController := newAdmissionController(capacity, 1, time.Second)
	handler.admission.Store(executionController)
	handler.readAdmission.Store(readController)
	body := admissionRequestWithMessages(1)
	wantVector, valid := inspectRequestComplexity(body)
	if !valid {
		t.Fatal("test request complexity is invalid")
	}
	wantWeight := executionAdmissionWeight(wantVector)

	engine := gin.New()
	engine.Use(handler.PreAuthIngressAdmissionMiddleware())
	engine.POST("/v1/chat/completions", func(c *gin.Context) {
		if active, queued := executionController.snapshot(); active != 0 || queued != 0 {
			t.Fatalf("pre-parse execution admission = %d/%d, want 0/0", active, queued)
		}
		if active, queued := readController.snapshot(); active != capacity || queued != 0 {
			t.Fatalf("pre-parse read admission = %d/%d, want %d/0", active, queued, capacity)
		}
		if _, errRead := ReadRequestBody(c); errRead != nil {
			t.Fatalf("ReadRequestBody() error = %v", errRead)
		}
		if active, queued := readController.snapshot(); active != 0 || queued != 0 {
			t.Fatalf("post-parse read admission = %d/%d, want 0/0", active, queued)
		}
		ctx := context.WithValue(context.Background(), "gin", c)
		_, releaseExecution, errAcquire := handler.inspectAndAcquireAdmission(ctx, body, &modelExecutionOptions{})
		if errAcquire != nil {
			t.Fatalf("execution admission error = %v", errAcquire)
		}
		defer releaseExecution()
		if active, queued := executionController.snapshot(); active != wantWeight || queued != 0 {
			t.Fatalf("post-parse execution admission = %d/%d, want %d/0", active, queued, wantWeight)
		}
		c.Status(http.StatusNoContent)
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	request.ContentLength = -1
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusNoContent, recorder.Body.String())
	}
	if active, queued := executionController.snapshot(); active != 0 || queued != 0 {
		t.Fatalf("released execution admission = %d/%d, want 0/0", active, queued)
	}
	if active, queued := readController.snapshot(); active != 0 || queued != 0 {
		t.Fatalf("released read admission = %d/%d, want 0/0", active, queued)
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
	wantWeight := executionAdmissionWeight(wantVector)

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

func TestIngressBodyAdmissionUpgradeFailsWithoutQueueingBehindItself(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewBaseAPIHandlers(nil, nil)
	body := admissionRequestWithMessages(257)
	controller := newAdmissionController(len(body), 2, time.Second)
	controller.strictCapacity = true
	handler.bodyAdmission.Store(controller)
	releaseOther, err := controller.acquire(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseOther()
	engine := gin.New()
	engine.Use(handler.IngressAdmissionMiddleware())
	engine.POST("/v1/chat/completions", func(c *gin.Context) {
		if _, errRead := ReadRequestBody(c); !errors.Is(errRead, errAdmissionQueueFull) {
			t.Fatalf("want byte rejection, got %v", errRead)
		} else {
			WriteRequestBodyError(c, errRead)
		}
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	// Exercise an underestimated body; real unknown bodies take the same measured transfer.
	request.ContentLength = 1
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", recorder.Code)
	}
	if active, queued := controller.snapshot(); active != 1 || queued != 0 {
		t.Fatalf("leaked byte lease %d/%d", active, queued)
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
