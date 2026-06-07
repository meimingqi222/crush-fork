package chat

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/tree"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/anim"
	"github.com/charmbracelet/crush/internal/ui/list"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/x/ansi"
)

// maxAgentPromptDisplayLines is the maximum number of lines to show for a
// subagent prompt or description in the main session view before truncating.
const maxAgentPromptDisplayLines = 3

// maxCollapsedAgentNestedTools is the number of nested tool calls rendered in
// collapsed mode before the user expands the agent block.
const (
	maxCollapsedAgentNestedTools = 10
	maxAgentTaskDisplayItems     = 8
)

const (
	taskStatusCompletedWithWarnings = message.ToolResultSubtaskStatus("completed_with_warnings")
	taskStatusBlocked               = message.ToolResultSubtaskStatus("blocked")
)

// -----------------------------------------------------------------------------
// Agent Tool
// -----------------------------------------------------------------------------

// NestedToolContainer is an interface for tool items that can contain nested tool calls.
type NestedToolContainer interface {
	NestedTools() []ToolMessageItem
	SetNestedTools(tools []ToolMessageItem)
	AddNestedTool(tool ToolMessageItem)
}

// ChildSessionStatusSetter updates the transient child-session status shown on
// parent agent tool items while the delegated work is still running.
type ChildSessionStatusSetter interface {
	SetChildSessionStatus(text string, isError bool)
	ClearChildSessionStatus()
}

// AgentToolMessageItem is a message item that represents an agent tool call.
type AgentToolMessageItem struct {
	*baseToolMessageItem

	nestedTools    []ToolMessageItem
	nestedExpanded bool

	childStatusText    string
	childStatusIsError bool

	// hasTaskNodes is true when TaskNodeItems have been injected below
	// this item, so the inline task list only renders the summary line.
	hasTaskNodes bool

	// Cached parsed data to avoid repeated json.Unmarshal during spinning renders.
	cachedParams     *agent.AgentParams
	cachedParamInput string
	cachedTasks      []agentTaskRenderEntry

	// Cached task status parsing to avoid repeated regexp during renders.
	cachedTaskStatuses      map[string]message.ToolResultSubtaskStatus
	cachedTaskStatusContent string
}

var (
	_ ToolMessageItem          = (*AgentToolMessageItem)(nil)
	_ NestedToolContainer      = (*AgentToolMessageItem)(nil)
	_ ChildSessionStatusSetter = (*AgentToolMessageItem)(nil)
)

// NewAgentToolMessageItem creates a new [AgentToolMessageItem].
func NewAgentToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
) *AgentToolMessageItem {
	t := &AgentToolMessageItem{}
	t.baseToolMessageItem = newBaseToolMessageItem(sty, toolCall, result, &AgentToolRenderContext{agent: t}, canceled)
	// For the agent tool we keep spinning until the tool call is finished.
	t.spinningFunc = func(state SpinningState) bool {
		return !state.HasResult() && !state.IsCanceled()
	}
	return t
}

// Animate progresses the message animation if it should be spinning.
// Kept for backward compatibility with any residual StepMsg in flight.
// Nested tools are animated independently by the global ticker.
func (a *AgentToolMessageItem) Animate(msg anim.StepMsg) tea.Cmd {
	if a.result != nil || a.Status() == ToolStatusCanceled {
		return nil
	}
	if msg.ID == a.ID() {
		return a.anim.Animate(msg)
	}
	return nil
}

// TickAnimation advances the animation by one frame.
// Nested tools are ticked independently by the global ticker (they are
// registered separately in the Chat.animating map), so this method only
// ticks the agent item's own animation.
func (a *AgentToolMessageItem) TickAnimation() {
	if a.result != nil || a.Status() == ToolStatusCanceled {
		return
	}
	if a.isSpinning() {
		a.anim.Tick()
	}
}

// IsAnimating reports whether the agent tool message is currently spinning.
// This checks both the agent item itself and its nested tools, since nested
// tools are registered independently in the animation set.
func (a *AgentToolMessageItem) IsAnimating() bool {
	if a.result != nil || a.Status() == ToolStatusCanceled {
		return false
	}
	if a.isSpinning() {
		return true
	}
	for _, nestedTool := range a.nestedTools {
		if s, ok := nestedTool.(Animatable); ok {
			if s.IsAnimating() {
				return true
			}
		}
	}
	return false
}

// NestedTools returns the nested tools.
func (a *AgentToolMessageItem) NestedTools() []ToolMessageItem {
	return a.nestedTools
}

// SetNestedTools sets the nested tools.
func (a *AgentToolMessageItem) SetNestedTools(tools []ToolMessageItem) {
	a.nestedTools = tools
	a.invalidateBodyCache()
	a.clearCache()
}

// AddNestedTool adds a nested tool.
func (a *AgentToolMessageItem) AddNestedTool(tool ToolMessageItem) {
	// Mark nested tools as simple (compact) rendering.
	if s, ok := tool.(Compactable); ok {
		s.SetCompact(true)
	}
	a.nestedTools = append(a.nestedTools, tool)
	a.invalidateBodyCache()
	a.clearCache()
}

// ToggleExpanded toggles the nested tool list expansion state.
func (a *AgentToolMessageItem) ToggleExpanded() bool {
	a.nestedExpanded = !a.nestedExpanded
	a.expandedContent = a.nestedExpanded
	a.invalidateBodyCache()
	a.clearCache()
	return a.nestedExpanded
}

// SetChildSessionStatus stores transient child-session status text.
func (a *AgentToolMessageItem) SetChildSessionStatus(text string, isError bool) {
	if a.childStatusText == text && a.childStatusIsError == isError {
		return
	}
	a.childStatusText = text
	a.childStatusIsError = isError
	a.invalidateBodyCache()
	a.clearCache()
}

// SetHasTaskNodes marks this item as having TaskNodeItem children in the list.
func (a *AgentToolMessageItem) SetHasTaskNodes(v bool) {
	if a.hasTaskNodes == v {
		return
	}
	a.hasTaskNodes = v
	a.invalidateBodyCache()
	a.clearCache()
}

// ClearChildSessionStatus removes transient child-session status text.
func (a *AgentToolMessageItem) ClearChildSessionStatus() {
	if a.childStatusText == "" && !a.childStatusIsError {
		return
	}
	a.childStatusText = ""
	a.childStatusIsError = false
	a.invalidateBodyCache()
	a.clearCache()
}

// SetToolCall overrides the base SetToolCall to invalidate the parsed-param cache
// when the tool call input changes.
func (a *AgentToolMessageItem) SetToolCall(tc message.ToolCall) {
	if tc.Input != a.cachedParamInput {
		a.cachedParams = nil
		a.cachedParamInput = tc.Input
		a.cachedTasks = nil
	}
	a.baseToolMessageItem.SetToolCall(tc)
}

// SetResult overrides the base SetResult to invalidate the cached task statuses
// when the result changes.
func (a *AgentToolMessageItem) SetResult(res *message.ToolResult) {
	a.cachedTaskStatuses = nil
	a.cachedTaskStatusContent = ""
	a.baseToolMessageItem.SetResult(res)
}

// getCachedAgentParams lazily parses and caches the agent params from the tool
// call input. During spinning renders (20fps), this avoids calling
// json.Unmarshal on every frame; parsing only happens once until the input changes.
func (a *AgentToolMessageItem) getCachedAgentParams(input string) (agent.AgentParams, []agentTaskRenderEntry, bool) {
	if a.cachedParams != nil && a.cachedParamInput == input {
		return *a.cachedParams, a.cachedTasks, true
	}
	var params agent.AgentParams
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		return params, nil, false
	}
	a.cachedParams = &params
	a.cachedParamInput = input
	a.cachedTasks = collectAgentTaskEntries(params)
	return params, a.cachedTasks, true
}

// getCachedTaskStatuses lazily parses and caches per-task statuses from the
// tool result content using regexp. This avoids re-running the regex on every
// render frame once the result is available.
func (a *AgentToolMessageItem) getCachedTaskStatuses(result *message.ToolResult) map[string]message.ToolResultSubtaskStatus {
	if result == nil {
		return make(map[string]message.ToolResultSubtaskStatus)
	}
	if a.cachedTaskStatuses != nil && a.cachedTaskStatusContent == result.Content {
		return a.cachedTaskStatuses
	}
	statuses := ParseTaskStatusesFromAgentResult(result)
	a.cachedTaskStatuses = statuses
	a.cachedTaskStatusContent = result.Content
	return statuses
}

// AgentToolRenderContext renders agent tool messages.
type AgentToolRenderContext struct {
	agent *AgentToolMessageItem
}

type agentTaskRenderEntry struct {
	id           string
	description  string
	prompt       string
	subagentType string
}

// RenderTool implements the [ToolRenderer] interface.
func (r *AgentToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	cappedWidth := cappedMessageWidth(width)
	if opts.IsPending() && len(r.agent.nestedTools) == 0 && r.agent.childStatusText == "" {
		return pendingTool(sty, "Agent", opts.Anim, opts.Compact)
	}

	// Use cached parsed params/tasks to avoid json.Unmarshal on every spinning frame.
	params, tasks, ok := r.agent.getCachedAgentParams(opts.ToolCall.Input)
	if !ok {
		return ""
	}

	firstTask := agentTaskRenderEntry{}
	if len(tasks) > 0 {
		firstTask = tasks[0]
	}
	description := strings.TrimSpace(firstTask.description)
	prompt := strings.TrimSpace(firstTask.prompt)
	if description == "" {
		description = prompt
	}
	description = strings.ReplaceAll(description, "\n", " ")
	prompt = strings.ReplaceAll(prompt, "\n", " ")
	subagentType := titleCase(firstTask.subagentType)
	if subagentType == "" {
		subagentType = titleCase(config.CanonicalSubagentID(params.SubagentType))
	}

	header := toolHeader(sty, opts.Status, "Agent", cappedWidth, opts.Compact)
	if opts.Compact {
		return header
	}

	// Build the subagent tag and description.
	taskTag := sty.Tool.AgentTaskTag.Render(subagentType)
	taskTagWidth := lipgloss.Width(taskTag)

	// Calculate remaining width for the title.
	remainingWidth := min(cappedWidth-taskTagWidth-3, maxTextWidth-taskTagWidth-3) // -3 for spacing

	descriptionText := sty.Tool.AgentPrompt.Width(remainingWidth).Render(truncateAgentPrompt(description, remainingWidth))
	headerParts := []string{
		header,
		"",
		lipgloss.JoinHorizontal(
			lipgloss.Left,
			taskTag,
			" ",
			descriptionText,
		),
	}
	if prompt != "" && prompt != description {
		promptTag := sty.Tool.AgenticFetchPromptTag.Render("Prompt")
		promptWidth := min(cappedWidth-lipgloss.Width(promptTag)-3, maxTextWidth-lipgloss.Width(promptTag)-3)
		promptText := sty.Tool.AgentPrompt.Width(promptWidth).Render(truncateAgentPrompt(prompt, promptWidth))
		headerParts = append(headerParts, lipgloss.JoinHorizontal(lipgloss.Left, promptTag, " ", promptText))
	}

	header = lipgloss.JoinVertical(lipgloss.Left, headerParts...)
	header = renderAgentTaskList(sty, header, tasks, remainingWidth, opts, r.agent.hasTaskNodes, r.agent)

	visibleNestedTools, hiddenNestedTools := agentNestedToolWindow(r.agent.nestedTools, r.agent.nestedExpanded)
	header = renderAgentHeaderWithToggle(sty, header, remainingWidth, r.agent.nestedExpanded, len(r.agent.nestedTools), hiddenNestedTools)

	// Build tree with nested tool calls.
	childTools := tree.Root(header)

	for _, nestedTool := range visibleNestedTools {
		childView := nestedTool.Render(remainingWidth)
		childTools.Child(childView)
	}

	// Build parts.
	var parts []string
	parts = append(parts, childTools.Enumerator(roundedEnumerator(2, taskTagWidth-5)).String())

	if !opts.HasResult() {
		if status := renderChildSessionStatus(sty, remainingWidth, r.agent.childStatusText, r.agent.childStatusIsError); status != "" {
			parts = append(parts, "", status)
		}
	}

	result := lipgloss.JoinVertical(lipgloss.Left, parts...)

	// Add body content when completed.
	if opts.HasResult() && opts.Result.Content != "" {
		body := toolOutputMarkdownContent(sty, opts.Result.Content, cappedWidth-toolBodyLeftPaddingTotal, opts.ExpandedContent)
		return joinToolParts(result, body)
	}

	return result
}

// truncateAgentPrompt truncates a single-line string to fit within
// maxAgentPromptDisplayLines lines at the given column width. If the string is
// truncated, "…" is appended to the last visible character.
func truncateAgentPrompt(text string, lineWidth int) string {
	if lineWidth <= 0 {
		return text
	}
	maxRunes := lineWidth * maxAgentPromptDisplayLines
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	return string(runes[:maxRunes-1]) + "…"
}

func titleCase(value string) string {
	if value == "" {
		return ""
	}
	runes := []rune(value)
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

// -----------------------------------------------------------------------------
// Agentic Fetch Tool
// -----------------------------------------------------------------------------

// AgenticFetchToolMessageItem is a message item that represents an agentic fetch tool call.
type AgenticFetchToolMessageItem struct {
	*baseToolMessageItem

	nestedTools    []ToolMessageItem
	nestedExpanded bool

	childStatusText    string
	childStatusIsError bool

	// Cached parsed params to avoid repeated json.Unmarshal during spinning renders.
	cachedParams     *agenticFetchParams
	cachedParamInput string
}

var (
	_ ToolMessageItem          = (*AgenticFetchToolMessageItem)(nil)
	_ NestedToolContainer      = (*AgenticFetchToolMessageItem)(nil)
	_ ChildSessionStatusSetter = (*AgenticFetchToolMessageItem)(nil)
)

// NewAgenticFetchToolMessageItem creates a new [AgenticFetchToolMessageItem].
func NewAgenticFetchToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
) *AgenticFetchToolMessageItem {
	t := &AgenticFetchToolMessageItem{}
	t.baseToolMessageItem = newBaseToolMessageItem(sty, toolCall, result, &AgenticFetchToolRenderContext{fetch: t}, canceled)
	// For the agentic fetch tool we keep spinning until the tool call is finished.
	t.spinningFunc = func(state SpinningState) bool {
		return !state.HasResult() && !state.IsCanceled()
	}
	return t
}

// NestedTools returns the nested tools.
func (a *AgenticFetchToolMessageItem) NestedTools() []ToolMessageItem {
	return a.nestedTools
}

// SetNestedTools sets the nested tools.
func (a *AgenticFetchToolMessageItem) SetNestedTools(tools []ToolMessageItem) {
	a.nestedTools = tools
	a.invalidateBodyCache()
	a.clearCache()
}

// AddNestedTool adds a nested tool.
func (a *AgenticFetchToolMessageItem) AddNestedTool(tool ToolMessageItem) {
	// Mark nested tools as simple (compact) rendering.
	if s, ok := tool.(Compactable); ok {
		s.SetCompact(true)
	}
	a.nestedTools = append(a.nestedTools, tool)
	a.invalidateBodyCache()
	a.clearCache()
}

// ToggleExpanded toggles the nested tool list expansion state.
func (a *AgenticFetchToolMessageItem) ToggleExpanded() bool {
	a.nestedExpanded = !a.nestedExpanded
	a.expandedContent = a.nestedExpanded
	a.invalidateBodyCache()
	a.clearCache()
	return a.nestedExpanded
}

// SetChildSessionStatus stores transient child-session status text.
func (a *AgenticFetchToolMessageItem) SetChildSessionStatus(text string, isError bool) {
	if a.childStatusText == text && a.childStatusIsError == isError {
		return
	}
	a.childStatusText = text
	a.childStatusIsError = isError
	a.invalidateBodyCache()
	a.clearCache()
}

// ClearChildSessionStatus removes transient child-session status text.
func (a *AgenticFetchToolMessageItem) ClearChildSessionStatus() {
	if a.childStatusText == "" && !a.childStatusIsError {
		return
	}
	a.childStatusText = ""
	a.childStatusIsError = false
	a.invalidateBodyCache()
	a.clearCache()
}

// SetToolCall overrides the base SetToolCall to invalidate the parsed-param cache
// when the tool call input changes.
func (a *AgenticFetchToolMessageItem) SetToolCall(tc message.ToolCall) {
	if tc.Input != a.cachedParamInput {
		a.cachedParams = nil
		a.cachedParamInput = tc.Input
	}
	a.baseToolMessageItem.SetToolCall(tc)
}

// getCachedFetchParams lazily parses and caches the fetch params from the tool
// call input. During spinning renders (20fps), this avoids calling
// json.Unmarshal on every frame; parsing only happens once until the input changes.
func (a *AgenticFetchToolMessageItem) getCachedFetchParams(input string) (agenticFetchParams, bool) {
	if a.cachedParams != nil && a.cachedParamInput == input {
		return *a.cachedParams, true
	}
	var params agenticFetchParams
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		return params, false
	}
	a.cachedParams = &params
	a.cachedParamInput = input
	return params, true
}

// AgenticFetchToolRenderContext renders agentic fetch tool messages.
type AgenticFetchToolRenderContext struct {
	fetch *AgenticFetchToolMessageItem
}

// agenticFetchParams matches tools.AgenticFetchParams.
type agenticFetchParams struct {
	URL    string `json:"url,omitempty"`
	Prompt string `json:"prompt"`
}

// RenderTool implements the [ToolRenderer] interface.
func (r *AgenticFetchToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	cappedWidth := cappedMessageWidth(width)
	if opts.IsPending() && len(r.fetch.nestedTools) == 0 && r.fetch.childStatusText == "" {
		return pendingTool(sty, "Agentic Fetch", opts.Anim, opts.Compact)
	}

	// Use cached parsed params to avoid json.Unmarshal on every spinning frame.
	params, ok := r.fetch.getCachedFetchParams(opts.ToolCall.Input)
	if !ok {
		return ""
	}

	prompt := params.Prompt
	prompt = strings.ReplaceAll(prompt, "\n", " ")

	// Build header with optional URL param.
	var toolParams []string
	if params.URL != "" {
		toolParams = append(toolParams, params.URL)
	}

	header := toolHeader(sty, opts.Status, "Agentic Fetch", cappedWidth, opts.Compact, toolParams...)
	if opts.Compact {
		return header
	}

	// Build the prompt tag.
	promptTag := sty.Tool.AgenticFetchPromptTag.Render("Prompt")
	promptTagWidth := lipgloss.Width(promptTag)

	// Calculate remaining width for prompt text.
	remainingWidth := min(cappedWidth-promptTagWidth-3, maxTextWidth-promptTagWidth-3) // -3 for spacing

	promptText := sty.Tool.AgentPrompt.Width(remainingWidth).Render(prompt)

	header = lipgloss.JoinVertical(
		lipgloss.Left,
		header,
		"",
		lipgloss.JoinHorizontal(
			lipgloss.Left,
			promptTag,
			" ",
			promptText,
		),
	)

	visibleNestedTools, hiddenNestedTools := agentNestedToolWindow(r.fetch.nestedTools, r.fetch.nestedExpanded)
	header = renderAgentHeaderWithToggle(sty, header, remainingWidth, r.fetch.nestedExpanded, len(r.fetch.nestedTools), hiddenNestedTools)

	// Build tree with nested tool calls.
	childTools := tree.Root(header)

	for _, nestedTool := range visibleNestedTools {
		childView := nestedTool.Render(remainingWidth)
		childTools.Child(childView)
	}

	// Build parts.
	var parts []string
	parts = append(parts, childTools.Enumerator(roundedEnumerator(2, promptTagWidth-5)).String())

	if !opts.HasResult() {
		if status := renderChildSessionStatus(sty, remainingWidth, r.fetch.childStatusText, r.fetch.childStatusIsError); status != "" {
			parts = append(parts, "", status)
		}
	}

	result := lipgloss.JoinVertical(lipgloss.Left, parts...)

	// Add body content when completed.
	if opts.HasResult() && opts.Result.Content != "" {
		body := toolOutputMarkdownContent(sty, opts.Result.Content, cappedWidth-toolBodyLeftPaddingTotal, opts.ExpandedContent)
		return joinToolParts(result, body)
	}

	return result
}

func collectAgentTaskEntries(params agent.AgentParams) []agentTaskRenderEntry {
	tasks := make([]agentTaskRenderEntry, 0, max(1, len(params.Tasks)))
	if len(params.Tasks) == 0 {
		tasks = append(tasks, agentTaskRenderEntry{
			id:           "",
			description:  strings.TrimSpace(params.Description),
			prompt:       strings.TrimSpace(params.Prompt),
			subagentType: config.CanonicalSubagentID(params.SubagentType),
		})
		return tasks
	}

	for _, task := range params.Tasks {
		tasks = append(tasks, agentTaskRenderEntry{
			id:           strings.TrimSpace(task.Name),
			description:  strings.TrimSpace(task.Description),
			prompt:       strings.TrimSpace(task.Assignment),
			subagentType: config.CanonicalSubagentID(task.SubagentType),
		})
	}
	return tasks
}

func renderAgentTaskList(sty *styles.Styles, header string, tasks []agentTaskRenderEntry, width int, opts *ToolRenderOpts, summaryOnly bool, agentItem *AgentToolMessageItem) string {
	if len(tasks) <= 1 || width <= 0 {
		return header
	}

	taskTag := sty.Tool.AgenticFetchPromptTag.Render("Tasks")
	availableWidth := max(0, width-lipgloss.Width(taskTag)-3)
	if availableWidth == 0 {
		return lipgloss.JoinVertical(lipgloss.Left, header, taskTag)
	}

	// Use cached task statuses to avoid repeated regexp execution during spinning renders.
	var statusesByID map[string]message.ToolResultSubtaskStatus
	if agentItem != nil {
		statusesByID = agentItem.getCachedTaskStatuses(opts.Result)
	} else {
		statusesByID = parseTaskStatusesFromAgentResult(opts)
	}
	completed, failed, canceled, blocked, inProgress, pending := summarizeTaskStatusCounts(tasks, statusesByID)
	summaryText := fmt.Sprintf("done %d · running %d · pending %d", completed, inProgress, pending)
	if failed > 0 {
		summaryText += fmt.Sprintf(" · failed %d", failed)
	}
	if canceled > 0 {
		summaryText += fmt.Sprintf(" · canceled %d", canceled)
	}
	if blocked > 0 {
		summaryText += fmt.Sprintf(" · blocked %d", blocked)
	}
	summaryLine := lipgloss.JoinHorizontal(
		lipgloss.Left,
		taskTag,
		" ",
		sty.Tool.AgentPrompt.Width(availableWidth).Render(summaryText),
	)

	// When TaskNodeItems are present below this item, only show the summary
	// to avoid duplicating the per-task lines.
	if summaryOnly {
		return lipgloss.JoinVertical(lipgloss.Left, header, summaryLine)
	}

	visibleCount := len(tasks)
	if visibleCount > maxAgentTaskDisplayItems {
		visibleCount = maxAgentTaskDisplayItems
	}

	lines := make([]string, 0, visibleCount+1)
	for i := range visibleCount {
		entry := tasks[i]
		label := strings.TrimSpace(entry.description)
		if label == "" {
			label = strings.TrimSpace(entry.prompt)
		}
		if label == "" {
			label = fmt.Sprintf("Task %d", i+1)
		}
		subagentLabel := titleCase(entry.subagentType)
		if subagentLabel != "" {
			label = fmt.Sprintf("[%s] %s", subagentLabel, label)
		}

		status := taskStatusIcon(sty, statusesByID[entry.id], opts, entry.id)
		lineText := strings.ReplaceAll(label, "\n", " ")
		lines = append(lines, fmt.Sprintf("%s %s", status, lineText))
	}
	if len(tasks) > visibleCount {
		lines = append(lines, fmt.Sprintf("… +%d more", len(tasks)-visibleCount))
	}

	taskText := sty.Tool.AgentPrompt.Width(availableWidth).Render(strings.Join(lines, "\n"))
	return lipgloss.JoinVertical(lipgloss.Left, header, summaryLine, taskText)
}

var taskStatusLinePattern = regexp.MustCompile(`(?m)^-\s+([^:]+):\s*(completed|completed_with_warnings|failed|canceled|blocked)\s*$`)

func parseTaskStatusesFromAgentResult(opts *ToolRenderOpts) map[string]message.ToolResultSubtaskStatus {
	statuses := make(map[string]message.ToolResultSubtaskStatus)
	if opts == nil || opts.Result == nil {
		return statuses
	}
	return ParseTaskStatusesFromAgentResult(opts.Result)
}

// ParseTaskStatusesFromAgentResult extracts per-task completion statuses from
// an agent tool result's content (lines like "- task_id: completed").
func ParseTaskStatusesFromAgentResult(result *message.ToolResult) map[string]message.ToolResultSubtaskStatus {
	statuses := make(map[string]message.ToolResultSubtaskStatus)
	if result == nil {
		return statuses
	}
	content := result.Content
	if strings.TrimSpace(content) == "" {
		return statuses
	}
	matches := taskStatusLinePattern.FindAllStringSubmatch(content, -1)
	for _, match := range matches {
		if len(match) < 3 {
			continue
		}
		taskID := strings.TrimSpace(match[1])
		if taskID == "" {
			continue
		}
		switch strings.TrimSpace(match[2]) {
		case string(message.ToolResultSubtaskStatusCompleted):
			statuses[taskID] = message.ToolResultSubtaskStatusCompleted
		case string(taskStatusCompletedWithWarnings):
			statuses[taskID] = taskStatusCompletedWithWarnings
		case string(message.ToolResultSubtaskStatusFailed):
			statuses[taskID] = message.ToolResultSubtaskStatusFailed
		case string(message.ToolResultSubtaskStatusCanceled):
			statuses[taskID] = message.ToolResultSubtaskStatusCanceled
		case string(taskStatusBlocked):
			statuses[taskID] = taskStatusBlocked
		}
	}
	return statuses
}

func summarizeTaskStatusCounts(tasks []agentTaskRenderEntry, statuses map[string]message.ToolResultSubtaskStatus) (completed, failed, canceled, blocked, inProgress, pending int) {
	completedSet := make(map[string]struct{}, len(tasks))
	for _, task := range tasks {
		if taskStatusReleasesDependents(statuses[task.id]) {
			completedSet[task.id] = struct{}{}
		}
	}
	for _, task := range tasks {
		status := statuses[task.id]
		switch status {
		case message.ToolResultSubtaskStatusCompleted, taskStatusCompletedWithWarnings:
			completed++
		case message.ToolResultSubtaskStatusFailed:
			failed++
		case message.ToolResultSubtaskStatusCanceled:
			canceled++
		case taskStatusBlocked:
			blocked++
		default:
			inProgress++
		}
	}
	return completed, failed, canceled, blocked, inProgress, pending
}

func taskStatusReleasesDependents(status message.ToolResultSubtaskStatus) bool {
	switch status {
	case message.ToolResultSubtaskStatusCompleted, taskStatusCompletedWithWarnings:
		return true
	default:
		return false
	}
}

func taskStatusIcon(sty *styles.Styles, status message.ToolResultSubtaskStatus, opts *ToolRenderOpts, taskID string) string {
	switch status {
	case message.ToolResultSubtaskStatusCompleted, taskStatusCompletedWithWarnings:
		return sty.Tool.IconSuccess.String()
	case message.ToolResultSubtaskStatusFailed, taskStatusBlocked:
		return sty.Tool.IconError.String()
	case message.ToolResultSubtaskStatusCanceled:
		return sty.Tool.IconCancelled.String()
	default:
		if opts != nil && !opts.HasResult() && taskID != "" {
			return sty.Tool.IconPending.String()
		}
		return sty.Tool.IconPending.String()
	}
}

func renderChildSessionStatus(sty *styles.Styles, width int, text string, isError bool) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\n", " "))
	if text == "" || width <= 0 {
		return ""
	}

	statusTag := sty.Tool.AgenticFetchPromptTag.Render("Status")
	availableWidth := max(0, width-lipgloss.Width(statusTag)-3)
	if availableWidth == 0 {
		return statusTag
	}

	if isError {
		errTag := sty.Tool.ErrorTag.Render("ERROR")
		errText := sty.Tool.ErrorMessage.Render(
			ansi.Truncate(text, max(0, availableWidth-lipgloss.Width(errTag)-1), "…"),
		)
		return lipgloss.JoinHorizontal(
			lipgloss.Left,
			statusTag,
			" ",
			fmt.Sprintf("%s %s", errTag, errText),
		)
	}

	return lipgloss.JoinHorizontal(
		lipgloss.Left,
		statusTag,
		" ",
		sty.Tool.StateWaiting.Render(ansi.Truncate(text, availableWidth, "…")),
	)
}

func agentNestedToolWindow(nestedTools []ToolMessageItem, expanded bool) ([]ToolMessageItem, int) {
	if expanded || len(nestedTools) <= maxCollapsedAgentNestedTools {
		return nestedTools, 0
	}

	visible := maxCollapsedAgentNestedTools
	return nestedTools[:visible], len(nestedTools) - visible
}

func renderAgentHeaderWithToggle(sty *styles.Styles, header string, width int, expanded bool, totalNested, hiddenNested int) string {
	if totalNested <= maxCollapsedAgentNestedTools {
		return header
	}

	var toggleLabel string
	if expanded {
		toggleLabel = "▾ Collapse"
	} else {
		toggleLabel = fmt.Sprintf("▸ Expand (%d more)", hiddenNested)
	}

	toggleTag := sty.Tool.AgenticFetchPromptTag.Render(toggleLabel)
	lines := strings.Split(header, "\n")
	if len(lines) == 0 {
		return header
	}

	firstLineWidth := ansi.StringWidth(lines[0])
	if width <= 0 {
		lines[0] = lipgloss.JoinHorizontal(lipgloss.Left, lines[0], " ", toggleTag)
		return strings.Join(lines, "\n")
	}

	availableWidth := max(0, width-firstLineWidth-1)
	if availableWidth == 0 {
		toggleTag = ansi.Truncate(toggleTag, width, "…")
		return lipgloss.JoinVertical(lipgloss.Left, header, toggleTag)
	}

	toggleTag = ansi.Truncate(toggleTag, availableWidth, "…")
	lines[0] = lipgloss.JoinHorizontal(lipgloss.Left, lines[0], " ", toggleTag)
	return strings.Join(lines, "\n")
}

func TaskNodeItemID(parentToolCallID, taskID string) string {
	return fmt.Sprintf("%s::task-node::%s", parentToolCallID, taskID)
}

type TaskNodeItem struct {
	*cachedMessageItem
	id               string
	parentToolCallID string
	childSessionID   string
	taskRef          string
	taskID           string
	description      string
	prompt           string
	subagentType     string
	sty              *styles.Styles
	focused          bool

	childStatusText    string
	childStatusIsError bool
	completionStatus   message.ToolResultSubtaskStatus

	// nestedTools holds the compact tool call summary from the child session.
	nestedTools    []ToolMessageItem
	nestedExpanded bool
}

var (
	_ ChildSessionStatusSetter = (*TaskNodeItem)(nil)
	_ NestedToolContainer      = (*TaskNodeItem)(nil)
	_ Expandable               = (*TaskNodeItem)(nil)
	_ list.MouseClickable      = (*TaskNodeItem)(nil)
)

func NewTaskNodeItem(sty *styles.Styles, parentToolCallID, taskID, description, prompt, subagentType, childSessionID string) *TaskNodeItem {
	return &TaskNodeItem{
		cachedMessageItem: &cachedMessageItem{},
		id:                TaskNodeItemID(parentToolCallID, taskID),
		parentToolCallID:  parentToolCallID,
		childSessionID:    childSessionID,
		taskID:            taskID,
		description:       description,
		prompt:            prompt,
		subagentType:      subagentType,
		sty:               sty,
	}
}

func (t *TaskNodeItem) ID() string { return t.id }

func (t *TaskNodeItem) ParentToolCallID() string { return t.parentToolCallID }

func (t *TaskNodeItem) ChildSessionID() string { return t.childSessionID }

func (t *TaskNodeItem) TaskRef() string { return t.taskRef }

func (t *TaskNodeItem) SetTaskRef(taskRef string) {
	taskRef = strings.TrimSpace(strings.TrimPrefix(taskRef, "subtask://"))
	if t.taskRef == taskRef {
		return
	}
	t.taskRef = taskRef
	t.clearCache()
}

// SetChildSessionStatus stores transient child-session status text for live display.
func (t *TaskNodeItem) SetChildSessionStatus(text string, isError bool) {
	if t.childStatusText == text && t.childStatusIsError == isError {
		return
	}
	t.childStatusText = text
	t.childStatusIsError = isError
	t.clearCache()
}

// ClearChildSessionStatus removes transient child-session status text.
func (t *TaskNodeItem) ClearChildSessionStatus() {
	if t.childStatusText == "" && !t.childStatusIsError {
		return
	}
	t.childStatusText = ""
	t.childStatusIsError = false
	t.clearCache()
}

// SetCompletionStatus stores the final completion status for this task node.
func (t *TaskNodeItem) SetCompletionStatus(status message.ToolResultSubtaskStatus) {
	if t.completionStatus == status {
		return
	}
	t.completionStatus = status
	t.clearCache()
}

// CompletionStatus returns the final completion status for this task node.
func (t *TaskNodeItem) CompletionStatus() message.ToolResultSubtaskStatus {
	return t.completionStatus
}

// NestedTools returns the nested tool calls from the child session.
func (t *TaskNodeItem) NestedTools() []ToolMessageItem {
	return t.nestedTools
}

// SetNestedTools sets the nested tool calls.
func (t *TaskNodeItem) SetNestedTools(tools []ToolMessageItem) {
	t.nestedTools = tools
	t.clearCache()
}

// AddNestedTool adds a nested tool call.
func (t *TaskNodeItem) AddNestedTool(tool ToolMessageItem) {
	if s, ok := tool.(Compactable); ok {
		s.SetCompact(true)
	}
	t.nestedTools = append(t.nestedTools, tool)
	t.clearCache()
}

// ToggleExpanded toggles the nested tool list expansion state.
func (t *TaskNodeItem) ToggleExpanded() bool {
	t.nestedExpanded = !t.nestedExpanded
	t.clearCache()
	return t.nestedExpanded
}

// HandleMouseClick implements MouseClickable.
// Returns false to let HandleDelayedClick fall through to ToggleExpanded.
func (t *TaskNodeItem) HandleMouseClick(btn ansi.MouseButton, x, y int) bool {
	return false
}

func (t *TaskNodeItem) SetFocused(focused bool) {
	if t.focused == focused {
		return
	}
	t.focused = focused
	t.clearCache()
}

func (t *TaskNodeItem) RawRender(width int) string {
	innerWidth := max(0, width-MessageLeftPaddingTotal)
	content, _, ok := t.getCachedRender(innerWidth)
	if !ok {
		content = t.renderContent(innerWidth)
		t.setCachedRender(content, innerWidth, lipgloss.Height(content))
	}
	return content
}

func (t *TaskNodeItem) Render(width int) string {
	var prefix string
	if t.focused {
		prefix = t.sty.Chat.Message.ToolCallFocused.Render()
	} else {
		prefix = t.sty.Chat.Message.ToolCallBlurred.Render()
	}
	return applyLinePrefix(t.RawRender(width), prefix)
}

func (t *TaskNodeItem) renderContent(width int) string {
	label := strings.ReplaceAll(strings.TrimSpace(t.description), "\n", " ")
	if label == "" {
		label = strings.ReplaceAll(strings.TrimSpace(t.prompt), "\n", " ")
	}
	if label == "" {
		label = t.taskID
	}

	var statusIcon string
	switch {
	case t.childStatusIsError:
		statusIcon = t.sty.Tool.IconError.String()
	case t.completionStatus == message.ToolResultSubtaskStatusCompleted, t.completionStatus == taskStatusCompletedWithWarnings:
		statusIcon = t.sty.Tool.IconSuccess.String()
	case t.completionStatus == message.ToolResultSubtaskStatusFailed, t.completionStatus == taskStatusBlocked:
		statusIcon = t.sty.Tool.IconError.String()
	case t.completionStatus == message.ToolResultSubtaskStatusCanceled:
		statusIcon = t.sty.Tool.IconCancelled.String()
	case t.childStatusText != "":
		statusIcon = t.sty.Tool.IconPending.String()
	default:
		statusIcon = t.sty.Tool.IconPending.String()
	}

	arrow := " ↳ "
	subagentLabel := titleCase(t.subagentType)
	tag := ""
	if subagentLabel != "" {
		tag = t.sty.Tool.AgentTaskTag.Render(subagentLabel) + " "
	}
	// statusIcon(1) + arrow(3) + tag
	indentWidth := 1 + ansi.StringWidth(arrow) + lipgloss.Width(tag)
	availWidth := max(0, width-indentWidth)
	labelText := t.sty.Tool.AgentPrompt.Width(availWidth).Render(
		ansi.Truncate(label, availWidth, "…"),
	)
	headerLine := lipgloss.JoinHorizontal(lipgloss.Left, statusIcon, arrow, tag, labelText)

	// If there are no nested tools, return the single header line.
	if len(t.nestedTools) == 0 {
		return headerLine
	}

	lines := []string{headerLine}

	// Show a collapsible operations summary.
	const nestedIndent = 6
	nestedWidth := max(0, width-nestedIndent)
	indent := strings.Repeat(" ", nestedIndent)

	if t.nestedExpanded {
		toggle := t.sty.Tool.AgenticFetchPromptTag.Render(
			fmt.Sprintf("▾ %d operations", len(t.nestedTools)),
		)
		lines = append(lines, indent+toggle)
		visible, _ := agentNestedToolWindow(t.nestedTools, true)
		for _, tool := range visible {
			toolView := tool.Render(nestedWidth)
			for _, ln := range strings.Split(toolView, "\n") {
				lines = append(lines, indent+ln)
			}
		}
	} else {
		toggle := t.sty.Tool.AgenticFetchPromptTag.Render(
			fmt.Sprintf("▸ %d operations", len(t.nestedTools)),
		)
		lines = append(lines, indent+toggle)
	}

	return strings.Join(lines, "\n")
}
