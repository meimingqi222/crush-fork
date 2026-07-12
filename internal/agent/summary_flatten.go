package agent

import (
	"fmt"
	"strings"

	"charm.land/fantasy"
)

const (
	// summaryFlattenMaxToolInputChars caps how much of a tool call's raw
	// JSON input is kept when flattening it to text for summarization. The
	// summary only needs enough of the input to identify what the call did.
	summaryFlattenMaxToolInputChars = 500

	// summaryFlattenMaxToolResultChars caps how much of a tool result's
	// output is kept when flattening it to text for summarization. Recent
	// tool results can be very large (file reads, command output); the
	// summarizer only needs a representative excerpt.
	summaryFlattenMaxToolResultChars = 4000
)

// truncateForSummaryFlatten truncates s to at most maxRunes runes,
// appending a truncation marker when content was dropped.
func truncateForSummaryFlatten(s string, maxRunes int) string {
	if len(s) <= maxRunes {
		// Fast path: byte length bounds rune length.
		return s
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "… (truncated)"
}

// renderToolCallForSummary renders a native tool-call part as plain text,
// e.g. `[Called grep with input: {"pattern":"foo"}]`.
func renderToolCallForSummary(tc fantasy.ToolCallPart) string {
	name := strings.TrimSpace(tc.ToolName)
	if name == "" {
		name = "unknown tool"
	}
	input := strings.TrimSpace(tc.Input)
	if input == "" {
		return fmt.Sprintf("[Called %s]", name)
	}
	return fmt.Sprintf("[Called %s with input: %s]", name, truncateForSummaryFlatten(input, summaryFlattenMaxToolInputChars))
}

// renderToolResultForSummary renders a native tool-result part as plain
// text, e.g. `[Tool result for grep: ...]` or `[Tool error for grep: ...]`.
// toolName is resolved by the caller from the matching tool call, since
// fantasy.ToolResultPart itself carries only the tool call ID.
func renderToolResultForSummary(tr fantasy.ToolResultPart, toolName string) string {
	name := strings.TrimSpace(toolName)
	if name == "" {
		name = "unknown tool"
	}
	switch output := tr.Output.(type) {
	case fantasy.ToolResultOutputContentText:
		return fmt.Sprintf("[Tool result for %s: %s]", name, truncateForSummaryFlatten(output.Text, summaryFlattenMaxToolResultChars))
	case fantasy.ToolResultOutputContentError:
		errText := "unknown error"
		if output.Error != nil {
			errText = output.Error.Error()
		}
		return fmt.Sprintf("[Tool error for %s: %s]", name, truncateForSummaryFlatten(errText, summaryFlattenMaxToolResultChars))
	case fantasy.ToolResultOutputContentMedia:
		if text := strings.TrimSpace(output.Text); text != "" {
			return fmt.Sprintf("[Tool result for %s (%s): %s]", name, output.MediaType, truncateForSummaryFlatten(text, summaryFlattenMaxToolResultChars))
		}
		return fmt.Sprintf("[Tool result for %s: %s content]", name, output.MediaType)
	default:
		return fmt.Sprintf("[Tool result for %s]", name)
	}
}

// flattenToolCallsForSummary rewrites a prompt so it contains no native
// tool-call or tool-result message parts, textualizing them instead:
//
//   - Assistant ToolCallPart parts become plain-text lines inside the same
//     assistant message (e.g. `[Called grep with input: {...}]`), preserving
//     any text/reasoning parts around them.
//   - Tool-role messages become user-role messages whose text describes the
//     results (multiple results in one tool message are merged into a single
//     user message), e.g. `[Tool result for grep: ...]`.
//   - All other messages and parts pass through unchanged.
//
// Motivation: the summarization request carries no tool definitions and sets
// ToolChoiceNone, but if the history still contains native tool_calls parts,
// the provider's server-side chat template renders them in the model's
// native tool-call syntax (e.g. DeepSeek DSML). A weak summary model then
// sees a prompt full of native tool-call traces and continues the pattern,
// streaming tool-call markup as its "summary" instead of prose (the DSML
// garbage-summary incident that isInvalidSummaryText guards against).
// Textualizing the history removes that inducement at the source; the
// content validation retry remains as a safety net.
//
// The input slice and its messages are never mutated: changed messages get
// freshly built Content slices (copy-on-write), which also keeps this safe
// with respect to fantasy's shared message state (see
// docs/pitfalls/fantasy-dual-message-state.md).
func flattenToolCallsForSummary(msgs []fantasy.Message) []fantasy.Message {
	// Tool result parts carry only the tool call ID; resolve names from the
	// assistant tool calls seen earlier in the conversation.
	toolNamesByCallID := make(map[string]string)
	out := make([]fantasy.Message, 0, len(msgs))
	for _, msg := range msgs {
		switch msg.Role {
		case fantasy.MessageRoleAssistant:
			hasToolCall := false
			for _, part := range msg.Content {
				if _, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok {
					hasToolCall = true
					break
				}
			}
			if !hasToolCall {
				out = append(out, msg)
				continue
			}
			parts := make([]fantasy.MessagePart, 0, len(msg.Content))
			for _, part := range msg.Content {
				tc, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part)
				if !ok {
					parts = append(parts, part)
					continue
				}
				toolNamesByCallID[tc.ToolCallID] = tc.ToolName
				parts = append(parts, fantasy.TextPart{Text: renderToolCallForSummary(tc)})
			}
			flattened := msg
			flattened.Content = parts
			out = append(out, flattened)
		case fantasy.MessageRoleTool:
			var b strings.Builder
			for _, part := range msg.Content {
				tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
				if !ok {
					// Tool messages built by preparePrompt only contain tool
					// results; anything else has no textual meaning here.
					continue
				}
				if b.Len() > 0 {
					b.WriteString("\n\n")
				}
				b.WriteString(renderToolResultForSummary(tr, toolNamesByCallID[tr.ToolCallID]))
			}
			if b.Len() == 0 {
				continue
			}
			// Providers reject tool-role messages without tool-result parts,
			// so the textualized results move to a user message, matching
			// how tool output flows back into the conversation.
			out = append(out, fantasy.Message{
				Role:    fantasy.MessageRoleUser,
				Content: []fantasy.MessagePart{fantasy.TextPart{Text: b.String()}},
			})
		default:
			out = append(out, msg)
		}
	}
	return out
}
