package kimi

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestExchangeDeviceCodeDoesNotExposeOAuthDescription(t *testing.T) {
	const secret = "kimi-oauth-description-sentinel"
	client := &DeviceFlowClient{httpClient: &http.Client{Transport: kimiRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":"invalid_grant","error_description":"` + secret + `"}`)),
			Request:    req,
		}, nil
	})}}

	_, err, retry := client.exchangeDeviceCode(context.Background(), "device-code")
	if retry || err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "reason=invalid_grant") || !strings.Contains(err.Error(), `"sha256":`) {
		t.Fatalf("unsafe Kimi OAuth error: retry=%t err=%v", retry, err)
	}
}
