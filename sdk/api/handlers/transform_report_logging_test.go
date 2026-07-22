package handlers

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestTransformReportLogObserverEmitsMetadataOnly(t *testing.T) {
	const secret = "secret-prompt-must-not-appear"
	hook := logtest.NewGlobal()
	hook.Reset()
	t.Cleanup(hook.Reset)
	previousLevel := log.GetLevel()
	log.SetLevel(log.InfoLevel)
	t.Cleanup(func() { log.SetLevel(previousLevel) })

	ctx := logging.WithRequestID(context.Background(), "req-transform-1")
	ctx = internalpayload.WithTransformReportBytes(ctx, 80, int64(len(secret)))
	if !addTransformReportLogObserver(ctx) {
		t.Fatal("observer was not registered")
	}
	release := internalpayload.RetainTransformReport(ctx)
	internalpayload.RecordTransformStage(ctx, internalpayload.TransformStageReport{
		Stage:        "executor/upstream-request",
		InputBytes:   int64(len(secret)),
		OutputBytes:  int64(len(secret) + 4),
		PatchedCount: 3,
	}, internalpayload.AmplificationOverride{})
	release()

	entries := hook.AllEntries()
	var found bool
	for _, entry := range entries {
		if entry.Data["event"] != "payload_transform_summary" {
			continue
		}
		found = true
		if entry.Data["request_id"] != "req-transform-1" || entry.Data["wire_input_bytes"] != int64(80) {
			t.Fatalf("unexpected transform log fields: %#v", entry.Data)
		}
		if entry.Data["transform_stage_count"] != 1 || !strings.Contains(fmt.Sprint(entry.Data["transform_stages"]), "executor/upstream-request") {
			t.Fatalf("missing stage metadata: %#v", entry.Data)
		}
		if entry.Data["transform_patched_count"] != int64(3) {
			t.Fatalf("patched count = %#v", entry.Data["transform_patched_count"])
		}
		if strings.Contains(fmt.Sprint(entry.Data), secret) || strings.Contains(entry.Message, secret) {
			t.Fatalf("transform log leaked request body: %#v", entry.Data)
		}
	}
	if !found {
		t.Fatalf("payload transform summary was not logged: %#v", entries)
	}
}

func TestAdmissionRegistersOneTransformObserverAcrossNestedExecution(t *testing.T) {
	hook := logtest.NewGlobal()
	hook.Reset()
	t.Cleanup(hook.Reset)
	previousLevel := log.GetLevel()
	log.SetLevel(log.InfoLevel)
	t.Cleanup(func() { log.SetLevel(previousLevel) })

	var handler *BaseAPIHandler
	ctx := logging.WithRequestID(context.Background(), "req-transform-nested")
	ctx, releaseOuter, err := handler.inspectAndAcquireAdmission(ctx, []byte(`{"messages":[]}`), &modelExecutionOptions{})
	if err != nil {
		t.Fatalf("outer admission: %v", err)
	}
	_, releaseNested, err := handler.inspectAndAcquireAdmission(ctx, []byte(`{"messages":[{}]}`), &modelExecutionOptions{InternalSource: true})
	if err != nil {
		t.Fatalf("nested admission: %v", err)
	}
	releaseNested()
	releaseOuter()

	count := 0
	for _, entry := range hook.AllEntries() {
		if entry.Data["event"] == "payload_transform_summary" && entry.Data["request_id"] == "req-transform-nested" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("nested execution emitted %d transform summaries, want 1", count)
	}
}
