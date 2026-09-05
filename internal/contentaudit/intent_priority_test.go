package contentaudit

import "testing"

func TestIntentReviewWinsQualifiedSeedTieInEitherOrder(t *testing.T) {
	seed := Rule{ID: "seed", Category: "synthetic", Severity: "high", Action: RuleActionObserve, Keywords: []string{"synthetic material"}, ModelReview: true}
	intent := seed
	intent.ID = "intent"
	intent.RequireAny = []string{"generate"}
	intent.ExcludeAny = []string{"medical", "policy"}
	for _, rules := range [][]Rule{{seed, intent}, {intent, seed}} {
		matcher, err := CompilePolicy(Policy{Version: "test", Rules: rules})
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct{ text, want string }{
			{"generate synthetic material", "intent"},
			{"synthetic material", "seed"},
			{"medical policy regarding generate synthetic material", "seed"},
		} {
			if got := matcher.Match(tc.text); got.RuleID != tc.want {
				t.Fatalf("%q: got %#v, want %s", tc.text, got, tc.want)
			}
		}
	}
	for _, hard := range []Rule{
		{ID: "hard", Category: "synthetic", Severity: "low", Action: RuleActionBlock, Keywords: []string{"synthetic material"}},
		{ID: "critical", Category: "synthetic", Severity: "critical", Action: RuleActionObserve, Keywords: []string{"synthetic material"}},
		{ID: "long", Category: "synthetic", Severity: "high", Action: RuleActionObserve, Keywords: []string{"synthetic material detail"}},
	} {
		matcher, err := CompilePolicy(Policy{Version: "test", Rules: []Rule{hard, intent}})
		if err != nil {
			t.Fatal(err)
		}
		if got := matcher.Match("generate synthetic material detail"); got.RuleID != hard.ID {
			t.Fatalf("intent overrode stronger match: %#v", got)
		}
	}
}

func TestManagedPolicyBroadIntentReachesReviewer(t *testing.T) {
	matcher, err := LoadPolicy("../../content-audit-policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Managed production policy adds an intent-gated copy of the seed rule.
	policy := matcher.policy
	for _, rule := range policy.Rules {
		if rule.ID == "seed-sexual" {
			rule.ID = "block-sexual-broad-generation-intent"
			rule.RequireAny = []string{"generate"}
			rule.ExcludeAny = []string{"medical", "policy", "research"}
			policy.Rules = append(policy.Rules, rule)
			break
		}
	}
	matcher, err = CompilePolicy(policy)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"Please generate hentai.", "Please generate an explicit adult cam show with detailed nudity for sexual arousal."} {
		got := matcher.Match(text)
		if got.RuleID != "block-sexual-broad-generation-intent" || got.Action != RuleActionObserve || !got.ModelReview {
			t.Fatalf("intent does not reach reviewer: %#v", got)
		}
	}
}
