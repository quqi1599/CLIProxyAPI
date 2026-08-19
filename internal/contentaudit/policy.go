package contentaudit

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
	"gopkg.in/yaml.v3"
)

const maxPolicyKeywords = 100_000

// Policy is the immutable source document compiled into a Matcher.
type Policy struct {
	Version         string   `yaml:"version" json:"version"`
	GlobalAllowlist []string `yaml:"global-allowlist" json:"global-allowlist"`
	Rules           []Rule   `yaml:"rules" json:"rules"`
}

// Rule groups terms that share the same enforcement category and severity.
type Rule struct {
	ID        string   `yaml:"id" json:"id"`
	Category  string   `yaml:"category" json:"category"`
	Severity  string   `yaml:"severity" json:"severity"`
	Keywords  []string `yaml:"keywords" json:"keywords"`
	Allowlist []string `yaml:"allowlist" json:"allowlist"`
	Disabled  bool     `yaml:"disabled,omitempty" json:"disabled,omitempty"`
}

// Decision is the non-sensitive result of a policy scan.
type Decision struct {
	Matched       bool   `json:"matched"`
	RuleID        string `json:"rule_id,omitempty"`
	Category      string `json:"category,omitempty"`
	Severity      string `json:"severity,omitempty"`
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
	policy       Policy
	nodes        []ahoNode
	terms        []compiledTerm
	keywordCount int
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

	m := &Matcher{
		policy: policy,
		nodes:  []ahoNode{{next: make(map[rune]int)}},
	}
	seenRuleIDs := make(map[string]struct{}, len(policy.Rules))
	seenTerms := make(map[string]struct{})
	for ruleIndex := range m.policy.Rules {
		rule := &m.policy.Rules[ruleIndex]
		rule.ID = strings.TrimSpace(rule.ID)
		rule.Category = strings.TrimSpace(rule.Category)
		rule.Severity = normalizeSeverity(rule.Severity)
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

		rule.Allowlist = normalizeTermList(rule.Allowlist)
		for _, rawTerm := range rule.Keywords {
			term := normalizeForMatch(rawTerm)
			if utf8.RuneCountInString(term) < 2 {
				return nil, fmt.Errorf("content audit rule %q has a keyword shorter than two normalized characters", rule.ID)
			}
			key := fmt.Sprintf("%d\x00%s", ruleIndex, term)
			if _, exists := seenTerms[key]; exists {
				continue
			}
			seenTerms[key] = struct{}{}
			m.addTerm(compiledTerm{ruleIndex: ruleIndex, term: term, runeLen: utf8.RuneCountInString(term)})
			m.keywordCount++
			if m.keywordCount > maxPolicyKeywords {
				return nil, fmt.Errorf("content audit policy exceeds %d keywords", maxPolicyKeywords)
			}
		}
	}
	if m.keywordCount == 0 {
		return nil, fmt.Errorf("content audit policy has no enabled keywords")
	}
	m.policy.GlobalAllowlist = normalizeTermList(m.policy.GlobalAllowlist)
	m.buildFailureLinks()
	return m, nil
}

func normalizeTermList(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		normalized := normalizeForMatch(value)
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

func normalizeForMatch(value string) string {
	value = norm.NFKC.String(strings.ToLower(value))
	var out strings.Builder
	out.Grow(len(value))
	for _, r := range value {
		if unicode.Is(unicode.Cf, r) || unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
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

func (m *Matcher) buildFailureLinks() {
	queue := make([]int, 0, len(m.nodes))
	for _, child := range m.nodes[0].next {
		m.nodes[child].fail = 0
		queue = append(queue, child)
	}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for r, child := range m.nodes[current].next {
			queue = append(queue, child)
			fallback := m.nodes[current].fail
			for fallback != 0 {
				if next, exists := m.nodes[fallback].next[r]; exists {
					fallback = next
					break
				}
				fallback = m.nodes[fallback].fail
			}
			if fallback == 0 {
				if next, exists := m.nodes[0].next[r]; exists && next != child {
					fallback = next
				}
			}
			m.nodes[child].fail = fallback
			m.nodes[child].out = append(m.nodes[child].out, m.nodes[fallback].out...)
		}
	}
}

// Match returns the highest-severity non-allowlisted rule match.
func (m *Matcher) Match(text string) Decision {
	if m == nil || len(m.nodes) == 0 {
		return Decision{}
	}
	normalized := []rune(normalizeForMatch(text))
	if len(normalized) == 0 {
		return Decision{PolicyVersion: m.policy.Version}
	}

	nodeIndex := 0
	best := Decision{PolicyVersion: m.policy.Version}
	bestRank := 0
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
			if start < 0 || m.allowlisted(normalized, start, position+1, term.ruleIndex) {
				continue
			}
			rule := m.policy.Rules[term.ruleIndex]
			rank := severityRank(rule.Severity)
			if rank < bestRank || rank == bestRank && term.runeLen <= bestLength {
				continue
			}
			best = Decision{
				Matched:       true,
				RuleID:        rule.ID,
				Category:      rule.Category,
				Severity:      rule.Severity,
				MatchedTerm:   term.term,
				PolicyVersion: m.policy.Version,
			}
			bestRank = rank
			bestLength = term.runeLen
		}
	}
	return best
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
