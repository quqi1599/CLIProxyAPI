package contentaudit

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

const (
	ModeStrict = "strict"
	ModeSimple = "simple"
	ModeOff    = "off"
)

// simpleModeBlockRules keeps high-confidence operational safety rules active
// while allowing broad political and contextual rules to remain observable.
var simpleModeBlockRules = map[string]struct{}{
	"block-jailbreak-high-confidence":               {},
	"block-csam-generation-intent":                  {},
	"block-csam-explicit-request-intent":            {},
	"block-weapons-operational-intent":              {},
	"block-drugs-production-intent":                 {},
	"block-criminal-operational-intent":             {},
	"block-fraud-operational-intent":                {},
	"block-fraud-deceptive-request-intent":          {},
	"block-cyber-unauthorized-intent":               {},
	"block-cyber-malicious-attack-intent":           {},
	"block-cyber-abusive-reverse-engineering":       {},
	"block-extremism-attack-intent":                 {},
	"block-extremism-promotion-operation-intent":    {},
	"block-sexual-generation-intent":                {},
	"block-sexual-novel-generation-intent":          {},
	"block-sexual-explicit-media-generation-intent": {},
	"block-sexual-explicit-request-intent":          {},
	"block-piracy-operational-intent":               {},
	"block-gambling-operational-intent":             {},
}

// EffectiveMode resolves legacy configuration without treating observation as
// simple enforcement. AuditOnly remains a separate, non-blocking override.
func EffectiveMode(cfg config.ContentAuditConfig) string {
	return normalizeMode(cfg.Mode, cfg.Enabled)
}

func normalizeMode(mode string, enabled bool) string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	switch mode {
	case ModeStrict, ModeSimple:
		if enabled {
			return mode
		}
		return ModeOff
	case ModeOff:
		return ModeOff
	default:
		if !enabled {
			return ModeOff
		}
		return ModeStrict
	}
}

func modeAllowsBlock(mode, ruleID string) bool {
	if mode == ModeOff {
		return false
	}
	if mode != ModeSimple {
		return true
	}
	_, ok := simpleModeBlockRules[strings.TrimSpace(ruleID)]
	return ok
}

// withMode shares the immutable automaton and original policy. Only candidate
// actions change, before priority selection, so observation cannot hide a block.
func (m *Matcher) withMode(mode string) *Matcher {
	if m == nil {
		return nil
	}
	snapshot := *m
	snapshot.enforcementMode = mode
	return &snapshot
}

func (m *Matcher) ruleAction(rule Rule) string {
	if rule.Action == RuleActionBlock && !modeAllowsBlock(m.enforcementMode, rule.ID) {
		return RuleActionObserve
	}
	return rule.Action
}
