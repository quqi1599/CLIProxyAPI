package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestGetContextWithCancelMarksDownstreamDisconnect(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	requestCtx, disconnect := context.WithCancel(context.Background())
	ginContext.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(requestCtx)

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	executionCtx, finish := handler.GetContextWithCancel(nil, ginContext, context.Background())
	defer finish()
	disconnect()

	select {
	case <-executionCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("execution context was not canceled")
	}
	if got := internallogging.CancellationOriginFromContext(executionCtx); got != internallogging.CancelOriginDownstreamDisconnected {
		t.Fatalf("cancel origin = %q", got)
	}
}
