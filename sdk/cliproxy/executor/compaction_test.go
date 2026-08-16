package executor

import "testing"

func TestDetectCompactionIntent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		payload string
		alt     string
		want    CompactionIntent
		wantErr bool
	}{
		{name: "none", payload: `{"input":[{"type":"message"}]}`, want: CompactionIntentNone},
		{name: "legacy", payload: `{}`, alt: ResponsesCompactAlt, want: CompactionIntentLegacyEndpoint},
		{name: "v2", payload: `{"input":[{"type":"message"},{"type":"compaction_trigger"}]}`, want: CompactionIntentV2Trigger},
		{name: "v2 trigger not final", payload: `{"input":[{"type":"compaction_trigger"},{"type":"message"}]}`, wantErr: true},
		{name: "v2 duplicate trigger", payload: `{"input":[{"type":"compaction_trigger"},{"type":"compaction_trigger"}]}`, wantErr: true},
		{name: "context management", payload: `{"context_management":[{"type":"compaction","compact_threshold":12000}],"input":[]}`, want: CompactionIntentContextManagement},
		{name: "conflicting protocols", payload: `{"context_management":[{"type":"compaction"}],"input":[{"type":"compaction_trigger"}]}`, wantErr: true},
		{name: "replay", payload: `{"input":[{"type":"compaction","encrypted_content":"opaque"},{"type":"message"}]}`, want: CompactionIntentReplay},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := DetectCompactionIntent([]byte(test.payload), test.alt)
			if test.wantErr {
				if err == nil {
					t.Fatalf("DetectCompactionIntent() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("DetectCompactionIntent() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("DetectCompactionIntent() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestCompactionIntentFromOptionsPrefersMetadata(t *testing.T) {
	t.Parallel()
	opts := Options{
		Alt: ResponsesCompactAlt,
		Metadata: map[string]any{
			CompactionIntentMetadataKey: string(CompactionIntentV2Trigger),
		},
	}
	if got := CompactionIntentFromOptions(Request{}, opts); got != CompactionIntentV2Trigger {
		t.Fatalf("CompactionIntentFromOptions() = %q, want %q", got, CompactionIntentV2Trigger)
	}
}
