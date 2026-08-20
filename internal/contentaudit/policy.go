package contentaudit

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const maxPolicyKeywords = 100_000

const (
	RuleActionBlock   = "block"
	RuleActionObserve = "observe"
)

// Policy is the immutable source document compiled into a Matcher.
type Policy struct {
	Version         string   `yaml:"version" json:"version"`
	GlobalAllowlist []string `yaml:"global-allowlist" json:"global_allowlist"`
	Rules           []Rule   `yaml:"rules" json:"rules"`
}

// Rule groups terms that share the same enforcement category and severity.
type Rule struct {
	ID         string   `yaml:"id" json:"id"`
	Category   string   `yaml:"category" json:"category"`
	Severity   string   `yaml:"severity" json:"severity"`
	Action     string   `yaml:"action,omitempty" json:"action"`
	Keywords   []string `yaml:"keywords" json:"keywords"`
	RequireAny []string `yaml:"require-any,omitempty" json:"require_any,omitempty"`
	ExcludeAny []string `yaml:"exclude-any,omitempty" json:"exclude_any,omitempty"`
	Allowlist  []string `yaml:"allowlist" json:"allowlist"`
	Disabled   bool     `yaml:"disabled,omitempty" json:"disabled,omitempty"`
}

// Decision is the non-sensitive result of a policy scan.
type Decision struct {
	Matched       bool   `json:"matched"`
	RuleID        string `json:"rule_id,omitempty"`
	Category      string `json:"category,omitempty"`
	Severity      string `json:"severity,omitempty"`
	Action        string `json:"action,omitempty"`
	MatchedTerm   string `json:"-"`
	PolicyVersion string `json:"policy_version,omitempty"`
}

type compiledTerm struct {
	ruleIndex int
	term      string
	runeLen   int
}

type ahoNode struct {
	next map[rune]int
	fail int
	out  []int
}

// Matcher is immutable after compilation and safe for concurrent use.
type Matcher struct {
	policy         Policy
	nodes          []ahoNode
	candidateNodes []ahoNode
	terms          []compiledTerm
	keywordCount   int
	analyzer       *moderationAnalyzer
}

type preparedPolicyTerm struct {
	ruleIndex int
	raw       string
	candidate string
}

// LoadPolicy compiles a YAML or JSON policy file.
func LoadPolicy(path string) (*Matcher, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read content audit policy: %w", err)
	}
	var policy Policy
	if err = yaml.Unmarshal(raw, &policy); err != nil {
		return nil, fmt.Errorf("parse content audit policy: %w", err)
	}
	return CompilePolicy(policy)
}

// CompilePolicy validates and compiles a policy into an Aho-Corasick automaton.
func CompilePolicy(policy Policy) (*Matcher, error) {
	policy.Version = strings.TrimSpace(policy.Version)
	if policy.Version == "" {
		return nil, fmt.Errorf("content audit policy version is required")
	}

	seenRuleIDs := make(map[string]struct{}, len(policy.Rules))
	seenTerms := make(map[string]struct{})
	preparedTerms := make([]preparedPolicyTerm, 0)
	rawKeywords := make([]string, 0)
	for ruleIndex := range policy.Rules {
		rule := &policy.Rules[ruleIndex]
		rule.ID = strings.TrimSpace(rule.ID)
		rule.Category = strings.TrimSpace(rule.Category)
		rule.Severity = normalizeSeverity(rule.Severity)
		rule.Action = normalizeRuleAction(rule.Action)
		if rule.Disabled {
			continue
		}
		if rule.ID == "" || rule.Category == "" {
			return nil, fmt.Errorf("content audit rule %d requires id and category", ruleIndex)
		}
		if _, exists := seenRuleIDs[rule.ID]; exists {
			return nil, fmt.Errorf("duplicate content audit rule id %q", rule.ID)
		}
		seenRuleIDs[rule.ID] = struct{}{}
		if rule.Severity == "" {
			return nil, fmt.Errorf("content audit rule %q has invalid severity", rule.ID)
		}
		if rule.Action == "" {
			return nil, fmt.Errorf("content audit rule %q has invalid action", rule.ID)
		}

		for _, rawTerm := range rule.Keywords {
			candidate := moderationCandidateText(rawTerm)
			if utf8.RuneCountInString(candidate) < 2 {
				return nil, fmt.Errorf("content audit rule %q has a keyword shorter than two normalized characters", rule.ID)
			}
			key := fmt.Sprintf("%d\x00%s", ruleIndex, candidate)
			if _, exists := seenTerms[key]; exists {
				continue
			}
			seenTerms[key] = struct{}{}
			preparedTerms = append(preparedTerms, preparedPolicyTerm{ruleIndex: ruleIndex, raw: rawTerm, candidate: candidate})
			rawKeywords = append(rawKeywords, rawTerm)
			if len(preparedTerms) > maxPolicyKeywords {
				return nil, fmt.Errorf("content audit policy exceeds %d keywords", maxPolicyKeywords)
			}
		}
		rawKeywords = append(rawKeywords, rule.RequireAny...)
		rawKeywords = append(rawKeywords, rule.ExcludeAny...)
	}
	if len(preparedTerms) == 0 {
		return nil, fmt.Errorf("content audit policy has no enabled keywords")
	}
	analyzer, err := buildModerationAnalyzer(rawKeywords)
	if err != nil {
		return nil, fmt.Errorf("initialize content audit analyzer: %w", err)
	}
	m := &Matcher{
		policy:         policy,
		nodes:          []ahoNode{{next: make(map[rune]int)}},
		candidateNodes: []ahoNode{{next: make(map[rune]int)}},
		analyzer:       analyzer,
	}
	for _, prepared := range preparedTerms {
		term := analyzer.normalize(prepared.raw)
		if term == "" {
			continue
		}
		m.addTerm(compiledTerm{ruleIndex: prepared.ruleIndex, term: term, runeLen: utf8.RuneCountInString(term)})
		m.addCandidateTerm(prepared.candidate)
		m.keywordCount++
	}
	if m.keywordCount == 0 {
		return nil, fmt.Errorf("content audit policy has no tokenizable keywords")
	}
	for ruleIndex := range m.policy.Rules {
		m.policy.Rules[ruleIndex].Allowlist = normalizeTermList(m.policy.Rules[ruleIndex].Allowlist, analyzer)
		m.policy.Rules[ruleIndex].RequireAny = normalizeTermList(m.policy.Rules[ruleIndex].RequireAny, analyzer)
		m.policy.Rules[ruleIndex].ExcludeAny = normalizeTermList(m.policy.Rules[ruleIndex].ExcludeAny, analyzer)
	}
	m.policy.GlobalAllowlist = normalizeTermList(m.policy.GlobalAllowlist, analyzer)
	m.buildFailureLinks()
	return m, nil
}

func normalizeTermList(values []string, analyzer *moderationAnalyzer) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		normalized := analyzer.normalize(value)
		if normalized == "" {
			continue
		}
		if _, exists := seen[normalized]; exists {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	sort.Strings(out)
	return out
}

func normalizeSeverity(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "critical":
		return "critical"
	case "high":
		return "high"
	case "medium":
		return "medium"
	case "low":
		return "low"
	default:
		return ""
	}
}

func normalizeRuleAction(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", RuleActionBlock:
		return RuleActionBlock
	case RuleActionObserve:
		return RuleActionObserve
	default:
		return ""
	}
}

func ruleActionRank(value string) int {
	switch value {
	case RuleActionBlock:
		return 2
	case RuleActionObserve:
		return 1
	default:
		return 0
	}
}

func severityRank(value string) int {
	switch value {
	case "critical":
		return 4
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	default:
		return 0
	}
}

func (m *Matcher) addTerm(term compiledTerm) {
	nodeIndex := 0
	for _, r := range term.term {
		nextIndex, exists := m.nodes[nodeIndex].next[r]
		if !exists {
			nextIndex = len(m.nodes)
			m.nodes[nodeIndex].next[r] = nextIndex
			m.nodes = append(m.nodes, ahoNode{next: make(map[rune]int)})
		}
		nodeIndex = nextIndex
	}
	termIndex := len(m.terms)
	m.terms = append(m.terms, term)
	m.nodes[nodeIndex].out = append(m.nodes[nodeIndex].out, termIndex)
}

func (m *Matcher) addCandidateTerm(term string) {
	nodeIndex := 0
	for _, r := range term {
		nextIndex, exists := m.candidateNodes[nodeIndex].next[r]
		if !exists {
			nextIndex = len(m.candidateNodes)
			m.candidateNodes[nodeIndex].next[r] = nextIndex
			m.candidateNodes = append(m.candidateNodes, ahoNode{next: make(map[rune]int)})
		}
		nodeIndex = nextIndex
	}
	m.candidateNodes[nodeIndex].out = append(m.candidateNodes[nodeIndex].out, 0)
}

func (m *Matcher) buildFailureLinks() {
	buildAhoFailureLinks(m.nodes)
	buildAhoFailureLinks(m.candidateNodes)
}

func buildAhoFailureLinks(nodes []ahoNode) {
	queue := make([]int, 0, len(nodes))
	for _, child := range nodes[0].next {
		nodes[child].fail = 0
		queue = append(queue, child)
	}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for r, child := range nodes[current].next {
			queue = append(queue, child)
			fallback := nodes[current].fail
			for fallback != 0 {
				if next, exists := nodes[fallback].next[r]; exists {
					fallback = next
					break
				}
				fallback = nodes[fallback].fail
			}
			if fallback == 0 {
				if next, exists := nodes[0].next[r]; exists && next != child {
					fallback = next
				}
			}
			nodes[child].fail = fallback
			nodes[child].out = append(nodes[child].out, nodes[fallback].out...)
		}
	}
}

// Match returns the highest-severity non-allowlisted rule match.
func (m *Matcher) Match(text string) Decision {
	return m.match(text, "", false)
}

// MatchScoped evaluates block rules against the current user scope while
// retaining full request coverage for observation rules.
func (m *Matcher) MatchScoped(enforcementText, observationText string, continuation bool) Decision {
	if !continuation && enforcementText == observationText {
		return m.Match(observationText)
	}
	if decision := m.match(enforcementText, RuleActionBlock, continuation); decision.Matched {
		return decision
	}
	return m.match(observationText, RuleActionObserve, false)
}

func (m *Matcher) match(text, action string, continuation bool) Decision {
	if m == nil || len(m.nodes) == 0 {
		return Decision{}
	}
	if !m.hasCandidate(moderationCandidateText(text)) {
		return Decision{PolicyVersion: m.policy.Version}
	}
	normalized := []rune(m.analyzer.normalize(text))
	if len(normalized) == 0 {
		return Decision{PolicyVersion: m.policy.Version}
	}

	nodeIndex := 0
	best := Decision{PolicyVersion: m.policy.Version}
	bestActionRank := 0
	bestSeverityRank := 0
	bestLength := 0
	for position, r := range normalized {
		for nodeIndex != 0 {
			if _, exists := m.nodes[nodeIndex].next[r]; exists {
				break
			}
			nodeIndex = m.nodes[nodeIndex].fail
		}
		if next, exists := m.nodes[nodeIndex].next[r]; exists {
			nodeIndex = next
		}
		for _, termIndex := range m.nodes[nodeIndex].out {
			term := m.terms[termIndex]
			start := position - term.runeLen + 1
			if start < 0 {
				continue
			}
			rule := m.policy.Rules[term.ruleIndex]
			if !matchOnRuleBoundaries(normalized, start, position+1, rule) || m.allowlisted(normalized, start, position+1, term.ruleIndex) {
				continue
			}
			if action != "" && rule.Action != action {
				continue
			}
			if !ruleMatchesContext(normalized, start, position+1, rule, continuation) {
				continue
			}
			actionRank := ruleActionRank(rule.Action)
			severity := severityRank(rule.Severity)
			if actionRank < bestActionRank ||
				actionRank == bestActionRank && severity < bestSeverityRank ||
				actionRank == bestActionRank && severity == bestSeverityRank && term.runeLen <= bestLength {
				continue
			}
			best = Decision{
				Matched:       true,
				RuleID:        rule.ID,
				Category:      rule.Category,
				Severity:      rule.Severity,
				Action:        rule.Action,
				MatchedTerm:   term.term,
				PolicyVersion: m.policy.Version,
			}
			bestActionRank = actionRank
			bestSeverityRank = severity
			bestLength = term.runeLen
		}
	}
	return best
}

func ruleMatchesContext(text []rune, matchStart, matchEnd int, rule Rule, continuation bool) bool {
	const contextRunes = 192
	start := max(0, matchStart-contextRunes)
	end := min(len(text), matchEnd+contextRunes)
	normalized := string(text[start:end])
	for _, excluded := range rule.ExcludeAny {
		if strings.Contains(normalized, excluded) {
			return false
		}
	}
	if len(rule.RequireAny) == 0 {
		return true
	}
	for _, required := range rule.RequireAny {
		if strings.Contains(normalized, required) {
			return true
		}
	}
	return continuation
}

func (m *Matcher) hasCandidate(normalized string) bool {
	if normalized == "" || len(m.candidateNodes) == 0 {
		return false
	}
	nodeIndex := 0
	for _, r := range normalized {
		for nodeIndex != 0 {
			if _, exists := m.candidateNodes[nodeIndex].next[r]; exists {
				break
			}
			nodeIndex = m.candidateNodes[nodeIndex].fail
		}
		if next, exists := m.candidateNodes[nodeIndex].next[r]; exists {
			nodeIndex = next
		}
		if len(m.candidateNodes[nodeIndex].out) > 0 {
			return true
		}
	}
	return false
}

func matchOnTermBoundaries(text []rune, start, end int) bool {
	return start >= 0 && end <= len(text) &&
		(start == 0 || text[start-1] == ' ') &&
		(end == len(text) || text[end] == ' ')
}

func matchOnRuleBoundaries(text []rune, start, end int, rule Rule) bool {
	if matchOnTermBoundaries(text, start, end) {
		return true
	}
	if start < 0 || end > len(text) || !containsHanRune(text[start:end]) || len(rule.RequireAny) == 0 {
		return false
	}
	leftBoundary := start == 0 || text[start-1] == ' '
	rightBoundary := end == len(text) || text[end] == ' '
	for _, required := range rule.RequireAny {
		requiredRunes := []rune(required)
		if !leftBoundary && len(requiredRunes) <= start && string(text[start-len(requiredRunes):start]) == required {
			leftBoundary = true
		}
		if !rightBoundary && end+len(requiredRunes) <= len(text) && string(text[end:end+len(requiredRunes)]) == required {
			rightBoundary = true
		}
		if leftBoundary && rightBoundary {
			return true
		}
	}
	return false
}

func containsHanRune(value []rune) bool {
	for _, character := range value {
		if unicode.Is(unicode.Han, character) {
			return true
		}
	}
	return false
}

func (m *Matcher) allowlisted(text []rune, matchStart, matchEnd, ruleIndex int) bool {
	const contextRunes = 64
	start := max(0, matchStart-contextRunes)
	end := min(len(text), matchEnd+contextRunes)
	window := text[start:end]
	for _, allowed := range m.policy.GlobalAllowlist {
		if runePatternCoversMatch(window, []rune(allowed), start, matchStart, matchEnd) {
			return true
		}
	}
	if ruleIndex >= 0 && ruleIndex < len(m.policy.Rules) {
		for _, allowed := range m.policy.Rules[ruleIndex].Allowlist {
			if runePatternCoversMatch(window, []rune(allowed), start, matchStart, matchEnd) {
				return true
			}
		}
	}
	return false
}

func runePatternCoversMatch(window, pattern []rune, windowStart, matchStart, matchEnd int) bool {
	if len(pattern) == 0 || len(pattern) > len(window) {
		return false
	}
	for index := 0; index <= len(window)-len(pattern); index++ {
		matched := true
		for patternIndex := range pattern {
			if window[index+patternIndex] != pattern[patternIndex] {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		patternStart := windowStart + index
		patternEnd := patternStart + len(pattern)
		if patternStart <= matchStart && patternEnd >= matchEnd {
			return true
		}
	}
	return false
}

// Version returns the source policy version.
func (m *Matcher) Version() string {
	if m == nil {
		return ""
	}
	return m.policy.Version
}

// KeywordCount returns the number of unique enabled terms.
func (m *Matcher) KeywordCount() int {
	if m == nil {
		return 0
	}
	return m.keywordCount
}

// Policy returns a detached copy of the active source policy.
func (m *Matcher) Policy() Policy {
	if m == nil {
		return Policy{}
	}
	policy := m.policy
	policy.GlobalAllowlist = append([]string(nil), m.policy.GlobalAllowlist...)
	policy.Rules = make([]Rule, len(m.policy.Rules))
	for index, rule := range m.policy.Rules {
		policy.Rules[index] = rule
		policy.Rules[index].Keywords = append([]string(nil), rule.Keywords...)
		policy.Rules[index].RequireAny = append([]string(nil), rule.RequireAny...)
		policy.Rules[index].ExcludeAny = append([]string(nil), rule.ExcludeAny...)
		policy.Rules[index].Allowlist = append([]string(nil), rule.Allowlist...)
	}
	return policy
}

// RuleActionCounts returns active block, observe, and disabled rule counts.
func (m *Matcher) RuleActionCounts() (block, observe, disabled int) {
	if m == nil {
		return 0, 0, 0
	}
	for _, rule := range m.policy.Rules {
		switch {
		case rule.Disabled:
			disabled++
		case rule.Action == RuleActionBlock:
			block++
		case rule.Action == RuleActionObserve:
			observe++
		}
	}
	return block, observe, disabled
}
