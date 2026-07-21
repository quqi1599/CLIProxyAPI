package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestPhasesAreFixedAndOrdered(t *testing.T) {
	want := []Phase{
		PreTranslateInspect,
		PostTranslateCanonicalize,
		ApplyThinking,
		RepairHistory,
		ProviderCapabilityScrub,
		ProviderQuirkPatch,
		ApplyUserPayloadConfig,
		PostConfigRevalidate,
		FinalizeHeadersAndSignature,
		AmplificationGuard,
	}
	got := Phases()
	if len(got) != len(want) {
		t.Fatalf("phase count = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] || phaseRank(got[i]) != i {
			t.Fatalf("phase[%d] = %q rank %d, want %q rank %d", i, got[i], phaseRank(got[i]), want[i], i)
		}
	}
}

func TestRegistryOrderDoesNotDependOnRegistrationOrder(t *testing.T) {
	policies := []Policy{
		validPolicy("z-last-id", PreTranslateInspect, 10, "body.z"),
		validPolicy("a-first-id", PreTranslateInspect, 10, "body.a"),
		validPolicy("priority-first", PreTranslateInspect, -1, "body.priority"),
		validPolicy("later-phase", RepairHistory, -100, "body.history"),
	}
	want := []string{"priority-first", "a-first-id", "z-last-id", "later-phase"}
	orders := [][]Policy{
		{policies[0], policies[1], policies[2], policies[3]},
		{policies[3], policies[2], policies[1], policies[0]},
		{policies[1], policies[3], policies[0], policies[2]},
	}
	for i, order := range orders {
		registry, err := NewRegistry(order...)
		if err != nil {
			t.Fatalf("order %d: NewRegistry() error = %v", i, err)
		}
		got := policyIDs(registry.Policies())
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("order %d: policy IDs = %v, want %v", i, got, want)
		}
	}
}

func TestRegistryRejectsIdentityPhaseAndFieldConflicts(t *testing.T) {
	t.Run("duplicate ID", func(t *testing.T) {
		first := validPolicy("duplicate", PreTranslateInspect, 0, "body.first")
		second := validPolicy("duplicate", RepairHistory, 0, "body.second")
		assertRegistryErrorContains(t, []Policy{first, second}, "duplicate policy ID")
	})

	t.Run("unknown phase", func(t *testing.T) {
		policy := validPolicy("unknown-phase", Phase("BeforeEverything"), 0, "body.model")
		assertRegistryErrorContains(t, []Policy{policy}, "unknown phase")
	})

	t.Run("overlapping field", func(t *testing.T) {
		first := validPolicy("first", ProviderQuirkPatch, 0, "body.thinking")
		first.Match.CompatKind = "kimi"
		second := validPolicy("second", ProviderQuirkPatch, 100, "body.thinking")
		second.Match.CompatKind = "kimi"
		assertRegistryErrorContains(t, []Policy{first, second}, "conflict on field")
	})

	t.Run("repeated field", func(t *testing.T) {
		policy := validPolicy("repeated-field", ProviderQuirkPatch, 0, "body.thinking")
		policy.MutatedFields = append(policy.MutatedFields, " body.thinking ")
		assertRegistryErrorContains(t, []Policy{policy}, "repeats mutated field")
	})

	t.Run("disjoint match", func(t *testing.T) {
		first := validPolicy("kimi", ProviderQuirkPatch, 0, "body.thinking")
		first.Match.CompatKind = "kimi"
		second := validPolicy("qwen", ProviderQuirkPatch, 0, "body.thinking")
		second.Match.CompatKind = "qwen"
		if _, err := NewRegistry(first, second); err != nil {
			t.Fatalf("NewRegistry() rejected disjoint matches: %v", err)
		}
	})

	t.Run("different phase", func(t *testing.T) {
		first := validPolicy("canonicalize", PostTranslateCanonicalize, 0, "body.model")
		second := validPolicy("finalize", FinalizeHeadersAndSignature, 0, "body.model")
		if _, err := NewRegistry(first, second); err != nil {
			t.Fatalf("NewRegistry() rejected phase-ordered mutations: %v", err)
		}
	})
}

func TestRegistryRejectsMissingRequiredMetadata(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Policy)
		message string
	}{
		{name: "owner", mutate: func(policy *Policy) { policy.Owner = "" }, message: "owner is required"},
		{name: "cost", mutate: func(policy *Policy) { policy.Cost = CostContract{} }, message: "cost complexity is required"},
		{name: "removal", mutate: func(policy *Policy) { policy.RemovalCondition = "" }, message: "removal condition is required"},
		{name: "fixture", mutate: func(policy *Policy) { policy.Lifecycle.Fixture = "" }, message: "fixture is required"},
		{name: "introduced version", mutate: func(policy *Policy) { policy.Lifecycle.IntroducedVersion = "" }, message: "introduced version is required"},
		{name: "upstream evidence", mutate: func(policy *Policy) { policy.Lifecycle.UpstreamEvidence = "" }, message: "upstream evidence is required"},
		{name: "retry semantics", mutate: func(policy *Policy) { policy.Lifecycle.RetrySemantics = "" }, message: "retry semantics are required"},
		{name: "review condition", mutate: func(policy *Policy) { policy.Lifecycle.ReviewDate = "" }, message: "review date or upstream version condition is required"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := validPolicy("metadata", ProviderQuirkPatch, 0, "body.model")
			test.mutate(&policy)
			assertRegistryErrorContains(t, []Policy{policy}, test.message)
		})
	}
}

func TestRegistryRejectsMissingTransformUnsupportedCostAndInvalidMatch(t *testing.T) {
	t.Run("transform", func(t *testing.T) {
		policy := validPolicy("missing-transform", ProviderQuirkPatch, 0, "body.model")
		policy.Apply = nil
		assertRegistryErrorContains(t, []Policy{policy}, "transform is required")
	})

	t.Run("complexity", func(t *testing.T) {
		policy := validPolicy("bad-complexity", ProviderQuirkPatch, 0, "body.model")
		policy.Cost.Complexity = "quadratic"
		assertRegistryErrorContains(t, []Policy{policy}, "cost complexity")
	})

	t.Run("model pattern", func(t *testing.T) {
		policy := validPolicy("bad-pattern", ProviderQuirkPatch, 0, "body.model")
		policy.Match.ModelPattern = "["
		assertRegistryErrorContains(t, []Policy{policy}, "model pattern is invalid")
	})

	t.Run("empty list item", func(t *testing.T) {
		policy := validPolicy("empty-endpoint", ProviderQuirkPatch, 0, "body.model")
		policy.Match.Endpoints = []EndpointKind{"responses", " "}
		assertRegistryErrorContains(t, []Policy{policy}, "empty endpoint match")
	})
}

func TestRegistryPoliciesForUsesStableNormalizedMatching(t *testing.T) {
	provider := validPolicy("provider", ProviderCapabilityScrub, 10, "body.metadata")
	provider.Match = MatchSpec{
		ProviderFamily: " OpenAI-Compatibility ",
		CompatKind:     " KIMI ",
		ModelPattern:   "kimi-*",
		Endpoints:      []EndpointKind{" responses ", "chat", "chat"},
		Modes:          []ExecutionMode{"stream"},
		SourceFormats:  []Format{"openai"},
		TargetFormats:  []Format{"claude"},
	}
	global := validPolicy("global", AmplificationGuard, -1, "body")

	registry, err := NewRegistry(global, provider)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	matched := registry.PoliciesFor(MatchContext{
		ProviderFamily: "OPENAI-COMPATIBILITY",
		CompatKind:     "kimi",
		Model:          "kimi-k2",
		Endpoint:       "responses",
		Mode:           "stream",
		SourceFormat:   "openai",
		TargetFormat:   "claude",
	})
	if got, want := strings.Join(policyIDs(matched), ","), "provider,global"; got != want {
		t.Fatalf("matched IDs = %q, want %q", got, want)
	}
	if got := matched[0].Match.Endpoints; strings.Join(stringsFrom(got), ",") != "chat,responses" {
		t.Fatalf("normalized endpoints = %v, want chat,responses", got)
	}

	matched[0].Match.Endpoints[0] = "changed"
	if fresh := registry.PoliciesFor(MatchContext{CompatKind: "kimi"}); fresh[0].Match.Endpoints[0] == "changed" {
		t.Fatal("registry query returned aliased policy metadata")
	}
	if got := registry.PoliciesFor(MatchContext{CompatKind: "qwen", Model: "qwen-plus"}); strings.Join(policyIDs(got), ",") != "global" {
		t.Fatalf("disjoint query IDs = %v, want global", policyIDs(got))
	}
	if got := registry.PoliciesFor(MatchContext{}); len(got) != 2 {
		t.Fatalf("inventory query count = %d, want 2", len(got))
	}
}

func TestRegistryConservativelyRejectsOverlappingModelGlobs(t *testing.T) {
	first := validPolicy("prefix", ProviderQuirkPatch, 0, "body.thinking")
	first.Match.ModelPattern = "kimi-*"
	second := validPolicy("suffix", ProviderQuirkPatch, 1, "body.thinking")
	second.Match.ModelPattern = "*-k2"
	assertRegistryErrorContains(t, []Policy{first, second}, "conflict on field")

	exact := validPolicy("exact", ProviderQuirkPatch, 2, "body.thinking")
	exact.Match.ModelPattern = "qwen-plus"
	if _, err := NewRegistry(first, exact); err != nil {
		t.Fatalf("NewRegistry() rejected disjoint exact model: %v", err)
	}
}

func TestReportIsDeterministicOwnedAndPayloadFree(t *testing.T) {
	secretPayload := []byte("secret-prompt-that-must-not-enter-report")
	applyCalled := false
	policy := validPolicy("report-policy", ProviderCapabilityScrub, 0, "body.metadata")
	policy.Match.Endpoints = []EndpointKind{"responses", "chat"}
	policy.Apply = func(context.Context, []byte) ([]byte, error) {
		applyCalled = true
		return secretPayload, nil
	}

	registry, err := NewRegistry(policy)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	report := registry.Report()
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("json.Marshal(report) error = %v", err)
	}
	if applyCalled {
		t.Fatal("Report() executed the policy transform")
	}
	if bytes.Contains(encoded, secretPayload) {
		t.Fatalf("report contains payload: %s", encoded)
	}
	if len(report.Policies) != 1 || report.Policies[0].Lifecycle.Fixture != "testdata/report-policy.json" {
		t.Fatalf("report metadata = %+v", report.Policies)
	}

	report.Policies[0].MutatedFields[0] = "body.changed"
	report.Policies[0].Match.Endpoints[0] = "changed"
	fresh := registry.Report()
	if fresh.Policies[0].MutatedFields[0] != "body.metadata" {
		t.Fatalf("registry mutated through report field: %v", fresh.Policies[0].MutatedFields)
	}
	if fresh.Policies[0].Match.Endpoints[0] == "changed" {
		t.Fatalf("registry match mutated through report: %v", fresh.Policies[0].Match.Endpoints)
	}
}

func TestEmptyRegistryProducesZeroPayloadReport(t *testing.T) {
	registry, err := NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	encoded, err := json.Marshal(registry.Report())
	if err != nil {
		t.Fatalf("json.Marshal(report) error = %v", err)
	}
	if string(encoded) != `{"policies":[]}` {
		t.Fatalf("empty report = %s", encoded)
	}
}

func validPolicy(id string, phase Phase, priority int, field string) Policy {
	return Policy{
		ID:       id,
		Owner:    "compat-team",
		Phase:    phase,
		Priority: priority,
		Cost: CostContract{
			Complexity:        "O(bytes)",
			MaxExpansionBytes: 1024,
			MaxExpansionRatio: 1.25,
		},
		RemovalCondition: "remove after upstream accepts the canonical field",
		Lifecycle: LifecycleMetadata{
			IntroducedVersion: "v7.0.0",
			Fixture:           "testdata/" + id + ".json",
			UpstreamEvidence:  "upstream rejects the canonical request",
			RetrySemantics:    "request-scoped and not retryable",
			ReviewDate:        "2026-10-01",
		},
		MutatedFields: []string{field},
		Apply: func(_ context.Context, payload []byte) ([]byte, error) {
			return payload, nil
		},
	}
}

func policyIDs(policies []Policy) []string {
	ids := make([]string, len(policies))
	for i := range policies {
		ids[i] = policies[i].ID
	}
	return ids
}

func assertRegistryErrorContains(t *testing.T, policies []Policy, substring string) {
	t.Helper()
	_, err := NewRegistry(policies...)
	if err == nil || !strings.Contains(err.Error(), substring) {
		t.Fatalf("NewRegistry() error = %v, want substring %q", err, substring)
	}
}
