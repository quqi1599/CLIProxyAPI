package helps

import (
	"fmt"
	"strings"

	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func DedupeClaudeToolResultParts(body []byte) ([]byte, int, error) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body, 0, nil
	}

	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return body, 0, nil
	}

	messageResults := messages.Array()
	outMessages := make([][]byte, 0, len(messageResults))
	changed := false
	removed := 0

	for _, msg := range messageResults {
		if strings.TrimSpace(msg.Get("role").String()) != "user" || !msg.Get("content").IsArray() {
			outMessages = append(outMessages, []byte(msg.Raw))
			continue
		}

		parts := msg.Get("content").Array()
		keep := make([]bool, len(parts))
		for idx := range keep {
			keep[idx] = true
		}
		messageChanged := false
		lastIndexByID := make(map[string]int)

		for idx, part := range parts {
			if part.Get("type").String() != "tool_result" {
				continue
			}
			toolUseID := strings.TrimSpace(part.Get("tool_use_id").String())
			if toolUseID == "" {
				continue
			}
			if previousIdx, ok := lastIndexByID[toolUseID]; ok {
				keep[previousIdx] = false
				changed = true
				messageChanged = true
				removed++
			}
			lastIndexByID[toolUseID] = idx
		}

		if !messageChanged {
			outMessages = append(outMessages, []byte(msg.Raw))
			continue
		}

		contentOut := make([][]byte, 0, len(parts))
		for idx, part := range parts {
			if !keep[idx] {
				continue
			}
			contentOut = append(contentOut, []byte(part.Raw))
		}
		if len(contentOut) == 0 {
			continue
		}
		msgOut, err := sjson.SetRawBytes([]byte(msg.Raw), "content", internalpayload.BuildRaw(contentOut))
		if err != nil {
			return body, 0, fmt.Errorf("failed to dedupe Claude tool_result parts: %w", err)
		}
		outMessages = append(outMessages, msgOut)
	}

	if !changed {
		return body, 0, nil
	}

	out, err := sjson.SetRawBytes(body, "messages", internalpayload.BuildRaw(outMessages))
	if err != nil {
		return body, 0, fmt.Errorf("failed to update Claude tool_result dedupe: %w", err)
	}
	return out, removed, nil
}

func MoveClaudeToolResultsBeforeUserContent(body []byte) ([]byte, int, error) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body, 0, nil
	}

	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return body, 0, nil
	}

	messageResults := messages.Array()
	outMessages := make([][]byte, 0, len(messageResults))
	pendingOrder := []string{}
	pendingSet := map[string]bool{}
	changed := false
	reordered := 0

	for _, msg := range messageResults {
		role := strings.TrimSpace(msg.Get("role").String())
		switch role {
		case "assistant":
			pendingOrder = claudeToolUseIDOrderInMessage(msg)
			pendingSet = make(map[string]bool, len(pendingOrder))
			for _, id := range pendingOrder {
				pendingSet[id] = true
			}
			outMessages = append(outMessages, []byte(msg.Raw))
		case "user":
			if len(pendingOrder) == 0 || !msg.Get("content").IsArray() {
				pendingOrder = nil
				pendingSet = map[string]bool{}
				outMessages = append(outMessages, []byte(msg.Raw))
				continue
			}

			orderedParts, moved := orderClaudePendingToolResultsFirst(msg.Get("content").Array(), pendingOrder, pendingSet)
			if !moved {
				pendingOrder = nil
				pendingSet = map[string]bool{}
				outMessages = append(outMessages, []byte(msg.Raw))
				continue
			}
			msgOut, err := setClaudeMessageContent(msg, orderedParts)
			if err != nil {
				return body, 0, err
			}
			outMessages = append(outMessages, msgOut)
			pendingOrder = nil
			pendingSet = map[string]bool{}
			changed = true
			reordered++
		default:
			pendingOrder = nil
			pendingSet = map[string]bool{}
			outMessages = append(outMessages, []byte(msg.Raw))
		}
	}

	if !changed {
		return body, 0, nil
	}

	out, err := sjson.SetRawBytes(body, "messages", internalpayload.BuildRaw(outMessages))
	if err != nil {
		return body, 0, fmt.Errorf("failed to reorder Claude tool_result parts: %w", err)
	}
	return out, reordered, nil
}

func orderClaudePendingToolResultsFirst(parts []gjson.Result, pendingOrder []string, pendingSet map[string]bool) ([]gjson.Result, bool) {
	resultByID := make(map[string]gjson.Result)
	otherParts := make([]gjson.Result, 0, len(parts))
	matched := 0

	for _, part := range parts {
		if part.Get("type").String() == "tool_result" {
			toolUseID := strings.TrimSpace(part.Get("tool_use_id").String())
			if toolUseID != "" && pendingSet[toolUseID] {
				if _, exists := resultByID[toolUseID]; !exists {
					matched++
				}
				resultByID[toolUseID] = part
				continue
			}
		}
		otherParts = append(otherParts, part)
	}
	if matched == 0 {
		return parts, false
	}

	ordered := make([]gjson.Result, 0, len(parts))
	for _, toolUseID := range pendingOrder {
		if part, ok := resultByID[toolUseID]; ok {
			ordered = append(ordered, part)
		}
	}
	ordered = append(ordered, otherParts...)
	if claudePartSequenceEqual(parts, ordered) {
		return parts, false
	}
	return ordered, true
}

func claudeToolUseIDOrderInMessage(msg gjson.Result) []string {
	ids := make([]string, 0)
	seen := make(map[string]bool)
	content := msg.Get("content")
	if !content.IsArray() {
		return ids
	}
	for _, part := range content.Array() {
		if part.Get("type").String() != "tool_use" {
			continue
		}
		toolUseID := strings.TrimSpace(part.Get("id").String())
		if toolUseID != "" && !seen[toolUseID] {
			ids = append(ids, toolUseID)
			seen[toolUseID] = true
		}
	}
	return ids
}

func setClaudeMessageContent(msg gjson.Result, parts []gjson.Result) ([]byte, error) {
	content := make([][]byte, 0, len(parts))
	for _, part := range parts {
		content = append(content, []byte(part.Raw))
	}
	out, err := sjson.SetRawBytes([]byte(msg.Raw), "content", internalpayload.BuildRaw(content))
	if err != nil {
		return nil, fmt.Errorf("failed to update Claude message content: %w", err)
	}
	return out, nil
}

func claudePartSequenceEqual(left []gjson.Result, right []gjson.Result) bool {
	if len(left) != len(right) {
		return false
	}
	for idx := range left {
		if left[idx].Raw != right[idx].Raw {
			return false
		}
	}
	return true
}
