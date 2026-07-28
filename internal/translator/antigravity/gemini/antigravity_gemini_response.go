// Package gemini provides request translation functionality for Gemini to Antigravity API compatibility.
// It handles parsing and transforming Gemini API requests into Antigravity API format,
// extracting model information, system instructions, message contents, and tool declarations.
// The package performs JSON data transformation to ensure compatibility
// between Gemini API format and Antigravity API's expected format.
package gemini

import (
	"bytes"
	"context"

	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	translatorcommon "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/common"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertAntigravityResponseToGemini parses and transforms a Antigravity API request into Gemini API format.
// It extracts the model name, system instruction, message contents, and tool declarations
// from the raw JSON request and returns them in the format expected by the Gemini API.
// The function performs the following transformations:
// 1. Extracts the response data from the request
// 2. Handles alternative response formats
// 3. Processes array responses by extracting individual response objects
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model to use for the request (unused in current implementation)
//   - rawJSON: The raw JSON request data from the Antigravity API
//   - param: A pointer to a parameter object for the conversion (unused in current implementation)
//
// Returns:
//   - [][]byte: The transformed response data in Gemini API format.
func ConvertAntigravityResponseToGemini(ctx context.Context, _ string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, _ *any) [][]byte {
	if bytes.HasPrefix(rawJSON, []byte("data:")) {
		rawJSON = bytes.TrimSpace(rawJSON[5:])
	}

	if alt, ok := ctx.Value("alt").(string); ok {
		var chunk []byte
		if alt == "" {
			responseResult := gjson.GetBytes(rawJSON, "response")
			if responseResult.Exists() {
				chunk = []byte(responseResult.Raw)
				chunk = restoreUsageMetadata(chunk)
				chunk = restoreGeminiFunctionNames(chunk, originalRequestRawJSON)
			}
		} else {
			responseResult := gjson.ParseBytes(chunk)
			responses := make([][]byte, 0, len(responseResult.Array()))
			if responseResult.IsArray() {
				for _, responseResultItem := range responseResult.Array() {
					if responseResultItem.Get("response").Exists() {
						responses = append(responses, []byte(responseResultItem.Get("response").Raw))
					}
				}
			}
			chunk = internalpayload.BuildRaw(responses)
		}
		return [][]byte{chunk}
	}
	return [][]byte{}
}

// ConvertAntigravityResponseToGeminiNonStream converts a non-streaming Antigravity request to a non-streaming Gemini response.
// This function processes the complete Antigravity request and transforms it into a single Gemini-compatible
// JSON response. It extracts the response data from the request and returns it in the expected format.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response (unused in current implementation)
//   - rawJSON: The raw JSON request data from the Antigravity API
//   - param: A pointer to a parameter object for the conversion (unused in current implementation)
//
// Returns:
//   - []byte: A Gemini-compatible JSON response containing the response data.
func ConvertAntigravityResponseToGeminiNonStream(_ context.Context, _ string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, _ *any) []byte {
	responseResult := gjson.GetBytes(rawJSON, "response")
	if responseResult.Exists() {
		chunk := restoreUsageMetadata([]byte(responseResult.Raw))
		return restoreGeminiFunctionNames(chunk, originalRequestRawJSON)
	}
	return restoreGeminiFunctionNames(rawJSON, originalRequestRawJSON)
}

func restoreGeminiFunctionNames(chunk, originalRequestRawJSON []byte) []byte {
	nameMap := util.DisambiguatedToolNameMap(originalRequestRawJSON)
	if len(nameMap) == 0 {
		return chunk
	}
	candidates := gjson.GetBytes(chunk, "candidates")
	if !candidates.IsArray() {
		return chunk
	}
	rewrittenCandidates := make([]string, 0, len(candidates.Array()))
	for _, candidate := range candidates.Array() {
		parts := candidate.Get("content.parts")
		if !parts.IsArray() {
			rewrittenCandidates = append(rewrittenCandidates, candidate.Raw)
			continue
		}
		rewrittenParts := make([]string, 0, len(parts.Array()))
		for _, part := range parts.Array() {
			partJSON := []byte(part.Raw)
			partJSON = restoreGeminiFunctionNameField(partJSON, part, "functionCall", nameMap)
			partJSON = restoreGeminiFunctionNameField(partJSON, part, "functionResponse", nameMap)
			partJSON = restoreGeminiFunctionNameField(partJSON, part, "function_call", nameMap)
			partJSON = restoreGeminiFunctionNameField(partJSON, part, "function_response", nameMap)
			rewrittenParts = append(rewrittenParts, string(partJSON))
		}
		candidateJSON := []byte(candidate.Raw)
		candidateJSON, _ = sjson.SetRawBytes(candidateJSON, "content.parts", internalpayload.BuildRaw(rewrittenParts))
		rewrittenCandidates = append(rewrittenCandidates, string(candidateJSON))
	}
	chunk, _ = sjson.SetRawBytes(chunk, "candidates", internalpayload.BuildRaw(rewrittenCandidates))
	return chunk
}

func restoreGeminiFunctionNameField(partJSON []byte, part gjson.Result, field string, nameMap map[string]string) []byte {
	name := part.Get(field + ".name").String()
	if name == "" {
		return partJSON
	}
	partJSON, _ = sjson.SetBytes(partJSON, field+".name", util.RestoreSanitizedToolName(nameMap, name))
	return partJSON
}

func GeminiTokenCount(ctx context.Context, count int64) []byte {
	return translatorcommon.GeminiTokenCountJSON(count)
}

// restoreUsageMetadata renames cpaUsageMetadata back to usageMetadata.
// The executor renames usageMetadata to cpaUsageMetadata in non-terminal chunks
// to preserve usage data while hiding it from clients that don't expect it.
// When returning standard Gemini API format, we must restore the original name.
func restoreUsageMetadata(chunk []byte) []byte {
	if cpaUsage := gjson.GetBytes(chunk, "cpaUsageMetadata"); cpaUsage.Exists() {
		chunk, _ = sjson.SetRawBytes(chunk, "usageMetadata", []byte(cpaUsage.Raw))
		chunk, _ = sjson.DeleteBytes(chunk, "cpaUsageMetadata")
	}
	return chunk
}
