package agent

import (
	"context"
	"strings"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/memory/engine"
)

const (
	maxSessionRecallBytes = 60 * 1024
)

type backgroundModel struct {
	model    Model
	provider config.ProviderConfig
}

func buildAutoRecallBlock(ctx context.Context, retriever engine.Retriever, sessionID string) string {
	if retriever == nil || !autoRecallMemoryEnabled(ctx) {
		return ""
	}
	recall, err := retriever.Recall(ctx, map[string]any{"session_id": sessionID})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(recall)
}

func autoRecallMemoryEnabled(ctx context.Context) bool {
	memoryPolicy := strings.ToLower(strings.TrimSpace(tools.GetAgentMemoryFromContext(ctx)))

	switch memoryPolicy {
	case "ephemeral":
		return false
	}

	return true
}

// FormatAutoRecallMessage wraps memory content in a system-reminder tag.
// This approach mirrors Claude Code's design: memories are presented as
// user-message content wrapped in <system-reminder> tags, and merged into
// existing user messages rather than prepended to the message list,
// preserving prompt cache.
func FormatAutoRecallMessage(content string) string {
	if content == "" {
		return ""
	}
	return "<system-reminder>\n" + content + "\n</system-reminder>"
}
