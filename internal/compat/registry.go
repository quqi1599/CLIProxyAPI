package compat

import (
	"fmt"
	"math"
	"path"
	"slices"
	"strings"
)

const maxPolicyDowngradeIDs = 32

// Registry is an immutable, explicitly constructed compatibility policy set.
type Registry struct {
	policies []Policy
}

// NewRegistry validates policies and stores them in deterministic execution
// order.
func NewRegistry(policies ...Policy) (*Registry, error) {
	owned := make([]Policy, len(policies))
	seenIDs := make(map[string]struct{}, len(policies))
	for i := range policies {
		owned[i] = clonePolicy(policies[i])
		normalizePolicy(&owned[i])
		if err := validatePolicy(owned[i]); err != nil {
			return nil, err
		}
		if _, exists := seenIDs[owned[i].ID]; exists {
			return nil, fmt.Errorf("compat: duplicate policy ID %q", owned[i].ID)
		}
		seenIDs[owned[i].ID] = struct{}{}
	}

	slices.SortStableFunc(owned, func(left, right Policy) int {
		if rank := phaseRank(left.Phase) - phaseRank(right.Phase); rank != 0 {
			return rank
		}
		if left.Priority < right.Priority {
			return -1
		}
		if left.Priority > right.Priority {
			return 1
		}
		return strings.Compare(left.ID, right.ID)
	})
	if err := validateConflicts(owned); err != nil {
		return nil, err
	}

	return &Registry{policies: owned}, nil
}

// Policies returns an owned copy in deterministic execution order.
func (registry *Registry) Policies() []Policy {
	if registry == nil {
		return nil
	}
	policies := make([]Policy, len(registry.policies))
	for i := range registry.policies {
		policies[i] = clonePolicy(registry.policies[i])
	}
	return policies
}

// PoliciesFor returns owned policies matching query in deterministic execution
// order. Empty query fields intentionally match every value for diagnostics.
func (registry *Registry) PoliciesFor(query MatchContext) []Policy {
	if registry == nil {
		return nil
	}
	normalizeMatchContext(&query)
	matched := make([]Policy, 0, len(registry.policies))
	for i := range registry.policies {
		if policyMatches(registry.policies[i].Match, query) {
			matched = append(matched, clonePolicy(registry.policies[i]))
		}
	}
	return matched
}

func validatePolicy(policy Policy) error {
	if policy.ID == "" {
		return fmt.Errorf("compat: policy ID is required")
	}
	if !isControlledID(policy.ID) {
		return fmt.Errorf("compat: policy ID %q is not a controlled identifier", policy.ID)
	}
	if phaseRank(policy.Phase) < 0 {
		return fmt.Errorf("compat: policy %q has unknown phase %q", policy.ID, policy.Phase)
	}
	if policy.Owner == "" {
		return fmt.Errorf("compat: policy %q owner is required", policy.ID)
	}
	if policy.RemovalCondition == "" {
		return fmt.Errorf("compat: policy %q removal condition is required", policy.ID)
	}
	if policy.Apply == nil {
		return fmt.Errorf("compat: policy %q transform is required", policy.ID)
	}
	if err := validateMatch(policy.ID, policy.Match); err != nil {
		return err
	}
	if err := validateCost(policy.ID, policy.Cost); err != nil {
		return err
	}
	if err := validateLifecycle(policy.ID, policy.Lifecycle); err != nil {
		return err
	}
	seenFields := make(map[string]struct{}, len(policy.MutatedFields))
	for _, field := range policy.MutatedFields {
		if field == "" {
			return fmt.Errorf("compat: policy %q has an empty mutated field", policy.ID)
		}
		if _, exists := seenFields[field]; exists {
			return fmt.Errorf("compat: policy %q repeats mutated field %q", policy.ID, field)
		}
		seenFields[field] = struct{}{}
	}
	for _, downgradeID := range policy.DowngradeIDs {
		if !isControlledID(downgradeID) {
			return fmt.Errorf("compat: policy %q downgrade ID %q is not a controlled identifier", policy.ID, downgradeID)
		}
	}
	if len(policy.DowngradeIDs) > maxPolicyDowngradeIDs {
		return fmt.Errorf("compat: policy %q declares too many downgrade IDs", policy.ID)
	}
	return nil
}

func validateCost(policyID string, cost CostContract) error {
	switch cost.Complexity {
	case "O(1)", "O(n)", "O(bytes)":
	case "":
		return fmt.Errorf("compat: policy %q cost complexity is required", policyID)
	default:
		return fmt.Errorf("compat: policy %q cost complexity %q is unsupported", policyID, cost.Complexity)
	}
	if cost.MaxExpansionBytes < 0 {
		return fmt.Errorf("compat: policy %q max expansion bytes must not be negative", policyID)
	}
	if math.IsNaN(cost.MaxExpansionRatio) || math.IsInf(cost.MaxExpansionRatio, 0) || cost.MaxExpansionRatio < 0 {
		return fmt.Errorf("compat: policy %q max expansion ratio is invalid", policyID)
	}
	if cost.MaxExpansionRatio > 0 && cost.MaxExpansionRatio < 1 {
		return fmt.Errorf("compat: policy %q max expansion ratio must be at least 1", policyID)
	}
	if cost.MaxExpansionBytes == 0 && cost.MaxExpansionRatio == 0 {
		return fmt.Errorf("compat: policy %q cost expansion bound is required", policyID)
	}
	return nil
}

func validateMatch(policyID string, match MatchSpec) error {
	if match.ModelPattern != "" {
		if _, err := path.Match(match.ModelPattern, ""); err != nil {
			return fmt.Errorf("compat: policy %q model pattern is invalid: %w", policyID, err)
		}
	}
	for name, values := range map[string][]string{
		"endpoint":      stringsFrom(match.Endpoints),
		"mode":          stringsFrom(match.Modes),
		"source format": stringsFrom(match.SourceFormats),
		"target format": stringsFrom(match.TargetFormats),
	} {
		for _, value := range values {
			if value == "" {
				return fmt.Errorf("compat: policy %q has an empty %s match", policyID, name)
			}
		}
	}
	return nil
}

func validateLifecycle(policyID string, metadata LifecycleMetadata) error {
	switch {
	case metadata.IntroducedVersion == "":
		return fmt.Errorf("compat: policy %q introduced version is required", policyID)
	case metadata.Fixture == "":
		return fmt.Errorf("compat: policy %q fixture is required", policyID)
	case metadata.UpstreamEvidence == "":
		return fmt.Errorf("compat: policy %q upstream evidence is required", policyID)
	case metadata.RetrySemantics == "":
		return fmt.Errorf("compat: policy %q retry semantics are required", policyID)
	case metadata.ReviewDate == "" && metadata.UpstreamVersionCondition == "":
		return fmt.Errorf("compat: policy %q review date or upstream version condition is required", policyID)
	default:
		return nil
	}
}

func validateConflicts(policies []Policy) error {
	for i := range policies {
		for j := i + 1; j < len(policies); j++ {
			if policies[i].Phase != policies[j].Phase {
				break
			}
			if !matchSpecsOverlap(policies[i].Match, policies[j].Match) {
				continue
			}
			if field, conflict := firstSharedField(policies[i].MutatedFields, policies[j].MutatedFields); conflict {
				return fmt.Errorf("compat: policies %q and %q conflict on field %q in phase %q", policies[i].ID, policies[j].ID, field, policies[i].Phase)
			}
		}
	}
	return nil
}

func matchSpecsOverlap(left, right MatchSpec) bool {
	return scalarMatchesOverlap(left.ProviderFamily, right.ProviderFamily) &&
		scalarMatchesOverlap(left.CompatKind, right.CompatKind) &&
		modelPatternsOverlap(left.ModelPattern, right.ModelPattern) &&
		listMatchesOverlap(left.Endpoints, right.Endpoints) &&
		listMatchesOverlap(left.Modes, right.Modes) &&
		listMatchesOverlap(left.SourceFormats, right.SourceFormats) &&
		listMatchesOverlap(left.TargetFormats, right.TargetFormats)
}

func modelPatternsOverlap(left, right string) bool {
	if left == "" || right == "" || left == "*" || right == "*" {
		return true
	}
	leftPattern := strings.ContainsAny(left, `*?[\\`)
	rightPattern := strings.ContainsAny(right, `*?[\\`)
	switch {
	case !leftPattern && !rightPattern:
		return left == right
	case !leftPattern:
		matched, _ := path.Match(right, left)
		return matched
	case !rightPattern:
		matched, _ := path.Match(left, right)
		return matched
	default:
		// Proving two arbitrary globs disjoint is more complex than this startup
		// guard should be. Treat them as overlapping to avoid false negatives.
		return true
	}
}

func scalarMatchesOverlap(left, right string) bool {
	return left == "" || right == "" || left == "*" || right == "*" || left == right
}

func listMatchesOverlap[T ~string](left, right []T) bool {
	if len(left) == 0 || len(right) == 0 || slices.Contains(left, T("*")) || slices.Contains(right, T("*")) {
		return true
	}
	for _, leftValue := range left {
		if slices.Contains(right, leftValue) {
			return true
		}
	}
	return false
}

func firstSharedField(left, right []string) (string, bool) {
	for _, field := range left {
		if slices.Contains(right, field) {
			return field, true
		}
	}
	return "", false
}

func clonePolicy(policy Policy) Policy {
	policy.Match.Endpoints = slices.Clone(policy.Match.Endpoints)         //nolint:payload-clone reason=small_policy_metadata
	policy.Match.Modes = slices.Clone(policy.Match.Modes)                 //nolint:payload-clone reason=small_policy_metadata
	policy.Match.SourceFormats = slices.Clone(policy.Match.SourceFormats) //nolint:payload-clone reason=small_policy_metadata
	policy.Match.TargetFormats = slices.Clone(policy.Match.TargetFormats) //nolint:payload-clone reason=small_policy_metadata
	policy.MutatedFields = slices.Clone(policy.MutatedFields)             //nolint:payload-clone reason=small_policy_metadata
	policy.DowngradeIDs = slices.Clone(policy.DowngradeIDs)               //nolint:payload-clone reason=small_policy_metadata
	return policy
}

func policyMatches(match MatchSpec, query MatchContext) bool {
	return scalarPolicyMatches(match.ProviderFamily, query.ProviderFamily) &&
		scalarPolicyMatches(match.CompatKind, query.CompatKind) &&
		modelPolicyMatches(match.ModelPattern, query.Model) &&
		listPolicyMatches(match.Endpoints, query.Endpoint) &&
		listPolicyMatches(match.Modes, query.Mode) &&
		listPolicyMatches(match.SourceFormats, query.SourceFormat) &&
		listPolicyMatches(match.TargetFormats, query.TargetFormat)
}

// policyMatchesExecution applies a policy to one concrete request. Unlike the
// diagnostic matcher above, a missing request dimension must not satisfy a
// constrained policy: incomplete execution metadata should fail closed.
func policyMatchesExecution(match MatchSpec, query MatchContext) bool {
	return scalarExecutionMatches(match.ProviderFamily, query.ProviderFamily) &&
		scalarExecutionMatches(match.CompatKind, query.CompatKind) &&
		modelExecutionMatches(match.ModelPattern, query.Model) &&
		listExecutionMatches(match.Endpoints, query.Endpoint) &&
		listExecutionMatches(match.Modes, query.Mode) &&
		listExecutionMatches(match.SourceFormats, query.SourceFormat) &&
		listExecutionMatches(match.TargetFormats, query.TargetFormat)
}

func scalarPolicyMatches(policyValue, queryValue string) bool {
	return queryValue == "" || policyValue == "" || policyValue == "*" || policyValue == queryValue
}

func scalarExecutionMatches(policyValue, queryValue string) bool {
	if queryValue == "" {
		return policyValue == "" || policyValue == "*"
	}
	return policyValue == "" || policyValue == "*" || policyValue == queryValue
}

func modelPolicyMatches(pattern, model string) bool {
	if model == "" || pattern == "" || pattern == "*" {
		return true
	}
	matched, _ := path.Match(pattern, model)
	return matched
}

func modelExecutionMatches(pattern, model string) bool {
	if model == "" {
		return pattern == "" || pattern == "*"
	}
	return modelPolicyMatches(pattern, model)
}

func listPolicyMatches[T ~string](policyValues []T, queryValue T) bool {
	return queryValue == "" || len(policyValues) == 0 || slices.Contains(policyValues, T("*")) || slices.Contains(policyValues, queryValue)
}

func listExecutionMatches[T ~string](policyValues []T, queryValue T) bool {
	if queryValue == "" {
		return len(policyValues) == 0 || slices.Contains(policyValues, T("*"))
	}
	return len(policyValues) == 0 || slices.Contains(policyValues, T("*")) || slices.Contains(policyValues, queryValue)
}

func normalizePolicy(policy *Policy) {
	policy.ID = strings.TrimSpace(policy.ID)
	policy.Owner = strings.TrimSpace(policy.Owner)
	policy.RemovalCondition = strings.TrimSpace(policy.RemovalCondition)
	policy.Match.ProviderFamily = strings.ToLower(strings.TrimSpace(policy.Match.ProviderFamily))
	policy.Match.CompatKind = strings.ToLower(strings.TrimSpace(policy.Match.CompatKind))
	policy.Match.ModelPattern = strings.TrimSpace(policy.Match.ModelPattern)
	policy.Match.Endpoints = normalizeStringList(policy.Match.Endpoints)
	policy.Match.Modes = normalizeStringList(policy.Match.Modes)
	policy.Match.SourceFormats = normalizeStringList(policy.Match.SourceFormats)
	policy.Match.TargetFormats = normalizeStringList(policy.Match.TargetFormats)
	policy.Cost.Complexity = strings.TrimSpace(policy.Cost.Complexity)
	policy.Lifecycle.IntroducedVersion = strings.TrimSpace(policy.Lifecycle.IntroducedVersion)
	policy.Lifecycle.Fixture = strings.TrimSpace(policy.Lifecycle.Fixture)
	policy.Lifecycle.UpstreamEvidence = strings.TrimSpace(policy.Lifecycle.UpstreamEvidence)
	policy.Lifecycle.RetrySemantics = strings.TrimSpace(policy.Lifecycle.RetrySemantics)
	policy.Lifecycle.ReviewDate = strings.TrimSpace(policy.Lifecycle.ReviewDate)
	policy.Lifecycle.UpstreamVersionCondition = strings.TrimSpace(policy.Lifecycle.UpstreamVersionCondition)
	for i := range policy.MutatedFields {
		policy.MutatedFields[i] = strings.TrimSpace(policy.MutatedFields[i])
	}
	slices.Sort(policy.MutatedFields)
	for i := range policy.DowngradeIDs {
		policy.DowngradeIDs[i] = strings.TrimSpace(policy.DowngradeIDs[i])
	}
	slices.Sort(policy.DowngradeIDs)
	policy.DowngradeIDs = slices.Compact(policy.DowngradeIDs)
}

func isControlledID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for i := range len(value) {
		char := value[i]
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' {
			continue
		}
		switch char {
		case '.', '_', '-', ':', '/', '@', '+':
			continue
		default:
			return false
		}
	}
	return true
}

func normalizeStringList[T ~string](values []T) []T {
	for i := range values {
		values[i] = T(strings.ToLower(strings.TrimSpace(string(values[i]))))
	}
	slices.Sort(values)
	return slices.Compact(values)
}

func normalizeMatchContext(query *MatchContext) {
	query.ProviderFamily = strings.ToLower(strings.TrimSpace(query.ProviderFamily))
	query.CompatKind = strings.ToLower(strings.TrimSpace(query.CompatKind))
	query.Model = strings.TrimSpace(query.Model)
	query.Endpoint = EndpointKind(strings.ToLower(strings.TrimSpace(string(query.Endpoint))))
	query.Mode = ExecutionMode(strings.ToLower(strings.TrimSpace(string(query.Mode))))
	query.SourceFormat = Format(strings.ToLower(strings.TrimSpace(string(query.SourceFormat))))
	query.TargetFormat = Format(strings.ToLower(strings.TrimSpace(string(query.TargetFormat))))
}

func stringsFrom[T ~string](values []T) []string {
	result := make([]string, len(values))
	for i := range values {
		result[i] = string(values[i])
	}
	return result
}
