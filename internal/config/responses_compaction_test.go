package config

import "testing"

func TestSanitizeResponsesCompactionConfig(t *testing.T) {
	t.Parallel()
	contextManagement := true
	got := sanitizeResponsesCompactionConfig(ResponsesCompactionConfig{
		LegacyEndpoint:     " NATIVE ",
		TriggerMode:        " Bridge-Legacy ",
		ContextManagement:  &contextManagement,
		CompatibilityGroup: " group-a ",
	})
	if got.LegacyEndpoint != "native" || got.TriggerMode != "bridge-legacy" || got.ContextManagement == nil || !*got.ContextManagement || got.CompatibilityGroup != "group-a" {
		t.Fatalf("sanitizeResponsesCompactionConfig() = %+v", got)
	}
	invalid := sanitizeResponsesCompactionConfig(ResponsesCompactionConfig{LegacyEndpoint: "auto", TriggerMode: "fallback"})
	if invalid.LegacyEndpoint != "unsupported" || invalid.TriggerMode != "unsupported" {
		t.Fatalf("invalid modes = %+v, want unsupported", invalid)
	}
}
