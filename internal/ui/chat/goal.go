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

// GoalToolMessageItem renders the goal tool in the chat view.
type GoalToolMessageItem struct {
	*baseToolMessageItem
}

var _ ToolMessageItem = (*GoalToolMessageItem)(nil)

// NewGoalToolMessageItem creates a new [GoalToolMessageItem].
func NewGoalToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
) ToolMessageItem {
	return newBaseToolMessageItem(sty, toolCall, result, &GoalToolRenderContext{}, canceled)
}

// GoalToolRenderContext renders goal tool messages.
type GoalToolRenderContext struct{}

// RenderTool implements the [ToolRenderer] interface.
func (r *GoalToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	cappedWidth := cappedMessageWidth(width)

	if opts.IsPending() {
		return pendingTool(sty, "Goal", opts.Anim, opts.Compact)
	}

	// Parse the tool params.
	var params tools.GoalParams
	if opts.ToolCall.Input != "" {
		_ = json.Unmarshal([]byte(opts.ToolCall.Input), &params)
	}

	// Build header with operation and objective.
	var headerParams []string
	headerParams = append(headerParams, params.Op)
	if params.Objective != "" {
		obj := params.Objective
		if len(obj) > 50 {
			obj = obj[:49] + "…"
		}
		headerParams = append(headerParams, obj)
	}
	header := toolHeader(sty, opts.Status, "Goal", cappedWidth, opts.Compact, headerParams...)
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
			var meta tools.GoalResponseMetadata
			if err := json.Unmarshal([]byte(opts.Result.Metadata), &meta); err == nil {
				body = renderGoalMeta(meta)
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

// renderGoalMeta renders the goal response metadata as a formatted block.
func renderGoalMeta(meta tools.GoalResponseMetadata) string {
	g := meta.Goal
	if g.Text == "" {
		return ""
	}

	var sb strings.Builder

	// Status line.
	sb.WriteString(fmt.Sprintf("%s %s\n", goalStatusIcon(g.Status), g.Status))
	sb.WriteString(fmt.Sprintf("  %s\n", g.Text))

	// Budget info.
	if g.HasBudget() {
		pct := 0
		if g.TokenBudget > 0 {
			pct = int(g.TokensUsed * 100 / g.TokenBudget)
		}
		bar := renderGoalProgressBar(pct, 20)
		sb.WriteString(fmt.Sprintf("  Budget: %s %d/%d (%d%%)\n", bar, g.TokensUsed, g.TokenBudget, pct))
		sb.WriteString(fmt.Sprintf("  Remaining: %d tokens\n", meta.RemainingTokens))
	}

	// Time info.
	if g.TimeSeconds > 0 {
		sb.WriteString(fmt.Sprintf("  Time: %ds\n", g.TimeSeconds))
	}

	return sb.String()
}

// goalStatusIcon returns a status icon for the goal status.
func goalStatusIcon(status session.GoalStatus) string {
	switch status {
	case session.GoalStatusActive:
		return "▶"
	case session.GoalStatusPaused:
		return "⏸"
	case session.GoalStatusBudgetLimited:
		return "⚠"
	case session.GoalStatusComplete:
		return "✓"
	case session.GoalStatusDropped:
		return "✗"
	default:
		return "•"
	}
}

// renderGoalProgressBar renders a simple text progress bar.
func renderGoalProgressBar(pct, width int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	filled := pct * width / 100
	empty := width - filled
	return "[" + strings.Repeat("█", filled) + strings.Repeat("░", empty) + "]"
}
