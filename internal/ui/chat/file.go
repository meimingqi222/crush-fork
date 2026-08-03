package chat

import (
	"encoding/json"
	"strings"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/fsext"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/styles"
)

// -----------------------------------------------------------------------------
// Read Tool
// -----------------------------------------------------------------------------

// ReadToolMessageItem is a message item that represents a read tool call.
type ReadToolMessageItem struct {
	*baseToolMessageItem
}

var _ ToolMessageItem = (*ReadToolMessageItem)(nil)

// NewReadToolMessageItem creates a new [ReadToolMessageItem].
func NewReadToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
) ToolMessageItem {
	return newBaseToolMessageItem(sty, toolCall, result, &ReadToolRenderContext{}, canceled)
}

// ReadToolRenderContext renders read tool messages.
type ReadToolRenderContext struct{}

// RenderTool implements the [ToolRenderer] interface.
func (v *ReadToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	cappedWidth := toolMessageWidth(width)
	if opts.IsPending() {
		return pendingTool(sty, "Read", opts.Anim, opts.Compact)
	}

	var params tools.ReadParams
	if err := json.Unmarshal([]byte(opts.ToolCall.Input), &params); err != nil {
		return ""
	}

	file := fsext.PrettyPath(params.Path)
	toolParams := []string{file}

	header := toolHeader(sty, opts.Status, "Read", cappedWidth, opts.Compact, toolParams...)
	if opts.Compact {
		return header
	}

	if earlyState, ok := toolEarlyStateContent(sty, opts, cappedWidth); ok {
		return joinToolParts(header, earlyState)
	}

	if !opts.HasResult() {
		return header
	}

	// Handle image content.
	if opts.Result.Data != "" && strings.HasPrefix(opts.Result.MIMEType, "image/") {
		body := toolOutputImageContent(sty, opts.Result.Data, opts.Result.MIMEType)
		return joinToolParts(header, body)
	}

	// Try to get content from metadata first (contains actual file content).
	var meta tools.ReadResponseMetadata
	content := opts.Result.Content
	if err := json.Unmarshal([]byte(opts.Result.Metadata), &meta); err == nil && meta.Content != "" {
		content = meta.Content
	}

	// Handle directory listing (read called on a directory path).
	if meta.IsDirectory {
		path := fsext.PrettyPath(params.Path)
		dirHeader := toolHeader(sty, opts.Status, "List", cappedWidth, opts.Compact, path)
		if opts.Compact {
			return dirHeader
		}
		if earlyState, ok := toolEarlyStateContent(sty, opts, cappedWidth); ok {
			return joinToolParts(dirHeader, earlyState)
		}
		if opts.HasEmptyResult() {
			return dirHeader
		}
		bodyWidth := cappedWidth - toolBodyLeftPaddingTotal
		body := sty.Tool.Body.Render(toolOutputPlainContent(sty, opts.Result.Content, bodyWidth, opts.ExpandedContent))
		return joinToolParts(dirHeader, body)
	}
	if review, ok := opts.Result.AutoReview(); ok && review.Sanitized {
		if opts.Result.ModelSafeContent() == "" {
			return header
		}
		bodyWidth := cappedWidth - toolBodyLeftPaddingTotal
		body := sty.Tool.Body.Render(toolOutputPlainContent(sty, opts.Result.ModelSafeContent(), bodyWidth, opts.ExpandedContent))
		return joinToolParts(header, body)
	}

	// Handle skill content.
	if meta.ResourceType == tools.ReadResourceSkill {
		body := toolOutputSkillContent(sty, meta.ResourceName, meta.ResourceDescription)
		return joinToolParts(header, body)
	}

	if content == "" {
		return header
	}

	// Render code content with syntax highlighting.
	startOffset := 0
	if meta.StartLine > 0 {
		startOffset = meta.StartLine - 1
	}
	body := toolOutputCodeContent(sty, params.Path, content, startOffset, cappedWidth, opts.ExpandedContent)
	return joinToolParts(header, body)
}

// -----------------------------------------------------------------------------
// Write Tool
// -----------------------------------------------------------------------------

// WriteToolMessageItem is a message item that represents a write tool call.
type WriteToolMessageItem struct {
	*baseToolMessageItem
}

var _ ToolMessageItem = (*WriteToolMessageItem)(nil)

// NewWriteToolMessageItem creates a new [WriteToolMessageItem].
func NewWriteToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
) ToolMessageItem {
	return newBaseToolMessageItem(sty, toolCall, result, &WriteToolRenderContext{}, canceled)
}

// WriteToolRenderContext renders write tool messages.
type WriteToolRenderContext struct{}

// RenderTool implements the [ToolRenderer] interface.
func (w *WriteToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	cappedWidth := toolMessageWidth(width)
	if opts.IsPending() {
		return pendingTool(sty, "Write", opts.Anim, opts.Compact)
	}

	var params tools.WriteParams
	if err := json.Unmarshal([]byte(opts.ToolCall.Input), &params); err != nil {
		return ""
	}

	file := fsext.PrettyPath(params.FilePath)
	header := toolHeader(sty, opts.Status, "Write", cappedWidth, opts.Compact, file)
	if opts.Compact {
		return header
	}

	if earlyState, ok := toolEarlyStateContent(sty, opts, cappedWidth); ok {
		return joinToolParts(header, earlyState)
	}

	if params.Content == "" {
		return header
	}

	// Render code content with syntax highlighting.
	body := toolOutputCodeContent(sty, params.FilePath, params.Content, 0, cappedWidth, opts.ExpandedContent)
	return joinToolParts(header, body)
}

// -----------------------------------------------------------------------------
// Edit Tool
// -----------------------------------------------------------------------------

// EditToolMessageItem is a message item that represents an edit tool call.
type EditToolMessageItem struct {
	*baseToolMessageItem
}

var _ ToolMessageItem = (*EditToolMessageItem)(nil)

// NewEditToolMessageItem creates a new [EditToolMessageItem].
func NewEditToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
) ToolMessageItem {
	return newBaseToolMessageItem(sty, toolCall, result, &EditToolRenderContext{}, canceled)
}

// EditToolRenderContext renders edit tool messages.
type EditToolRenderContext struct{}

// RenderTool implements the [ToolRenderer] interface.
func (e *EditToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	// Edit tool uses full width for diffs.
	if opts.IsPending() {
		return pendingTool(sty, "Edit", opts.Anim, opts.Compact)
	}

	var params tools.EditParams
	if err := json.Unmarshal([]byte(opts.ToolCall.Input), &params); err != nil {
		return ""
	}

	file := fsext.PrettyPath(params.FilePath)
	header := toolHeader(sty, opts.Status, "Edit", width, opts.Compact, file)
	if opts.Compact {
		return header
	}

	if earlyState, ok := toolEarlyStateContent(sty, opts, width); ok {
		return joinToolParts(header, earlyState)
	}

	if !opts.HasResult() {
		return header
	}

	// Get diff content from metadata.
	var meta tools.EditResponseMetadata
	if err := json.Unmarshal([]byte(opts.Result.Metadata), &meta); err != nil {
		bodyWidth := width - toolBodyLeftPaddingTotal
		body := sty.Tool.Body.Render(toolOutputPlainContent(sty, opts.Result.Content, bodyWidth, opts.ExpandedContent))
		return joinToolParts(header, body)
	}

	// Render diff: use multi-edit path for batch edits, plain diff otherwise.
	var body string
	if len(params.Edits) > 0 {
		body = toolOutputMultiEditDiffContent(sty, file, meta, len(params.Edits), width, opts.ExpandedContent)
	} else {
		body = toolOutputDiffContent(sty, file, meta.OldContent, meta.NewContent, width, opts.ExpandedContent)
	}
	return joinToolParts(header, body)
}

// -----------------------------------------------------------------------------
// Download Tool
// -----------------------------------------------------------------------------

// DownloadToolMessageItem is a message item that represents a download tool call.
type DownloadToolMessageItem struct {
	*baseToolMessageItem
}

var _ ToolMessageItem = (*DownloadToolMessageItem)(nil)

// NewDownloadToolMessageItem creates a new [DownloadToolMessageItem].
func NewDownloadToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
) ToolMessageItem {
	return newBaseToolMessageItem(sty, toolCall, result, &DownloadToolRenderContext{}, canceled)
}

// DownloadToolRenderContext renders download tool messages.
type DownloadToolRenderContext struct{}

// RenderTool implements the [ToolRenderer] interface.
func (d *DownloadToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	cappedWidth := toolMessageWidth(width)
	if opts.IsPending() {
		return pendingTool(sty, "Download", opts.Anim, opts.Compact)
	}

	var params tools.DownloadParams
	if err := json.Unmarshal([]byte(opts.ToolCall.Input), &params); err != nil {
		return ""
	}

	toolParams := []string{params.URL}
	if params.FilePath != "" {
		toolParams = append(toolParams, "file_path", fsext.PrettyPath(params.FilePath))
	}
	if params.Timeout != 0 {
		toolParams = append(toolParams, "timeout", formatTimeout(params.Timeout))
	}

	header := toolHeader(sty, opts.Status, "Download", cappedWidth, opts.Compact, toolParams...)
	if opts.Compact {
		return header
	}

	if earlyState, ok := toolEarlyStateContent(sty, opts, cappedWidth); ok {
		return joinToolParts(header, earlyState)
	}

	if opts.HasEmptyResult() {
		return header
	}

	bodyWidth := cappedWidth - toolBodyLeftPaddingTotal
	body := sty.Tool.Body.Render(toolOutputPlainContent(sty, opts.Result.Content, bodyWidth, opts.ExpandedContent))
	return joinToolParts(header, body)
}
