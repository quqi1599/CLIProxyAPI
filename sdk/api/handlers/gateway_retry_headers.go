package handlers

import (
	"context"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const gatewayRetryEvidenceContextKey = "cpa.gateway-retry-evidence"

func withGatewayRetryEvidence(c *gin.Context, ctx context.Context) context.Context {
	var evidence *coreauth.GatewayRetryEvidence
	if c != nil {
		value, _ := c.Get(gatewayRetryEvidenceContextKey)
		evidence, _ = value.(*coreauth.GatewayRetryEvidence)
	}
	ctx, evidence = coreauth.WithGatewayRetryEvidence(ctx, evidence)
	if c != nil {
		c.Set(gatewayRetryEvidenceContextKey, evidence)
	}
	return ctx
}

// WriteGatewayRetryHeaders is called after addon filtering and status
// normalization, immediately before a terminal HTTP error. A gateway scope
// permits independent fallback, while a request scope prohibits all replay.
func WriteGatewayRetryHeaders(c *gin.Context, msg *interfaces.ErrorMessage, status int) {
	if c == nil || c.Writer == nil || c.Writer.Written() {
		return
	}
	headers := c.Writer.Header()
	for _, key := range []string{"X-CPA-Retry-Scope", "X-CPA-Retry-Attempts", "X-CPA-Retry-Reason"} {
		headers.Del(key)
	}
	if c.Request == nil || c.Request.Context().Err() != nil || msg == nil || msg.Error == nil || status < 400 || status > 599 {
		return
	}
	value, _ := c.Get(gatewayRetryEvidenceContextKey)
	evidence, _ := value.(*coreauth.GatewayRetryEvidence)
	attempts, reason := evidence.Snapshot(msg.Error)
	if attempts < 1 || attempts > 1<<20 {
		return
	}
	if reason != "" {
		headers.Set("X-CPA-Retry-Scope", "request")
		headers.Set("X-CPA-Retry-Reason", reason)
	} else if status == http.StatusTooManyRequests || status >= 500 {
		headers.Set("X-CPA-Retry-Scope", "gateway")
	} else {
		return
	}
	headers.Set("X-CPA-Retry-Attempts", strconv.Itoa(attempts))
}
