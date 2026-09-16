package auth

import (
	"context"
	"reflect"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestDeepSeekResponsesCapabilityPrefilter(t *testing.T) {
	coding := &Auth{ID: "coding", Provider: "openai-compatibility", Attributes: map[string]string{"base_url": "https://ark.cn-beijing.volces.com/api/coding/v1", "api_key": "test"}}
	standard := &Auth{ID: "ark-standard", Provider: "openai-compatibility", Attributes: map[string]string{"base_url": "https://ark.cn-beijing.volces.com/api/v3"}}
	claude := &Auth{ID: "claude", Provider: "claude", Attributes: map[string]string{"base_url": "https://ark.cn-beijing.volces.com/api/coding/v1"}}
	all := []*Auth{coding, standard, claude}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
	got, n := prefilterDeepSeekResponsesAuths(all, "deepseek-v4-flash", opts)
	if n != 1 || !reflect.DeepEqual(authIDs(got), []string{"ark-standard", "claude"}) {
		t.Fatalf("got=%v excluded=%d", authIDs(got), n)
	}
	if all[0] != coding {
		t.Fatal("mutated caller slice")
	}
	got, n = prefilterDeepSeekResponsesAuths([]*Auth{coding}, "deepseek-v4-flash", opts)
	if len(got) != 1 || n != 0 {
		t.Fatal("lost actionable executor rejection for only route")
	}
	for _, tc := range []struct {
		model string
		opts  cliproxyexecutor.Options
	}{
		{"gpt-5", opts},
		{"deepseek-flash", cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI}},
		{"deepseek-flash", cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: "coding"}}},
	} {
		if got, n := prefilterDeepSeekResponsesAuths(all, tc.model, tc.opts); n != 0 || len(got) != len(all) {
			t.Fatal("unrelated or pinned request changed")
		}
	}
}

func TestManagerDeepSeekPrefilterPrecedesPriority(t *testing.T) {
	const model = "deepseek-flash"
	const provider = "openai-compatibility"
	m := NewManager(nil, &RoundRobinSelector{}, nil)
	m.executors[provider] = schedulerTestExecutor{}
	registerSchedulerModels(t, provider, model, "coding-first", "compatible-second")
	for _, a := range []*Auth{
		{ID: "coding-first", Provider: provider, Attributes: map[string]string{"base_url": "https://ark.cn-beijing.volces.com/api/coding/v1", "priority": "100", "api_key": "test"}},
		{ID: "compatible-second", Provider: provider, Attributes: map[string]string{"base_url": "https://api.deepseek.com/v1", "priority": "0"}},
	} {
		if _, err := m.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
	for _, mixed := range []bool{false, true} {
		var a *Auth
		var err error
		if mixed {
			a, _, _, err = m.pickNextMixed(context.Background(), []string{provider}, model, opts, map[string]struct{}{})
		} else {
			a, _, err = m.pickNext(context.Background(), provider, model, opts, map[string]struct{}{})
		}
		if err != nil || a == nil || a.ID != "compatible-second" {
			t.Fatalf("mixed=%v selected=%v err=%v", mixed, a, err)
		}
	}
}
