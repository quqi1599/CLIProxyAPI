package registry

import "testing"

func TestKimiCodeCatalog(t *testing.T) {
	for _, id := range []string{"k3.1-pro", "k3.1-pro-highspeed", "k3", "k3-256k", "kimi-for-coding", "kimi-for-coding-highspeed"} {
		want := 1048576
		if id == "k3-256k" || id == "kimi-for-coding-highspeed" {
			want = 262144
		}
		info := LookupStaticModelInfo(id)
		if info == nil || info.ContextLength != want || info.MaxCompletionTokens != 0 {
			t.Fatalf("invalid metadata for %s: %+v", id, info)
		}
		count := 0
		for _, listed := range GetKimiModels() {
			if listed.ID == id {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("%s listed %d times", id, count)
		}
		info.Thinking.ZeroAllowed = !info.Thinking.ZeroAllowed
		if LookupStaticModelInfo(id).Thinking.ZeroAllowed == info.Thinking.ZeroAllowed {
			t.Fatal("shared mutable catalog")
		}
	}
	stale := []*ModelInfo{{ID: "k3.1-pro", ContextLength: 1}, {ID: "custom-model"}}
	updated := withKimiCodeBuiltins(stale)
	if len(updated) != 7 {
		t.Fatalf("builtins not restored: %+v", updated)
	}
	for _, info := range updated {
		if info.ID == "k3.1-pro" && info.ContextLength != 1048576 {
			t.Fatal("stale entry not replaced")
		}
	}
}
