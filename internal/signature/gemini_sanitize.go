package signature

import (
	"encoding/json"
	"strings"

	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// GeminiReplaySignatureOrBypass returns a Gemini-replayable thoughtSignature.
// Compatible Gemini signatures are normalized and preserved. Missing, unknown,
// or cross-provider signatures are replaced with Gemini's bypass sentinel.
func GeminiReplaySignatureOrBypass(rawSignature string, blockKind SignatureBlockKind) string {
	if signature, ok := CompatibleSignatureForProviderBlock(SignatureProviderGemini, rawSignature, blockKind); ok {
		return signature
	}
	decision := DecideSignatureCompatibility(SignatureProviderGemini, rawSignature, blockKind)
	if decision.Action == SignatureActionReplaceWithGeminiBypass && decision.ReplacementSignature != "" {
		return decision.ReplacementSignature
	}
	return GeminiSkipThoughtSignatureValidator
}

// SanitizeGeminiRequestThoughtSignatures applies Gemini replay policy to a
// Gemini-shaped request. Model-turn functionCall, thought, and signed parts keep
// compatible Gemini signatures and use the bypass sentinel otherwise. User-turn
// functionResponse parts must not carry thoughtSignature fields.
func SanitizeGeminiRequestThoughtSignatures(payload []byte, contentsPath string) []byte {
	contentsPath = strings.TrimSpace(contentsPath)
	if contentsPath == "" {
		contentsPath = "contents"
	}

	contents := gjson.GetBytes(payload, contentsPath)
	if !contents.IsArray() {
		return payload
	}

	contentResults := contents.Array()
	updatedContents := make([]string, len(contentResults))
	contentsChanged := false
	for contentIdx, content := range contentResults {
		updatedContents[contentIdx] = content.Raw
		isModelTurn := content.Get("role").String() == "model"
		parts := content.Get("parts")
		if !parts.IsArray() {
			continue
		}

		partResults := parts.Array()
		updatedParts := make([]string, len(partResults))
		partsChanged := false
		for partIdx, part := range partResults {
			updatedParts[partIdx] = part.Raw
			if part.Get("functionResponse").Exists() {
				_, hadSignature := geminiPartThoughtSignature(part)
				if updatedPart, changed := rewriteGeminiPartThoughtSignatures(part, "", false); changed {
					updatedParts[partIdx] = updatedPart
					partsChanged = true
				}
				if hadSignature {
					logGeminiThoughtSignatureSanitize(contentsPath, contentIdx, partIdx, SignatureCompatibilityDecision{
						TargetProvider: SignatureProviderGemini,
						BlockKind:      SignatureBlockKindGeminiModelPart,
						Action:         SignatureActionDropSignature,
						Reason:         "user-turn functionResponse parts cannot replay thought signatures",
					}, "", true)
				}
				continue
			}
			if !isModelTurn {
				continue
			}

			hasFunctionCall := part.Get("functionCall").Exists()
			hasThought := part.Get("thought").Exists()
			rawSignature, hasSignature := geminiPartThoughtSignature(part)
			if !hasFunctionCall && !hasThought && !hasSignature {
				continue
			}

			blockKind := SignatureBlockKindGeminiModelPart
			if hasFunctionCall {
				blockKind = SignatureBlockKindGeminiFunctionCall
			}
			decision := DecideSignatureCompatibility(SignatureProviderGemini, rawSignature, blockKind)
			replaySignature := GeminiReplaySignatureOrBypass(rawSignature, blockKind)
			if updatedPart, changed := rewriteGeminiPartThoughtSignatures(part, replaySignature, true); changed {
				updatedParts[partIdx] = updatedPart
				partsChanged = true
			}
			if decision.Action != SignatureActionPreserve {
				logGeminiThoughtSignatureSanitize(contentsPath, contentIdx, partIdx, decision, rawSignature, hasSignature)
			}
		}

		if !partsChanged {
			continue
		}
		partsJSON := string(internalpayload.BuildRaw(updatedParts))
		updatedContent, changed, _ := rewriteSignatureJSONObject(content.Raw, func(key string, _ gjson.Result) (signatureJSONFieldEdit, bool) {
			if key != "parts" {
				return signatureJSONFieldEdit{}, false
			}
			return signatureJSONFieldEdit{replacement: partsJSON}, true
		})
		if changed {
			updatedContents[contentIdx] = updatedContent
			contentsChanged = true
		}
	}

	if !contentsChanged {
		return payload
	}
	updatedPayload, err := sjson.SetRawBytes(payload, contentsPath, internalpayload.BuildRaw(updatedContents))
	if err != nil {
		return payload
	}
	return updatedPayload
}
func rewriteGeminiPartThoughtSignatures(part gjson.Result, replacement string, addReplacement bool) (string, bool) {
	object := gjson.Parse(part.Raw)
	if !object.IsObject() {
		return part.Raw, false
	}
	encodedReplacement, _ := json.Marshal(replacement)
	canonicalWritten := false
	changed := false
	fields := 0
	var builder strings.Builder
	builder.Grow(len(part.Raw) + len(encodedReplacement) + 24)
	builder.WriteByte('{')
	object.ForEach(func(key, value gjson.Result) bool {
		fieldName := key.String()
		fieldRaw := value.Raw
		keep := true
		switch fieldName {
		case "thoughtSignature":
			if addReplacement {
				canonicalWritten = true
				if fieldRaw != string(encodedReplacement) {
					fieldRaw = string(encodedReplacement)
					changed = true
				}
			} else {
				keep = false
				changed = true
			}
		case "thought_signature":
			keep = false
			changed = true
		case "functionCall", "functionResponse":
			if value.IsObject() {
				updated, nestedChanged, _ := rewriteSignatureJSONObject(value.Raw, dropGeminiThoughtSignatureField)
				if nestedChanged {
					fieldRaw = updated
					changed = true
				}
			}
		case "extra_content":
			if value.IsObject() {
				updated, nestedChanged := rewriteGeminiExtraContent(value.Raw)
				if nestedChanged {
					fieldRaw = updated
					changed = true
				}
			}
		}
		if !keep {
			return true
		}
		if fields > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(key.Raw)
		builder.WriteByte(':')
		builder.WriteString(fieldRaw)
		fields++
		return true
	})
	if addReplacement && !canonicalWritten {
		if fields > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(`"thoughtSignature":`)
		builder.Write(encodedReplacement)
		changed = true
	}
	builder.WriteByte('}')
	if !changed {
		return part.Raw, false
	}
	return builder.String(), true
}

func dropGeminiThoughtSignatureField(key string, _ gjson.Result) (signatureJSONFieldEdit, bool) {
	if key != "thoughtSignature" && key != "thought_signature" {
		return signatureJSONFieldEdit{}, false
	}
	return signatureJSONFieldEdit{remove: true}, true
}

func rewriteGeminiExtraContent(raw string) (string, bool) {
	updated, changed, _ := rewriteSignatureJSONObject(raw, func(key string, value gjson.Result) (signatureJSONFieldEdit, bool) {
		if key != "google" || !value.IsObject() {
			return signatureJSONFieldEdit{}, false
		}
		google, googleChanged, _ := rewriteSignatureJSONObject(value.Raw, func(googleKey string, _ gjson.Result) (signatureJSONFieldEdit, bool) {
			if googleKey != "thought_signature" {
				return signatureJSONFieldEdit{}, false
			}
			return signatureJSONFieldEdit{remove: true}, true
		})
		if !googleChanged {
			return signatureJSONFieldEdit{}, false
		}
		return signatureJSONFieldEdit{replacement: google}, true
	})
	return updated, changed
}

func logGeminiThoughtSignatureSanitize(contentsPath string, contentIndex, partIndex int, decision SignatureCompatibilityDecision, rawSignature string, hasSignature bool) {
	log.WithFields(log.Fields{
		"component":         "signature_sanitizer",
		"target_provider":   string(SignatureProviderGemini),
		"action":            string(decision.Action),
		"reason":            decision.Reason,
		"contents_path":     contentsPath,
		"content_index":     contentIndex,
		"part_index":        partIndex,
		"block_kind":        string(decision.BlockKind),
		"detected_provider": string(decision.DetectedProvider),
		"has_signature":     hasSignature,
		"signature_length":  len(strings.TrimSpace(rawSignature)),
	}).Debug("gemini request: sanitized thoughtSignature before upstream")
}

func geminiPartThoughtSignature(part gjson.Result) (string, bool) {
	for _, path := range []string{
		"thoughtSignature",
		"thought_signature",
		"functionCall.thoughtSignature",
		"functionCall.thought_signature",
		"functionResponse.thoughtSignature",
		"functionResponse.thought_signature",
		"extra_content.google.thought_signature",
	} {
		result := part.Get(path)
		if result.Exists() {
			return result.String(), true
		}
	}
	return "", false
}
