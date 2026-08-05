package tools

import (
	"context"

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
