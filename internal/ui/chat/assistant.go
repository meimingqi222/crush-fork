package chat

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/planmode"
	"github.com/charmbracelet/crush/internal/ui/anim"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/x/ansi"
)

// assistantMessageTruncateFormat is the text shown when an assistant message is
// truncated.
const assistantMessageTruncateFormat = "… (%d lines hidden) [click or space to expand]"

// maxCollapsedThinkingHeight defines the maximum height of the thinking
// content before it is collapsed.
const maxCollapsedThinkingHeight = 10

// maxCollapsedSummaryHeight defines the maximum number of lines shown in a
// collapsed context summary.
const maxCollapsedSummaryHeight = 8

// AssistantMessageItem represents an assistant message in the chat UI.
//
// This item includes thinking, and the content but does not include the tool calls.
type AssistantMessageItem struct {
	*highlightableMessageItem
	*cachedMessageItem
	*focusableMessageItem

	message           *message.Message
	sty               *styles.Styles
	anim              *anim.Anim
	showLoadingState  bool
	thinkingExpanded  bool
	thinkingBoxHeight int // Tracks the rendered thinking box height for click detection.
	summaryExpanded   bool
	summaryBoxHeight  int // Tracks the rendered summary box height for click detection.
	summaryBoxStart   int // Y offset where the summary box begins.

	// currentAnimLabel tracks the current animation label to avoid redundant
	// SetLabel calls on every animation frame.
	currentAnimLabel string

	// prefixedCache stores the content with focus/blur prefixes already
	// applied. This avoids re-splitting and re-joining the entire message on
	// every animation frame when only the spinner changes.
	prefixedCache struct {
		rendered string
		width    int
		focused  bool
	}
}

// NewAssistantMessageItem creates a new AssistantMessageItem.
func NewAssistantMessageItem(sty *styles.Styles, message *message.Message) MessageItem {
	a := &AssistantMessageItem{
		highlightableMessageItem: defaultHighlighter(sty),
		cachedMessageItem:        &cachedMessageItem{},
		focusableMessageItem:     &focusableMessageItem{},
		message:                  message,
		sty:                      sty,
		showLoadingState:         true,
	}

	a.anim = anim.New(anim.Settings{
		ID:          a.ID(),
		Size:        15,
		GradColorA:  sty.Primary,
		GradColorB:  sty.Secondary,
		LabelColor:  sty.FgBase,
		CycleColors: true,
	})
	return a
}

// StartAnimation starts the assistant message animation if it should be spinning.
func (a *AssistantMessageItem) StartAnimation() tea.Cmd {
	if !a.isSpinning() {
		return nil
	}
	return a.anim.Start()
}

// Animate progresses the assistant message animation if it should be spinning.
func (a *AssistantMessageItem) Animate(msg anim.StepMsg) tea.Cmd {
	if !a.isSpinning() {
		return nil
	}
	return a.anim.Animate(msg)
}

// ID implements MessageItem.
func (a *AssistantMessageItem) ID() string {
	return a.message.ID
}

// RawRender implements [MessageItem].
func (a *AssistantMessageItem) RawRender(width int) string {
	cappedWidth := cappedMessageWidth(width)

	var spinner string
	if a.isSpinning() {
		spinner = a.renderSpinning()
	}

	content, height, ok := a.getCachedRender(cappedWidth)
	if !ok {
		content = a.renderMessageContent(cappedWidth)
		height = lipgloss.Height(content)
		// cache the rendered content
		a.setCachedRender(content, cappedWidth, height)
	}

	highlightedContent := a.renderHighlighted(content, cappedWidth, height)
	if spinner != "" {
		if highlightedContent != "" {
			highlightedContent += "\n\n"
		}
		return highlightedContent + spinner
	}

	return highlightedContent
}

// Render implements MessageItem.
func (a *AssistantMessageItem) Render(width int) string {
	cappedWidth := cappedMessageWidth(width)

	// Build the content (thinking + main content + finish reason).
	content, contentHeight, ok := a.getCachedRender(cappedWidth)
	if !ok {
		content = a.renderMessageContent(cappedWidth)
		contentHeight = lipgloss.Height(content)
		a.setCachedRender(content, cappedWidth, contentHeight)
	}
	content = a.renderHighlighted(content, cappedWidth, contentHeight)

	// Apply focus/blur prefix, using cache when possible.
	var prefixedContent string
	if a.prefixedCache.width == cappedWidth && a.prefixedCache.focused == a.focused {
		prefixedContent = a.prefixedCache.rendered
	} else {
		prefix := a.sty.Chat.Message.AssistantBlurred.Render()
		if a.focused {
			prefix = a.sty.Chat.Message.AssistantFocused.Render()
		}
		prefixedContent = applyLinePrefix(content, prefix)
		a.prefixedCache.rendered = prefixedContent
		a.prefixedCache.width = cappedWidth
		a.prefixedCache.focused = a.focused
	}

	// Append spinner if the message is still loading.
	var spinner string
	if a.isSpinning() {
		spinner = a.renderSpinning()
	}
	if spinner != "" {
		if prefixedContent != "" {
			return prefixedContent + "\n\n" + spinner
		}
		return spinner
	}

	return prefixedContent
}

// SetLoadingStateVisible controls whether loading UI should be rendered for
// unfinished assistant messages restored from history.
func (a *AssistantMessageItem) SetLoadingStateVisible(visible bool) {
	if a.showLoadingState == visible {
		return
	}
	a.showLoadingState = visible
	a.invalidateCache()
}

// renderMessageContent renders the message content including thinking, main content, and finish reason.
func (a *AssistantMessageItem) renderMessageContent(width int) string {
	var messageParts []string
	thinking, _ := message.StripTextualToolCallProtocol(a.message.ReasoningContent().Thinking)
	thinking = strings.TrimSpace(thinking)
	content, _ := message.StripTextualToolCallProtocol(a.message.Content().Text)
	content = strings.TrimSpace(content)
	// if the massage has reasoning content add that first
	if thinking != "" {
		messageParts = append(messageParts, a.renderThinking(thinking, width))
	}

	// then add the main content
	if content != "" {
		// Compute the Y offset at which the content block starts.
		summaryStart := 0
		if thinking != "" {
			// Spacer line between thinking and content adds 1.
			summaryStart = a.thinkingBoxHeight + 1
			messageParts = append(messageParts, "")
		}
		if a.message.IsSummaryMessage {
			a.summaryBoxStart = summaryStart
			messageParts = append(messageParts, a.renderSummary(content, width))
		} else if plan, ok := planmode.ExtractProposedPlan(content); ok && a.hasToolCall(tools.PlanExitToolName) {
			messageParts = append(messageParts, a.renderPlan(plan, width))
		} else {
			messageParts = append(messageParts, a.renderMarkdown(content, width))
		}
	}

	// finally add any finish reason info
	if a.message.IsFinished() {
		switch a.message.FinishReason() {
		case message.FinishReasonCanceled:
			messageParts = append(messageParts, a.sty.Base.Italic(true).Render("Canceled"))
		case message.FinishReasonError:
			messageParts = append(messageParts, a.renderError(width))
		}
	}

	return strings.Join(messageParts, "\n")
}

// renderThinking renders the thinking/reasoning content with footer.
func (a *AssistantMessageItem) renderThinking(thinking string, width int) string {
	renderer := common.PlainMarkdownRenderer(a.sty, width)
	rendered, err := renderer.Render(thinking)
	if err != nil {
		rendered = thinking
	}
	rendered = strings.TrimSpace(rendered)

	lines := strings.Split(rendered, "\n")
	totalLines := len(lines)

	isTruncated := totalLines > maxCollapsedThinkingHeight
	if !a.thinkingExpanded && isTruncated {
		lines = lines[totalLines-maxCollapsedThinkingHeight:]
		hint := a.sty.Chat.Message.ThinkingTruncationHint.Render(
			fmt.Sprintf(assistantMessageTruncateFormat, totalLines-maxCollapsedThinkingHeight),
		)
		lines = append([]string{hint, ""}, lines...)
	}

	thinkingStyle := a.sty.Chat.Message.ThinkingBox.Width(width)
	result := thinkingStyle.Render(strings.Join(lines, "\n"))
	a.thinkingBoxHeight = lipgloss.Height(result)

	var footer string
	// if thinking is done add the thought for footer
	if !a.message.IsThinking() || len(a.message.ToolCalls()) > 0 {
		duration := a.message.ThinkingDuration()
		if duration.String() != "0s" {
			footer = a.sty.Chat.Message.ThinkingFooterTitle.Render("Thought for ") +
				a.sty.Chat.Message.ThinkingFooterDuration.Render(duration.String())
		}
	}

	if footer != "" {
		result += "\n\n" + footer
	}

	return result
}

// renderSummary renders context compaction summary content in a collapsible
// box with a distinct header tag row and a left-bordered body.
func (a *AssistantMessageItem) renderSummary(content string, width int) string {
	// Account for left border (1) + left padding (2) + right padding (1).
	const boxOverhead = 4
	innerWidth := max(width-boxOverhead, 10)

	renderer := common.MarkdownRenderer(a.sty, innerWidth)
	rendered, err := renderer.Render(content)
	if err != nil {
		rendered = content
	}
	rendered = strings.TrimSpace(rendered)

	lines := strings.Split(rendered, "\n")
	totalLines := len(lines)

	isTruncated := totalLines > maxCollapsedSummaryHeight
	var hint string
	if !a.summaryExpanded && isTruncated {
		lines = lines[:maxCollapsedSummaryHeight]
		hint = a.sty.Chat.Message.SummaryTruncationHint.Render(
			fmt.Sprintf(assistantMessageTruncateFormat, totalLines-maxCollapsedSummaryHeight),
		)
	}

	// Header row: pill tag followed by a fill line.
	tag := a.sty.Chat.Message.SummaryHeader.Render("◈ CONTEXT SUMMARY")
	tagWidth := lipgloss.Width(tag)
	fillWidth := width - tagWidth - 1
	var headerRow string
	if fillWidth > 0 {
		fill := a.sty.Chat.Message.SummaryHeaderLine.Render(
			strings.Repeat(styles.SectionSeparator, fillWidth),
		)
		headerRow = tag + " " + fill
	} else {
		headerRow = tag
	}

	// Body box with left border accent and padding.
	bodyContent := strings.Join(lines, "\n")
	summaryStyle := a.sty.Chat.Message.SummaryBox.Width(width)
	body := summaryStyle.Render(bodyContent)

	parts := []string{headerRow, body}
	if hint != "" {
		parts = append(parts, "  "+hint)
	}
	result := strings.Join(parts, "\n")
	a.summaryBoxHeight = lipgloss.Height(result)
	return result
}

// renderMarkdown renders content as markdown.
func (a *AssistantMessageItem) renderMarkdown(content string, width int) string {
	renderer := common.MarkdownRenderer(a.sty, width)
	result, err := renderer.Render(content)
	if err != nil {
		return content
	}
	return strings.TrimSuffix(result, "\n")
}

func (a *AssistantMessageItem) renderPlan(plan string, width int) string {
	header := common.Section(a.sty, "Proposed Plan", width)
	body := a.renderMarkdown(plan, width)
	return strings.Join([]string{header, "", body}, "\n")
}

func (a *AssistantMessageItem) hasToolCall(toolName string) bool {
	for _, toolCall := range a.message.ToolCalls() {
		if toolCall.Name == toolName {
			return true
		}
	}
	return false
}

func (a *AssistantMessageItem) renderSpinning() string {
	var label string
	if a.message.IsThinking() {
		label = "Thinking"
	} else if a.message.IsSummaryMessage {
		label = "Summarizing"
	}
	// Only update the animation label when it actually changes to avoid
	// re-rendering overhead on every animation frame.
	if label != "" && label != a.currentAnimLabel {
		a.currentAnimLabel = label
		a.anim.SetLabel(label)
	}
	return a.anim.Render()
}

// invalidateCache clears both the base content cache and the prefixed render
// cache. Call this whenever the message content or visual state changes.
func (a *AssistantMessageItem) invalidateCache() {
	a.clearCache()
	a.prefixedCache.width = 0
	a.prefixedCache.rendered = ""
	a.prefixedCache.focused = false
}

// renderError renders an error message.
func (a *AssistantMessageItem) renderError(width int) string {
	finishPart := a.message.FinishPart()
	errTag := a.sty.Chat.Message.ErrorTag.Render("ERROR")
	truncated := ansi.Truncate(finishPart.Message, width-2-lipgloss.Width(errTag), "...")
	title := fmt.Sprintf("%s %s", errTag, a.sty.Chat.Message.ErrorTitle.Render(truncated))
	details := a.sty.Chat.Message.ErrorDetails.Width(width - 2).Render(finishPart.Details)
	return fmt.Sprintf("%s\n\n%s", title, details)
}

// isSpinning returns true if the assistant message is still generating.
func (a *AssistantMessageItem) isSpinning() bool {
	if !a.showLoadingState {
		return false
	}
	isThinking := a.message.IsThinking()
	isFinished := a.message.IsFinished()
	hasContent := strings.TrimSpace(a.message.Content().Text) != ""
	hasToolCalls := len(a.message.ToolCalls()) > 0
	return (isThinking || !isFinished) && !hasContent && !hasToolCalls
}

// SetMessage is used to update the underlying message.
func (a *AssistantMessageItem) SetMessage(message *message.Message) tea.Cmd {
	wasSpinning := a.isSpinning()
	a.message = message
	a.invalidateCache()
	if !wasSpinning && a.isSpinning() {
		return a.StartAnimation()
	}
	return nil
}

// ToggleExpanded toggles the expanded state of the thinking box.
func (a *AssistantMessageItem) ToggleExpanded() {
	a.thinkingExpanded = !a.thinkingExpanded
	a.invalidateCache()
}

// HandleMouseClick implements MouseClickable.
func (a *AssistantMessageItem) HandleMouseClick(btn ansi.MouseButton, x, y int) bool {
	if btn != ansi.MouseLeft {
		return false
	}
	// Check if the click is within the thinking box.
	if a.thinkingBoxHeight > 0 && y < a.thinkingBoxHeight {
		a.thinkingExpanded = !a.thinkingExpanded
		a.invalidateCache()
		return true
	}
	// Check if the click is within the summary box.
	summaryEnd := a.summaryBoxStart + a.summaryBoxHeight
	if a.summaryBoxHeight > 0 && y >= a.summaryBoxStart && y < summaryEnd {
		a.summaryExpanded = !a.summaryExpanded
		a.invalidateCache()
		return true
	}
	return false
}

// HandleKeyEvent implements KeyEventHandler.
func (a *AssistantMessageItem) HandleKeyEvent(key tea.KeyMsg) (bool, tea.Cmd) {
	if k := key.String(); k == "c" || k == "y" {
		text := a.message.Content().Text
		return true, common.CopyToClipboard(text, "Message copied to clipboard")
	}
	return false, nil
}
