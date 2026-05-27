package chat

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/planmode"
	"github.com/charmbracelet/crush/internal/ui/anim"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/x/ansi"
	"github.com/zeebo/xxh3"
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

// thinkingStreamThrottle is the minimum interval between cache invalidations
// during pure thinking streaming. This prevents expensive re-renders on every
// streaming delta while keeping the spinner animation smooth at 20 FPS.
const thinkingStreamThrottle = 200 * time.Millisecond

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

	// Thinking glamour render cache. Separates the expensive glamour markdown
	// render from the truncation/box-styling step so that expand/collapse can
	// reuse the glamour output without re-rendering.
	thinkingFullRender  string // Full glamour-rendered thinking (not boxed).
	thinkingContentHash uint64 // xxh3 hash of the raw thinking text.
	thinkingRenderWidth int    // Width at which thinkingFullRender was rendered.
	plainThinkingMode   bool   // True when cache holds raw text (streaming+expanded skip).

	// Streaming invalidation throttle state. During pure thinking streaming,
	// cache invalidation is throttled to avoid expensive re-renders on every
	// delta while keeping the spinner smooth at 20 FPS.
	lastInvalidation time.Time
	wasThinking      bool

	// prefixedCache stores the content with focus/blur prefixes already
	// applied. This avoids re-splitting and re-joining the entire message on
	// every animation frame when only the spinner changes.
	prefixedCache struct {
		rendered  string
		width     int
		focused   bool
		startLine int
		startCol  int
		endLine   int
		endCol    int
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
		if content != "" {
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

	prefix := a.sty.Chat.Message.AssistantBlurred.Render()
	if a.focused {
		prefix = a.sty.Chat.Message.AssistantFocused.Render()
	}

	// Apply focus/blur prefix, using cache when possible.
	var prefixedContent string
	startLine, startCol, endLine, endCol := a.Highlight()
	if a.prefixedCache.width == cappedWidth &&
		a.prefixedCache.focused == a.focused &&
		a.prefixedCache.startLine == startLine &&
		a.prefixedCache.startCol == startCol &&
		a.prefixedCache.endLine == endLine &&
		a.prefixedCache.endCol == endCol {
		prefixedContent = a.prefixedCache.rendered
	} else {
		prefixedContent = applyLinePrefix(content, prefix)
		a.prefixedCache.rendered = prefixedContent
		a.prefixedCache.width = cappedWidth
		a.prefixedCache.focused = a.focused
		a.prefixedCache.startLine = startLine
		a.prefixedCache.startCol = startCol
		a.prefixedCache.endLine = endLine
		a.prefixedCache.endCol = endCol
	}

	// Append spinner if the message is still loading.
	var spinner string
	if a.isSpinning() {
		spinner = a.renderSpinning()
	}
	if spinner != "" {
		if content != "" {
			return prefixedContent + "\n\n" + applyLinePrefix(spinner, prefix)
		}
		return applyLinePrefix(spinner, prefix)
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
//
// The method is split into two phases:
//   - Phase A (cacheable): glamour markdown render, keyed by content hash + width.
//     This is the expensive step (~40-80ms for 800+ lines).
//   - Phase B (cheap): truncation + box styling (~1-5ms collapsed, ~5-15ms expanded).
//
// Expand/collapse only re-runs Phase B, preserving the glamour cache.
func (a *AssistantMessageItem) renderThinking(thinking string, width int) string {
	// Phase A: glamour markdown render (cached by content hash + width).
	contentHash := xxh3.HashString(thinking)

	// During active streaming with the thinking block expanded, skip the
	// expensive glamour render (~40-80ms for 800+ lines) to keep scroll
	// responsive. If a glamour render was cached before the user expanded,
	// keep showing that (slightly stale but smooth). Once streaming ends
	// (plainThinkingMode flips back to false), a full glamour re-render
	// happens automatically because the cache is marked stale.
	plainMode := a.message.IsThinking() && a.thinkingExpanded
	if plainMode != a.plainThinkingMode {
		// Mode transition — force glamour re-render on next non-plain pass.
		a.thinkingContentHash = 0
		a.plainThinkingMode = plainMode
	}

	if !plainMode && (contentHash != a.thinkingContentHash || width != a.thinkingRenderWidth) {
		renderer := common.PlainMarkdownRenderer(a.sty, width)
		rendered, err := renderer.Render(thinking)
		if err != nil {
			rendered = thinking
		}
		a.thinkingFullRender = strings.TrimSpace(rendered)
		a.thinkingContentHash = contentHash
		a.thinkingRenderWidth = width
	}
	// In plainMode: keep thinkingFullRender as-is (cached glamour or raw text).
	// Fallback: if cache was cleared (e.g. just expanded during streaming),
	// use raw thinking text to avoid showing empty content.
	if a.thinkingFullRender == "" {
		a.thinkingFullRender = thinking
	}

	// Phase B: truncation + box styling (runs on every call, relatively cheap).
	lines := strings.Split(a.thinkingFullRender, "\n")
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
	// If thinking is done add the thought for footer.
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

// invalidateCache clears all render caches: base content, prefixed render,
// and thinking glamour cache. Call this when the message content or visual
// state changes in a way that affects the thinking glamour output.
func (a *AssistantMessageItem) invalidateCache() {
	a.clearCache()
	a.invalidatePrefixedCache()
	a.invalidateThinkingCache()
}

// invalidateContentCache clears the base content cache and prefixed render
// cache but preserves the thinking glamour cache. Use this for state changes
// that only affect truncation or styling, not the markdown content itself
// (e.g. expand/collapse).
func (a *AssistantMessageItem) invalidateContentCache() {
	a.clearCache()
	a.invalidatePrefixedCache()
}

// invalidatePrefixedCache clears the prefixed render cache.
func (a *AssistantMessageItem) invalidatePrefixedCache() {
	a.prefixedCache.width = 0
	a.prefixedCache.rendered = ""
	a.prefixedCache.focused = false
	a.prefixedCache.startLine = -1
	a.prefixedCache.startCol = -1
	a.prefixedCache.endLine = -1
	a.prefixedCache.endCol = -1
}

// invalidateThinkingCache clears the glamour render cache for thinking content.
func (a *AssistantMessageItem) invalidateThinkingCache() {
	a.thinkingFullRender = ""
	a.thinkingContentHash = 0
	a.thinkingRenderWidth = 0
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

// SetMessage is used to update the underlying message. During pure thinking
// streaming, cache invalidation is throttled to thinkingStreamThrottle to
// prevent expensive re-renders on every delta while keeping the spinner
// animation smooth at 20 FPS.
func (a *AssistantMessageItem) SetMessage(msg *message.Message) tea.Cmd {
	wasSpinning := a.isSpinning()
	wasThinking := a.wasThinking

	thinkingNow := msg.ReasoningContent().Thinking != ""
	contentNow := msg.Content().Text != "" || len(msg.ToolCalls()) > 0
	a.wasThinking = thinkingNow

	a.message = msg

	shouldInvalidate := false
	if contentNow {
		// Content appeared or is streaming — always invalidate immediately.
		// This covers the critical thinking→content transition.
		shouldInvalidate = true
	} else if thinkingNow {
		if !wasThinking {
			// First thinking delta — always show.
			shouldInvalidate = true
		} else if time.Since(a.lastInvalidation) >= thinkingStreamThrottle {
			// Throttle window passed — safe to re-render.
			shouldInvalidate = true
		}
		// else: throttled, skip invalidation; old render stays visible.
	}

	if shouldInvalidate {
		a.invalidateCache()
		a.lastInvalidation = time.Now()
	}

	if !wasSpinning && a.isSpinning() {
		return a.StartAnimation()
	}
	return nil
}

// ToggleExpanded toggles the expanded state of the thinking box.
func (a *AssistantMessageItem) ToggleExpanded() {
	a.thinkingExpanded = !a.thinkingExpanded
	// Preserve the thinking glamour cache — only truncation/boxing changes.
	a.invalidateContentCache()
}

// HandleMouseClick implements MouseClickable.
func (a *AssistantMessageItem) HandleMouseClick(btn ansi.MouseButton, x, y int) bool {
	if btn != ansi.MouseLeft {
		return false
	}
	// Check if the click is within the thinking box.
	if a.thinkingBoxHeight > 0 && y < a.thinkingBoxHeight {
		a.thinkingExpanded = !a.thinkingExpanded
		// Preserve the thinking glamour cache — only truncation/boxing changes.
		a.invalidateContentCache()
		return true
	}
	// Check if the click is within the summary box.
	summaryEnd := a.summaryBoxStart + a.summaryBoxHeight
	if a.summaryBoxHeight > 0 && y >= a.summaryBoxStart && y < summaryEnd {
		a.summaryExpanded = !a.summaryExpanded
		a.invalidateContentCache()
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
