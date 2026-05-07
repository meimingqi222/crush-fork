package agent

import (
	"fmt"
	"strings"

	"charm.land/fantasy"

	"github.com/charmbracelet/crush/internal/session"
)

func mergeConsecutiveSameRoleFantasyMessages(msgs []fantasy.Message) []fantasy.Message {
	if len(msgs) < 2 {
		return msgs
	}
	out := make([]fantasy.Message, 0, len(msgs))
	for _, m := range msgs {
		if len(out) == 0 {
			out = append(out, m)
			continue
		}
		prev := &out[len(out)-1]
		if prev.Role != m.Role || prev.Role == fantasy.MessageRoleSystem {
			out = append(out, m)
			continue
		}
		// Same role, non-system: concatenate content with adjacent
		// duplicate text-part suppression.
		for _, part := range m.Content {
			if isDuplicateAdjacentTextPart(prev.Content, part) {
				continue
			}
			prev.Content = append(prev.Content, part)
		}
		// Preserve cache_control / provider hints from the latest
		// message in the merged group. Anthropic's cache_control marker
		// must sit on the tail of the user turn to be effective.
		if m.ProviderOptions != nil {
			prev.ProviderOptions = m.ProviderOptions
		}
	}
	return out
}

// isDuplicateAdjacentTextPart returns true if appending `part` to `existing`
// would produce two adjacent text parts with identical text. This drops the
// typical "继续 继续 继续" repetition without affecting tool_result, file,
// source, or interleaved text+other-content patterns.
func isDuplicateAdjacentTextPart(existing []fantasy.MessagePart, part fantasy.MessagePart) bool {
	if len(existing) == 0 {
		return false
	}
	newText, ok := fantasy.AsMessagePart[fantasy.TextPart](part)
	if !ok {
		return false
	}
	prevText, ok := fantasy.AsMessagePart[fantasy.TextPart](existing[len(existing)-1])
	if !ok {
		return false
	}
	return strings.TrimSpace(newText.Text) == strings.TrimSpace(prevText.Text) && strings.TrimSpace(newText.Text) != ""
}

// stripImagePartsFromFantasyMessages removes all image content from fantasy
// messages for models that do not support image inputs. This prevents
// "invalid content type" errors when conversation history contains images
// recorded during a previous session with a vision-capable model.
//
// It strips FilePart entries from user messages and replaces media tool
// results with a text placeholder. Empty user messages are dropped entirely.
func stripImagePartsFromFantasyMessages(messages []fantasy.Message) []fantasy.Message {
	result := make([]fantasy.Message, 0, len(messages))
	for _, msg := range messages {
		switch msg.Role {
		case fantasy.MessageRoleUser:
			filtered := make([]fantasy.MessagePart, 0, len(msg.Content))
			for _, part := range msg.Content {
				// Check for both value and pointer types of FilePart
				var isFilePart bool
				if _, ok := part.(fantasy.FilePart); ok {
					isFilePart = true
				} else if _, ok := part.(*fantasy.FilePart); ok {
					isFilePart = true
				}
				if !isFilePart {
					filtered = append(filtered, part)
				}
			}
			if len(filtered) == 0 {
				continue
			}
			msg.Content = filtered
			result = append(result, msg)
		case fantasy.MessageRoleTool:
			filtered := make([]fantasy.MessagePart, 0, len(msg.Content))
			for _, part := range msg.Content {
				tr, ok := part.(fantasy.ToolResultPart)
				if !ok {
					filtered = append(filtered, part)
					continue
				}
				if _, isMedia := tr.Output.(fantasy.ToolResultOutputContentMedia); isMedia {
					tr.Output = fantasy.ToolResultOutputContentText{
						Text: "[Image/media content not supported by current model]",
					}
				}
				filtered = append(filtered, tr)
			}
			msg.Content = filtered
			result = append(result, msg)
		default:
			result = append(result, msg)
		}
	}
	return result
}

// stripToolCallPartsFromFantasyMessages removes tool-call parts from
// assistant messages and drops tool-result messages entirely. This is used
// for auxiliary flows (such as prompt enhancement) that send sanitized chat
// history without the corresponding tool execution results: strict
// OpenAI-compatible providers reject assistant messages whose tool_calls
// have no matching tool response, so leaving the parts in place produces
// HTTP 400 errors.
//
// Assistant messages that become empty after stripping are dropped so that
// providers do not see an assistant turn with no content.
func stripToolCallPartsFromFantasyMessages(messages []fantasy.Message) []fantasy.Message {
	result := make([]fantasy.Message, 0, len(messages))
	for _, msg := range messages {
		switch msg.Role {
		case fantasy.MessageRoleAssistant:
			filtered := make([]fantasy.MessagePart, 0, len(msg.Content))
			hasMeaningful := false
			for _, part := range msg.Content {
				if _, ok := part.(fantasy.ToolCallPart); ok {
					continue
				}
				if _, ok := part.(*fantasy.ToolCallPart); ok {
					continue
				}
				filtered = append(filtered, part)
				switch p := part.(type) {
				case fantasy.TextPart:
					if strings.TrimSpace(p.Text) != "" {
						hasMeaningful = true
					}
				case *fantasy.TextPart:
					if p != nil && strings.TrimSpace(p.Text) != "" {
						hasMeaningful = true
					}
				case fantasy.ReasoningPart:
					if strings.TrimSpace(p.Text) != "" {
						hasMeaningful = true
					}
				case *fantasy.ReasoningPart:
					if p != nil && strings.TrimSpace(p.Text) != "" {
						hasMeaningful = true
					}
				}
			}
			if !hasMeaningful {
				continue
			}
			msg.Content = filtered
			result = append(result, msg)
		case fantasy.MessageRoleTool:
			// Drop tool result messages entirely: their matching tool_calls
			// have already been stripped from the preceding assistant turn.
			continue
		default:
			result = append(result, msg)
		}
	}
	return result
}

// buildSummaryPrompt constructs the prompt text for session summarization.
func buildSummaryPrompt(todos []session.Todo) string {
	var sb strings.Builder
	sb.WriteString("Provide a detailed summary of our conversation above.")
	if len(todos) > 0 {
		sb.WriteString("\n\n## Tracked Tasks\n\n")
		for _, todo := range todos {
			fmt.Fprintf(&sb, "- [%s] %s\n", todo.Status, todo.Content)
		}
		sb.WriteString("\nInclude these tasks and their statuses in your summary.")
	}
	return sb.String()
}

// hasAutoRecallInMessages checks if any user message contains system-reminder
// with memory recall content. Memory is merged into existing user messages
// (not prepended), matching Claude Code's attachment-merge approach to preserve
// prompt cache.
func hasAutoRecallInMessages(messages []fantasy.Message) bool {
	for _, msg := range messages {
		if msg.Role != fantasy.MessageRoleUser {
			continue
		}
		for _, part := range msg.Content {
			if textPart, ok := part.(fantasy.TextPart); ok {
				if strings.Contains(textPart.Text, "<system-reminder>") && strings.Contains(textPart.Text, "Relevant long-term memory:") {
					return true
				}
			}
		}
	}
	return false
}
