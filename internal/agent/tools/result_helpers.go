package tools

import (
	"context"
	"strings"

	"github.com/charmbracelet/crush/internal/message"
)

// toolAlreadyCalled checks whether a prior successful tool invocation exists
// in this session. The checker should inspect the ToolResult metadata to
// confirm the call actually succeeded (e.g., via Yield()).
//
// Used to make terminal subagent tools idempotent — calling yield twice is a
// signal of agent confusion and should return an error, not duplicate the
// payload.
func toolAlreadyCalled(ctx context.Context, messages message.Service, sessionID string, checker func(message.ToolResult) bool) bool {
	msgs, err := messages.List(ctx, sessionID)
	if err != nil {
		return false
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != message.Tool {
			continue
		}
		for _, toolResult := range msgs[i].ToolResults() {
			if checker(toolResult) {
				return true
			}
		}
	}
	return false
}

// dedupeTrimmed returns a copy of values with whitespace trimmed and empty /
// duplicate entries removed, preserving the original order of first
// occurrence. Used to normalize list-shaped fields on tool params (artifacts,
// files touched, risks, etc.) before persisting to metadata.
func dedupeTrimmed(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
