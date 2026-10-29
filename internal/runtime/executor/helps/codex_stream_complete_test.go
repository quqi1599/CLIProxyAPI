package helps

import "testing"

func TestCodexResponseCompletedRequiresSuccessfulEnvelope(t *testing.T) {
	for _, body := range []string{
		`{"type":"response.completed","response":{"id":"resp_fixture","status":"completed"}}`,
		`{"type":"response.completed","error":null,"response":{"id":"resp_fixture","status":"completed","error":null,"output":[],"usage":{"output_tokens":0}}}`,
	} {
		if !CodexResponseCompleted([]byte(body)) {
			t.Errorf("valid completion rejected: %s", body)
		}
	}
	for _, body := range []string{
		`{"type":"response.completed","response":{"id":"resp_fixture","status":"incomplete"}}`,
		`{"type":"response.completed","response":{"id":"resp_fixture","status":"failed"}}`,
		`{"type":"response.completed","response":{"id":"resp_fixture"}}`,
		`{"type":"response.completed","response":{"status":"completed"}}`,
		`{"type":"response.completed","error":{},"response":{"id":"resp_fixture","status":"completed"}}`,
		`{"type":"response.completed","response":{"id":"resp_fixture","status":"completed","error":{"code":"server_error"}}}`,
		`{"type":"response.completed","type":"response.failed","response":{"id":"resp_fixture","status":"completed"}}`,
		`{"type":"response.completed","response":{"id":"resp_fixture","status":"completed","stat\u0075s":"failed"}}`,
		`{"type":"response.output_text.delta","delta":"response.completed"}`,
		`{"type":"response.done","response":{"id":"resp_fixture","status":"completed"}}`,
		`{"type":"response.completed","response":{"id":"resp_fixture","status":"completed"}} trailing`,
	} {
		if CodexResponseCompleted([]byte(body)) {
			t.Errorf("non-completion accepted: %s", body)
		}
	}
}
