package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

func TestHealthDetailsReportsEffectiveAmplificationMode(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "health-secret")
	server := newTestServer(t)
	server.handlers.UpdateClients(&sdkconfig.SDKConfig{RequestGuards: sdkconfig.RequestGuardsConfig{
		Amplification: sdkconfig.AmplificationGuardConfig{Mode: "observe"},
	}})

	request := httptest.NewRequest(http.MethodGet, "/healthz/details", nil)
	request.Header.Set("X-Management-Key", "health-secret")
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := gjson.GetBytes(recorder.Body.Bytes(), "amplification_guard.mode").String(); got != "observe" {
		t.Fatalf("amplification mode = %q, want observe", got)
	}
	if !gjson.GetBytes(recorder.Body.Bytes(), "amplification_guard.configured").Bool() {
		t.Fatalf("amplification guard was not reported as configured: %s", recorder.Body.String())
	}

	unauthorized := httptest.NewRecorder()
	server.engine.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/healthz/details", nil))
	if gjson.GetBytes(unauthorized.Body.Bytes(), "amplification_guard").Exists() {
		t.Fatalf("unauthorized response leaked amplification policy: %s", unauthorized.Body.String())
	}
}
