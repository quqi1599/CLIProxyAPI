package management

import (
	"encoding/json"
	"net/http"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestGetMonitorGPTFirstEventPolicy(t *testing.T) {
	h := &Handler{authManager: coreauth.NewManager(nil, nil, nil)}
	rr := executeMonitorRequest(h.GetMonitorGPTFirstEventPolicy, "/monitor/gpt-first-event-policy?model=gpt-5.6-sol&days=7")
	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d, body=%s", rr.Code, rr.Body.String())
	}

	var response struct {
		Model         string `json:"model"`
		RuntimeScoped bool   `json:"runtime_scoped"`
		Current       struct {
			PolicyState       string `json:"policy_state"`
			EnforcedTimeoutMs int64  `json:"enforced_timeout_ms"`
		} `json:"current"`
		Daily            []coreauth.GPTFirstEventDailySnapshot `json:"daily"`
		DurableLogEvents []string                              `json:"durable_log_events"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Model != "gpt-5.6-sol" || !response.RuntimeScoped {
		t.Fatalf("unexpected response metadata: %+v", response)
	}
	if response.Current.PolicyState != "normal" || response.Current.EnforcedTimeoutMs != 25000 {
		t.Fatalf("unexpected current policy: %+v", response.Current)
	}
	if response.Daily == nil || len(response.DurableLogEvents) != 3 {
		t.Fatalf("unexpected monitoring payload: daily=%v log_events=%v", response.Daily, response.DurableLogEvents)
	}
}

func TestGetMonitorGPTFirstEventPolicyRejectsInvalidDays(t *testing.T) {
	h := &Handler{authManager: coreauth.NewManager(nil, nil, nil)}
	rr := executeMonitorRequest(h.GetMonitorGPTFirstEventPolicy, "/monitor/gpt-first-event-policy?days=0")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}
