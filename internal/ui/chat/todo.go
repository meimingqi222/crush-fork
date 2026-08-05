package chat

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/ui/styles"
)

// TodoToolMessageItem renders the todo tool in the chat view.
type TodoToolMessageItem struct {
	*baseToolMessageItem
}

var _ ToolMessageItem = (*TodoToolMessageItem)(nil)

// NewTodoToolMessageItem creates a new [TodoToolMessageItem].
func NewTodoToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
) ToolMessageItem {
	return newBaseToolMessageItem(sty, toolCall, result, &TodoToolRenderContext{}, canceled)
}

// TodoToolRenderContext renders todo tool messages.
type TodoToolRenderContext struct{}

// RenderTool implements the [ToolRenderer] interface.
func (r *TodoToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	cappedWidth := toolMessageWidth(width)

	if opts.IsPending() {
		return pendingTool(sty, "Todo", opts.Anim, opts.Compact)
	}

	// Parse the tool params.
	var params tools.TodoParams
	if opts.ToolCall.Input != "" {
		_ = json.Unmarshal([]byte(opts.ToolCall.Input), &params)
	}

	header := toolHeader(sty, opts.Status, "Todo", cappedWidth, opts.Compact, params.Op)
	if opts.Compact {
		return header
	}

	// Check for early state (error, canceled, etc.).
	if early, handled := toolEarlyStateContent(sty, opts, cappedWidth); handled {
		return joinToolParts(header, early)
	}

	// Render result.
	var body string
	if opts.HasResult() {
		// Try to render from metadata.
		if opts.Result.Metadata != "" {
			var meta tools.TodoResponseMetadata
			if err := json.Unmarshal([]byte(opts.Result.Metadata), &meta); err == nil {
				body = renderTodoMeta(meta)
			}
		}
		// Fallback to plain content.
		if body == "" && opts.Result.Content != "" {
			body = toolOutputPlainContent(sty, opts.Result.Content, cappedWidth, false)
		}
	}

	if body != "" {
		return joinToolParts(header, sty.Tool.Body.Render(body))
	}
	return header
}

// renderTodoMeta renders the todo response metadata as a formatted task list.
func renderTodoMeta(meta tools.TodoResponseMetadata) string {
	if len(meta.Tasks) == 0 {
		return ""
	}

	var sb strings.Builder
	for i, t := range meta.Tasks {
		marker := "  "
		switch t.Status {
		case session.TaskStatusCompleted:
			marker = "✓ "
		case session.TaskStatusInProgress:
			marker = "▶ "
		case session.TaskStatusBlocked:
			marker = "⛔"
		case session.TaskStatusDropped:
			marker = "✗ "
		}
		line := fmt.Sprintf("%s%d. %s", marker, i+1, t.Content)
		if t.Evidence != "" {
			line += fmt.Sprintf(" (evidence: %s)", t.Evidence)
		} else if t.DropReason != "" {
			line += fmt.Sprintf(" (dropped: %s)", t.DropReason)
		} else if t.Blocker != "" {
			line += fmt.Sprintf(" (blocker: %s)", t.Blocker)
		}
		sb.WriteString(line + "\n")
	}
	return sb.String()
}
