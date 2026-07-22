package signature

import (
	"encoding/json"
	"fmt"
	"strings"

	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type ClaudeMessagesSignatureSanitizeOptions struct {
	TargetProvider                SignatureProvider
	TargetModel                   string
	DropEmptyMessages             bool
	DropToolSignatures            bool
	DropEmptyThinkingPlaceholders bool
}

type SignatureSanitizeReport struct {
	TargetProvider     SignatureProvider
	Preserved          int
	DroppedBlocks      int
	DroppedSignatures  int
	ReplacedSignatures int
	Decisions          []SignatureCompatibilityDecision
}

// SanitizeClaudeMessagesSignaturesForModel removes or preserves Claude
// /v1/messages signed history according to the provider family implied by
// targetModel.
func SanitizeClaudeMessagesSignaturesForModel(payload []byte, targetModel string) ([]byte, SignatureSanitizeReport) {
	return SanitizeClaudeMessagesSignaturesForTarget(payload, ClaudeMessagesSignatureSanitizeOptions{
		TargetProvider:    SignatureProviderFromModelName(targetModel),
		TargetModel:       targetModel,
		DropEmptyMessages: true,
	})
}

// SanitizeClaudeMessagesForClaudeUpstream prepares a Claude /v1/messages body
// for native Claude upstreams. Invalid thinking blocks are dropped, valid
// thinking signatures are normalized to Claude provider-native E-form, and
// tool_use blocks keep only their tool-call payload.
func SanitizeClaudeMessagesForClaudeUpstream(payload []byte, targetModel string) ([]byte, SignatureSanitizeReport) {
	return SanitizeClaudeMessagesSignaturesForTarget(payload, ClaudeMessagesSignatureSanitizeOptions{
		TargetProvider:                SignatureProviderClaude,
		TargetModel:                   targetModel,
		DropEmptyMessages:             true,
		DropToolSignatures:            true,
		DropEmptyThinkingPlaceholders: true,
	})
}

// SanitizeClaudeMessagesSignaturesForTarget applies provider-aware signature
// compatibility rules to Claude /v1/messages history. Compatible thinking
// signatures are preserved. Incompatible thinking blocks are removed so a user
// can continue a conversation after switching between Claude, GPT/Codex,
// and Gemini models.
func SanitizeClaudeMessagesSignaturesForTarget(payload []byte, opts ClaudeMessagesSignatureSanitizeOptions) ([]byte, SignatureSanitizeReport) {
	targetProvider := normalizeSignatureTargetProvider(opts.TargetProvider)
	if targetProvider == SignatureProviderUnknown && opts.TargetModel != "" {
		targetProvider = SignatureProviderFromModelName(opts.TargetModel)
	}
	report := SignatureSanitizeReport{TargetProvider: targetProvider}

	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return payload, report
	}

	messageResults := messages.Array()
	keptMessages := make([]string, 0, len(messageResults))
	modified := false

	for i, message := range messageResults {
		content := message.Get("content")
		if !content.IsArray() {
			keptMessages = append(keptMessages, message.Raw)
			continue
		}

		contentResults := content.Array()
		keptParts := make([]string, 0, len(contentResults))
		messageModified := false

		for j, part := range contentResults {
			partType := part.Get("type").String()
			if partType == "tool_use" {
				if opts.DropToolSignatures {
					updatedPart, changed := stripClaudeToolUseSignatureFields(part)
					if changed {
						messageModified = true
						report.DroppedSignatures++
					}
					keptParts = append(keptParts, updatedPart)
					continue
				}
				updatedPart, changed, decisions := sanitizeClaudeToolUseSignature(part, targetProvider, i, j)
				report.Decisions = append(report.Decisions, decisions...)
				if changed {
					messageModified = true
				}
				for _, decision := range decisions {
					switch decision.Action {
					case SignatureActionPreserve:
						report.Preserved++
					case SignatureActionReplaceWithGeminiBypass:
						report.ReplacedSignatures++
					default:
						report.DroppedSignatures++
					}
				}
				keptParts = append(keptParts, updatedPart)
				continue
			}

			if partType != "thinking" {
				keptParts = append(keptParts, part.Raw)
				continue
			}

			if targetProvider == SignatureProviderClaude && isEmptyClaudeThinkingPlaceholder(part) && !opts.DropEmptyThinkingPlaceholders {
				keptParts = append(keptParts, part.Raw)
				continue
			}

			rawSignature := part.Get("signature").String()
			decision := DecideSignatureCompatibility(targetProvider, rawSignature, SignatureBlockKindClaudeThinking)
			decision.Reason = fmt.Sprintf("messages[%d].content[%d]: %s", i, j, decision.Reason)
			report.Decisions = append(report.Decisions, decision)

			switch decision.Action {
			case SignatureActionPreserve:
				report.Preserved++
				if decision.NormalizedSignature != "" && decision.NormalizedSignature != rawSignature {
					updated, _ := sjson.Set(part.Raw, "signature", decision.NormalizedSignature)
					keptParts = append(keptParts, updated)
					messageModified = true
					continue
				}
				keptParts = append(keptParts, part.Raw)
			case SignatureActionReplaceWithGeminiBypass:
				report.ReplacedSignatures++
				updated, _ := sjson.Set(part.Raw, "signature", decision.ReplacementSignature)
				keptParts = append(keptParts, updated)
				messageModified = true
			case SignatureActionDropSignature:
				report.DroppedSignatures++
				updated, _ := sjson.Delete(part.Raw, "signature")
				keptParts = append(keptParts, updated)
				messageModified = true
			default:
				report.DroppedBlocks++
				messageModified = true
			}
		}

		if messageModified {
			modified = true
			if len(keptParts) == 0 && opts.DropEmptyMessages {
				continue
			}
			updated, _ := sjson.SetRawBytes([]byte(message.Raw), "content", internalpayload.BuildRaw(keptParts))
			keptMessages = append(keptMessages, string(updated))
			continue
		}

		keptMessages = append(keptMessages, message.Raw)
	}

	if !modified {
		return payload, report
	}
	output, _ := sjson.SetRawBytes(payload, "messages", internalpayload.BuildRaw(keptMessages))
	return output, report
}

func stripClaudeToolUseSignatureFields(part gjson.Result) (string, bool) {
	edits := make(map[string]signatureJSONFieldEdit, len(claudeToolUseProvenancePaths()))
	for _, sigPath := range claudeToolUseProvenancePaths() {
		edits[sigPath] = signatureJSONFieldEdit{remove: true}
	}
	return rewriteClaudeToolUsePart(part.Raw, edits)
}

func sanitizeClaudeToolUseSignature(part gjson.Result, targetProvider SignatureProvider, messageIdx, partIdx int) (string, bool, []SignatureCompatibilityDecision) {
	edits := make(map[string]signatureJSONFieldEdit, len(claudeToolUseSignaturePaths()))
	var decisions []SignatureCompatibilityDecision

	for _, sigPath := range claudeToolUseSignaturePaths() {
		sigResult := part.Get(sigPath)
		if !sigResult.Exists() {
			continue
		}

		blockKind := SignatureBlockKindGeminiFunctionCall
		if targetProvider == SignatureProviderClaude {
			blockKind = SignatureBlockKindClaudeThinking
		} else if targetProvider == SignatureProviderGPT {
			blockKind = SignatureBlockKindGPTReasoning
		}
		decision := DecideSignatureCompatibility(targetProvider, sigResult.String(), blockKind)
		decision.Reason = fmt.Sprintf("messages[%d].content[%d].%s: %s", messageIdx, partIdx, sigPath, decision.Reason)
		decisions = append(decisions, decision)

		switch decision.Action {
		case SignatureActionPreserve:
			if decision.NormalizedSignature != "" && decision.NormalizedSignature != sigResult.String() {
				encoded, _ := json.Marshal(decision.NormalizedSignature)
				edits[sigPath] = signatureJSONFieldEdit{replacement: string(encoded)}
			}
		case SignatureActionReplaceWithGeminiBypass:
			encoded, _ := json.Marshal(decision.ReplacementSignature)
			edits[sigPath] = signatureJSONFieldEdit{replacement: string(encoded)}
		default:
			edits[sigPath] = signatureJSONFieldEdit{remove: true}
		}
	}

	updated, changed := rewriteClaudeToolUsePart(part.Raw, edits)
	return updated, changed, decisions
}

type signatureJSONFieldEdit struct {
	replacement string
	remove      bool
}

func rewriteClaudeToolUsePart(raw string, edits map[string]signatureJSONFieldEdit) (string, bool) {
	updated, changed, _ := rewriteSignatureJSONObject(raw, func(key string, value gjson.Result) (signatureJSONFieldEdit, bool) {
		if !strings.Contains(key, ".") {
			if edit, ok := edits[key]; ok {
				return edit, true
			}
		}
		if key != "extra_content" || !value.IsObject() {
			return signatureJSONFieldEdit{}, false
		}

		extraContent, extraChanged, fields := rewriteSignatureJSONObject(value.Raw, func(extraKey string, extraValue gjson.Result) (signatureJSONFieldEdit, bool) {
			if extraKey != "google" || !extraValue.IsObject() {
				return signatureJSONFieldEdit{}, false
			}
			google, googleChanged, googleFields := rewriteSignatureJSONObject(extraValue.Raw, func(googleKey string, _ gjson.Result) (signatureJSONFieldEdit, bool) {
				edit, ok := edits["extra_content.google."+googleKey]
				return edit, ok
			})
			if googleFields == 0 {
				return signatureJSONFieldEdit{remove: true}, true
			}
			if googleChanged {
				return signatureJSONFieldEdit{replacement: google}, true
			}
			return signatureJSONFieldEdit{}, false
		})
		if fields == 0 {
			return signatureJSONFieldEdit{remove: true}, true
		}
		if extraChanged {
			return signatureJSONFieldEdit{replacement: extraContent}, true
		}
		return signatureJSONFieldEdit{}, false
	})
	return updated, changed
}

func rewriteSignatureJSONObject(raw string, edit func(string, gjson.Result) (signatureJSONFieldEdit, bool)) (string, bool, int) {
	object := gjson.Parse(raw)
	if !object.IsObject() {
		return raw, false, 0
	}

	var builder strings.Builder
	builder.Grow(len(raw))
	builder.WriteByte('{')
	changed := false
	fields := 0
	object.ForEach(func(key, value gjson.Result) bool {
		fieldEdit, hasEdit := edit(key.String(), value)
		if hasEdit && fieldEdit.remove {
			changed = true
			return true
		}
		replacement := value.Raw
		if hasEdit && fieldEdit.replacement != value.Raw {
			replacement = fieldEdit.replacement
			changed = true
		}
		if fields > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(key.Raw)
		builder.WriteByte(':')
		builder.WriteString(replacement)
		fields++
		return true
	})
	builder.WriteByte('}')
	if !changed {
		return raw, false, fields
	}
	return builder.String(), true, fields
}

func claudeToolUseSignaturePaths() []string {
	return []string{
		"signature",
		"thoughtSignature",
		"thought_signature",
		"extra_content.google.thought_signature",
	}
}

func claudeToolUseProvenancePaths() []string {
	return append(claudeToolUseSignaturePaths(), "model")
}
