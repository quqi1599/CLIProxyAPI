package contentaudit

import (
	"fmt"
	"os"
	"regexp"
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
	ID                 string     `yaml:"id" json:"id"`
	Category           string     `yaml:"category" json:"category"`
	Severity           string     `yaml:"severity" json:"severity"`
	Action             string     `yaml:"action,omitempty" json:"action"`
	Keywords           []string   `yaml:"keywords" json:"keywords"`
	MinKeywordMatches  int        `yaml:"min-keyword-matches,omitempty" json:"min_keyword_matches,omitempty"`
	MaxKeywordSpan     int        `yaml:"max-keyword-span,omitempty" json:"max_keyword_span,omitempty"`
	RequireAny         []string   `yaml:"require-any,omitempty" json:"require_any,omitempty"`
	ExcludeAny         []string   `yaml:"exclude-any,omitempty" json:"exclude_any,omitempty"`
	ExcludeAll         [][]string `yaml:"exclude-all,omitempty" json:"exclude_all,omitempty"`
	OverrideExcludeAny []string   `yaml:"override-exclude-any,omitempty" json:"override_exclude_any,omitempty"`
	Allowlist          []string   `yaml:"allowlist" json:"allowlist"`
	ModelReview        bool       `yaml:"model-review,omitempty" json:"model_review,omitempty"`
	Disabled           bool       `yaml:"disabled,omitempty" json:"disabled,omitempty"`
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
	ModelReview   bool   `json:"-"`
	MatchSource   string `json:"match_source,omitempty"`
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
		if rule.MinKeywordMatches < 0 || rule.MinKeywordMatches > len(rule.Keywords) {
			return nil, fmt.Errorf("content audit rule %q has invalid min-keyword-matches", rule.ID)
		}
		if rule.MaxKeywordSpan < 0 || rule.MaxKeywordSpan > 0 && rule.MinKeywordMatches <= 1 {
			return nil, fmt.Errorf("content audit rule %q has invalid max-keyword-span", rule.ID)
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
		term := analyzer.normalizeVariant(prepared.raw)
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
		m.policy.Rules[ruleIndex].OverrideExcludeAny = normalizeTermList(m.policy.Rules[ruleIndex].OverrideExcludeAny, analyzer)
		for groupIndex := range m.policy.Rules[ruleIndex].ExcludeAll {
			m.policy.Rules[ruleIndex].ExcludeAll[groupIndex] = normalizeTermList(m.policy.Rules[ruleIndex].ExcludeAll[groupIndex], analyzer)
		}
	}
	m.policy.GlobalAllowlist = normalizeTermList(m.policy.GlobalAllowlist, analyzer)
	m.buildFailureLinks()
	return m, nil
}

func normalizeTermList(values []string, analyzer *moderationAnalyzer) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		normalized := analyzer.normalizeVariant(value)
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

// MatchExtracted keeps referenced history out of the local blocking decision.
// A continuation with risky reference material still enters semantic review.
func (m *Matcher) MatchExtracted(request ExtractedRequest) Decision {
	decision := m.MatchScoped(request.EnforcementText, request.Text, false)
	if decision.Matched && decision.Action == RuleActionBlock {
		if !request.CurrentTruncated {
			return decision
		}
		// A retained fragment may have lost the task or governing negation.
		// Missing remote history alone does not weaken a complete current request.
		decision.Action = RuleActionObserve
		decision.ModelReview = true
		decision.MatchSource = "truncated"
	}
	if request.Continuation && strings.TrimSpace(request.ReferenceText) != "" {
		reference := m.Match(request.ReferenceText)
		if reference.Matched {
			reference.Action = RuleActionObserve
			reference.ModelReview = true
			reference.MatchSource = "reference"
			if !decision.Matched || !decision.ModelReview || severityRank(reference.Severity) > severityRank(decision.Severity) {
				return reference
			}
		}
	}
	return decision
}

// MatchScoped evaluates both block and observation rules against the current
// user scope. Explicit continuation prompts may include the previous user turn
// in enforcementText, but non-user roles and ordinary history stay out of the
// decision scope.
func (m *Matcher) MatchScoped(enforcementText, _ string, continuation bool) Decision {
	return m.match(enforcementText, "", continuation)
}

func (m *Matcher) match(text, action string, continuation bool) Decision {
	if m == nil || len(m.nodes) == 0 {
		return Decision{}
	}
	if !m.hasCandidate(moderationCandidateText(text)) {
		return Decision{PolicyVersion: m.policy.Version}
	}
	normalizedText := m.analyzer.normalize(text)
	decision := m.matchNormalized(normalizedText, action, continuation)
	if decision.Matched && decision.Action == RuleActionBlock {
		if outside, isReview := quotedReviewOutside(text); isReview {
			outsideDecision := m.matchNormalized(m.analyzer.normalize(outside), action, false)
			if outsideDecision.Matched && outsideDecision.Action == RuleActionBlock {
				return outsideDecision
			}
			decision.Action = RuleActionObserve
			decision.ModelReview = true
			decision.MatchSource = "quoted"
		}
		return decision
	}
	variantText := m.analyzer.normalizeVariant(text)
	if variantText == normalizedText {
		return decision
	}
	variant := m.matchNormalized(variantText, action, false)
	if variant.Matched {
		variant.Action = RuleActionObserve
		variant.ModelReview = true
		variant.MatchSource = "variant"
		if !decision.Matched || severityRank(variant.Severity) > severityRank(decision.Severity) {
			return variant
		}
	}
	return decision
}

func (m *Matcher) matchNormalized(normalizedText, action string, continuation bool) Decision {
	normalized := []rune(normalizedText)
	if len(normalized) == 0 {
		return Decision{PolicyVersion: m.policy.Version}
	}
	qualifiedThresholdRules := m.qualifyingThresholdRules(normalized, action, continuation)

	nodeIndex := 0
	clause := 0
	best := Decision{PolicyVersion: m.policy.Version}
	bestActionRank := 0
	bestSeverityRank := 0
	bestLength := 0
	for position, r := range normalized {
		if r == '\n' {
			clause++
		}
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
			if rule.MinKeywordMatches > 1 && !qualifiedThresholdRules[term.ruleIndex][clause] {
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
				ModelReview:   rule.ModelReview,
				MatchSource:   "current",
			}
			bestActionRank = actionRank
			bestSeverityRank = severity
			bestLength = term.runeLen
		}
	}
	return best
}

type thresholdMatch struct {
	start  int
	term   string
	clause int
}

// qualifyingThresholdRules precomputes only the multi-keyword thresholds used
// by dense-content rules. Ordinary rules keep the existing single-pass path.
func (m *Matcher) qualifyingThresholdRules(text []rune, action string, continuation bool) map[int]map[int]bool {
	hasThresholdRule := false
	for _, rule := range m.policy.Rules {
		if rule.MinKeywordMatches > 1 && (action == "" || rule.Action == action) {
			hasThresholdRule = true
			break
		}
	}
	if !hasThresholdRule {
		return nil
	}

	matches := make(map[int][]thresholdMatch)
	nodeIndex := 0
	clause := 0
	for position, character := range text {
		if character == '\n' {
			clause++
		}
		for nodeIndex != 0 {
			if _, exists := m.nodes[nodeIndex].next[character]; exists {
				break
			}
			nodeIndex = m.nodes[nodeIndex].fail
		}
		if next, exists := m.nodes[nodeIndex].next[character]; exists {
			nodeIndex = next
		}
		for _, termIndex := range m.nodes[nodeIndex].out {
			term := m.terms[termIndex]
			rule := m.policy.Rules[term.ruleIndex]
			if rule.MinKeywordMatches <= 1 || action != "" && rule.Action != action {
				continue
			}
			start := position - term.runeLen + 1
			if start < 0 {
				continue
			}
			if !matchOnRuleBoundaries(text, start, position+1, rule) || m.allowlisted(text, start, position+1, term.ruleIndex) {
				continue
			}
			if !ruleMatchesContext(text, start, position+1, rule, continuation) {
				continue
			}
			matches[term.ruleIndex] = append(matches[term.ruleIndex], thresholdMatch{
				start:  start,
				term:   term.term,
				clause: clause,
			})
		}
	}

	qualified := make(map[int]map[int]bool, len(matches))
	for ruleIndex, ruleMatches := range matches {
		rule := m.policy.Rules[ruleIndex]
		sort.Slice(ruleMatches, func(i, j int) bool {
			return ruleMatches[i].start < ruleMatches[j].start
		})
		termCounts := make(map[string]int, rule.MinKeywordMatches)
		left := 0
		for right, match := range ruleMatches {
			termCounts[match.term]++
			for left <= right && (ruleMatches[left].clause != match.clause || rule.MaxKeywordSpan > 0 && match.start-ruleMatches[left].start > rule.MaxKeywordSpan) {
				leftTerm := ruleMatches[left].term
				termCounts[leftTerm]--
				if termCounts[leftTerm] == 0 {
					delete(termCounts, leftTerm)
				}
				left++
			}
			if len(termCounts) >= rule.MinKeywordMatches {
				if qualified[ruleIndex] == nil {
					qualified[ruleIndex] = make(map[int]bool)
				}
				qualified[ruleIndex][match.clause] = true
			}
		}
	}
	return qualified
}

func ruleMatchesContext(text []rune, matchStart, matchEnd int, rule Rule, _ bool) bool {
	const contextRunes = 192
	start := max(0, matchStart-contextRunes)
	end := min(len(text), matchEnd+contextRunes)
	for index := matchStart - 1; index >= start; index-- {
		if text[index] == '\n' {
			start = index + 1
			break
		}
	}
	for index := matchEnd; index < end; index++ {
		if text[index] == '\n' {
			end = index
			break
		}
	}
	normalized := string(text[start:end])
	if rule.Action == RuleActionBlock && locallyNegatedMatch(text[start:matchStart]) {
		return false
	}
	overrideExclusions := containsAnyTerm(normalized, rule.OverrideExcludeAny)
	if !overrideExclusions {
		for _, excluded := range rule.ExcludeAny {
			if containsContextTerm(normalized, excluded) {
				return false
			}
		}
		if len(rule.ExcludeAll) > 0 {
			matchedAllGroups := true
			for _, group := range rule.ExcludeAll {
				if len(group) == 0 || !containsAnyTerm(normalized, group) {
					matchedAllGroups = false
					break
				}
			}
			if matchedAllGroups {
				return false
			}
		}
	}
	if len(rule.RequireAny) == 0 {
		return true
	}
	for _, required := range rule.RequireAny {
		if containsContextTerm(normalized, required) {
			return true
		}
	}
	return false
}

func containsAnyTerm(text string, terms []string) bool {
	for _, term := range terms {
		if containsContextTerm(text, term) {
			return true
		}
	}
	return false
}

// Tokenization may split a Han intent marker differently when it occurs inside
// a longer seeded term. Ignore tokenizer spaces, never original clause breaks.
func containsContextTerm(text, term string) bool {
	if containsHanRune([]rune(term)) {
		text = strings.ReplaceAll(text, " ", "")
		term = strings.ReplaceAll(term, " ", "")
	}
	return strings.Contains(text, term)
}

// Negation is scoped to the clause immediately governing this occurrence. It
// cannot whitelist another occurrence in a later instruction or another field.
func locallyNegatedMatch(prefix []rune) bool {
	if len(prefix) > 64 {
		prefix = prefix[len(prefix)-64:]
	}
	text := strings.ReplaceAll(string(prefix), " ", "")
	english := " " + string(prefix) + " "
	for _, reset := range []string{"而是", "但是", "然而", "不过", "然后", "接下来", "转而", "\n"} {
		if index := strings.LastIndex(text, reset); index >= 0 {
			text = text[index+len(reset):]
		}
	}
	for _, reset := range []string{" but ", " however ", " instead ", " then ", "\n"} {
		if index := strings.LastIndex(english, reset); index >= 0 {
			english = " " + english[index+len(reset):]
		}
	}
	for _, marker := range []string{"禁止", "不得", "不允许", "请勿", "不要", "不能", "拒绝", "避免", "严禁"} {
		index := strings.LastIndex(text, marker)
		if index < 0 {
			continue
		}
		if marker == "拒绝" && (strings.HasSuffix(text[:index], "不要") || strings.HasSuffix(text[:index], "不得") || strings.HasSuffix(text[:index], "不能") || strings.HasSuffix(text[:index], "禁止")) {
			continue
		}
		if moderationNegatedActionSuffix.MatchString(text[index+len(marker):]) {
			return true
		}
	}
	for _, marker := range []string{" do not ", " must not ", " refuse to ", " cannot "} {
		index := strings.LastIndex(english, marker)
		if index < 0 {
			continue
		}
		if marker == " refuse to " && (strings.HasSuffix(english[:index], "do not") || strings.HasSuffix(english[:index], "must not")) {
			continue
		}
		if moderationNegatedEnglishSuffix.MatchString(strings.TrimSpace(english[index+len(marker):])) {
			return true
		}
	}
	return false
}

var moderationNegatedActionSuffix = regexp.MustCompile(`^(?:直接|再次|继续|帮助|协助|用户|客户|任何|提供|生成|制作|编写|写出|完成|描述|描写|一段|露骨|相关|上述|以下|这类|这种|这些|那种|内容|为我|给我|给出|帮我|我们|请|并)*$`)
var moderationNegatedEnglishSuffix = regexp.MustCompile(`^(?:(?:directly|again|help|assist|users?|any|provide|generate|create|produce|write|complete|describe|this|that|the|to|me|please)\s*)*$`)

var moderationQuotedMaterial = regexp.MustCompile("(?s)```.*?```|`[^`]*`|“[^”]*”|「[^」]*」|\"(?:\\\\.|[^\"\\\\])*\"")

// This recognizes a narrow material-review task, not a research whitelist.
// Quoted risk remains an observation candidate; explicit requests to perform
// the quoted action and independent harmful clauses retain their local block.
func quotedReviewOutside(text string) (string, bool) {
	if !moderationQuotedMaterial.MatchString(text) {
		return "", false
	}
	outside := moderationQuotedMaterial.ReplaceAllString(text, "\n")
	normalized := moderationCandidateText(outside)
	literalOutside := filterModerationText(outside)
	for _, action := range []string{"照做", "执行", "实施", "帮我做", "帮我写", "生成", "编写", "写出", "输出", "做出来", "照着", "给我", "实现", "完成", "implement", "execute", "perform", "follow", "generat", "write me", "do this", "do that", "carry out", "produce", "build", "show me"} {
		offset := 0
		for offset < len(literalOutside) {
			index := strings.Index(literalOutside[offset:], action)
			if index < 0 {
				break
			}
			index += offset
			if !locallyNegatedMatch([]rune(literalOutside[:index])) {
				return outside, false
			}
			offset = index + len(action)
		}
	}
	return outside, containsAny(normalized, []string{"请分析", "请解释", "解释以下", "解释这段", "识别风险", "判断是否", "审核清单", "政策清单", "拒绝策略", "论文引用", "classify", "explain", "analyz", "evaluat", "safetychecklist"})
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
		(start == 0 || isModerationTermBoundary(text[start-1])) &&
		(end == len(text) || isModerationTermBoundary(text[end]))
}

func isModerationTermBoundary(character rune) bool {
	return character == ' ' || character == '\n'
}

func matchOnRuleBoundaries(text []rune, start, end int, rule Rule) bool {
	if matchOnTermBoundaries(text, start, end) {
		return true
	}
	if start < 0 || end > len(text) || !containsHanRune(text[start:end]) || len(rule.RequireAny) == 0 {
		return false
	}
	leftBoundary := start == 0 || isModerationTermBoundary(text[start-1])
	rightBoundary := end == len(text) || isModerationTermBoundary(text[end])
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
