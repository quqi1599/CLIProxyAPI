package thinking

import "testing"

func TestDeepSeekOfficialEffortVocabulary(t *testing.T) {
	for _, model := range []string{"deepseek-flash", "deepseek-v4.1-flash", "deepseek-v4-flash", "deepseek-v4-pro"} {
		for input, want := range map[string]string{"minimal": "low", "low": "low", "medium": "high", "high": "high", "xhigh": "high", "max": "max", "ultra": "max", "none": "none"} {
			if got := NormalizeDeepSeekOfficialReasoningEffortForModel(model, input); got != want {
				t.Fatalf("%s/%s = %s, want %s", model, input, got, want)
			}
		}
	}
}

func TestDeepSeekV41ModelAliases(t *testing.T) {
	for _, model := range []string{"deepseek-flash", "deepseek-v4.1-flash", "deepseek-v4-flash", "deepseek-v4-flash-vision-exp", "official/deepseek-flash(high)", "deepseek-v4.1-flash[1m]"} {
		if !IsDeepSeekFlashModel(model) || !IsDeepSeekV4Model(model) || !IsDeepSeekReasoningIntentModel(model) {
			t.Fatalf("alias missed: %s", model)
		}
	}
	for _, model := range []string{"deepseek-v4.1-flash-expires-on-0910", "not-deepseek-flash", "deepseek-v4-pro"} {
		if IsDeepSeekFlashModel(model) {
			t.Fatalf("unknown/expired or Pro model accepted as current Flash: %s", model)
		}
	}
}
