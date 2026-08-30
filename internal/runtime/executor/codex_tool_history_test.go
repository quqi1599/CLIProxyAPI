package executor

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/tidwall/gjson"
)

func TestRepairCodexResponsesToolHistoryDropsInvalidToolHistory(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"input": [
			{"type":"message","role":"user","content":[]},
			{"type":"message","role":"user","content":[
				{"type":"unsupported","value":1},
				{"type":"input_text","text":"run"}
			]},
			{"type":"function_call","call_id":"call_ok","name":"read_file","arguments":{"path":"README.md"}},
			{"type":"function_call_output","call_id":"call_ok","output":"ok"},
			{"type":"function_call","call_id":"call_ok","name":"read_file","arguments":"{}"},
			{"type":"function_call","call_id":"call_no_output","name":"grep","arguments":"{}"},
			{"type":"function_call","call_id":"call_missing_name","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_orphan","output":"orphan"},
			{"type":"function_call_output","call_id":"call_ok","output":"duplicate"}
		]
	}`)

	out := repairCodexResponsesToolHistory(body)
	input := gjson.GetBytes(out, "input").Array()
	if len(input) != 3 {
		t.Fatalf("input length = %d, want 3: %s", len(input), string(out))
	}
	if got := input[0].Get("type").String(); got != "message" {
		t.Fatalf("item 0 type = %q, want message: %s", got, string(out))
	}
	if got := input[0].Get("content.#").Int(); got != 1 {
		t.Fatalf("cleaned message content length = %d, want 1: %s", got, string(out))
	}
	if got := input[1].Get("type").String(); got != "function_call" {
		t.Fatalf("item 1 type = %q, want function_call: %s", got, string(out))
	}
	if got := input[1].Get("arguments").String(); got != `{"path":"README.md"}` {
		t.Fatalf("function_call arguments = %q, want serialized object: %s", got, string(out))
	}
	if got := input[2].Get("type").String(); got != "function_call_output" {
		t.Fatalf("item 2 type = %q, want function_call_output: %s", got, string(out))
	}
	if gjson.GetBytes(out, `input.#(call_id=="call_orphan")`).Exists() {
		t.Fatalf("orphan output should be removed: %s", string(out))
	}
	if gjson.GetBytes(out, `input.#(call_id=="call_no_output")`).Exists() {
		t.Fatalf("unanswered call should be removed: %s", string(out))
	}
}

func TestRepairCodexResponsesToolHistoryKeepsPreviousResponseOutput(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"previous_response_id": "resp_1",
		"input": [
			{"type":"function_call_output","call_id":"call_prev","output":"ok"}
		]
	}`)

	out := repairCodexResponsesToolHistory(body)
	input := gjson.GetBytes(out, "input").Array()
	if len(input) != 1 {
		t.Fatalf("input length = %d, want 1: %s", len(input), string(out))
	}
	if got := input[0].Get("call_id").String(); got != "call_prev" {
		t.Fatalf("call_id = %q, want call_prev: %s", got, string(out))
	}
}

func TestRepairCodexResponsesToolHistoryNormalizesClientToolSearchOversizedKey(t *testing.T) {
	t.Parallel()

	oversizedKey := strings.Repeat("x", 300)
	body := []byte(`{
		"input": [
			{"type":"tool_search_call","execution":"client","call_id":"search_1","arguments":{"query":"calendar","limit":3,"` + oversizedKey + `":"garbage"}},
			{"type":"tool_search_output","execution":"client","call_id":"search_1","status":"completed","tools":[]}
		]
	}`)

	out, stats := repairCodexResponsesToolHistoryWithStats(body)
	if stats.toolSearchRepairs != 1 {
		t.Fatalf("tool search repairs = %d, want 1", stats.toolSearchRepairs)
	}
	arguments := gjson.GetBytes(out, "input.0.arguments")
	if got := arguments.Get("query").String(); got != "calendar" {
		t.Fatalf("query = %q, want calendar: %s", got, string(out))
	}
	if got := arguments.Get("limit").Int(); got != 3 {
		t.Fatalf("limit = %d, want 3: %s", got, string(out))
	}
	if got := len(arguments.Map()); got != 2 {
		t.Fatalf("arguments fields = %d, want 2: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "input.#").Int(); got != 2 {
		t.Fatalf("input length = %d, want 2: %s", got, string(out))
	}
}

func TestRepairCodexResponsesToolHistoryParsesStringClientToolSearchArguments(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"input": [
			{"type":"tool_search_call","execution":"client","call_id":"search_2","arguments":"{\"query\":\"files\",\"limit\":4,\"junk\":true}"},
			{"type":"tool_search_output","execution":"client","call_id":"search_2","status":"completed","tools":[]}
		]
	}`)

	out, stats := repairCodexResponsesToolHistoryWithStats(body)
	if stats.toolSearchRepairs != 1 {
		t.Fatalf("tool search repairs = %d, want 1", stats.toolSearchRepairs)
	}
	arguments := gjson.GetBytes(out, "input.0.arguments")
	if !arguments.IsObject() {
		t.Fatalf("arguments should be an object: %s", string(out))
	}
	if got := arguments.Get("query").String(); got != "files" {
		t.Fatalf("query = %q, want files: %s", got, string(out))
	}
	if got := arguments.Get("limit").Int(); got != 4 {
		t.Fatalf("limit = %d, want 4: %s", got, string(out))
	}
	if got := len(arguments.Map()); got != 2 {
		t.Fatalf("arguments fields = %d, want 2: %s", got, string(out))
	}
}

func TestRepairCodexResponsesToolHistoryDropsClientToolSearchWithoutQuery(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"input": [
			{"type":"message","role":"user","content":"continue"},
			{"type":"tool_search_call","execution":"client","call_id":"search_bad","arguments":{"limit":3,"garbage":"bad"}},
			{"type":"tool_search_output","execution":"client","call_id":"search_bad","status":"failed","tools":[]},
			{"type":"function_call_output","call_id":"","output":"failed to parse tool_search arguments"}
		]
	}`)

	out, stats := repairCodexResponsesToolHistoryWithStats(body)
	if stats.toolSearchRepairs != 1 {
		t.Fatalf("tool search repairs = %d, want 1", stats.toolSearchRepairs)
	}
	input := gjson.GetBytes(out, "input").Array()
	if len(input) != 1 || input[0].Get("type").String() != "message" {
		t.Fatalf("input should retain only the message: %s", string(out))
	}
	if strings.Contains(string(out), "search_bad") || strings.Contains(string(out), "failed to parse tool_search arguments") {
		t.Fatalf("malformed tool search history survived repair: %s", string(out))
	}
}

func TestRepairCodexResponsesToolHistoryLeavesValidAndUnrelatedCallsUnchanged(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"input": [
			{"type":"tool_search_call","execution":"client","call_id":"search_client","arguments":{"query":"calendar","limit":3}},
			{"type":"tool_search_output","execution":"client","call_id":"search_client","status":"completed","tools":[]},
			{"type":"tool_search_call","execution":"server","call_id":"search_server","arguments":{"unexpected":true}},
			{"type":"tool_search_output","execution":"server","call_id":"search_server","status":"completed","tools":[]},
			{"type":"function_call","call_id":"function_1","name":"read_file","arguments":"{}"},
			{"type":"function_call_output","call_id":"function_1","output":"ok"},
			{"type":"custom_tool_call","call_id":"custom_1","name":"apply_patch","input":"patch"},
			{"type":"custom_tool_call_output","call_id":"custom_1","output":"ok"}
		]
	}`)

	out, stats := repairCodexResponsesToolHistoryWithStats(body)
	if stats.toolSearchRepairs != 0 {
		t.Fatalf("tool search repairs = %d, want 0", stats.toolSearchRepairs)
	}
	if !bytes.Equal(out, body) {
		t.Fatalf("valid and unrelated calls changed:\nwant: %s\n got: %s", string(body), string(out))
	}
}

func TestLogCodexToolSearchHistoryRepairEmitsSafeCountOnly(t *testing.T) {
	hook := logtest.NewGlobal()
	defer hook.Reset()
	hook.Reset()

	logCodexToolSearchHistoryRepair(logging.WithRequestID(context.Background(), "req-tool-search-1"), 2)
	entries := hook.AllEntries()
	if len(entries) != 1 {
		t.Fatalf("log entries = %d, want 1: %+v", len(entries), entries)
	}
	entry := entries[0]
	if got := entry.Data["event"]; got != "codex_tool_search_history_repaired" {
		t.Fatalf("event = %#v, want codex_tool_search_history_repaired", got)
	}
	if got := entry.Data["repairs_count"]; got != 2 {
		t.Fatalf("repairs_count = %#v, want 2", got)
	}
	if got := entry.Data["request_id"]; got != "req-tool-search-1" {
		t.Fatalf("request_id = %#v, want req-tool-search-1", got)
	}
	for field := range entry.Data {
		switch field {
		case "event", "repairs_count", "request_id":
		default:
			t.Fatalf("unexpected log field %q: %+v", field, entry.Data)
		}
	}

	logCodexToolSearchHistoryRepair(context.Background(), 0)
	if got := len(hook.AllEntries()); got != 1 {
		t.Fatalf("zero repairs emitted a log entry; entries = %d", got)
	}
}
