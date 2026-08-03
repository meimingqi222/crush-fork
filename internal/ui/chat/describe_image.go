package chat

import (
	"encoding/json"
	"fmt"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/fsext"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/styles"
)

var describeImageSpinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// DescribeImageToolMessageItem renders the describe_image tool in the chat view.
type DescribeImageToolMessageItem struct {
	*baseToolMessageItem
}

var _ ToolMessageItem = (*DescribeImageToolMessageItem)(nil)

// NewDescribeImageToolMessageItem creates a new [DescribeImageToolMessageItem].
func NewDescribeImageToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
) ToolMessageItem {
	base := newBaseToolMessageItem(sty, toolCall, result, &DescribeImageToolRenderContext{}, canceled)
	base.displayName = "Describe Image"
	base.spinningFunc = describeImageSpinning
	return &DescribeImageToolMessageItem{baseToolMessageItem: base}
}

func describeImageSpinning(state SpinningState) bool {
	if state.Status == ToolStatusCanceled {
		return false
	}
	return state.Result == nil
}

// TickAnimation advances the braille spinner while the vision helper runs.
func (d *DescribeImageToolMessageItem) TickAnimation() {
	d.baseToolMessageItem.TickAnimation()
	if !d.isSpinning() {
		return
	}
	d.clearCache()
	d.invalidateBodyCache()
}

// DescribeImageToolRenderContext renders describe_image tool messages.
type DescribeImageToolRenderContext struct{}

// RenderTool implements the [ToolRenderer] interface.
func (d *DescribeImageToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	cappedWidth := toolMessageWidth(width)
	if opts.IsPending() {
		return pendingDescribeImageTool(sty, opts)
	}

	var params tools.DescribeImageParams
	if err := json.Unmarshal([]byte(opts.ToolCall.Input), &params); err != nil {
		return ""
	}

	header := toolHeader(sty, opts.Status, "Describe Image", cappedWidth, opts.Compact, describeImageHeaderParams(params)...)
	if opts.Compact {
		return header
	}

	if earlyState, ok := toolEarlyStateContent(sty, opts, cappedWidth); ok {
		return joinToolParts(header, earlyState)
	}

	if !opts.HasResult() || opts.Result.Content == "" {
		return header
	}

	body := renderToolResultTextContent(sty, opts.Result.Content, toolResultContentWidths{Body: cappedWidth - toolBodyLeftPaddingTotal, Diff: cappedWidth}, opts.ExpandedContent)
	return joinToolParts(header, body)
}

func pendingDescribeImageTool(sty *styles.Styles, opts *ToolRenderOpts) string {
	frame := 0
	if opts.Anim != nil {
		frame = opts.Anim.StepIndex()
	}

	var params tools.DescribeImageParams
	if opts.ToolCall.Input != "" {
		_ = json.Unmarshal([]byte(opts.ToolCall.Input), &params)
	}

	prefix := spinnerPrefix(sty, "Describe Image", opts.Compact)
	if headerParams := describeImageHeaderParams(params); len(headerParams) > 0 {
		prefix += sty.Tool.ParamMain.Render(headerParams[0]) + " "
	}

	braille := describeImageSpinnerFrames[frame%len(describeImageSpinnerFrames)]
	detail := sty.Base.Faint(true).Render(braille + " analyzing image...")
	return joinToolParts(prefix, detail)
}

func describeImageHeaderParams(params tools.DescribeImageParams) []string {
	switch {
	case params.Path != "":
		return []string{fsext.PrettyPath(params.Path)}
	case params.MessageID != "" && params.ImageIndex > 0:
		return []string{fmt.Sprintf("attachment #%d", params.ImageIndex)}
	case params.MessageID != "":
		return []string{"attachment"}
	default:
		return nil
	}
}
