package agent

import (
	"fmt"
	"strings"
)

const (
	subagentMailboxMessagesLimit    = 6
	subagentMailboxPromptCharsLimit = 1_800
	// subagentOutputPreviewCharsLimit is used only for the TUI/metadata
	// preview row (ToolResultReducerChildSession.Preview). The model-facing
	// output uses the full Content (subAgentResponseCharsLimit) so the
	// parent agent does not need to call subtask_result for every fan-out
	// task.
	subagentOutputPreviewCharsLimit   = 5_000
	subagentOutputPerTaskCharsLimit   = 40_000
	subagentOutputAggregateCharsLimit = 160_000
	subagentReducerMessageCharsLimit  = 280
	subagentTodoContentCharsLimit     = 240
	subagentTodoNodeContentCharsLimit = 120
	subagentTodoMailboxCharsLimit     = 80
	subAgentResponseCharsLimit        = 30_000
)

func compactText(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return strings.Join(strings.Fields(value), " ")
}

func ellipsizeText(value string, maxRunes int) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	if maxRunes <= 0 {
		return "", true
	}
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value, false
	}
	if maxRunes == 1 {
		return "…", true
	}
	return string(runes[:maxRunes-1]) + "…", true
}

func promptWithMailboxMessages(basePrompt string, messages []string) string {
	base := strings.TrimSpace(basePrompt)
	if base == "" {
		base = "Continue with the assigned task."
	}
	if len(messages) == 0 {
		return base
	}

	start := max(0, len(messages)-subagentMailboxMessagesLimit)
	selected := make([]string, 0, len(messages)-start+1)
	used := 0
	for _, raw := range messages[start:] {
		msg := compactText(raw)
		if msg == "" {
			continue
		}
		runeLen := len([]rune(msg))
		if used+runeLen > subagentMailboxPromptCharsLimit {
			remaining := subagentMailboxPromptCharsLimit - used
			if remaining <= 0 {
				break
			}
			trimmed, _ := ellipsizeText(msg, remaining)
			if trimmed != "" {
				selected = append(selected, trimmed)
			}
			used = subagentMailboxPromptCharsLimit
			break
		}
		selected = append(selected, msg)
		used += runeLen
	}

	if len(selected) == 0 {
		return base
	}
	if omitted := start; omitted > 0 {
		selected = append(selected, fmt.Sprintf("… %d earlier mailbox message(s) omitted.", omitted))
	}
	actualMessages := len(selected)
	if start > 0 {
		actualMessages--
	}
	if omitted := len(messages) - start - actualMessages; omitted > 0 {
		selected = append(selected, fmt.Sprintf("… %d mailbox message(s) omitted due to context budget.", omitted))
	}
	return base + "\n\nMailbox messages:\n- " + strings.Join(selected, "\n- ")
}

func modelSafeSubAgentText(content, sessionID string) string {
	content = compactText(content)
	if content == "" {
		return ""
	}
	trimmed, truncated := ellipsizeText(content, subAgentResponseCharsLimit)
	if !truncated {
		return trimmed
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return trimmed + " [subagent output truncated; inspect child session for full details]"
	}
	return fmt.Sprintf("%s [subagent output truncated; inspect child session %s for full details]", trimmed, sessionID)
}
