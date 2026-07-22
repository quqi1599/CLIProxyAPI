package api

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/tidwall/gjson"
)

func TestHealthDetailsRequiresManagementAuthAndReturnsLocalSnapshot(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "health-secret")
	server := newTestServer(t)
	server.ready.Store(true)

	configData := []byte("port: 8317\n")
	if err := os.WriteFile(server.configFilePath, configData, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	unauthorized := httptest.NewRecorder()
	server.engine.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/healthz/details", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d; body=%s", unauthorized.Code, http.StatusUnauthorized, unauthorized.Body.String())
	}
	if gjson.GetBytes(unauthorized.Body.Bytes(), "payload_body_limits").Exists() {
		t.Fatalf("unauthorized response leaked payload body-limit details: %s", unauthorized.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/healthz/details", nil)
	req.Header.Set("X-Management-Key", "health-secret")
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	var response healthDetailsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Status != "ok" {
		t.Fatalf("status = %q, want ok", response.Status)
	}
	if !response.Ready {
		t.Fatal("health details should report the test server ready")
	}
	sum := sha256.Sum256(configData)
	if want := fmt.Sprintf("sha256:%x", sum); response.Config.Version != want {
		t.Fatalf("config version = %q, want %q", response.Config.Version, want)
	}
	if !response.Config.Available {
		t.Fatal("config snapshot should be available")
	}
	if response.Build.Version == "" || response.Build.Commit == "" || response.Build.BuildDate == "" {
		t.Fatalf("missing build details: %+v", response.Build)
	}
	if response.Process.HeapInUseBytes == 0 || response.Process.HeapLiveBytes == 0 || response.Process.Goroutines == 0 {
		t.Fatalf("missing process details: %+v", response.Process)
	}
	if response.Process.OpenFDs != nil && *response.Process.OpenFDs < 0 {
		t.Fatalf("open FDs = %d, want non-negative", *response.Process.OpenFDs)
	}
	if response.Process.OpenSockets != nil && *response.Process.OpenSockets < 0 {
		t.Fatalf("open sockets = %d, want non-negative", *response.Process.OpenSockets)
	}
	if response.Process.StartedAt == "" || response.Process.UptimeSeconds < 0 {
		t.Fatalf("invalid process lifetime details: %+v", response.Process)
	}
	if !response.Dependencies.ConfigAvailable || !response.Dependencies.AdmissionReady || response.Dependencies.HomeEnabled || response.Dependencies.HomeHeartbeat != nil {
		t.Fatalf("unexpected dependency details: %+v", response.Dependencies)
	}
	if response.Admission.Enabled {
		t.Fatal("admission should be disabled in the test server")
	}
	if response.PayloadBodyLimits.EmergencyCeilingBytes != handlers.EmergencyPayloadBodyBytes {
		t.Fatalf("payload body emergency ceiling = %d, want %d", response.PayloadBodyLimits.EmergencyCeilingBytes, handlers.EmergencyPayloadBodyBytes)
	}
	if !gjson.GetBytes(recorder.Body.Bytes(), "payload_body_limits.total.wire_size_buckets.le_64_kib").Exists() ||
		!gjson.GetBytes(recorder.Body.Bytes(), "payload_body_limits.total.decoded_size_buckets.overflow").Exists() {
		t.Fatalf("payload body-limit histograms missing: %s", recorder.Body.String())
	}
	if !gjson.GetBytes(recorder.Body.Bytes(), "payload_body_limits.kinds.json.decoded_size_buckets.unknown").Exists() ||
		!gjson.GetBytes(recorder.Body.Bytes(), "payload_body_limits.kinds.websocket.wire_size_buckets.le_64_mib").Exists() ||
		!gjson.GetBytes(recorder.Body.Bytes(), "payload_body_limits.kinds.other.wire_size_buckets.samples").Exists() {
		t.Fatalf("payload body-limit kind histograms missing: %s", recorder.Body.String())
	}
	if !gjson.GetBytes(recorder.Body.Bytes(), "transforms.reports").Exists() {
		t.Fatalf("transform metrics missing: %s", recorder.Body.String())
	}
	if !gjson.GetBytes(recorder.Body.Bytes(), "transforms.report_distribution.input_size_buckets.le_64_kib").Exists() ||
		!gjson.GetBytes(recorder.Body.Bytes(), "transforms.stage_catalog.normalize.duration_buckets.le_1_ms").Exists() ||
		!gjson.GetBytes(recorder.Body.Bytes(), "transforms.stage_catalog.other.amplification_ratio_buckets.overflow").Exists() ||
		!gjson.GetBytes(recorder.Body.Bytes(), "transforms.policy_catalog.other.results.exceeded").Exists() {
		t.Fatalf("transform distributions missing: %s", recorder.Body.String())
	}
	if !gjson.GetBytes(recorder.Body.Bytes(), "large_clones.count").Exists() {
		t.Fatalf("large clone metrics missing: %s", recorder.Body.String())
	}
}

func TestHealthDetailsUnavailableWithoutManagementSecret(t *testing.T) {
	server := newTestServer(t)
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz/details", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusNotFound, recorder.Body.String())
	}
}

func TestHealthDetailsAllowsAuthenticatedFreshGCSnapshot(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "health-secret")
	server := newTestServer(t)
	server.ready.Store(true)

	request := httptest.NewRequest(http.MethodGet, "/healthz/details?gc=1", nil)
	request.Header.Set("X-Management-Key", "health-secret")
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if heapLive := gjson.GetBytes(recorder.Body.Bytes(), "process.heap_live_bytes").Uint(); heapLive == 0 {
		t.Fatal("fresh GC snapshot did not report live heap")
	}
}

func TestParseProcStatusRSS(t *testing.T) {
	rss := parseProcStatusRSS([]byte("Name:\tserver\nVmRSS:\t2048 kB\n"))
	if rss == nil || *rss != 2<<20 {
		t.Fatalf("RSS = %v, want %d", rss, 2<<20)
	}
}
