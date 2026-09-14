package contentaudit

import (
	"encoding/json"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// EducationDocumentProfile is selected by server configuration and verified identity only.
const EducationDocumentProfile = "education_document"

func profileForIdentity(cfg config.ContentAuditConfig, identity Identity) string {
	if !identity.Verified || identity.ChannelTest || identity.UserID <= 0 || identity.TokenID <= 0 {
		return ""
	}
	for _, tokenID := range cfg.EducationDocumentTokenIDs {
		if tokenID > 0 && tokenID == identity.TokenID {
			return EducationDocumentProfile
		}
	}
	return ""
}

// withDocumentProfile recognizes an explicit data envelope, not natural-language
// claims such as "this is research". The original payload remains unchanged.
func (r ExtractedRequest) withDocumentProfile(profile string) ExtractedRequest {
	if profile != EducationDocumentProfile || r.CurrentTruncated {
		return r
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal([]byte(r.CurrentUserText), &envelope) != nil || len(envelope) != 1 {
		return r
	}
	raw, ok := envelope["cpa_document_v1"]
	if !ok {
		return r
	}
	var document map[string]json.RawMessage
	if json.Unmarshal(raw, &document) != nil || len(document) != 2 {
		return r
	}
	var task, material string
	if json.Unmarshal(document["task"], &task) != nil || json.Unmarshal(document["material"], &material) != nil || strings.TrimSpace(task) == "" || strings.TrimSpace(material) == "" {
		return r
	}
	r.CurrentUserText, r.EnforcementText = task, task
	r.MaterialText = material
	r.enforcementParts = []promptSegment{{text: task, role: "user"}}
	// Keep exact original source material in the fingerprint and encrypted evidence.
	return r
}

func broadPoliticalRule(ruleID string) bool {
	switch ruleID {
	case "block-political-china-leadership", "block-political-china-leadership-context", "block-political-china-sensitive-topics":
		return true
	default:
		return false
	}
}

// MatchExtractedForProfile changes only topic restrictions for opted-in tokens.
// Material is still scanned and reviewed; it is never an allowlist for the task.
func (m *Matcher) MatchExtractedForProfile(request ExtractedRequest, profile string) Decision {
	decision := m.MatchExtracted(request)
	if profile != EducationDocumentProfile {
		return decision
	}
	if decision.Matched && broadPoliticalRule(decision.RuleID) {
		// Re-evaluate without topic rules so a political mention cannot hide a
		// separate direct harmful request selected at the same severity.
		direct := m.matchWithoutBroadPolitical(request.EnforcementText)
		if direct.Matched && direct.Action == RuleActionBlock {
			return direct
		}
		decision.Action, decision.ModelReview = RuleActionObserve, true
	}
	if decision.Action == RuleActionBlock || request.MaterialText == "" {
		return decision
	}
	material := m.Match(request.MaterialText)
	if material.Matched && (!decision.Matched || severityRank(material.Severity) > severityRank(decision.Severity)) {
		material.Action, material.ModelReview, material.MatchSource = RuleActionObserve, true, "material"
		return material
	}
	return decision
}

func (m *Matcher) matchWithoutBroadPolitical(text string) Decision {
	// Reuse the compiled automaton and analyzer without modifying shared policy.
	copyMatcher := *m
	copyMatcher.policy = m.policy
	copyMatcher.policy.Rules = append([]Rule(nil), m.policy.Rules...)
	for i := range copyMatcher.policy.Rules {
		if broadPoliticalRule(copyMatcher.policy.Rules[i].ID) {
			copyMatcher.policy.Rules[i].Action = RuleActionObserve
		}
	}
	return copyMatcher.Match(text)
}
