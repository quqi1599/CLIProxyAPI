// Package gemini provides in-provider request normalization for Gemini API.
// It ensures incoming v1beta requests meet minimal schema requirements
// expected by Google's Generative Language API.
package gemini

import (
	"strings"

	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/signature"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/translator/gemini/common"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertGeminiRequestToGemini normalizes Gemini v1beta requests.
//   - Adds a default role for each content if missing or invalid.
//     The first message defaults to "user", then alternates user/model when needed.
//
// It keeps the payload otherwise unchanged.
func ConvertGeminiRequestToGemini(_ string, inputRawJSON []byte, _ bool) []byte {
	rawJSON := inputRawJSON
	// Fast path: if no contents field, only attach safety settings
	contents := gjson.GetBytes(rawJSON, "contents")
	if !contents.Exists() {
		return common.AttachDefaultSafetySettings(rawJSON, "safetySettings")
	}

	rawJSON = normalizeGeminiRequestTools(rawJSON)
	contents = gjson.GetBytes(rawJSON, "contents")

	// Walk contents and fix roles
	out := rawJSON
	prevRole := ""
	var normalizedContents [][]byte
	rolesChanged := false
	contents.ForEach(func(_ gjson.Result, value gjson.Result) bool {
		role := value.Get("role").String()
		content := []byte(value.Raw)

		// Only user/model are valid for Gemini v1beta requests
		valid := role == "user" || role == "model"
		if role == "" || !valid {
			var newRole string
			if prevRole == "" {
				newRole = "user"
			} else if prevRole == "user" {
				newRole = "model"
			} else {
				newRole = "user"
			}
			content, _ = sjson.SetBytes(content, "role", newRole)
			role = newRole
			rolesChanged = true
		}

		prevRole = role
		normalizedContents = append(normalizedContents, content)
		return true
	})
	if rolesChanged {
		out, _ = sjson.SetRawBytes(out, "contents", internalpayload.BuildRaw(normalizedContents))
	}

	out = signature.SanitizeGeminiRequestThoughtSignatures(out, "contents")

	if gjson.GetBytes(rawJSON, "generationConfig.responseSchema").Exists() {
		strJson, _ := util.RenameKey(string(out), "generationConfig.responseSchema", "generationConfig.responseJsonSchema")
		out = []byte(strJson)
	}

	// Backfill empty functionResponse.name from the preceding functionCall.name.
	// Some clients send function responses with empty names; the Gemini API rejects these.
	out = backfillEmptyFunctionResponseNames(out)

	out = common.AttachDefaultSafetySettings(out, "safetySettings")
	return out
}

func normalizeGeminiRequestTools(data []byte) []byte {
	tools := gjson.GetBytes(data, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return data
	}

	var normalizedTools [][]byte
	toolsChanged := false
	tools.ForEach(func(_, tool gjson.Result) bool {
		toolJSON := []byte(tool.Raw)
		toolChanged := false
		if tool.Get("functionDeclarations").Exists() {
			if renamed, err := util.RenameKey(string(toolJSON), "functionDeclarations", "function_declarations"); err == nil {
				toolJSON = []byte(renamed)
				toolChanged = true
			}
		}

		declarations := gjson.GetBytes(toolJSON, "function_declarations")
		if declarations.Exists() && declarations.IsArray() {
			var normalizedDeclarations [][]byte
			declarationsChanged := false
			declarations.ForEach(func(_, declaration gjson.Result) bool {
				declarationJSON := []byte(declaration.Raw)
				if declaration.Get("parameters").Exists() {
					if renamed, err := util.RenameKey(string(declarationJSON), "parameters", "parametersJsonSchema"); err == nil {
						declarationJSON = []byte(renamed)
						declarationsChanged = true
					}
				}
				normalizedDeclarations = append(normalizedDeclarations, declarationJSON)
				return true
			})
			if declarationsChanged {
				toolJSON, _ = sjson.SetRawBytes(toolJSON, "function_declarations", internalpayload.BuildRaw(normalizedDeclarations))
				toolChanged = true
			}
		}

		normalizedTools = append(normalizedTools, toolJSON)
		toolsChanged = toolsChanged || toolChanged
		return true
	})
	if !toolsChanged {
		return data
	}

	out, _ := sjson.SetRawBytes(data, "tools", internalpayload.BuildRaw(normalizedTools))
	return out
}

// backfillEmptyFunctionResponseNames walks the contents array and for each
// model turn containing functionCall parts, records the call names in order.
// For the immediately following user/function turn containing functionResponse
// parts, any empty name is replaced with the corresponding call name.
func backfillEmptyFunctionResponseNames(data []byte) []byte {
	contents := gjson.GetBytes(data, "contents")
	if !contents.Exists() {
		return data
	}

	var normalizedContents [][]byte
	contentsChanged := false
	var pendingCallNames []string

	contents.ForEach(func(contentIdx, content gjson.Result) bool {
		role := content.Get("role").String()
		contentJSON := []byte(content.Raw)

		// Collect functionCall names from model turns
		if role == "model" {
			var names []string
			content.Get("parts").ForEach(func(_, part gjson.Result) bool {
				if part.Get("functionCall").Exists() {
					names = append(names, part.Get("functionCall.name").String())
				}
				return true
			})
			if len(names) > 0 {
				pendingCallNames = names
			} else {
				pendingCallNames = nil
			}
			normalizedContents = append(normalizedContents, contentJSON)
			return true
		}

		// Backfill empty functionResponse names from pending call names
		if len(pendingCallNames) > 0 {
			ri := 0
			var normalizedParts [][]byte
			partsChanged := false
			content.Get("parts").ForEach(func(_, part gjson.Result) bool {
				partJSON := []byte(part.Raw)
				if part.Get("functionResponse").Exists() {
					name := part.Get("functionResponse.name").String()
					if strings.TrimSpace(name) == "" {
						if ri < len(pendingCallNames) {
							partJSON, _ = sjson.SetBytes(partJSON, "functionResponse.name", pendingCallNames[ri])
							partsChanged = true
						} else {
							log.Debugf("more function responses than calls at contents[%d], skipping name backfill", contentIdx.Int())
						}
					}
					ri++
				}
				normalizedParts = append(normalizedParts, partJSON)
				return true
			})
			if partsChanged {
				contentJSON, _ = sjson.SetRawBytes(contentJSON, "parts", internalpayload.BuildRaw(normalizedParts))
				contentsChanged = true
			}
			pendingCallNames = nil
		}

		normalizedContents = append(normalizedContents, contentJSON)
		return true
	})

	if !contentsChanged {
		return data
	}

	out, _ := sjson.SetRawBytes(data, "contents", internalpayload.BuildRaw(normalizedContents))
	return out
}
