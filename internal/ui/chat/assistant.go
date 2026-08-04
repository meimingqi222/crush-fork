package chat

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/anim"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/list"
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

// contentStreamThrottle limits cache invalidation during pure content streaming.
const contentStreamThrottle = 200 * time.Millisecond

type clickRegion struct {
	name          string
	start, height int
}

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
	thinkingBoxHeight int // Legacy Y bookkeeping; prefer clickRegions.
	summaryExpanded   bool
	summaryBoxHeight  int
	summaryBoxStart   int
	clickRegions      []clickRegion

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

	// Streaming invalidation throttle state. During thinking and content
	// streaming, cache invalidation is throttled to avoid expensive
	// re-renders on every delta while keeping the spinner smooth at 20 FPS.
	// lastInvalidation.IsZero() doubles as the "first delta of this run"
	// signal so pure-content models (which never set wasThinking) also get
	// an immediate first render before throttling kicks in.
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

	// Viewport rendering cache. Stores the full content render split into
	// lines so that RenderVisible can slice the visible range without
	// re-rendering. This is the primary optimization for large expanded
	// thinking blocks: only ~40-50 visible lines are processed per frame
	// instead of all 500+.
	viewportLines  []string
	viewportWidth  int
	viewportHeight int

	// Cached display content: avoids applying protocol filters on every render
	// frame.
	cachedStrippedThinking      string
	cachedStrippedThinkingInput string
	cachedStrippedContent       string
	cachedStrippedContentInput  string

	// Cached main content render: avoids expensive glamour markdown render on
	// every frame when the content hasn't changed.
	contentRenderCache string
	contentContentHash uint64
	contentRenderWidth int

	// Cached summary render: avoids expensive glamour markdown render on
	// every frame when the summary content hasn't changed.
	summaryRenderCache string
	summaryContentHash uint64
	summaryRenderWidth int

	// loadingStartedAt is used as a fallback for messages created without a
	// persisted timestamp, such as lightweight UI tests.
	loadingStartedAt time.Time
}

var (
	_ Expandable              = (*AssistantMessageItem)(nil)
	_ list.ViewportRenderable = (*AssistantMessageItem)(nil)
)

// NewAssistantMessageItem creates a new AssistantMessageItem.
func NewAssistantMessageItem(sty *styles.Styles, message *message.Message) MessageItem {
	a := &AssistantMessageItem{
		highlightableMessageItem: defaultHighlighter(sty),
		cachedMessageItem:        &cachedMessageItem{},
		focusableMessageItem:     &focusableMessageItem{},
		message:                  message,
		sty:                      sty,
		showLoadingState:         true,
		loadingStartedAt:         time.Now(),
	}
	if message.CreatedAt > 0 {
		a.loadingStartedAt = time.Unix(message.CreatedAt, 0)
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
	return nil
}

// Animate progresses the assistant message animation if it should be spinning.
func (a *AssistantMessageItem) Animate(msg anim.StepMsg) tea.Cmd {
	if !a.isSpinning() {
		return nil
	}
	return a.anim.Animate(msg)
}

// TickAnimation advances the animation by one frame.
func (a *AssistantMessageItem) TickAnimation() {
	if a.isSpinning() {
		a.anim.Tick()
	}
}

// IsAnimating reports whether the assistant message is currently spinning.
func (a *AssistantMessageItem) IsAnimating() bool {
	return a.isSpinning()
}

// ID implements MessageItem.
func (a *AssistantMessageItem) ID() string {
	return a.message.ID
}

// Message returns the underlying message for this assistant item.
func (a *AssistantMessageItem) Message() *message.Message {
	return a.message
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

// renderThinkingIndicator renders a compact indicator line for reasoning content.
// It is shown when the thinking block is collapsed to avoid cluttering the chat
// with long reasoning text.
func (a *AssistantMessageItem) renderThinkingIndicator(width int) string {
	duration := a.message.ThinkingDuration()
	var label string
	switch {
	case a.isSpinning():
		label = "Thinking..."
	case duration.String() != "0s":
		label = fmt.Sprintf("Thought for %s", duration.String())
	default:
		label = "Reasoning"
	}
	hint := a.sty.Chat.Message.ThinkingIndicator.Render(label + " (Ctrl+O to view)")
	result := a.sty.Chat.Message.ThinkingBox.Width(width).Render(hint)
	a.thinkingBoxHeight = lipgloss.Height(result)
	return result
}

func (a *AssistantMessageItem) renderMessageContent(width int) string {
	var messageParts []string

	// Use cached display content to avoid protocol filtering on every frame.
	rawThinking := a.message.ReasoningContent().Thinking
	if rawThinking != a.cachedStrippedThinkingInput {
		a.cachedStrippedThinking, _ = message.DisplayText(rawThinking)
		a.cachedStrippedThinkingInput = rawThinking
	}
	thinking := strings.TrimSpace(a.cachedStrippedThinking)

	rawContent := a.message.Content().Text
	if rawContent != a.cachedStrippedContentInput {
		a.cachedStrippedContent, _ = message.DisplayText(rawContent)
		a.cachedStrippedContentInput = rawContent
	}
	content := strings.TrimSpace(a.cachedStrippedContent)
	if display, ok := agent.RetryStatusDisplayText(content); ok {
		content = display
	}
	// If there is reasoning content, show either a compact indicator (when
	// collapsed) or the full thinking block (when expanded). The full block is
	// no longer rendered by default to keep long conversations readable and
	// fast; press Ctrl+O or click the indicator to view the reasoning.
	showThinking := thinking != "" && !a.message.IsSummaryMessage
	if showThinking {
		if a.thinkingExpanded {
			messageParts = append(messageParts, a.renderThinking(thinking, width))
		} else {
			messageParts = append(messageParts, a.renderThinkingIndicator(width))
		}
	}

	// then add the main content
	if content != "" {
		// Compute the Y offset at which the content block starts.
		summaryStart := 0
		if showThinking {
			// Spacer line between thinking and content adds 1.
			summaryStart = a.thinkingBoxHeight + 1
			messageParts = append(messageParts, "")
		}
		if a.message.IsSummaryMessage {
			a.summaryBoxStart = summaryStart
			messageParts = append(messageParts, a.renderSummary(content, width))
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

	out := strings.Join(messageParts, "\n")
	a.clickRegions = a.buildClickRegions(out)
	return out
}

func (a *AssistantMessageItem) buildClickRegions(content string) []clickRegion {
	if content == "" {
		return nil
	}
	lines := strings.Split(content, "\n")
	var regions []clickRegion
	y := 0
	if a.thinkingBoxHeight > 0 && !a.message.IsSummaryMessage {
		regions = append(regions, clickRegion{name: "thinking", start: 0, height: a.thinkingBoxHeight})
		y = a.thinkingBoxHeight
		if y < len(lines) && lines[y] == "" {
			y++
		}
	}
	if a.summaryBoxHeight > 0 && a.message.IsSummaryMessage {
		start := a.summaryBoxStart
		if start < 0 {
			start = y
		}
		regions = append(regions, clickRegion{name: "summary", start: start, height: a.summaryBoxHeight})
	}
	return regions
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
	a.thinkingBoxHeight = lipgloss.Height(result)

	return result
}

// renderSummary renders context compaction summary content in a collapsible
// box with a distinct header tag row and a left-bordered body.
func (a *AssistantMessageItem) renderSummary(content string, width int) string {
	// Check cache: skip expensive glamour render if content and width unchanged.
	contentHash := xxh3.HashString(content)
	if a.summaryRenderCache != "" && a.summaryContentHash == contentHash && a.summaryRenderWidth == width {
		return a.summaryRenderCache
	}

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

	// Cache the result.
	a.summaryRenderCache = result
	a.summaryContentHash = contentHash
	a.summaryRenderWidth = width

	return result
}

// renderMarkdown renders content as markdown.
func (a *AssistantMessageItem) renderMarkdown(content string, width int) string {
	contentHash := xxh3.HashString(content)
	if a.contentRenderCache != "" && a.contentContentHash == contentHash && a.contentRenderWidth == width {
		return a.contentRenderCache
	}

	renderer := common.MarkdownRenderer(a.sty, width)
	result, err := renderer.Render(content)
	if err != nil {
		result = content
	}
	result = strings.TrimSuffix(result, "\n")

	a.contentRenderCache = result
	a.contentContentHash = contentHash
	a.contentRenderWidth = width
	return result
}

// invalidateContentRenderCache clears the main content glamour render cache.
func (a *AssistantMessageItem) invalidateContentRenderCache() {
	a.contentRenderCache = ""
	a.contentContentHash = 0
	a.contentRenderWidth = 0
}

func (a *AssistantMessageItem) renderSpinning() string {
	label := "Thinking"
	if a.message.IsSummaryMessage {
		label = "Summarizing"
	}
	label = fmt.Sprintf("%s (%s)", label, formatLoadingDuration(a.loadingElapsed()))

	// Only update the animation label when it actually changes to avoid
	// re-rendering overhead on every animation frame. The label changes once
	// per second as the elapsed time advances.
	if label != a.currentAnimLabel {
		a.currentAnimLabel = label
		a.anim.SetLabel(label)
	}
	return a.anim.Render()
}

func (a *AssistantMessageItem) loadingElapsed() time.Duration {
	if a.loadingStartedAt.IsZero() {
		return 0
	}
	elapsed := time.Since(a.loadingStartedAt)
	if elapsed < 0 {
		return 0
	}
	return elapsed
}

func formatLoadingDuration(duration time.Duration) string {
	return duration.Truncate(time.Second).String()
}

// invalidateCache clears all render caches: base content, prefixed render,
// thinking glamour cache, viewport cache, and summary cache. Call this when
// the message content or visual state changes in a way that affects the
// thinking glamour output. Legacy height/offset fields are reset too so a
// block that disappears on the next render does not leave stale geometry
// behind for click hit-testing (see buildClickRegions).
func (a *AssistantMessageItem) invalidateCache() {
	a.clearCache()
	a.invalidatePrefixedCache()
	a.invalidateThinkingCache()
	a.invalidateContentRenderCache()
	a.invalidateViewportCache()
	a.invalidateSummaryCache()
	a.invalidateStrippedCache()
	a.thinkingBoxHeight = 0
	a.summaryBoxHeight = 0
	a.summaryBoxStart = 0
}

// invalidateContentCache clears the base content cache, prefixed render
// cache, viewport cache, and summary cache but preserves the thinking glamour
// cache. Use this for state changes that only affect truncation or styling,
// not the markdown content itself (e.g. expand/collapse).
func (a *AssistantMessageItem) invalidateContentCache() {
	a.clearCache()
	a.invalidatePrefixedCache()
	a.invalidateContentRenderCache()
	a.invalidateViewportCache()
	a.invalidateSummaryCache()
}

// invalidatePrefixedCache clears the prefixed render cache.

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

// invalidateViewportCache clears the viewport rendering cache.
func (a *AssistantMessageItem) invalidateViewportCache() {
	a.viewportLines = nil
	a.viewportWidth = 0
	a.viewportHeight = 0
}

// invalidateSummaryCache clears the summary render cache.
func (a *AssistantMessageItem) invalidateSummaryCache() {
	a.summaryRenderCache = ""
	a.summaryContentHash = 0
	a.summaryRenderWidth = 0
}

// invalidateStrippedCache clears the cached display-filter results.
func (a *AssistantMessageItem) invalidateStrippedCache() {
	a.cachedStrippedThinking = ""
	a.cachedStrippedThinkingInput = ""
	a.cachedStrippedContent = ""
	a.cachedStrippedContentInput = ""
}

// ensureViewportCache populates the viewport cache if it has been
// invalidated or the width has changed. This calls renderMessageContent
// which internally reuses the cached glamour render, so repeated calls
// within the same frame are O(1).
func (a *AssistantMessageItem) ensureViewportCache(cappedWidth int) {
	if a.viewportWidth == cappedWidth && len(a.viewportLines) > 0 {
		return
	}
	content := a.renderMessageContent(cappedWidth)
	a.viewportWidth = cappedWidth
	if content == "" {
		// strings.Split("", "\n") returns one empty line. Treat an empty
		// assistant message as zero content lines so the spinner does not
		// reserve a blank line before rendering.
		a.viewportLines = nil
		a.viewportHeight = 0
		return
	}
	a.viewportLines = strings.Split(content, "\n")
	a.viewportHeight = len(a.viewportLines)
}

// TotalHeight implements [list.ViewportRenderable]. It returns the total
// number of lines the item would occupy when fully rendered, without doing
// the expensive post-processing (highlight, prefix, ANSI parsing).
func (a *AssistantMessageItem) TotalHeight(width int) int {
	cappedWidth := cappedMessageWidth(width)
	a.ensureViewportCache(cappedWidth)
	h := a.viewportHeight
	if a.isSpinning() {
		if h > 0 {
			// Non-empty content: blank separator line + spinner line.
			h += 2
		} else {
			// Empty content: just the spinner (no separator).
			h += 1
		}
	}
	return h
}

// RenderVisible implements [list.ViewportRenderable]. It renders only the
// visible line range [startLine, endLine), applying highlight and prefix to
// just those lines instead of the full content. This is the primary
// performance optimization for large expanded thinking blocks.
func (a *AssistantMessageItem) RenderVisible(width, startLine, endLine int) string {
	cappedWidth := cappedMessageWidth(width)
	a.ensureViewportCache(cappedWidth)

	totalLines := len(a.viewportLines)

	// Determine the focus/blur prefix.
	prefix := a.sty.Chat.Message.AssistantBlurred.Render()
	if a.focused {
		prefix = a.sty.Chat.Message.AssistantFocused.Render()
	}

	// Handle the case where startLine is past the content (into the
	// spinner area). Only relevant when spinning.
	if startLine >= totalLines {
		if a.isSpinning() {
			spinner := a.renderSpinning()
			return applyLinePrefix(spinner, prefix)
		}
		return ""
	}

	if endLine < 0 || endLine > totalLines {
		endLine = totalLines
	}
	startLine = max(0, startLine)

	// Slice visible lines from cached content.
	visibleLines := a.viewportLines[startLine:endLine]
	visibleContent := strings.Join(visibleLines, "\n")

	// Apply highlight with adjusted coordinates.
	visibleHeight := len(visibleLines)
	visibleContent = a.renderHighlightedOffset(visibleContent, cappedWidth, visibleHeight, startLine)

	// Apply focus/blur prefix to visible lines only.
	result := applyLinePrefix(visibleContent, prefix)

	// Append spinner if the visible range extends to the end of content
	// and the message is still generating. Use len(visibleLines) > 0
	// (not result) to decide whether to insert the separator, because
	// applyLinePrefix("", prefix) produces a non-empty string (just the
	// prefix) which would incorrectly trigger the separator.
	if a.isSpinning() && endLine >= totalLines {
		spinner := a.renderSpinning()
		prefixedSpinner := applyLinePrefix(spinner, prefix)
		if len(visibleLines) > 0 {
			result += "\n\n" + prefixedSpinner
		} else {
			result = prefixedSpinner
		}
	}

	return result
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
	hasContent := hasNonWhitespace(a.message.Content().Text)
	hasToolCalls := len(a.message.ToolCalls()) > 0
	return (isThinking || !isFinished) && !hasContent && !hasToolCalls
}

// hasNonWhitespace returns true if s contains at least one non-whitespace
// character. This avoids the allocation of strings.TrimSpace.
func hasNonWhitespace(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			return true
		}
	}
	return false
}

// SetMessage is used to update the underlying message. During thinking and
// content streaming, cache invalidation is throttled to prevent expensive
// re-renders on every delta while keeping the spinner animation smooth at
// 20 FPS. The first delta of a streaming run (detected via
// lastInvalidation.IsZero()) is always shown immediately so pure-content
// models — which never set wasThinking — do not bypass the throttle.
func (a *AssistantMessageItem) SetMessage(msg *message.Message) tea.Cmd {
	wasSpinning := a.isSpinning()
	wasThinking := a.wasThinking
	wasFinished := a.message.IsFinished()
	wasFinishReason := a.message.FinishReason()

	thinkingNow := msg.ReasoningContent().Thinking != ""
	contentNow := msg.Content().Text != "" || len(msg.ToolCalls()) > 0
	a.wasThinking = thinkingNow

	a.message = msg

	shouldInvalidate := false
	if msg.IsFinished() && (!wasFinished || msg.FinishReason() != wasFinishReason) {
		// Step boundaries must bypass streaming throttle so final text and
		// finish/cancel/error footers render immediately.
		shouldInvalidate = true
	}
	firstDelta := a.lastInvalidation.IsZero()
	if !shouldInvalidate && contentNow {
		if firstDelta {
			// First delta of this run — show immediately. Covers pure-content
			// models where wasThinking never becomes true; the previous
			// !wasThinking check failed to throttle those.
			shouldInvalidate = true
		} else if time.Since(a.lastInvalidation) >= contentStreamThrottle {
			shouldInvalidate = true
		}
	} else if !shouldInvalidate && thinkingNow {
		if firstDelta || !wasThinking {
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
func (a *AssistantMessageItem) ToggleExpanded() bool {
	a.thinkingExpanded = !a.thinkingExpanded
	// Preserve the thinking glamour cache — only truncation/boxing changes.
	a.invalidateContentCache()
	return a.thinkingExpanded
}

// SetThinkingExpanded sets the expanded state of the thinking box. Used by
// the global ctrl+o reasoning-visibility toggle.
func (a *AssistantMessageItem) SetThinkingExpanded(expanded bool) {
	if a.thinkingExpanded == expanded {
		return
	}
	a.thinkingExpanded = expanded
	// Preserve the thinking glamour cache — only truncation/boxing changes.
	a.invalidateContentCache()
}

// HandleMouseClick implements MouseClickable.
//
// Clicks on the thinking/summary regions toggle their expanded state. Any
// other click on the item (e.g. the main markdown content) is consumed
// without action so HandleDelayedClick does not fall through to
// ToggleExpanded — clicking or selecting non-thinking text must not
// unexpectedly expand the thinking block.
func (a *AssistantMessageItem) HandleMouseClick(btn ansi.MouseButton, x, y int) (bool, tea.Cmd) {
	if btn != ansi.MouseLeft {
		return false, nil
	}
	for _, region := range a.clickRegions {
		if y >= region.start && y < region.start+region.height {
			switch region.name {
			case "thinking":
				a.thinkingExpanded = !a.thinkingExpanded
				a.invalidateContentCache()
				return true, nil
			case "summary":
				a.summaryExpanded = !a.summaryExpanded
				a.invalidateSummaryCache()
				a.invalidateContentCache()
				return true, nil
			}
		}
	}
	return true, nil
}

// HandleKeyEvent implements KeyEventHandler.
func (a *AssistantMessageItem) HandleKeyEvent(key tea.KeyMsg) (bool, tea.Cmd) {
	if k := key.String(); k == "c" || k == "y" {
		text, _ := message.DisplayText(a.message.Content().Text)
		return true, common.CopyToClipboard(text, "Message copied to clipboard")
	}
	return false, nil
}
