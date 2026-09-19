package helps

import (
	"encoding/json"
	"strings"
)

// CodexToolSurfaceReduction is emitted only for a compatibility retry. The
// original request is never changed in place.
type CodexToolSurfaceReduction struct {
	Original int
	Kept     int
	Dropped  int
}

// ReduceCodexToolSurface keeps built-in tools and tools referenced by the
// current input history, then bounds unreferenced function/custom declarations.
func ReduceCodexToolSurface(payload []byte, maxDeclarations int) ([]byte, CodexToolSurfaceReduction) {
	var total CodexToolSurfaceReduction
	if len(payload) == 0 || maxDeclarations <= 0 {
		return payload, total
	}
	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil || root == nil {
		return payload, total
	}
	references := reduceCodexToolReferences(root["input"])
	changed := false
	if tools, ok := root["tools"].([]any); ok {
		updated, stats, didChange := reduceCodexToolArray(tools, references, maxDeclarations)
		if didChange {
			root["tools"] = updated
			changed = true
		}
		total = mergeCodexToolReduction(total, stats)
	}
	if input, ok := root["input"].([]any); ok {
		for index, rawItem := range input {
			item, okItem := rawItem.(map[string]any)
			if !okItem || strings.TrimSpace(stringValue(item["type"])) != "additional_tools" {
				continue
			}
			tools, okTools := item["tools"].([]any)
			if !okTools {
				continue
			}
			updated, stats, didChange := reduceCodexToolArray(tools, references, maxDeclarations)
			if didChange {
				item["tools"] = updated
				input[index] = item
				changed = true
			}
			total = mergeCodexToolReduction(total, stats)
		}
		if changed {
			root["input"] = input
		}
	}
	if !changed {
		return payload, total
	}
	out, err := json.Marshal(root)
	if err != nil {
		return payload, CodexToolSurfaceReduction{}
	}
	return out, total
}

func reduceCodexToolArray(tools []any, references map[string]struct{}, maxDeclarations int) ([]any, CodexToolSurfaceReduction, bool) {
	var stats CodexToolSurfaceReduction
	type candidate struct {
		value       any
		name        string
		declaration bool
		referenced  bool
	}
	candidates := make([]candidate, 0, len(tools))
	for _, rawTool := range tools {
		tool, okTool := rawTool.(map[string]any)
		if !okTool {
			candidates = append(candidates, candidate{value: rawTool})
			continue
		}
		toolType := strings.TrimSpace(stringValue(tool["type"]))
		if toolType == "namespace" {
			if children, okChildren := tool["tools"].([]any); okChildren {
				updated, childStats, changed := reduceCodexToolArray(children, references, maxDeclarations)
				stats = mergeCodexToolReduction(stats, childStats)
				if changed {
					copyTool := make(map[string]any, len(tool))
					for key, value := range tool {
						copyTool[key] = value
					}
					copyTool["tools"] = updated
					candidates = append(candidates, candidate{value: copyTool})
					continue
				}
			}
			candidates = append(candidates, candidate{value: rawTool})
			continue
		}
		declaration := toolType == "" || toolType == "function" || toolType == "custom"
		name := strings.TrimSpace(stringValue(tool["name"]))
		if name == "" {
			if function, okFunction := tool["function"].(map[string]any); okFunction {
				name = strings.TrimSpace(stringValue(function["name"]))
			}
		}
		candidates = append(candidates, candidate{
			value:       rawTool,
			name:        name,
			declaration: declaration,
			referenced:  reduceCodexToolReferenced(name, references),
		})
	}

	declarationCount := 0
	referencedCount := 0
	for _, item := range candidates {
		if item.declaration {
			declarationCount++
			if item.referenced {
				referencedCount++
			}
		}
	}
	if declarationCount <= maxDeclarations {
		return tools, stats, stats.Dropped > 0
	}
	stats.Original = declarationCount
	unreferencedLimit := maxDeclarations - referencedCount
	if unreferencedLimit < 0 {
		unreferencedLimit = 0
	}
	unreferencedKept := 0
	updated := make([]any, 0, len(candidates))
	for _, item := range candidates {
		if !item.declaration {
			updated = append(updated, item.value)
			continue
		}
		if item.referenced || unreferencedKept < unreferencedLimit {
			if !item.referenced {
				unreferencedKept++
			}
			stats.Kept++
			updated = append(updated, item.value)
			continue
		}
		stats.Dropped++
	}
	return updated, stats, stats.Dropped > 0
}

func reduceCodexToolReferences(raw any) map[string]struct{} {
	references := make(map[string]struct{})
	items, ok := raw.([]any)
	if !ok {
		return references
	}
	for _, rawItem := range items {
		item, okItem := rawItem.(map[string]any)
		if !okItem {
			continue
		}
		itemType := strings.TrimSpace(stringValue(item["type"]))
		if itemType == "function_call" || itemType == "custom_tool_call" {
			if name := strings.TrimSpace(stringValue(item["name"])); name != "" {
				references[name] = struct{}{}
			}
		}
	}
	return references
}

func reduceCodexToolReferenced(name string, references map[string]struct{}) bool {
	if name == "" {
		return false
	}
	if _, ok := references[name]; ok {
		return true
	}
	for reference := range references {
		if strings.HasSuffix(reference, "__"+name) {
			return true
		}
	}
	return false
}

func mergeCodexToolReduction(left, right CodexToolSurfaceReduction) CodexToolSurfaceReduction {
	left.Original += right.Original
	left.Kept += right.Kept
	left.Dropped += right.Dropped
	return left
}
