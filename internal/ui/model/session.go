package model

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/agent"
	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/diff"
	"github.com/charmbracelet/crush/internal/fsext"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/ui/chat"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/crush/internal/ui/util"
	"github.com/charmbracelet/x/ansi"
)

// loadSessionMsg is a message indicating that a session and its files have
// been loaded.
type loadSessionMsg struct {
	session             *session.Session
	messages            []message.Message
	files               []SessionFile
	readFiles           []string
	selectedMessageID   string
	childSessionInfo    map[string]childSessionInfo
	totalMessageCount   int64
	skippedMessageCount int64
	goalPausedNotice    string
}

// forkSessionResultMsg is sent when a session has been forked.
type forkSessionResultMsg struct {
	sessionID string
	err       error
}

// loadMoreMessagesMsg is sent when older messages have been loaded from the
// database to prepend to the chat list.
type loadMoreMessagesMsg struct {
	messages []message.Message
	count    int64 // Number of messages that were skipped before this batch
}

// loadMoreMessagesCount is the number of older messages to load per batch
// when the user scrolls to the top.
const loadMoreMessagesCount = 100

// lspFilePaths returns deduplicated file paths from both modified and read
// files for starting LSP servers.
func (msg loadSessionMsg) lspFilePaths() []string {
	seen := make(map[string]struct{}, len(msg.files)+len(msg.readFiles))
	paths := make([]string, 0, len(msg.files)+len(msg.readFiles))
	for _, f := range msg.files {
		p := f.LatestVersion.Path
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		paths = append(paths, p)
	}
	for _, p := range msg.readFiles {
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		paths = append(paths, p)
	}
	return paths
}

// SessionFile tracks the first and latest versions of a file in a session,
// along with the total additions and deletions.
type SessionFile struct {
	FirstVersion  history.File
	LatestVersion history.File
	Additions     int
	Deletions     int
}

var modifiedFilesRootMarkers = []string{
	".git",
	"go.mod",
	"package.json",
	"Cargo.toml",
	"pyproject.toml",
	"AGENTS.md",
}

// loadSession loads the session along with its associated files and computes
// the diff statistics (additions and deletions) for each file in the session.
// It returns a tea.Cmd that, when executed, fetches the session data and
// returns a sessionFilesLoadedMsg containing the processed session files.
func (m *UI) loadSession(sessionID string) tea.Cmd {
	return m.loadSessionWithSelection(sessionID, "")
}

// initialMessageLoadLimit is the maximum number of messages to load initially
// when switching sessions. If the session has more messages, they can be
// loaded on demand when the user scrolls to the top.
const initialMessageLoadLimit = 200

func (m *UI) loadSessionWithSelection(sessionID string, selectedMessageID string) tea.Cmd {
	m.pendingSessionLoad = sessionID
	return func() tea.Msg {
		session, err := m.com.App.Sessions.Get(context.Background(), sessionID)
		if err != nil {
			return util.ReportError(err)
		}
		// A Responses WebSocket can be closed remotely while this session is
		// inactive but remain in the local pool. Reset only the pooled socket
		// at the resume boundary so the first prompt does not discover the
		// stale connection. Provider fallback and response-chain state remain
		// intact.
		if resetter, ok := m.com.App.AgentCoordinator.(agent.ResponsesWebSocketResetter); ok {
			resetter.ResetResponsesWebSocket(sessionID)
		}

		// Pause an active goal when the session is resumed from storage. The
		// preserve flag is left false for normal UI loads; automatic goal
		// continuation chains never go through this code path.
		var goalPausedNotice string
		if m.com.App.GoalRuntime != nil {
			_, notice, pauseErr := m.com.App.GoalRuntime.PauseActiveGoalOnLoad(context.Background(), sessionID, false)
			if pauseErr != nil {
				slog.Warn("Failed to pause active goal on session load", "error", pauseErr, "session_id", sessionID)
			} else if notice != "" {
				goalPausedNotice = notice
				// Reload the session to reflect the paused goal state.
				session, err = m.com.App.Sessions.Get(context.Background(), sessionID)
				if err != nil {
					return util.ReportError(err)
				}
			}
		}

		sessionFiles, err := m.loadSessionFiles(sessionID)
		if err != nil {
			return util.ReportError(err)
		}

		readFiles, err := m.com.App.FileTracker.ListReadFiles(context.Background(), sessionID)
		if err != nil {
			slog.Error("Failed to load read files for session", "error", err)
		}

		// Load only the most recent messages initially. If the session has
		// more messages than initialMessageLoadLimit, older messages can be
		// loaded on demand when the user scrolls to the top.
		totalCount, err := m.com.App.Messages.Count(context.Background(), sessionID)
		if err != nil {
			return util.ReportError(err)
		}

		var msgs []message.Message
		var skippedCount int64
		if totalCount > int64(initialMessageLoadLimit) {
			skippedCount = totalCount - int64(initialMessageLoadLimit)
			msgs, err = m.com.App.Messages.ListPage(context.Background(), sessionID, int(skippedCount), initialMessageLoadLimit)
		} else {
			msgs, err = m.com.App.Messages.List(context.Background(), sessionID)
		}
		if err != nil {
			return util.ReportError(err)
		}

		childInfo := make(map[string]childSessionInfo)
		if session.ParentSessionID != "" {
			if info, ok := fetchChildSessionMetadata(m.com.App, session.ID); ok {
				childInfo[session.ID] = info
			}
		}

		return loadSessionMsg{
			session:             &session,
			messages:            msgs,
			files:               sessionFiles,
			readFiles:           readFiles,
			selectedMessageID:   selectedMessageID,
			childSessionInfo:    childInfo,
			totalMessageCount:   totalCount,
			skippedMessageCount: skippedCount,
			goalPausedNotice:    goalPausedNotice,
		}
	}
}

// loadMoreMessages loads older messages from the database and returns them
// as a loadMoreMessagesMsg. The offset is the number of messages to skip
// from the beginning (i.e., m.skippedMessageCount - loadMoreMessagesCount).
func (m *UI) loadMoreMessages() tea.Cmd {
	if m.session == nil || m.skippedMessageCount <= 0 || m.loadingMoreMessages {
		return nil
	}
	m.loadingMoreMessages = true
	sessionID := m.session.ID
	skipCount := m.skippedMessageCount
	limit := int64(loadMoreMessagesCount)
	if skipCount < limit {
		limit = skipCount
	}
	offset := skipCount - limit
	return func() tea.Msg {
		msgs, err := m.com.App.Messages.ListPage(context.Background(), sessionID, int(offset), int(limit))
		if err != nil {
			return util.ReportError(err)
		}
		return loadMoreMessagesMsg{
			messages: msgs,
			count:    offset,
		}
	}
}

func (m *UI) loadSessionFiles(sessionID string) ([]SessionFile, error) {
	files, err := m.com.App.History.ListBySession(context.Background(), sessionID)
	if err != nil {
		return nil, err
	}

	filesByPath := make(map[string][]history.File)
	for _, f := range files {
		filesByPath[f.Path] = append(filesByPath[f.Path], f)
	}
	sessionFiles := make([]SessionFile, 0, len(filesByPath))
	for _, versions := range filesByPath {
		if len(versions) == 0 {
			continue
		}

		first := versions[0]
		last := versions[0]
		for _, v := range versions {
			if v.Version < first.Version {
				first = v
			}
			if v.Version > last.Version {
				last = v
			}
		}

		_, additions, deletions := diff.GenerateDiff(first.Content, last.Content, first.Path)

		sessionFiles = append(sessionFiles, SessionFile{
			FirstVersion:  first,
			LatestVersion: last,
			Additions:     additions,
			Deletions:     deletions,
		})
	}

	slices.SortFunc(sessionFiles, func(a, b SessionFile) int {
		if a.LatestVersion.UpdatedAt > b.LatestVersion.UpdatedAt {
			return -1
		}
		if a.LatestVersion.UpdatedAt < b.LatestVersion.UpdatedAt {
			return 1
		}
		return 0
	})
	return sessionFiles, nil
}

// handleFileEvent processes file change events and updates the session file
// list with new or updated file information.
func (m *UI) handleFileEvent(file history.File) tea.Cmd {
	if m.session == nil || file.SessionID != m.session.ID {
		return nil
	}

	return func() tea.Msg {
		sessionFiles, err := m.loadSessionFiles(m.session.ID)
		// could not load session files
		if err != nil {
			return util.NewErrorMsg(err)
		}

		return sessionFilesUpdatesMsg{
			sessionFiles: sessionFiles,
		}
	}
}

// filesInfo renders the modified files section for the sidebar, showing files
// with their addition/deletion counts.
func (m *UI) filesInfo(cwd string, width, maxItems int, isSection bool) string {
	t := m.com.Styles

	title := t.Subtle.Render("Modified Files")
	if isSection {
		title = common.Section(t, "Modified Files", width)
	}
	list := t.Subtle.Render("None")
	var filesWithChanges []SessionFile
	for _, f := range m.sessionFiles {
		if f.Additions == 0 && f.Deletions == 0 {
			continue
		}
		filesWithChanges = append(filesWithChanges, f)
	}
	if len(filesWithChanges) > 0 {
		list = fileList(t, cwd, filesWithChanges, width, maxItems)
	}

	return lipgloss.NewStyle().Width(width).Render(fmt.Sprintf("%s\n\n%s", title, list))
}

// fileList renders a list of files with their diff statistics, truncating to
// maxItems and showing a "...and N more" message if needed.
func fileList(t *styles.Styles, cwd string, filesWithChanges []SessionFile, width, maxItems int) string {
	if maxItems <= 0 {
		return ""
	}
	var renderedFiles []string
	filesShown := 0

	for _, f := range filesWithChanges {
		// Skip files with no changes
		if filesShown >= maxItems {
			break
		}

		// Build stats string with colors
		var statusParts []string
		if f.Additions > 0 {
			statusParts = append(statusParts, t.Files.Additions.Render(fmt.Sprintf("+%d", f.Additions)))
		}
		if f.Deletions > 0 {
			statusParts = append(statusParts, t.Files.Deletions.Render(fmt.Sprintf("-%d", f.Deletions)))
		}
		extraContent := strings.Join(statusParts, " ")

		// Format file path relative to the detected project root when possible.
		filePath := formatModifiedFilePath(cwd, f.FirstVersion.Path)
		filePath = compactModifiedFilePath(filePath, width-(lipgloss.Width(extraContent)-2))

		line := t.Files.Path.Render(filePath)
		if extraContent != "" {
			line = fmt.Sprintf("%s %s", line, extraContent)
		}

		renderedFiles = append(renderedFiles, line)
		filesShown++
	}

	if len(filesWithChanges) > maxItems {
		remaining := len(filesWithChanges) - maxItems
		renderedFiles = append(renderedFiles, t.Subtle.Render(fmt.Sprintf("…and %d more", remaining)))
	}

	return lipgloss.JoinVertical(lipgloss.Left, renderedFiles...)
}

func formatModifiedFilePath(cwd, filePath string) string {
	absPath := filePath
	if !filepath.IsAbs(absPath) {
		absPath = filepath.Join(cwd, filePath)
	}
	absPath = filepath.Clean(absPath)

	if root, ok := detectModifiedFilesRoot(absPath); ok {
		if rel, ok := relativeDisplayPath(root, absPath); ok {
			return rel
		}
	}
	if rel, ok := relativeDisplayPath(cwd, absPath); ok {
		return rel
	}
	return fsext.PrettyPath(absPath)
}

func detectModifiedFilesRoot(filePath string) (string, bool) {
	startDir := filepath.Dir(filePath)
	for _, marker := range modifiedFilesRootMarkers {
		if found, ok := fsext.LookupClosest(startDir, marker); ok {
			if filepath.Base(found) == marker && marker != ".git" {
				return filepath.Dir(found), true
			}
			if marker == ".git" {
				info, err := os.Stat(found)
				if err == nil && info.IsDir() {
					return filepath.Dir(found), true
				}
			}
		}
	}
	return "", false
}

func relativeDisplayPath(root, filePath string) (string, bool) {
	if root == "" {
		return "", false
	}
	rel, err := filepath.Rel(root, filePath)
	if err != nil {
		return "", false
	}
	rel = filepath.Clean(rel)
	if rel == "." || rel == "" || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

func compactModifiedFilePath(path string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(path) <= width {
		return path
	}

	cleanPath := filepath.Clean(path)
	sep := string(filepath.Separator)
	parts := strings.Split(cleanPath, sep)
	if len(parts) >= 2 {
		candidate := filepath.Join("…", parts[len(parts)-2], parts[len(parts)-1])
		if lipgloss.Width(candidate) <= width {
			return candidate
		}
	}
	if len(parts) >= 1 {
		candidate := filepath.Join("…", parts[len(parts)-1])
		if lipgloss.Width(candidate) <= width {
			return candidate
		}
	}
	return ansi.Truncate(path, width, "…")
}

func (m *UI) childSessions(parentID string) ([]session.Session, error) {
	msgs, err := m.com.App.Messages.List(context.Background(), parentID)
	if err != nil {
		return nil, err
	}

	allSessions, _ := m.com.App.Sessions.ListChildren(context.Background(), parentID)
	parentChildren := allSessions

	seen := make(map[string]struct{})
	children := make([]session.Session, 0)
	for _, msg := range msgs {
		for _, tc := range msg.ToolCalls() {
			if !isChildSessionToolCall(tc.Name) {
				continue
			}
			childID := m.com.App.Sessions.CreateAgentToolSessionID(msg.ID, tc.ID)
			child, err := m.com.App.Sessions.Get(context.Background(), childID)
			if err == nil && child.ParentSessionID == parentID {
				if _, exists := seen[child.ID]; !exists {
					seen[child.ID] = struct{}{}
					children = append(children, child)
				}
				continue
			}
			// Fallback: the derived ID did not resolve directly, so scan the
			// parent's children for a "::"-suffixed match. This is needed
			// for batch task-graph subagents, whose session ID uses the
			// per-task "toolCallID::taskName" composite form rather than the
			// bare derived ID, and for older sessions created before this
			// composite form existed. Both cases are expected to persist
			// long-term, not just during an in-flight run, so this scan is
			// not a temporary shim.
			prefix := childID + "::"
			for _, s := range parentChildren {
				if strings.HasPrefix(s.ID, prefix) {
					if _, exists := seen[s.ID]; !exists {
						slog.Debug("Child session resolved via legacy prefix scan", "child_session_id", s.ID, "parent", parentID)
						seen[s.ID] = struct{}{}
						children = append(children, s)
					}
				}
			}
		}
	}

	return children, nil
}

func (m *UI) sessionRoleLabel(sess *session.Session) string {
	if sess == nil || sess.ParentSessionID == "" {
		return "Main"
	}

	if m.childSessionInfoCache != nil {
		if info, ok := m.childSessionInfoCache[sess.ID]; ok {
			return info.RoleLabel
		}
	}

	return ""
}

func titleCase(value string) string {
	if value == "" {
		return ""
	}
	runes := []rune(value)
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

type childSessionInfo struct {
	RoleLabel string
}

func fetchChildSessionMetadata(app *app.App, sessionID string) (childSessionInfo, bool) {
	messageID, toolCallID, ok := app.Sessions.ParseAgentToolSessionID(sessionID)
	if !ok {
		return childSessionInfo{}, false
	}

	msg, err := app.Messages.Get(context.Background(), messageID)
	if err != nil {
		return childSessionInfo{}, false
	}

	for _, tc := range msg.ToolCalls() {
		if tc.ID != toolCallID {
			continue
		}
		switch tc.Name {
		case agent.AgentToolName:
			var params agent.AgentParams
			if err := json.Unmarshal([]byte(tc.Input), &params); err != nil {
				return childSessionInfo{}, false
			}
			return childSessionInfo{
				RoleLabel: titleCase(config.CanonicalSubagentID(params.SubagentType)),
			}, true
		case agenttools.AgenticFetchToolName:
			return childSessionInfo{RoleLabel: "Fetch"}, true
		}
	}

	return childSessionInfo{}, false
}

func isChildSessionToolCall(toolName string) bool {
	switch toolName {
	case agent.AgentToolName, agenttools.AgenticFetchToolName:
		return true
	default:
		return false
	}
}

func childSessionIDForTaskRef(app *app.App, parentSessionID, taskRef string) string {
	if app == nil || app.Messages == nil {
		return ""
	}
	taskRef = strings.TrimSpace(strings.TrimPrefix(taskRef, "subtask://"))
	if parentSessionID == "" || taskRef == "" {
		return ""
	}
	msgs, err := app.Messages.List(context.Background(), parentSessionID)
	if err != nil {
		return ""
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		toolResults := msgs[i].ToolResults()
		for j := len(toolResults) - 1; j >= 0; j-- {
			if reducer, ok := toolResults[j].Reducer(); ok {
				for k := len(reducer.ChildSessions) - 1; k >= 0; k-- {
					child := reducer.ChildSessions[k]
					if !taskRefMatches(taskRef, child.TaskRef, child.TaskID) {
						continue
					}
					if sessionID := strings.TrimSpace(child.SessionID); sessionID != "" {
						return sessionID
					}
				}
			}
			if subtask, ok := toolResults[j].SubtaskResult(); ok && taskRefMatches(taskRef, subtask.TaskRef) {
				if sessionID := strings.TrimSpace(subtask.ChildSessionID); sessionID != "" {
					return sessionID
				}
			}
		}
	}
	return ""
}

func taskRefMatches(target string, candidates ...string) bool {
	target = strings.TrimSpace(strings.TrimPrefix(target, "subtask://"))
	if target == "" {
		return false
	}
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(strings.TrimPrefix(candidate, "subtask://"))
		if candidate != "" && strings.EqualFold(target, candidate) {
			return true
		}
	}
	return false
}

type openChildSessionMsg struct {
	sessionID string
}

// openLatestChildSession opens the most recently created running child session
// of the current session. If no child is running, it opens the most recently
// created child. It is used as a fallback for the "]" key when no row with a
// child session is selected.
func (m *UI) openLatestChildSession() tea.Cmd {
	if m.session == nil {
		return nil
	}
	parentSessionID := m.session.ID

	// Prefer a running subagent, identified by an active (non-finished)
	// timeline task. getSubagentTasks sorts active tasks first, newest first.
	tasks := getSubagentTasks(m.timelineEvents)
	for _, task := range tasks {
		if !isActiveTask(task.eventType) {
			continue
		}
		return func() tea.Msg {
			return openChildSessionMsg{sessionID: task.id}
		}
	}

	// No running child: fall back to the most recently created child session.
	return func() tea.Msg {
		children, err := m.childSessions(parentSessionID)
		if err != nil || len(children) == 0 {
			return nil
		}
		// childSessions returns children in creation order (ascending); pick
		// the last one as the most recently created.
		return openChildSessionMsg{sessionID: children[len(children)-1].ID}
	}
}

// captureViewState saves the current session's selected item ID so it can
// be restored when navigating back from a child session.
func (m *UI) captureViewState() {
	if m.session == nil {
		return
	}
	selectedID := ""
	if sel := m.chat.SelectedMessageItem(); sel != nil {
		selectedID = sel.ID()
	}
	if m.viewStateCache == nil {
		m.viewStateCache = make(map[string]sessionViewState)
	}
	m.viewStateCache[m.session.ID] = sessionViewState{
		SelectedItemID: selectedID,
	}
}

func (m *UI) selectedHasChildSession() bool {
	selected := m.chat.SelectedMessageItem()
	if selected == nil {
		return false
	}
	if _, ok := selected.(*chat.TaskNodeItem); ok {
		return true
	}
	toolItem, ok := selected.(chat.ToolMessageItem)
	return ok && isChildSessionToolCall(toolItem.ToolCall().Name)
}

func (m *UI) openSelectedChildSession() tea.Cmd {
	if m.session == nil {
		return nil
	}

	// Save current view state before navigating into the child session.
	m.captureViewState()

	selected := m.chat.SelectedMessageItem()
	if selected == nil {
		return nil
	}

	if taskNode, ok := selected.(*chat.TaskNodeItem); ok {
		childID := taskNode.ChildSessionID()
		taskRef := taskNode.TaskRef()
		parentSessionID := m.session.ID
		return func() tea.Msg {
			if sessionID := childSessionIDForTaskRef(m.com.App, parentSessionID, taskRef); sessionID != "" {
				return openChildSessionMsg{sessionID: sessionID}
			}
			s, err := m.com.App.Sessions.Get(context.Background(), childID)
			if err == nil && s.ParentSessionID == parentSessionID {
				return openChildSessionMsg{sessionID: s.ID}
			}
			// Fallback: scan child session IDs by the derived
			// "msgID::toolCallID" prefix. This is needed for batch
			// task-graph subagents, whose session ID carries a
			// "toolCallID::taskName" composite suffix instead of the bare
			// derived ID, and for older sessions predating that composite
			// form. This scan is a long-lived fallback, not a stopgap for
			// missing spawn-time metadata.
			prefix := childID + "::"
			children, err := m.com.App.Sessions.ListChildren(context.Background(), parentSessionID)
			if err != nil {
				return nil
			}
			for _, s := range children {
				if strings.HasPrefix(s.ID, prefix) {
					slog.Debug("Child session resolved via legacy prefix scan", "child_session_id", s.ID, "parent", parentSessionID)
					return openChildSessionMsg{sessionID: s.ID}
				}
			}
			return nil
		}
	}

	toolItem, ok := selected.(chat.ToolMessageItem)
	if !ok || !isChildSessionToolCall(toolItem.ToolCall().Name) {
		return nil
	}

	childID := m.com.App.Sessions.CreateAgentToolSessionID(toolItem.MessageID(), toolItem.ToolCall().ID)
	parentSessionID := m.session.ID

	return func() tea.Msg {
		child, err := m.com.App.Sessions.Get(context.Background(), childID)
		if err == nil && child.ParentSessionID == parentSessionID {
			return openChildSessionMsg{sessionID: child.ID}
		}
		// Fallback: scan child session IDs by the derived
		// "msgID::toolCallID" prefix. This is needed for batch
		// task-graph subagents, whose session ID carries a
		// "toolCallID::taskName" composite suffix instead of the bare
		// derived ID, and for older sessions predating that composite
		// form. This scan is a long-lived fallback, not a stopgap for
		// missing spawn-time metadata.
		prefix := childID + "::"
		children, err := m.com.App.Sessions.ListChildren(context.Background(), parentSessionID)
		if err != nil {
			return nil
		}
		for _, s := range children {
			if strings.HasPrefix(s.ID, prefix) {
				slog.Debug("Child session resolved via legacy prefix scan", "child_session_id", s.ID, "parent", parentSessionID)
				return openChildSessionMsg{sessionID: s.ID}
			}
		}
		return nil
	}
}

func (m *UI) openParentSession() tea.Cmd {
	if m.session == nil || m.session.ParentSessionID == "" {
		return nil
	}

	parentID := m.session.ParentSessionID

	// Prefer the cached selected item from before the user entered this child.
	if cached, ok := m.viewStateCache[parentID]; ok {
		return m.loadSessionWithSelection(parentID, cached.SelectedItemID)
	}

	_, toolCallID, ok := m.com.App.Sessions.ParseAgentToolSessionID(m.session.ID)
	if ok {
		outerToolCallID := toolCallID
		if idx := strings.Index(toolCallID, "::"); idx != -1 {
			outerToolCallID = toolCallID[:idx]
		}
		return m.loadSessionWithSelection(parentID, outerToolCallID)
	}

	return m.loadSession(parentID)
}

func (m *UI) cycleSiblingChildSession(step int) tea.Cmd {
	if m.session == nil || m.session.ParentSessionID == "" {
		return nil
	}

	parentID := m.session.ParentSessionID
	currentID := m.session.ID

	return func() tea.Msg {
		children, err := m.childSessions(parentID)
		if err != nil {
			return util.ReportError(err)
		}
		if len(children) < 2 {
			return nil
		}

		currentIndex := -1
		for i, child := range children {
			if child.ID == currentID {
				currentIndex = i
				break
			}
		}
		if currentIndex == -1 {
			return nil
		}

		nextIndex := currentIndex + step
		if nextIndex < 0 || nextIndex >= len(children) {
			return nil
		}

		return openChildSessionMsg{sessionID: children[nextIndex].ID}
	}
}

// startLSPs starts LSP servers for the given file paths.
func (m *UI) startLSPs(paths []string) tea.Cmd {
	if len(paths) == 0 {
		return nil
	}

	return func() tea.Msg {
		ctx := context.Background()
		for _, path := range paths {
			m.com.App.LSPManager.Start(ctx, path)
		}
		return nil
	}
}
