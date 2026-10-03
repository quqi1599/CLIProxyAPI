package claude

import (
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

func TestClaudeKnownSafetyErrorPreservesCode(t *testing.T) {
	response := claudeErrorFromStatusText(400, "upstream request failed reason=content_policy_violation error_code=1301")
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(body, "error.code").String() != "content_policy_violation" || gjson.GetBytes(body, "error.type").String() != "invalid_request_error" {
		t.Fatalf("error = %s", body)
	}
}
