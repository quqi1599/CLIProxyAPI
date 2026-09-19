package contentaudit

import "strings"

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

func normalizeMode(mode string, enabled, auditOnly bool) string {
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
		if auditOnly {
			return ModeSimple
		}
		return ModeStrict
	}
}

func modeAllowsBlock(mode, ruleID string) bool {
	if mode != ModeSimple {
		return true
	}
	_, ok := simpleModeBlockRules[strings.TrimSpace(ruleID)]
	return ok
}
