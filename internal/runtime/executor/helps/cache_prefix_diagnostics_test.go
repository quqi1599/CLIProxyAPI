package helps

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func newTestCachePrefixDiagnostics(t *testing.T, threshold, maxEntries int) *CachePrefixDiagnostics {
	t.Helper()
	return NewCachePrefixDiagnostics(CachePrefixDiagnosticsOptions{
		Enabled:             true,
		SampleRate:          1,
		CompareWindow:       10 * time.Minute,
		StableMissThreshold: threshold,
		MaxEntries:          maxEntries,
		Secret:              []byte("independent-test-secret"),
		KeyID:               "test-v1",
	})
}

func testCacheDiagnosticMeta(affinity, auth string) CachePrefixDiagnosticMeta {
	return CachePrefixDiagnosticMeta{
		Model:            "deepseek-v4-flash",
		SourceFormat:     "claude",
		RouteKind:        "deepseek",
		BaseHost:         "api.deepseek.com",
		Endpoint:         "/anthropic/v1/messages",
		ConsumerIdentity: "test-client-api-key-value",
		AuthIdentity:     auth,
		AffinityIdentity: affinity,
	}
}

func completeCacheDiagnostic(t *testing.T, session *CachePrefixDiagnosticSession, cacheRead int64) CachePrefixDiagnosticEvent {
	t.Helper()
	response := []byte(`{"id":"msg_1","content":[{"type":"text","text":"assistant-secret-output"}],"usage":{"input_tokens":10,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":2}}`)
	if cacheRead != 0 {
		response = []byte(`{"id":"msg_1","content":[{"type":"text","text":"assistant-secret-output"}],"usage":{"input_tokens":10,"cache_read_input_tokens":9,"cache_creation_input_tokens":0,"output_tokens":2}}`)
	}
	event, ok := session.CompleteNonStream(response)
	if !ok {
		t.Fatal("CompleteNonStream() did not emit an event")
	}
	return event
}

func TestCachePrefixDiagnosticsCanonicalizesJSONAndIgnoresCacheControl(t *testing.T) {
	diagnostics := newTestCachePrefixDiagnostics(t, 3, 100)
	bodyA := []byte(`{
		"system":[{"type":"text","text":"private-system","cache_control":{"type":"ephemeral","ttl":"5m"}}],
		"tools":[{"name":"PrivateTool","description":"private-description","input_schema":{"type":"object","properties":{"b":{"type":"string"},"a":{"type":"number"}}}}],
		"messages":[{"role":"user","content":[{"type":"text","text":"private-question"}]}]
	}`)
	bodyB := []byte(`{"messages":[{"content":[{"text":"private-question","type":"text"}],"role":"user"}],"tools":[{"input_schema":{"properties":{"a":{"type":"number"},"b":{"type":"string"}},"type":"object"},"description":"private-description","name":"PrivateTool"}],"system":[{"text":"private-system","type":"text"}]}`)

	fingerprintA, okA := diagnostics.fingerprintMessages(bodyA)
	fingerprintB, okB := diagnostics.fingerprintMessages(bodyB)
	if !okA || !okB {
		t.Fatal("fingerprintMessages() rejected valid Messages payload")
	}
	if !reflect.DeepEqual(fingerprintA, fingerprintB) {
		t.Fatalf("semantic fingerprints differ:\nA=%+v\nB=%+v", fingerprintA, fingerprintB)
	}
	counts := countCacheControls(bodyA)
	if counts.Total != 1 || counts.TTL5m != 1 || counts.TTL1h != 0 {
		t.Fatalf("cache-control counts = %+v", counts)
	}
}

func TestCachePrefixDiagnosticsLocatesChangedSegment(t *testing.T) {
	diagnostics := newTestCachePrefixDiagnostics(t, 3, 100)
	base := []byte(`{"system":"s1","tools":[{"name":"tool-a"}],"messages":[{"role":"user","content":"question"}]}`)
	systemChanged := []byte(`{"system":"s2","tools":[{"name":"tool-a"}],"messages":[{"role":"user","content":"question"}]}`)
	toolsChanged := []byte(`{"system":"s1","tools":[{"name":"tool-b"}],"messages":[{"role":"user","content":"question"}]}`)
	baseFingerprint, _ := diagnostics.fingerprintMessages(base)
	systemFingerprint, _ := diagnostics.fingerprintMessages(systemChanged)
	toolsFingerprint, _ := diagnostics.fingerprintMessages(toolsChanged)
	if got := firstCacheDiagnosticPartChange(baseFingerprint, systemFingerprint); got != "system" {
		t.Fatalf("system change = %q", got)
	}
	if got := firstCacheDiagnosticPartChange(baseFingerprint, toolsFingerprint); got != "tools" {
		t.Fatalf("tools change = %q", got)
	}
}

func TestCachePrefixDiagnosticsClassifiesColdHitAndStableAnomaly(t *testing.T) {
	diagnostics := newTestCachePrefixDiagnostics(t, 3, 100)
	body := []byte(`{"system":"stable","messages":[{"role":"user","content":"same"}]}`)
	meta := testCacheDiagnosticMeta("session-stable", "auth-1")

	first := completeCacheDiagnostic(t, diagnostics.Begin(meta, body, body, body), 0)
	if first.Classification != cacheDiagnosticColdMiss {
		t.Fatalf("first classification = %q", first.Classification)
	}
	second := completeCacheDiagnostic(t, diagnostics.Begin(meta, body, body, body), 0)
	if second.Classification != cacheDiagnosticStableMiss {
		t.Fatalf("second classification = %q", second.Classification)
	}
	third := completeCacheDiagnostic(t, diagnostics.Begin(meta, body, body, body), 0)
	if third.Classification != cacheDiagnosticStableMiss {
		t.Fatalf("third classification = %q", third.Classification)
	}
	fourth := completeCacheDiagnostic(t, diagnostics.Begin(meta, body, body, body), 0)
	if fourth.Classification != cacheDiagnosticStableAnomaly {
		t.Fatalf("fourth classification = %q", fourth.Classification)
	}
	hit := completeCacheDiagnostic(t, diagnostics.Begin(meta, body, body, body), 9)
	if hit.Classification != cacheDiagnosticHit || hit.CacheReadInputTokens != 9 {
		t.Fatalf("hit event = %+v", hit)
	}
}

func TestCachePrefixDiagnosticsRecognizesNormalConversationExtension(t *testing.T) {
	diagnostics := newTestCachePrefixDiagnostics(t, 3, 100)
	meta := testCacheDiagnosticMeta("session-extension", "auth-1")
	firstBody := []byte(`{"system":"stable","messages":[{"role":"user","content":"first"}]}`)
	first := completeCacheDiagnostic(t, diagnostics.Begin(meta, firstBody, firstBody, firstBody), 0)
	if first.TurnEndPrefixAnchor == "" {
		t.Fatal("first request did not produce turn-end anchor")
	}
	secondBody := []byte(`{"system":"stable","messages":[{"role":"user","content":"first"},{"role":"assistant","content":[{"type":"text","text":"assistant-secret-output"}]},{"role":"user","content":"second"}]}`)
	second := completeCacheDiagnostic(t, diagnostics.Begin(meta, secondBody, secondBody, secondBody), 0)
	if second.Classification != cacheDiagnosticStableMiss || second.PrefixAnchorMatch != "prior_turn_end" {
		t.Fatalf("extension event = %+v", second)
	}
}

func TestCachePrefixDiagnosticsDetectsCPAAndRouteChanges(t *testing.T) {
	diagnostics := newTestCachePrefixDiagnostics(t, 3, 100)
	received := []byte(`{"system":"stable","messages":[{"role":"user","content":"same"}]}`)
	meta := testCacheDiagnosticMeta("session-change", "auth-1")
	completeCacheDiagnostic(t, diagnostics.Begin(meta, received, received, received), 0)

	changedUpstream := []byte(`{"system":"dynamic-cpa-value","messages":[{"role":"user","content":"same"}]}`)
	cpaChanged := completeCacheDiagnostic(t, diagnostics.Begin(meta, received, received, changedUpstream), 0)
	if cpaChanged.Classification != cacheDiagnosticCPAChanged || cpaChanged.FirstChangedStage != "upstream_final" || cpaChanged.FirstChangedPart != "system" {
		t.Fatalf("CPA change event = %+v", cpaChanged)
	}

	meta.AuthIdentity = "auth-2"
	routeChanged := completeCacheDiagnostic(t, diagnostics.Begin(meta, received, received, received), 0)
	if routeChanged.Classification != cacheDiagnosticRouteChanged {
		t.Fatalf("route/auth classification = %q", routeChanged.Classification)
	}
}

func TestCachePrefixDiagnosticsStreamUsageAndCompletion(t *testing.T) {
	diagnostics := newTestCachePrefixDiagnostics(t, 3, 100)
	body := []byte(`{"messages":[{"role":"user","content":"stream-secret-question"}]}`)
	session := diagnostics.Begin(testCacheDiagnosticMeta("session-stream", "auth-1"), body, body, body)
	lines := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":2,"cache_read_input_tokens":100,"cache_creation_input_tokens":3,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"stream-secret-output"}}`),
		[]byte(`data: {"type":"content_block_stop","index":0}`),
		[]byte(`data: {"type":"message_delta","usage":{"output_tokens":4}}`),
		[]byte(`data: {"type":"message_stop"}`),
	}
	var event CachePrefixDiagnosticEvent
	emitted := false
	for _, line := range lines {
		if current, ok := session.ObserveStreamLine(line); ok {
			event = current
			emitted = true
		}
	}
	if !emitted || event.Classification != cacheDiagnosticHit || event.CacheReadInputTokens != 100 || event.OutputTokens != 4 || event.UsageSource != "message_start+message_delta" {
		t.Fatalf("stream event = %+v", event)
	}
	if event.TurnEndPrefixAnchor == "" {
		t.Fatal("complete text stream did not generate a turn-end anchor")
	}
	extendedBody := []byte(`{"messages":[{"role":"user","content":"stream-secret-question"},{"role":"assistant","content":[{"type":"text","text":"stream-secret-output"}]},{"role":"user","content":"next"}]}`)
	extended := completeCacheDiagnostic(t, diagnostics.Begin(testCacheDiagnosticMeta("session-stream", "auth-1"), extendedBody, extendedBody, extendedBody), 0)
	if extended.PrefixAnchorMatch != "prior_turn_end" || extended.Classification != cacheDiagnosticStableMiss {
		t.Fatalf("stream extension event = %+v", extended)
	}

	incomplete := diagnostics.Begin(testCacheDiagnosticMeta("session-incomplete", "auth-1"), body, body, body)
	if _, ok := incomplete.ObserveStreamLine(lines[0]); ok {
		t.Fatal("incomplete stream emitted an event")
	}
}

func TestCachePrefixDiagnosticsDoesNotRetainStreamToolInputForTurnAnchor(t *testing.T) {
	diagnostics := newTestCachePrefixDiagnostics(t, 3, 100)
	body := []byte(`{"messages":[{"role":"user","content":"tool-question"}]}`)
	session := diagnostics.Begin(testCacheDiagnosticMeta("session-tool", "auth-1"), body, body, body)
	lines := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"PrivateTool","input":{}}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"private\":\"value\"}"}}`),
		[]byte(`data: {"type":"content_block_stop","index":0}`),
		[]byte(`data: {"type":"message_delta","usage":{"output_tokens":4}}`),
		[]byte(`data: {"type":"message_stop"}`),
	}
	var event CachePrefixDiagnosticEvent
	emitted := false
	for _, line := range lines {
		if current, ok := session.ObserveStreamLine(line); ok {
			event = current
			emitted = true
		}
	}
	if !emitted {
		t.Fatal("tool stream did not emit request/usage diagnostics")
	}
	if event.TurnEndPrefixAnchor != "" {
		t.Fatalf("tool stream retained enough output to create turn-end anchor: %+v", event)
	}
}

func TestCachePrefixDiagnosticsEventDoesNotLeakRawValues(t *testing.T) {
	diagnostics := newTestCachePrefixDiagnostics(t, 3, 100)
	body := []byte(`{"system":"private-system","tools":[{"name":"PrivateTool"}],"messages":[{"role":"user","content":"private-question"}]}`)
	meta := testCacheDiagnosticMeta("private-session-id", "private-auth-id")
	event := completeCacheDiagnostic(t, diagnostics.Begin(meta, body, body, body), 0)
	encoded, err := json.Marshal(event.Fields())
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, secret := range []string{"private-system", "PrivateTool", "private-question", "assistant-secret-output", "test-client-api-key-value", "private-session-id", "private-auth-id", "independent-test-secret"} {
		if strings.Contains(text, secret) {
			t.Fatalf("diagnostic event leaked %q: %s", secret, text)
		}
	}
}

func TestCachePrefixDiagnosticsRequiresSecretAndBoundsState(t *testing.T) {
	if got := NewCachePrefixDiagnostics(CachePrefixDiagnosticsOptions{Enabled: true, SampleRate: 1}); got != nil {
		t.Fatal("diagnostics enabled without secret")
	}
	diagnostics := newTestCachePrefixDiagnostics(t, 3, 2)
	body := []byte(`{"messages":[{"role":"user","content":"same"}]}`)
	for _, affinity := range []string{"one", "two", "three"} {
		completeCacheDiagnostic(t, diagnostics.Begin(testCacheDiagnosticMeta(affinity, "auth"), body, body, body), 0)
	}
	diagnostics.mu.Lock()
	defer diagnostics.mu.Unlock()
	if len(diagnostics.history) > 2 {
		t.Fatalf("history size = %d, want <= 2", len(diagnostics.history))
	}
}

func BenchmarkCachePrefixDiagnosticsBegin256KiBSampled(b *testing.B) {
	diagnostics := NewCachePrefixDiagnostics(CachePrefixDiagnosticsOptions{
		Enabled:             true,
		SampleRate:          1,
		CompareWindow:       10 * time.Minute,
		StableMissThreshold: 3,
		MaxEntries:          100,
		Secret:              []byte("benchmark-independent-secret"),
	})
	text := strings.Repeat("x", 256*1024)
	body, err := json.Marshal(map[string]any{
		"system": "stable",
		"messages": []any{
			map[string]any{"role": "user", "content": text},
		},
	})
	if err != nil {
		b.Fatal(err)
	}
	compatibleBody, err := json.Marshal(map[string]any{
		"model":      "deepseek-v4-flash",
		"max_tokens": 4096,
		"system":     "stable",
		"messages": []any{
			map[string]any{"role": "user", "content": text},
		},
	})
	if err != nil {
		b.Fatal(err)
	}
	upstreamBody, err := json.Marshal(map[string]any{
		"model":      "deepseek-v4-flash",
		"max_tokens": 4096,
		"stream":     true,
		"system":     "stable",
		"messages": []any{
			map[string]any{"role": "user", "content": text},
		},
	})
	if err != nil {
		b.Fatal(err)
	}
	meta := testCacheDiagnosticMeta("benchmark-session", "benchmark-auth")
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if session := diagnostics.Begin(meta, body, compatibleBody, upstreamBody); session == nil {
			b.Fatal("sampled diagnostic session is nil")
		}
	}
}
