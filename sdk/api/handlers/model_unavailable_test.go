package handlers

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAliyunModelUnavailableHint(t *testing.T) {
	for _, text := range []string{
		`{"code":"InvalidParameter","message":"Model not exist."}`,
		"upstream request failed reason=model_not_supported model unavailable,invalidparameter [BODY METADATA v1]",
	} {
		status, normalized := NormalizeKnownUserError(400, text, nil)
		if status != 400 {
			t.Fatalf("status=%d", status)
		}
		var body ErrorResponse
		if err := json.Unmarshal(BuildErrorResponseBody(status, normalized), &body); err != nil {
			t.Fatal(err)
		}
		if body.Error.Code != "model_not_supported" || !strings.Contains(body.Error.Message, "模型不存在") {
			t.Fatalf("error=%+v", body.Error)
		}
		if isInvalidRequestParameterError(status, text) {
			t.Fatal("model rejection classified as request parameters")
		}
	}
	// A generic parameter rejection must not gain model-routing semantics.
	body, ok := clientHintErrorDetail(400, `{"code":"InvalidParameter","message":"invalid temperature"}`)
	if !ok || body.Code != "invalid_request_parameters" {
		t.Fatalf("error=%+v", body)
	}
	if isUpstreamModelUnavailableError(200, "Model not exist.") || isUpstreamModelUnavailableError(500, "Model not exist.") {
		t.Fatal("unexpected status reclassification")
	}
}
