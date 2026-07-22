package openai

import (
	"bytes"
	"strings"
	"testing"
	"time"

	requestlogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
)

func TestResponsesWebsocketLoggingOmitsRawPayloadAndErrorMessage(t *testing.T) {
	const secret = "ws-secret-marker-never-log"
	payload := []byte(`{"type":"error","status":502,"error":{"message":"` + secret + `","code":"upstream_busy"}}`)

	preview := websocketPayloadPreview(payload)
	if strings.Contains(preview, secret) || !strings.Contains(preview, "[BODY METADATA v1]") {
		t.Fatalf("websocket preview is not metadata-only: %s", preview)
	}

	errMessage := responsesWebsocketErrorMessageFromPayload(payload)
	if errMessage == nil || errMessage.Error == nil {
		t.Fatal("expected websocket error metadata")
	}
	if got := errMessage.Error.Error(); strings.Contains(got, secret) || got != "upstream websocket error code=upstream_busy" {
		t.Fatalf("websocket error log text = %q", got)
	}
}

func TestResponsesWebsocketErrorDropsUnsafeCode(t *testing.T) {
	payload := []byte(`{"status":500,"error":{"code":"secret value with spaces"}}`)
	errMessage := responsesWebsocketErrorMessageFromPayload(payload)
	if errMessage == nil || errMessage.Error == nil || errMessage.Error.Error() != "upstream websocket error" {
		t.Fatalf("unsafe error code was retained: %+v", errMessage)
	}
}

func TestWebsocketTimelineStoresMetadataOnly(t *testing.T) {
	secret := "unique-websocket-timeline-secret"
	timeline := newInMemoryWebsocketTimelineLog()
	timeline.Append("request", []byte(`{"input":"`+secret+`","tool_output":"private"}`), time.Now())
	timeline.Append("response", []byte(`{"reasoning":"`+secret+`","image":"private"}`), time.Now())

	got := timeline.String()
	if strings.Contains(got, secret) || strings.Contains(got, "tool_output") || strings.Contains(got, "reasoning") || strings.Contains(got, "image") {
		t.Fatalf("timeline leaked raw frame content: %s", got)
	}
	if count := strings.Count(got, "[BODY METADATA v1]"); count != 2 {
		t.Fatalf("timeline metadata count = %d, want 2: %s", count, got)
	}
}

func TestWebsocketTimelineTempSourceStoresMetadataOnly(t *testing.T) {
	secret := "unique-websocket-temp-source-secret"
	source, errSource := requestlogging.NewFileBodySourceInDir(t.TempDir(), "websocket-safe-source")
	if errSource != nil {
		t.Fatalf("NewFileBodySourceInDir: %v", errSource)
	}
	t.Cleanup(func() { _ = source.Cleanup() })
	timeline := newWebsocketTimelineLog(true, source)
	timeline.Append("request", []byte(`{"input":"`+secret+`","tool_output":"private"}`), time.Now())
	timeline.Append("response", []byte(`{"reasoning":"`+secret+`","image":"private"}`), time.Now())

	raw, errRead := source.Bytes()
	if errRead != nil {
		t.Fatalf("source.Bytes: %v", errRead)
	}
	if bytes.Contains(raw, []byte(secret)) || bytes.Contains(raw, []byte("tool_output")) || bytes.Contains(raw, []byte("reasoning")) || bytes.Contains(raw, []byte("image")) {
		t.Fatalf("temp source leaked raw frame content: %s", raw)
	}
	if count := bytes.Count(raw, []byte("[BODY METADATA v1]")); count != 2 {
		t.Fatalf("temp source metadata count = %d, want 2: %s", count, raw)
	}
}

func TestWebsocketPayloadEventTypeRejectsLogInjection(t *testing.T) {
	if got := websocketPayloadEventType([]byte(`{"type":"secret value\nforged"}`)); got != "-" {
		t.Fatalf("unsafe event type = %q, want -", got)
	}
	if got := websocketPayloadEventType([]byte(`{"type":"response.completed"}`)); got != "response.completed" {
		t.Fatalf("safe event type = %q, want response.completed", got)
	}
}
