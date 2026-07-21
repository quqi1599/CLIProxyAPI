package cache

import (
	"context"
	"testing"
)

func useLocalAntigravityReasoningReplayStore(t *testing.T) {
	t.Helper()
	previous := currentAntigravityReasoningReplayKVClient
	currentAntigravityReasoningReplayKVClient = func() (antigravityReasoningReplayKVClient, bool, error) {
		return nil, false, nil
	}
	t.Cleanup(func() {
		currentAntigravityReasoningReplayKVClient = previous
	})
}

func TestAntigravityReasoningReplayCacheNormalizesMixedItems(t *testing.T) {
	useLocalAntigravityReasoningReplayStore(t)
	ClearAntigravityReasoningReplayCache()
	t.Cleanup(ClearAntigravityReasoningReplayCache)

	items := [][]byte{
		[]byte(`{"type":"thought_signature","thought_signature":"sig","contentIndex":1,"partIndex":2}`),
		[]byte(`{"type":"message","content":"ignored"}`),
		[]byte(`{"type":"function_call_part","functionCall":{"id":"call-1","name":"lookup","args":{"q":"x"}}}`),
	}
	if !CacheAntigravityReasoningReplayItems("gemini-3", "session", items) {
		t.Fatal("cache mixed items failed")
	}

	stored, found := GetAntigravityReasoningReplayItems("gemini-3", "session")
	if !found || len(stored) != 2 {
		t.Fatalf("stored items = %q, %v; want two valid items", stored, found)
	}
	if got := string(stored[0]); got != `{"type":"thought_signature","thoughtSignature":"sig","contentIndex":1,"partIndex":2}` {
		t.Fatalf("thought signature item = %s", got)
	}
	if got := string(stored[1]); got != `{"type":"function_call_part","call_id":"call-1","name":"lookup","args":{"q":"x"}}` {
		t.Fatalf("function call item = %s", got)
	}

	if errDelete := DeleteAntigravityReasoningReplayItemRequired(context.Background(), "gemini-3", "session"); errDelete != nil {
		t.Fatalf("delete replay: %v", errDelete)
	}
	if _, found := GetAntigravityReasoningReplayItems("gemini-3", "session"); found {
		t.Fatal("deleted replay still exists")
	}
}
