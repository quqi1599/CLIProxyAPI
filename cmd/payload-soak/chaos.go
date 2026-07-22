package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	soakScenarioPrefix       = "soak-scenario:"
	soakUpstreamMarker       = "payload-soak-upstream"
	minimumSlowHeaderLatency = 200 * time.Millisecond
	maxScenarioResponseBytes = 4 << 20
)

var requiredReleaseScenarios = []string{
	"slow_upstream",
	"http_half_close",
	"http_reset",
	"rate_limit_429",
	"upstream_5xx",
	"bad_gzip",
	"malformed_sse",
	"downstream_stream",
	"client_mid_stream_cancel",
	"responses_ws_connect",
	"responses_ws_idle",
	"responses_ws_frame",
	"responses_ws_cancel",
}

type scenarioResult struct {
	Expected                     string  `json:"expected"`
	Attempts                     uint64  `json:"attempts"`
	Succeeded                    uint64  `json:"succeeded"`
	Failed                       uint64  `json:"failed"`
	LastStatus                   int     `json:"last_status,omitempty"`
	LastCategory                 string  `json:"last_category,omitempty"`
	LastError                    string  `json:"last_error,omitempty"`
	AverageHeadersLatencyMillis  float64 `json:"average_headers_latency_ms"`
	MaximumHeadersLatencyMillis  float64 `json:"maximum_headers_latency_ms"`
	AverageEndToEndLatencyMillis float64 `json:"average_end_to_end_latency_ms"`
	MaximumEndToEndLatencyMillis float64 `json:"maximum_end_to_end_latency_ms"`
	headerLatencyTotalNS         uint64
	endToEndLatencyTotalNS       uint64
}

type scenarioObservation struct {
	name           string
	expected       string
	status         int
	category       string
	headersLatency time.Duration
	endToEnd       time.Duration
	passed         bool
	err            error
}

func runChaosMatrixWithRecovery(ctx context.Context, client *http.Client, transport *http.Transport, cfg configuration, stats *soakStats) error {
	errMatrix := runChaosMatrix(ctx, client, cfg, stats)
	transport.CloseIdleConnections()
	recoverResources(ctx, client, cfg, stats)
	recovery := stats.report().Resources.Recovery
	return errors.Join(errMatrix, recoveryGateError(recovery))
}

func recoveryGateError(recovery resourceRecovery) error {
	if recovery.Attempted && recovery.Recovered {
		return nil
	}
	if len(recovery.Unmet) > 0 {
		return fmt.Errorf("resources did not recover: %s", strings.Join(recovery.Unmet, "; "))
	}
	return errors.New("resources did not return to baseline")
}

func runChaosMatrix(ctx context.Context, client *http.Client, cfg configuration, stats *soakStats) error {
	var failures []error
	for _, name := range requiredReleaseScenarios[:9] {
		observation := runHTTPScenario(ctx, client, cfg, name)
		stats.recordScenario(observation)
		if !observation.passed {
			failures = append(failures, fmt.Errorf("%s: %w", name, observation.err))
		}
		if name != "slow_upstream" && name != "downstream_stream" {
			if errRecovery := waitForScenarioRouteRecovery(ctx, client, cfg); errRecovery != nil {
				failures = append(failures, fmt.Errorf("route recovery after %s: %w", name, errRecovery))
			}
		}
	}
	if cfg.responsesWS {
		for _, name := range requiredReleaseScenarios[9:] {
			observation := runResponsesWebsocketScenario(ctx, cfg, name)
			stats.recordScenario(observation)
			if !observation.passed {
				failures = append(failures, fmt.Errorf("%s: %w", name, observation.err))
			}
		}
	}
	stats.finishScenarioMatrixRun()
	return errors.Join(failures...)
}

func waitForScenarioRouteRecovery(parent context.Context, client *http.Client, cfg configuration) error {
	ctx, cancel := context.WithTimeout(parent, cfg.scenarioTimeout)
	defer cancel()
	var lastErr error
	for {
		observation := runHTTPScenario(ctx, client, cfg, "route_recovery")
		if observation.passed {
			return nil
		}
		lastErr = observation.err
		select {
		case <-ctx.Done():
			return fmt.Errorf("route did not recover before deadline: %w", errors.Join(lastErr, ctx.Err()))
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func runHTTPScenario(parent context.Context, client *http.Client, cfg configuration, name string) scenarioObservation {
	observation := scenarioObservation{name: name, expected: expectedScenarioOutcome(name)}
	started := time.Now()
	ctx, cancel := context.WithTimeout(parent, cfg.scenarioTimeout)
	defer cancel()
	stream := name == "malformed_sse" || name == "downstream_stream" || name == "client_mid_stream_cancel"
	body, errPayload := scenarioPayload(cfg.model, name, stream)
	if errPayload != nil {
		observation.err = errPayload
		return observation
	}
	request, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, cfg.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if errRequest != nil {
		observation.err = errRequest
		return observation
	}
	request.Header.Set("Authorization", "Bearer "+cfg.apiKey)
	request.Header.Set("Content-Type", "application/json")
	response, errDo := client.Do(request)
	observation.headersLatency = time.Since(started)
	if errDo != nil {
		observation.endToEnd = time.Since(started)
		observation.category = "downstream_transport_error"
		observation.err = fmt.Errorf("downstream request failed: %w", errDo)
		return observation
	}
	observation.status = response.StatusCode

	var responseBody []byte
	var errRead error
	if name == "client_mid_stream_cancel" {
		buffer := make([]byte, 4096)
		var read int
		read, errRead = response.Body.Read(buffer)
		if read > 0 {
			responseBody = bytes.Clone(buffer[:read])
		}
	} else {
		responseBody, errRead = io.ReadAll(io.LimitReader(response.Body, maxScenarioResponseBytes+1))
		if len(responseBody) > maxScenarioResponseBytes {
			errRead = fmt.Errorf("scenario response exceeded %d bytes", maxScenarioResponseBytes)
		}
	}
	errClose := response.Body.Close()
	observation.endToEnd = time.Since(started)
	if errRead == nil {
		errRead = errClose
	}
	observation.passed, observation.category, observation.err = evaluateHTTPScenario(name, observation.status, responseBody, observation.headersLatency, errRead)
	return observation
}

func scenarioPayload(model, name string, stream bool) ([]byte, error) {
	return json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]string{{
			"role":    "user",
			"content": soakScenarioPrefix + name,
		}},
		"stream":     stream,
		"max_tokens": 8,
	})
}

func expectedScenarioOutcome(name string) string {
	switch name {
	case "slow_upstream":
		return "HTTP 2xx with fake-upstream marker and headers latency >= 200ms"
	case "route_recovery":
		return "HTTP 2xx with fake-upstream marker"
	case "http_half_close":
		return "controlled HTTP 500/502 proxy transport error"
	case "http_reset":
		return "controlled HTTP 500/502 proxy transport error"
	case "rate_limit_429":
		return "HTTP 429 error envelope"
	case "upstream_5xx":
		return "HTTP 503 error envelope"
	case "bad_gzip":
		return "HTTP 502 upstream decode error"
	case "malformed_sse":
		return "HTTP 200 terminal stream error or bootstrap HTTP 502 error envelope"
	case "downstream_stream":
		return "HTTP 200 SSE stream with data and terminal marker"
	case "client_mid_stream_cancel":
		return "HTTP 200 first stream bytes followed by client cancellation"
	case "responses_ws_connect":
		return "WebSocket 101 upgrade and clean client close"
	case "responses_ws_idle":
		return "idle WebSocket remains usable and completes a response"
	case "responses_ws_frame":
		return "WebSocket emits multiple frames and response.completed"
	case "responses_ws_cancel":
		return "WebSocket emits a frame before client cancellation"
	default:
		return "known deterministic scenario outcome"
	}
}

func evaluateHTTPScenario(name string, status int, body []byte, headersLatency time.Duration, readErr error) (bool, string, error) {
	lowerBody := bytes.ToLower(body)
	hasError := bytes.Contains(lowerBody, []byte("error"))
	switch name {
	case "route_recovery":
		if readErr == nil && status >= 200 && status < 300 && bytes.Contains(body, []byte(soakUpstreamMarker)) {
			return true, "route_recovered", nil
		}
	case "slow_upstream":
		if readErr == nil && status >= 200 && status < 300 && bytes.Contains(body, []byte(soakUpstreamMarker)) && headersLatency >= minimumSlowHeaderLatency {
			return true, "slow_success", nil
		}
	case "http_half_close", "http_reset":
		if readErr == nil && (status == http.StatusInternalServerError || status == http.StatusBadGateway) && hasError {
			return true, "proxy_transport_error", nil
		}
	case "rate_limit_429":
		if readErr == nil && status == http.StatusTooManyRequests && hasError {
			return true, "upstream_rate_limit", nil
		}
	case "upstream_5xx":
		if readErr == nil && status == http.StatusServiceUnavailable && hasError {
			return true, "upstream_5xx", nil
		}
	case "bad_gzip":
		if readErr == nil && status == http.StatusBadGateway && hasError {
			return true, "upstream_decode_error", nil
		}
	case "malformed_sse":
		if readErr == nil && hasError && !bytes.Contains(lowerBody, []byte("response.completed")) &&
			(status == http.StatusOK || status == http.StatusBadGateway) {
			return true, "stream_protocol_error", nil
		}
	case "downstream_stream":
		if readErr == nil && status == http.StatusOK && bytes.Contains(lowerBody, []byte("data:")) &&
			(bytes.Contains(lowerBody, []byte("[done]")) || bytes.Contains(lowerBody, []byte("response.completed"))) {
			return true, "stream_completed", nil
		}
	case "client_mid_stream_cancel":
		if (readErr == nil || errors.Is(readErr, io.EOF)) && status == http.StatusOK && len(bytes.TrimSpace(body)) > 0 {
			return true, "client_cancelled", nil
		}
	}
	return false, observedHTTPScenarioCategory(status, readErr), fmt.Errorf("got status=%d category=%s read_error=%v", status, observedHTTPScenarioCategory(status, readErr), readErr)
}

func observedHTTPScenarioCategory(status int, readErr error) string {
	if readErr != nil {
		return "response_read_error"
	}
	if status == http.StatusTooManyRequests {
		return "upstream_rate_limit"
	}
	if status >= 500 {
		return "server_error"
	}
	if status >= 400 {
		return "client_error"
	}
	if status >= 200 && status < 300 {
		return "unexpected_success"
	}
	return "unexpected_status"
}

func runResponsesWebsocketScenario(parent context.Context, cfg configuration, name string) scenarioObservation {
	observation := scenarioObservation{name: name, expected: expectedScenarioOutcome(name)}
	started := time.Now()
	ctx, cancel := context.WithTimeout(parent, cfg.scenarioTimeout)
	defer cancel()
	endpoint, errURL := responsesWebsocketURL(cfg.baseURL)
	if errURL != nil {
		observation.err = errURL
		return observation
	}
	dialer := websocket.Dialer{HandshakeTimeout: min(cfg.scenarioTimeout, 10*time.Second)}
	headers := http.Header{"Authorization": []string{"Bearer " + cfg.apiKey}}
	conn, response, errDial := dialer.DialContext(ctx, endpoint, headers)
	observation.headersLatency = time.Since(started)
	if response != nil {
		observation.status = response.StatusCode
	}
	if errDial != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		observation.endToEnd = time.Since(started)
		observation.category = "websocket_connect_error"
		observation.err = errDial
		return observation
	}
	if observation.status == 0 {
		observation.status = http.StatusSwitchingProtocols
	}
	deadline := time.Now().Add(cfg.scenarioTimeout)
	_ = conn.SetReadDeadline(deadline)
	_ = conn.SetWriteDeadline(deadline)
	closed := false
	defer func() {
		if !closed {
			_ = conn.Close()
		}
	}()

	switch name {
	case "responses_ws_connect":
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "soak connect complete"), deadline)
		observation.passed = observation.status == http.StatusSwitchingProtocols
		observation.category = "websocket_connected"
	case "responses_ws_idle":
		select {
		case <-ctx.Done():
			observation.err = ctx.Err()
			observation.category = "websocket_idle_timeout"
			_ = conn.Close()
			closed = true
			observation.endToEnd = time.Since(started)
			return observation
		case <-time.After(cfg.websocketIdle):
		}
		observation.passed, observation.category, observation.err = exchangeResponsesWebsocket(conn, cfg.model, "downstream_stream", false)
	case "responses_ws_frame":
		observation.passed, observation.category, observation.err = exchangeResponsesWebsocket(conn, cfg.model, "downstream_stream", true)
	case "responses_ws_cancel":
		observation.passed, observation.category, observation.err = exchangeResponsesWebsocket(conn, cfg.model, "client_mid_stream_cancel", false)
	}
	_ = conn.Close()
	closed = true
	observation.endToEnd = time.Since(started)
	if !observation.passed && observation.err == nil {
		observation.err = fmt.Errorf("got status=%d category=%s", observation.status, observation.category)
	}
	return observation
}

func responsesWebsocketURL(baseURL string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	switch parsed.Scheme {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	default:
		return "", fmt.Errorf("unsupported websocket base URL scheme %q", parsed.Scheme)
	}
	parsed.Path = "/v1/responses"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func exchangeResponsesWebsocket(conn *websocket.Conn, model, upstreamScenario string, requireMultipleFrames bool) (bool, string, error) {
	payload, errMarshal := json.Marshal(map[string]any{
		"type":  "response.create",
		"model": model,
		"input": []map[string]any{{
			"type":    "message",
			"role":    "user",
			"content": soakScenarioPrefix + upstreamScenario,
		}},
	})
	if errMarshal != nil {
		return false, "websocket_request_error", errMarshal
	}
	if errWrite := conn.WriteMessage(websocket.TextMessage, payload); errWrite != nil {
		return false, "websocket_write_error", errWrite
	}
	frames := 0
	for frames < 128 {
		_, frame, errRead := conn.ReadMessage()
		if errRead != nil {
			return false, "websocket_read_error", errRead
		}
		frames++
		var event struct {
			Type string `json:"type"`
		}
		if errDecode := json.Unmarshal(frame, &event); errDecode != nil {
			return false, "websocket_frame_decode_error", errDecode
		}
		if event.Type == "error" {
			return false, "websocket_error_event", fmt.Errorf("websocket returned error event: %s", frame)
		}
		if upstreamScenario == "client_mid_stream_cancel" {
			return true, "websocket_client_cancelled", nil
		}
		if event.Type == "response.completed" || event.Type == "response.done" {
			if requireMultipleFrames && frames < 2 {
				return false, "websocket_frame_count_error", fmt.Errorf("received %d frame, want at least 2", frames)
			}
			return true, "websocket_completed", nil
		}
	}
	return false, "websocket_frame_limit", errors.New("websocket response exceeded 128 frames without completion")
}

func (s *soakStats) recordScenario(observation scenarioObservation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := s.scenarios[observation.name]
	result.Expected = observation.expected
	result.Attempts++
	result.LastStatus = observation.status
	result.LastCategory = observation.category
	result.headerLatencyTotalNS += uint64(max(observation.headersLatency, 0))
	result.endToEndLatencyTotalNS += uint64(max(observation.endToEnd, 0))
	result.AverageHeadersLatencyMillis = float64(result.headerLatencyTotalNS) / float64(result.Attempts) / float64(time.Millisecond)
	result.AverageEndToEndLatencyMillis = float64(result.endToEndLatencyTotalNS) / float64(result.Attempts) / float64(time.Millisecond)
	result.MaximumHeadersLatencyMillis = max(result.MaximumHeadersLatencyMillis, float64(observation.headersLatency)/float64(time.Millisecond))
	result.MaximumEndToEndLatencyMillis = max(result.MaximumEndToEndLatencyMillis, float64(observation.endToEnd)/float64(time.Millisecond))
	if observation.passed {
		result.Succeeded++
		result.LastError = ""
	} else {
		result.Failed++
		if observation.err != nil {
			result.LastError = observation.err.Error()
		}
	}
	s.scenarios[observation.name] = result
}

func (s *soakStats) finishScenarioMatrixRun() {
	s.mu.Lock()
	s.scenarioMatrixRuns++
	s.mu.Unlock()
}

func cloneScenarioResults(source map[string]scenarioResult) map[string]scenarioResult {
	cloned := make(map[string]scenarioResult, len(source))
	for name, result := range source {
		cloned[name] = result
	}
	return cloned
}

func validateReleaseScenarios(report soakReport) error {
	if report.ScenarioMatrixRuns < 2 {
		return fmt.Errorf("release gate completed %d chaos matrix runs, want at least 2", report.ScenarioMatrixRuns)
	}
	for _, name := range requiredReleaseScenarios {
		result, ok := report.Scenarios[name]
		if !ok || result.Attempts != report.ScenarioMatrixRuns {
			return fmt.Errorf("release gate scenario %s attempts=%d, want %d", name, result.Attempts, report.ScenarioMatrixRuns)
		}
		if result.Failed != 0 || result.Succeeded != result.Attempts {
			return fmt.Errorf("release gate scenario %s failed: expected %s, last status=%d category=%s error=%s", name, result.Expected, result.LastStatus, result.LastCategory, result.LastError)
		}
	}
	return nil
}
