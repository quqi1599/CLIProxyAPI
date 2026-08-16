package handlers

import (
	"net/http"
	"testing"

	"github.com/tidwall/gjson"
)

func TestBuildRemoteCompactionErrorBody(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		status     int
		text       string
		wantStatus int
		wantCode   string
	}{
		{name: "unsupported", status: http.StatusBadGateway, text: "remote_compaction_trigger_unsupported: route is not compatible", wantStatus: http.StatusBadRequest, wantCode: "remote_compaction_trigger_unsupported"},
		{name: "invalid response", status: http.StatusOK, text: "invalid_compaction_response: missing encrypted content", wantStatus: http.StatusBadGateway, wantCode: "invalid_compaction_response"},
		{name: "route unavailable", status: http.StatusBadRequest, text: "compaction_route_unavailable: breaker open", wantStatus: http.StatusServiceUnavailable, wantCode: "compaction_route_unavailable"},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := NormalizeKnownUserErrorStatus(test.status, test.text); got != test.wantStatus {
				t.Fatalf("NormalizeKnownUserErrorStatus() = %d, want %d", got, test.wantStatus)
			}
			body := BuildErrorResponseBody(test.status, test.text)
			if got := gjson.GetBytes(body, "error.code").String(); got != test.wantCode {
				t.Fatalf("error.code = %q, want %q; body=%s", got, test.wantCode, body)
			}
		})
	}
}

func TestBuildOpenAIResponsesStreamErrorChunkPreservesCompactionCode(t *testing.T) {
	t.Parallel()
	body := BuildOpenAIResponsesStreamErrorChunk(http.StatusBadGateway, "invalid_compaction_stream: missing response.completed", 3)
	if got := gjson.GetBytes(body, "code").String(); got != "invalid_compaction_stream" {
		t.Fatalf("code = %q; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "sequence_number").Int(); got != 3 {
		t.Fatalf("sequence_number = %d, want 3", got)
	}
}
