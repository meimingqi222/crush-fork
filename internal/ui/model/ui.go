package model

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/agent/notify"
	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/commands"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/fsext"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/home"
	"github.com/charmbracelet/crush/internal/imageutil"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/planmode"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/skills"
	"github.com/charmbracelet/crush/internal/timeline"
	"github.com/charmbracelet/crush/internal/toolruntime"
	"github.com/charmbracelet/crush/internal/ui/anim"
	"github.com/charmbracelet/crush/internal/ui/attachments"
	"github.com/charmbracelet/crush/internal/ui/chat"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/completions"
	"github.com/charmbracelet/crush/internal/ui/dialog"
	fimage "github.com/charmbracelet/crush/internal/ui/image"
	"github.com/charmbracelet/crush/internal/ui/logo"
	"github.com/charmbracelet/crush/internal/ui/notification"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/crush/internal/ui/util"
	"github.com/charmbracelet/crush/internal/userinput"
	"github.com/charmbracelet/crush/internal/version"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/ultraviolet/layout"
	"github.com/charmbracelet/ultraviolet/screen"
	"github.com/charmbracelet/x/editor"
)

type agentToolParams struct {
	Tasks []struct {
		ID           string `json:"id,omitempty"`
		Name         string `json:"name,omitempty"`
		Description  string `json:"description,omitempty"`
		Assignment   string `json:"assignment,omitempty"`
		SubagentType string `json:"subagent_type,omitempty"`
	} `json:"tasks,omitempty"`
}

// MouseScrollThreshold defines how many lines to scroll the chat when a mouse
// wheel event occurs.
const MouseScrollThreshold = 5

// Compact mode breakpoints.
const (
	compactModeWidthBreakpoint  = 120
	compactModeHeightBreakpoint = 30
)

// If pasted text has more than 10 newlines, treat it as a file attachment.
const pasteLinesThreshold = 10

// If pasted text has more than 1000 columns, treat it as a file attachment.
const pasteColsThreshold = 1000

// Session details panel max height.
const sessionDetailsMaxHeight = 20

// TextareaMaxHeight is the maximum height of the prompt textarea.
const TextareaMaxHeight = 15

// editorBottomMargin is the blank line reserved below the textarea.
const editorBottomMargin = 1

// TextareaMinHeight is the minimum height of the prompt textarea.
const TextareaMinHeight = 3

// uiFocusState represents the current focus state of the UI.
type uiFocusState uint8

// Possible uiFocusState values.
const (
	uiFocusNone uiFocusState = iota
	uiFocusEditor
	uiFocusMain
)

type uiState uint8

// Possible uiState values.
const (
	uiOnboarding uiState = iota
	uiInitialize
	uiLanding
	uiChat
)

type openEditorMsg struct {
	Text string
}

type (
	// cancelTimerExpiredMsg is sent when the cancel timer expires.
	cancelTimerExpiredMsg struct{}
	// userCommandsLoadedMsg is sent when user commands are loaded.
	userCommandsLoadedMsg struct {
		Commands []commands.CustomCommand
	}
	// mcpPromptsLoadedMsg is sent when mcp prompts are loaded.
	mcpPromptsLoadedMsg struct {
		Prompts []commands.MCPPrompt
	}
	// mcpStateChangedMsg is sent when there is a change in MCP client states.
	mcpStateChangedMsg struct {
		states map[string]mcp.ClientInfo
	}
	// sendMessageMsg is sent to send a message.
	// currently only used for mcp prompts.
	sendMessageMsg struct {
		Content     string
		Attachments []message.Attachment
	}
	executionModeChangedMsg struct {
		SessionID string
		Status    string
	}
	planModeChangedMsg struct {
		SessionID string
		Status    string
		Mode      session.CollaborationMode
	}

	// closeDialogMsg is sent to close the current dialog.
	closeDialogMsg struct{}

	// copyChatHighlightMsg is sent to copy the current chat highlight to clipboard.
	copyChatHighlightMsg struct{}

	// sessionViewState captures the view state of a session so it can be
	// restored when navigating back from a child session.
	sessionViewState struct {
		SelectedItemID string
		ExpandedItems  map[string]bool
	}

	// sessionFilesUpdatesMsg is sent when the files for this session have been updated
	sessionFilesUpdatesMsg struct {
		sessionFiles []SessionFile
	}
	sessionUsageRefreshedMsg struct {
		session *session.Session
	}
	modelSwitchPreparedMsg struct {
		action       dialog.ActionSelectModel
		isOnboarding bool
		err          error
	}
	modelSwitchCompletedMsg struct {
		modelType    config.SelectedModelType
		modelName    string
		isOnboarding bool
		closeDialog  bool
		err          error
	}
	promptEnhanceResultMsg struct {
		enhanced string
		err      error
	}
	handoffGeneratedMsg struct {
		sessionID string
		title     string
		err       error
	}
)

// UI represents the main user interface model.
type UI struct {
	com             *common.Common
	session         *session.Session
	sessionMessages []message.Message
	sessionMsgIndex map[string]int // Maps message ID to index in sessionMessages for O(1) lookup.
	sessionFiles    []SessionFile

	// childSessionInfoCache caches child session metadata to avoid
	// DB I/O in the render path.
	childSessionInfoCache map[string]childSessionInfo
	timelineEvents        []timeline.Event

	// keeps track of read files while we don't have a session id
	sessionFileReads []string

	// initialSessionID is set when loading a specific session on startup.
	initialSessionID string
	// continueLastSession is set to continue the most recent session on startup.
	continueLastSession bool

	lastUserMessageTime int64
	latestProposedPlan  string
	lastPromptedPlanMsg string

	// The width and height of the terminal in cells.
	width  int
	height int
	layout uiLayout

	isTransparent bool

	focus uiFocusState
	state uiState

	keyMap KeyMap
	keyenh tea.KeyboardEnhancementsMsg

	dialog *dialog.Overlay
	status *Status

	// isCanceling tracks whether the user has pressed escape once to cancel.
	isCanceling bool

	header *header

	// sendProgressBar instructs the TUI to send progress bar updates to the
	// terminal.
	sendProgressBar    bool
	progressBarEnabled bool

	// caps hold different terminal capabilities that we query for.
	caps common.Capabilities

	// Editor components
	textarea textarea.Model

	// Attachment list
	attachments *attachments.Attachments

	readyPlaceholder   string
	workingPlaceholder string

	// Completions state
	completions              *completions.Completions
	completionsOpen          bool
	completionsStartIndex    int
	completionsQuery         string
	completionsPositionStart image.Point // x,y where user typed '@'

	// Chat components
	chat *Chat

	// onboarding state
	onboarding struct {
		yesInitializeSelected bool
	}

	// lsp
	lspStates map[string]app.LSPClientInfo

	// mcp
	mcpStates map[string]mcp.ClientInfo

	// skills
	skillsState skills.DiscoveryState

	// sidebarLogo keeps a cached version of the sidebar sidebarLogo.
	sidebarLogo string

	// Notification state
	notifyBackend       notification.Backend
	notifyWindowFocused bool
	statusMsgSeq        uint64
	// custom commands & mcp commands
	customCommands []commands.CustomCommand
	mcpPrompts     []commands.MCPPrompt

	// forceCompactMode tracks whether compact mode is forced by user toggle
	forceCompactMode bool

	// pendingSubagentNotifications stores completed subagent XML notifications when user is typing, isolated by session ID.
	pendingSubagentNotifications map[string][]string

	// isCompact tracks whether we're currently in compact layout mode (either
	// by user toggle or auto-switch based on window size)
	isCompact bool

	// detailsOpen tracks whether the details panel is open (in compact mode)
	detailsOpen bool

	// pills state
	pillsExpanded      bool
	focusedPillSection pillSection
	promptQueue        int
	queuePaused        bool
	isEnhancingPrompt  bool
	selectedQueueIndex int
	pillsPreviousFocus uiFocusState
	pillsView          string

	// pendingSessionLoad is set when an async session load is in flight.
	// Events with this session ID are ignored until loadSessionMsg arrives,
	// avoiding the race where m.session.ID hasn't switched yet.
	pendingSessionLoad string

	// viewStateCache caches view state per session ID so returning from a
	// child session can restore scroll position and expand states.
	viewStateCache map[string]sessionViewState

	// Todo spinner
	todoSpinner    spinner.Model
	todoIsSpinning bool

	// globalTickActive tracks whether the global animation ticker is
	// currently running. It is started when the first animation is
	// registered and stopped when no animations remain.
	globalTickActive bool

	// sidebar cache to avoid full re-render every frame.
	sidebarCacheDirty   bool
	sidebarCacheContent string
	sidebarCacheWidth   int
	sidebarCacheHeight  int

	// Per-frame usage snapshot cache: computed once in Draw(), consumed
	// by both drawHeader and modelInfo (sidebar) without recomputing.
	frameUsageSnapshot      contextUsageSnapshot
	frameUsageSnapshotValid bool

	// mouse highlighting related state
	lastClickTime              time.Time
	lastClipboardPasteShortcut time.Time
	lastClipboardAttachmentSig string
	lastClipboardAttachmentAt  time.Time

	// Prompt history for up/down navigation through previous messages.
	promptHistory struct {
		messages []string
		index    int
		draft    string
	}
}

type executionMode string

const (
	executionModeAsk  executionMode = "ask"
	executionModeAuto executionMode = "auto"
	executionModeYolo executionMode = "yolo"
)

// New creates a new instance of the [UI] model.
func New(com *common.Common, initialSessionID string, continueLast bool) *UI {
	// Editor components
	ta := textarea.New()
	ta.SetStyles(com.Styles.TextArea)
	ta.ShowLineNumbers = false
	ta.CharLimit = -1
	ta.SetVirtualCursor(false)
	ta.DynamicHeight = true
	ta.MinHeight = TextareaMinHeight
	ta.MaxHeight = TextareaMaxHeight
	ta.Focus()

	ch := NewChat(com)

	keyMap := DefaultKeyMap()

	// Completions component
	comp := completions.New(
		com.Styles.Completions.Normal,
		com.Styles.Completions.Focused,
		com.Styles.Completions.Match,
	)

	todoSpinner := spinner.New(
		spinner.WithSpinner(spinner.MiniDot),
		spinner.WithStyle(com.Styles.Pills.TodoSpinner),
	)

	// Attachments component
	attachments := attachments.New(
		attachments.NewRenderer(
			com.Styles.Attachments.Normal,
			com.Styles.Attachments.Deleting,
			com.Styles.Attachments.Image,
			com.Styles.Attachments.Text,
		),
		attachments.Keymap{
			DeleteMode: keyMap.Editor.AttachmentDeleteMode,
			DeleteAll:  keyMap.Editor.DeleteAllAttachments,
			Escape:     keyMap.Editor.Escape,
		},
	)

	header := newHeader(com)

	ui := &UI{
		com:                 com,
		dialog:              dialog.NewOverlay(),
		keyMap:              keyMap,
		textarea:            ta,
		chat:                ch,
		header:              header,
		completions:         comp,
		attachments:         attachments,
		todoSpinner:         todoSpinner,
		lspStates:           make(map[string]app.LSPClientInfo),
		mcpStates:           make(map[string]mcp.ClientInfo),
		skillsState:         skills.DiscoveryState{Errors: make(map[string]error)},
		notifyBackend:       notification.NoopBackend{},
		notifyWindowFocused: true,
		initialSessionID:    initialSessionID,
		continueLastSession: continueLast,
		sessionMsgIndex:     make(map[string]int),
	}

	status := NewStatus(com, ui)

	ui.setEditorPrompt(false)
	ui.randomizePlaceholders()
	ui.refreshEditorPlaceholder()
	ui.status = status

	// Initialize compact mode from config
	ui.forceCompactMode = com.Config().Options.TUI.CompactMode

	// set onboarding state defaults
	ui.onboarding.yesInitializeSelected = true

	desiredState := uiLanding
	desiredFocus := uiFocusEditor
	if !com.Config().IsConfigured() {
		desiredState = uiOnboarding
	} else if n, _ := config.ProjectNeedsInitialization(com.Store()); n {
		desiredState = uiInitialize
	}

	// set initial state
	ui.setState(desiredState, desiredFocus)

	opts := com.Config().Options

	// disable indeterminate progress bar
	ui.progressBarEnabled = opts.Progress == nil || *opts.Progress
	// enable transparent mode
	ui.isTransparent = opts.TUI.Transparent != nil && *opts.TUI.Transparent

	return ui
}

// Init initializes the UI model.
func (m *UI) Init() tea.Cmd {
	var cmds []tea.Cmd
	if m.state == uiOnboarding {
		if cmd := m.openModelsDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	// load the user commands async
	cmds = append(cmds, m.loadCustomCommands())
	// load prompt history async
	cmds = append(cmds, m.loadPromptHistory())
	// load initial session if specified
	if cmd := m.loadInitialSession(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	return tea.Batch(cmds...)
}

// loadInitialSession loads the initial session if one was specified on startup.
func (m *UI) loadInitialSession() tea.Cmd {
	switch {
	case m.state != uiLanding:
		// Only load if we're in landing state (i.e., fully configured)
		return nil
	case m.initialSessionID != "":
		return m.loadSession(m.initialSessionID)
	case m.continueLastSession:
		return func() tea.Msg {
			sess, err := m.com.App.Sessions.GetLast(context.Background())
			if err != nil {
				return util.NewWarnMsg("No sessions found to continue")
			}
			if sess.ParentSessionID != "" {
				return util.NewWarnMsg("Cannot continue a child session")
			}
			return m.loadSession(sess.ID)()
		}
	default:
		return nil
	}
}

// sendNotification returns a command that sends a notification if allowed by policy.
func (m *UI) sendNotification(n notification.Notification) tea.Cmd {
	if !m.shouldSendNotification() {
		return nil
	}

	backend := m.notifyBackend
	return func() tea.Msg {
		if err := backend.Send(n); err != nil {
			slog.Error("Failed to send notification", "error", err)
		}
		return nil
	}
}

// shouldSendNotification returns true if notifications should be sent based on
// current state. Focus reporting must be supported, window must not focused,
// and notifications must not be disabled in config.
func (m *UI) shouldSendNotification() bool {
	cfg := m.com.Config()
	if cfg != nil && cfg.Options != nil && cfg.Options.DisableNotifications {
		return false
	}
	return m.caps.ReportFocusEvents && !m.notifyWindowFocused
}

// setState changes the UI state and focus.
func (m *UI) setState(state uiState, focus uiFocusState) {
	if state == uiLanding {
		// Always turn off compact mode when going to landing
		m.isCompact = false
	}
	m.state = state
	m.focus = focus
	// Changing the state may change layout, so update it.
	m.updateLayoutAndSize()
}

// loadCustomCommands loads the custom commands asynchronously.
func (m *UI) loadCustomCommands() tea.Cmd {
	return func() tea.Msg {
		customCommands, err := commands.LoadCustomCommands(m.com.Config())
		if err != nil {
			slog.Error("Failed to load custom commands", "error", err)
		}
		return userCommandsLoadedMsg{Commands: customCommands}
	}
}

// loadMCPrompts loads the MCP prompts asynchronously.
func (m *UI) loadMCPrompts() tea.Msg {
	prompts, err := commands.LoadMCPPrompts()
	if err != nil {
		slog.Error("Failed to load MCP prompts", "error", err)
	}
	if prompts == nil {
		// flag them as loaded even if there is none or an error
		prompts = []commands.MCPPrompt{}
	}
	return mcpPromptsLoadedMsg{Prompts: prompts}
}

// Update handles updates to the UI model.
func (m *UI) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	// Only sync prompt queue for message types that could change queue state.
	// Skip high-frequency types (animation ticks, mouse, keyboard) to avoid
	// mutex contention on every frame.
	switch msg.(type) {
	case anim.GlobalTickMsg, anim.StepMsg, spinner.TickMsg,
		tea.MouseMsg, tea.MouseClickMsg, tea.MouseMotionMsg,
		tea.MouseReleaseMsg, tea.MouseWheelMsg,
		tea.KeyPressMsg, tea.KeyReleaseMsg, tea.PasteMsg:
		// Skip prompt queue sync for high-frequency events.
	default:
		if m.syncPromptQueue() {
			m.updateLayoutAndSize()
		}
	}
	// Update terminal capabilities
	m.caps.Update(msg)
	switch msg := msg.(type) {
	case tea.EnvMsg:
		// Is this Windows Terminal?
		if !m.sendProgressBar {
			m.sendProgressBar = slices.Contains(msg, "WT_SESSION")
		}
		cmds = append(cmds, common.QueryCmd(uv.Environ(msg)))
	case tea.ModeReportMsg:
		if m.caps.ReportFocusEvents {
			m.notifyBackend = notification.NewNativeBackend(notification.Icon)
		}
	case tea.FocusMsg:
		m.notifyWindowFocused = true
	case tea.BlurMsg:
		m.notifyWindowFocused = false
	case pubsub.Event[notify.Notification]:
		if cmd := m.handleAgentNotification(msg.Payload); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case loadSessionMsg:
		m.pendingSessionLoad = ""
		if m.forceCompactMode {
			m.isCompact = true
		}
		// Force focus to the main chat list when entering a subagent
		// session because the editor is hidden (read-only view).
		initialFocus := m.focus
		if msg.session != nil && msg.session.ParentSessionID != "" {
			initialFocus = uiFocusMain
		}
		m.setState(uiChat, initialFocus)
		m.isCanceling = false
		m.todoIsSpinning = false
		m.session = msg.session
		m.sessionFiles = msg.files
		m.childSessionInfoCache = msg.childSessionInfo
		m.timelineEvents = nil
		if m.com != nil && m.com.App != nil && m.com.App.GetTimeline() != nil && m.session != nil {
			m.timelineEvents = m.com.App.GetTimeline().ListBySession(m.session.ID)
		}
		cmds = append(cmds, m.startLSPs(msg.lspFilePaths()))
		if cmd := m.setSessionMessages(msg.messages); cmd != nil {
			cmds = append(cmds, cmd)
		}
		if m.session != nil && m.session.Kind == session.KindHandoff && m.session.MessageCount == 0 && strings.TrimSpace(m.session.HandoffDraftPrompt) != "" {
			prevHeight := m.textarea.Height()
			m.textarea.SetValue(m.session.HandoffDraftPrompt)
			m.textarea.MoveToEnd()
			if cmd := m.updateTextareaWithPrevHeight(nil, prevHeight); cmd != nil {
				cmds = append(cmds, cmd)
			}
			if m.attachments != nil {
				m.attachments.Clear()
			}
		}
		if msg.selectedMessageID != "" && m.chat.SelectMessage(msg.selectedMessageID) {
			if cmd := m.chat.ScrollToSelectedAndAnimate(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		if hasInProgressTodo(m.session.Todos) {
			// only start spinner if there is active or queued work
			if m.hasLiveSessionActivity() {
				m.todoIsSpinning = true
				cmds = append(cmds, m.todoSpinner.Tick)
			}
			m.updateLayoutAndSize()
		}
		// Reload prompt history for the new session.
		m.historyReset()
		cmds = append(cmds, m.loadPromptHistory())
		m.updateLayoutAndSize()
		m.invalidateSidebarCache()

	case openChildSessionMsg:
		cmds = append(cmds, m.loadSession(msg.sessionID))

	case sessionFilesUpdatesMsg:
		m.sessionFiles = msg.sessionFiles
		var paths []string
		for _, f := range msg.sessionFiles {
			paths = append(paths, f.LatestVersion.Path)
		}
		cmds = append(cmds, m.startLSPs(paths))
		m.invalidateSidebarCache()

	case sessionUsageRefreshedMsg:
		if msg.session != nil && m.session != nil && msg.session.ID == m.session.ID {
			m.session = msg.session
		}
		m.invalidateSidebarCache()

	case sendMessageMsg:
		cmds = append(cmds, m.sendMessage(msg.Content, msg.Attachments...))

	case executionModeChangedMsg:
		cmds = append(cmds, util.ReportInfo(msg.Status))
		if msg.SessionID != "" && (m.session == nil || m.session.ID != msg.SessionID) {
			cmds = append(cmds, m.loadSession(msg.SessionID))
		}
		m.invalidateSidebarCache()

	case planModeChangedMsg:
		cmds = append(cmds, util.ReportInfo(msg.Status))
		if m.session != nil && m.session.ID == msg.SessionID && msg.Mode != "" {
			m.session.CollaborationMode = msg.Mode
			m.refreshEditorPlaceholder()
		} else if msg.SessionID != "" && (m.session == nil || m.session.ID != msg.SessionID) {
			cmds = append(cmds, m.loadSession(msg.SessionID))
		}
		m.invalidateSidebarCache()

	case userCommandsLoadedMsg:
		m.customCommands = msg.Commands
		dia := m.dialog.Dialog(dialog.CommandsID)
		if dia == nil {
			break
		}

		commands, ok := dia.(*dialog.Commands)
		if ok {
			commands.SetCustomCommands(m.customCommands)
		}

	case mcpStateChangedMsg:
		m.mcpStates = msg.states
		m.invalidateSidebarCache()
		dia := m.dialog.Dialog(dialog.MCPID)
		if mcpDialog, ok := dia.(*dialog.MCP); ok {
			mcpDialog.SetStates(msg.states)
		}
		// Also refresh the MCP detail dialog if it's currently open.
		detailDia := m.dialog.Dialog(dialog.MCPDetailID)
		if detailDialog, ok := detailDia.(*dialog.MCPDetail); ok {
			if state, ok := msg.states[detailDialog.Name()]; ok {
				detailDialog.SetState(state)
			}
			if cfg, ok := m.com.Config().MCP[detailDialog.Name()]; ok {
				detailDialog.SetConfig(cfg)
			}
		}
	case mcpPromptsLoadedMsg:
		m.mcpPrompts = msg.Prompts
		dia := m.dialog.Dialog(dialog.CommandsID)
		if dia == nil {
			break
		}

		commands, ok := dia.(*dialog.Commands)
		if ok {
			commands.SetMCPPrompts(m.mcpPrompts)
		}

	case promptHistoryLoadedMsg:
		m.promptHistory.messages = msg.messages
		m.promptHistory.index = -1
		m.promptHistory.draft = ""

	case closeDialogMsg:
		m.dialog.CloseFrontDialog()

	case pubsub.Event[session.Session]:
		if msg.Type == pubsub.DeletedEvent {
			if m.session != nil && m.session.ID == msg.Payload.ID {
				if cmd := m.newSession(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			}
			m.invalidateSidebarCache()
			break
		}
		if m.session != nil && msg.Payload.ID == m.session.ID {
			prevHasInProgress := hasInProgressTodo(m.session.Todos)
			m.session = &msg.Payload
			if !prevHasInProgress && hasInProgressTodo(m.session.Todos) {
				m.todoIsSpinning = true
				cmds = append(cmds, m.todoSpinner.Tick)
				m.updateLayoutAndSize()
			}
			m.invalidateSidebarCache()
		}
	case pubsub.Event[message.Message]:
		// Check if this is a child session message for an agent tool.
		if m.session == nil {
			break
		}
		if msg.Payload.SessionID != m.session.ID {
			// If we're async-loading this session, skip — loadSessionMsg will
			// bring the UI up to date once the load finishes.
			if m.pendingSessionLoad != "" && msg.Payload.SessionID == m.pendingSessionLoad {
				break
			}
			// This might be a child session message from an agent tool.
			if cmd := m.handleChildSessionMessage(msg); cmd != nil {
				cmds = append(cmds, cmd)
			}
			break
		}
		// Messages affect the sidebar via usage counts and file info.
		m.invalidateSidebarCache()
		switch msg.Type {
		case pubsub.CreatedEvent:
			cmds = append(cmds, m.appendSessionMessage(msg.Payload))
		case pubsub.UpdatedEvent:
			cmds = append(cmds, m.updateSessionMessage(msg.Payload))
		case pubsub.DeletedEvent:
			m.removeCurrentSessionMessage(msg.Payload.ID)
			if msg.Payload.Role == message.Assistant {
				m.removeToolItemsForMessage(msg.Payload.ID, nil)
				m.chat.RemoveMessage(chat.AssistantInfoID(msg.Payload.ID))
			}
			m.chat.RemoveMessage(msg.Payload.ID)
		}
		if m.shouldRefreshSessionUsage(msg.Type, msg.Payload) {
			cmds = append(cmds, m.refreshCurrentSessionUsage())
		}
		// start the spinner if there is a new message
		if hasInProgressTodo(m.session.Todos) && m.isAgentBusy() && !m.todoIsSpinning {
			m.todoIsSpinning = true
			cmds = append(cmds, m.todoSpinner.Tick)
			if !m.globalTickActive {
				m.globalTickActive = true
				cmds = append(cmds, anim.GlobalTick())
			}
		}
		// stop the spinner if the agent is not busy anymore
		if m.todoIsSpinning && !m.isAgentBusy() {
			m.todoIsSpinning = false
		}
		if !m.hasLiveSessionActivity() {
			m.stopStaleLoadingIndicators()
		}
		// there is a number of things that could change the pills here so we want to re-render
		m.renderPills()
	case pubsub.Event[toolruntime.State]:
		if cmd := m.handleToolRuntimeEvent(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case pubsub.Event[timeline.Event]:
		if cmd := m.handleTimelineEvent(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}
		m.invalidateSidebarCache()
	case pubsub.Event[history.File]:
		cmds = append(cmds, m.handleFileEvent(msg.Payload))
		m.invalidateSidebarCache()
	case pubsub.Event[app.LSPEvent]:
		m.lspStates = app.GetLSPStates()
		m.invalidateSidebarCache()
	case pubsub.Event[mcp.Event]:
		switch msg.Payload.Type {
		case mcp.EventStateChanged:
			m.invalidateSidebarCache()
			return m, tea.Batch(
				m.handleStateChanged(),
				m.loadMCPrompts,
			)
		case mcp.EventPromptsListChanged:
			return m, handleMCPPromptsEvent(msg.Payload.Name)
		case mcp.EventToolsListChanged:
			m.invalidateSidebarCache()
			return m, handleMCPToolsEvent(m.com.Store(), msg.Payload.Name)
		case mcp.EventResourcesListChanged:
			return m, handleMCPResourcesEvent(msg.Payload.Name)
		}
	case pubsub.Event[skills.DiscoveryState]:
		m.skillsState = msg.Payload
		m.invalidateSidebarCache()
		m.updateLayoutAndSize()
	case pubsub.Event[permission.PermissionRequest]:
		if cmd := m.openPermissionsDialog(msg.Payload); cmd != nil {
			cmds = append(cmds, cmd)
		}
		if cmd := m.sendNotification(notification.Notification{
			Title:   "Crush is waiting...",
			Message: fmt.Sprintf("Permission required to execute \"%s\"", msg.Payload.ToolName),
		}); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case pubsub.Event[userinput.Request]:
		if cmd := m.openRequestUserInputDialog(msg.Payload); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case pubsub.Event[permission.PermissionNotification]:
		m.handlePermissionNotification(msg.Payload)
	case cancelTimerExpiredMsg:
		m.isCanceling = false
	case tea.TerminalVersionMsg:
		termVersion := strings.ToLower(msg.Name)
		// Only enable progress bar for the following terminals.
		if !m.sendProgressBar {
			m.sendProgressBar = strings.Contains(termVersion, "ghostty")
		}
		return m, nil
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.updateLayoutAndSize()
		if m.state == uiChat && m.chat.Follow() {
			if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	case tea.KeyboardEnhancementsMsg:
		m.keyenh = msg
		if msg.SupportsKeyDisambiguation() {
			m.keyMap.Models.SetHelp("ctrl+m", "models")
			m.keyMap.Editor.Newline.SetHelp("shift+enter", "newline")
		}
	case copyChatHighlightMsg:
		cmds = append(cmds, m.copyChatHighlight())
	case DelayedClickMsg:
		// Handle delayed single-click action (e.g., expansion).
		m.chat.HandleDelayedClick(msg)
	case tea.MouseClickMsg:
		// Pass mouse events to dialogs first if any are open.
		if m.dialog.HasDialogs() {
			m.dialog.Update(msg)
			return m, tea.Batch(cmds...)
		}

		if cmd := m.handleClickFocus(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}

		switch m.state {
		case uiChat:
			x, y := msg.X, msg.Y
			// Adjust for chat area position
			x -= m.layout.main.Min.X
			y -= m.layout.main.Min.Y
			if !image.Pt(msg.X, msg.Y).In(m.layout.sidebar) {
				if handled, cmd := m.chat.HandleMouseDown(x, y); handled {
					m.lastClickTime = time.Now()
					if m.chat.ClickCount() == 2 {
						if navCmd := m.openSelectedChildSession(); navCmd != nil {
							cmds = append(cmds, navCmd)
						}
					}
					if cmd != nil {
						cmds = append(cmds, cmd)
					}
				}
			}
		}

	case tea.MouseMotionMsg:
		// Pass mouse events to dialogs first if any are open.
		if m.dialog.HasDialogs() {
			m.dialog.Update(msg)
			return m, tea.Batch(cmds...)
		}

		switch m.state {
		case uiChat:
			if msg.Y <= 0 {
				if cmd := m.chat.ScrollByAndAnimate(-1); cmd != nil {
					cmds = append(cmds, cmd)
				}
				if !m.chat.SelectedItemInView() {
					m.chat.SelectPrev()
					if cmd := m.chat.ScrollToSelectedAndAnimate(); cmd != nil {
						cmds = append(cmds, cmd)
					}
				}
			} else if msg.Y >= m.chat.Height()-1 {
				if cmd := m.chat.ScrollByAndAnimate(1); cmd != nil {
					cmds = append(cmds, cmd)
				}
				if !m.chat.SelectedItemInView() {
					m.chat.SelectNext()
					if cmd := m.chat.ScrollToSelectedAndAnimate(); cmd != nil {
						cmds = append(cmds, cmd)
					}
				}
			}

			x, y := msg.X, msg.Y
			// Adjust for chat area position
			x -= m.layout.main.Min.X
			y -= m.layout.main.Min.Y
			m.chat.HandleMouseDrag(x, y)
		}

	case tea.MouseReleaseMsg:
		// Pass mouse events to dialogs first if any are open.
		if m.dialog.HasDialogs() {
			m.dialog.Update(msg)
			return m, tea.Batch(cmds...)
		}

		switch m.state {
		case uiChat:
			x, y := msg.X, msg.Y
			// Adjust for chat area position
			x -= m.layout.main.Min.X
			y -= m.layout.main.Min.Y
			if m.chat.HandleMouseUp(x, y) && m.chat.HasHighlight() {
				cmds = append(cmds, tea.Tick(doubleClickThreshold, func(t time.Time) tea.Msg {
					if time.Since(m.lastClickTime) >= doubleClickThreshold {
						return copyChatHighlightMsg{}
					}
					return nil
				}))
			}
		}
	case tea.MouseWheelMsg:
		// Pass mouse events to dialogs first if any are open.
		if m.dialog.HasDialogs() {
			m.dialog.Update(msg)
			return m, tea.Batch(cmds...)
		}

		// Otherwise handle mouse wheel for chat.
		switch m.state {
		case uiChat:
			switch msg.Button {
			case tea.MouseWheelUp:
				if cmd := m.chat.ScrollByAndAnimate(-MouseScrollThreshold); cmd != nil {
					cmds = append(cmds, cmd)
				}
				// After scrolling, anchor selection to the last visible item so the
				// viewport stays where the user scrolled (mirroring PageUp behaviour).
				// The old approach of SelectPrev + ScrollToSelectedAndAnimate caused
				// the view to jump back toward the selection when it drifted out of
				// the viewport.
				if !m.chat.SelectedItemInView() {
					m.chat.SelectLastInView()
				}
			case tea.MouseWheelDown:
				if cmd := m.chat.ScrollByAndAnimate(MouseScrollThreshold); cmd != nil {
					cmds = append(cmds, cmd)
				}
				// Mirror PageDown behaviour: keep selection inside the viewport.
				if !m.chat.SelectedItemInView() {
					if m.chat.AtBottom() {
						m.chat.SelectLast()
					} else {
						m.chat.SelectFirstInView()
					}
				}
			}
		}
	case anim.StepMsg:
		// Per-animation StepMsg is no longer used for driving animations
		// (the global ticker handles that), but we keep the handler for
		// any residual messages in flight.
	case anim.GlobalTickMsg:
		if m.state == uiChat {
			m.chat.TickAnimations()
		}
		// Keep the global ticker running as long as there are animations.
		if m.chat.HasAnimating() || m.todoIsSpinning {
			cmds = append(cmds, anim.GlobalTick())
		} else {
			m.globalTickActive = false
		}
	case spinner.TickMsg:
		if m.dialog.HasDialogs() {
			// route to dialog
			if cmd := m.handleDialogMsg(msg); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		if m.state == uiChat && m.hasSession() && hasInProgressTodo(m.session.Todos) && m.todoIsSpinning {
			var cmd tea.Cmd
			m.todoSpinner, cmd = m.todoSpinner.Update(msg)
			if cmd != nil {
				m.renderPills()
				cmds = append(cmds, cmd)
			}
		}

	case tea.KeyPressMsg:
		if cmd := m.handleKeyPressMsg(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case tea.PasteMsg:
		if cmd := m.handlePasteMsg(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case openEditorMsg:
		prevHeight := m.textarea.Height()
		m.textarea.SetValue(msg.Text)
		m.textarea.MoveToEnd()
		if cmd := m.updateTextareaWithPrevHeight(msg, prevHeight); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case message.Attachment:
		// Check for duplicate clipboard paste before adding.
		if m.shouldSkipClipboardAttachment(msg) {
			return m, tea.Batch(cmds...)
		}
		if m.attachments.Update(msg) {
			m.updateLayoutAndSize()
		}
		return m, tea.Batch(cmds...)
	case clipboardImageMsg:
		if cmd := m.handleClipboardImageMsg(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case clipboardPathsMsg:
		if cmd := m.handleClipboardPathsMsg(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case clipboardFallbackMsg:
		if cmd := m.handleClipboardFallback(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case modelSwitchPreparedMsg:
		if msg.err != nil {
			m.dialog.StopLoading()
			cmds = append(cmds, util.ReportError(msg.err))
			break
		}

		// Check if the dialog is still open - if closed, the user cancelled.
		if !m.dialog.ContainsDialog(dialog.ModelsID) {
			m.dialog.StopLoading()
			break
		}

		// Keep loading state active during complete phase to prevent concurrent switches.
		// Note: We don't call ApplyPreferredModel here anymore - completeModelSwitchCmd
		// will call UpdatePreferredModel which handles both memory and persistence atomically.

		var defaultSmallModel *config.SelectedModel
		cfg := m.com.Config()
		if cfg != nil {
			if _, ok := cfg.Models[config.SelectedModelTypeSmall]; !ok {
				smallModel := m.com.App.GetDefaultSmallModel(msg.action.Model.Provider)
				defaultSmallModel = &smallModel
			}
		}

		if msg.isOnboarding {
			m.com.Config().SetupAgents()
		}

		cmds = append(cmds, m.completeModelSwitchCmd(msg.action, defaultSmallModel, msg.isOnboarding))
	case modelSwitchCompletedMsg:
		m.dialog.StopLoading()
		if msg.err != nil {
			cmds = append(cmds, util.ReportError(msg.err))
			break
		}

		if dia := m.dialog.Dialog(dialog.ModelsID); dia != nil {
			if models, ok := dia.(*dialog.Models); ok {
				if err := models.HandleModelApplied(msg.modelType); err != nil {
					cmds = append(cmds, util.ReportError(err))
				}
			}
		}

		m.dialog.CloseDialog(dialog.APIKeyInputID)
		m.dialog.CloseDialog(dialog.OAuthID)
		if msg.closeDialog || msg.isOnboarding {
			m.dialog.CloseDialog(dialog.ModelsID)
		}

		if msg.isOnboarding {
			m.setState(uiLanding, uiFocusEditor)
		}

		cmds = append(cmds, util.ReportInfo(fmt.Sprintf("%s model changed to %s", msg.modelType, msg.modelName)))
	case promptEnhanceResultMsg:
		m.isEnhancingPrompt = false
		if msg.err != nil {
			cmds = append(cmds, util.ReportError(fmt.Errorf("prompt enhancement failed: %w", msg.err)))
			break
		}
		m.textarea.SetValue(msg.enhanced)
		m.textarea.CursorEnd()
		cmds = append(cmds, util.ReportInfo("Prompt enhanced."))
	case handoffGeneratedMsg:
		m.dialog.StopLoading()
		if msg.err != nil {
			cmds = append(cmds, util.ReportError(msg.err))
			break
		}
		m.dialog.CloseDialog(dialog.HandoffID)
		cmds = append(cmds, util.ReportInfo("Created handoff "+msg.title))
		if msg.sessionID != "" {
			cmds = append(cmds, m.loadSession(msg.sessionID))
		}
	case util.InfoMsg:
		m.statusMsgSeq++
		currentSeq := m.statusMsgSeq
		m.status.SetInfoMsg(msg)
		if msg.Type == util.InfoTypeError {
			break
		}
		ttl := msg.TTL
		if ttl <= 0 {
			ttl = DefaultStatusTTL
		}
		cmds = append(cmds, clearInfoMsgCmd(ttl, currentSeq))
	case util.ClearStatusMsg:
		if msg.Seq == m.statusMsgSeq {
			m.status.ClearInfoMsg()
		}
	case completions.CompletionItemsLoadedMsg:
		if m.completionsOpen {
			m.completions.SetItems(msg.Files, msg.Resources)
		}
	case uv.KittyGraphicsEvent:
		if !bytes.HasPrefix(msg.Payload, []byte("OK")) {
			slog.Warn("Unexpected Kitty graphics response",
				"response", string(msg.Payload),
				"options", msg.Options)
		}
	default:
		if m.dialog.HasDialogs() {
			if cmd := m.handleDialogMsg(msg); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	}

	// Refresh editor placeholder on state changes, but skip for
	// high-frequency messages (animation ticks, mouse, keyboard) to
	// avoid unnecessary work on every frame.
	switch msg.(type) {
	case anim.GlobalTickMsg, anim.StepMsg, spinner.TickMsg,
		tea.MouseMsg, tea.MouseClickMsg, tea.MouseMotionMsg,
		tea.MouseReleaseMsg, tea.MouseWheelMsg,
		tea.KeyPressMsg, tea.KeyReleaseMsg, tea.PasteMsg:
		// Skip placeholder refresh for high-frequency events.
	default:
		switch m.focus {
		case uiFocusMain:
		case uiFocusEditor:
			m.refreshEditorPlaceholder()
		}
	}

	// at this point this can only handle [message.Attachment] message, and we
	// should return all cmds anyway.
	_ = m.attachments.Update(msg)
	return m, tea.Batch(cmds...)
}

func (m *UI) setCurrentSessionMessages(msgs []message.Message) {
	m.sessionMessages = slices.Clone(msgs)
	// Rebuild the index from scratch.
	m.sessionMsgIndex = make(map[string]int, len(m.sessionMessages))
	for i, msg := range m.sessionMessages {
		m.sessionMsgIndex[msg.ID] = i
	}
}

func (m *UI) appendCurrentSessionMessage(msg message.Message) {
	if m.sessionMsgIndex == nil {
		m.sessionMsgIndex = make(map[string]int, len(m.sessionMessages))
		for i, sm := range m.sessionMessages {
			m.sessionMsgIndex[sm.ID] = i
		}
	}
	m.sessionMsgIndex[msg.ID] = len(m.sessionMessages)
	m.sessionMessages = append(m.sessionMessages, msg)
}

func (m *UI) updateCurrentSessionMessage(msg message.Message) {
	if m.sessionMsgIndex == nil {
		m.sessionMsgIndex = make(map[string]int, len(m.sessionMessages))
		for i, sm := range m.sessionMessages {
			m.sessionMsgIndex[sm.ID] = i
		}
	}
	if i, ok := m.sessionMsgIndex[msg.ID]; ok {
		m.sessionMessages[i] = msg
		return
	}
	m.sessionMsgIndex[msg.ID] = len(m.sessionMessages)
	m.sessionMessages = append(m.sessionMessages, msg)
}

func (m *UI) removeCurrentSessionMessage(messageID string) {
	if m.sessionMsgIndex == nil {
		m.sessionMsgIndex = make(map[string]int, len(m.sessionMessages))
		for i, sm := range m.sessionMessages {
			m.sessionMsgIndex[sm.ID] = i
		}
	}
	i, ok := m.sessionMsgIndex[messageID]
	if !ok {
		return
	}
	m.sessionMessages = slices.Delete(m.sessionMessages, i, i+1)
	// Rebuild index from the deleted position onward.
	delete(m.sessionMsgIndex, messageID)
	for j := i; j < len(m.sessionMessages); j++ {
		m.sessionMsgIndex[m.sessionMessages[j].ID] = j
	}
}

// setSessionMessages sets the messages for the current session in the chat
func (m *UI) setSessionMessages(msgs []message.Message) tea.Cmd {
	var cmds []tea.Cmd
	m.setCurrentSessionMessages(msgs)
	// Build tool result map to link tool calls with their results
	msgPtrs := make([]*message.Message, len(msgs))
	for i := range msgs {
		msgPtrs[i] = &msgs[i]
	}
	toolResultMap := chat.BuildToolResultMap(msgPtrs)
	if len(msgPtrs) > 0 {
		m.lastUserMessageTime = msgPtrs[0].CreatedAt
	}
	m.latestProposedPlan = ""

	// Add messages to chat with linked tool results.
	// Filter out incomplete summary messages with no content only when the
	// session has no active or queued work left. Otherwise they represent a
	// live summarization spinner that should remain visible across restores.
	items := make([]chat.MessageItem, 0, len(msgs)*2)
	for _, msg := range msgPtrs {
		if msg.IsSummaryMessage && !msg.IsFinished() && strings.TrimSpace(msg.Content().Text) == "" && !m.hasLiveSessionActivity() {
			continue
		}
		switch msg.Role {
		case message.User:
			m.lastUserMessageTime = msg.CreatedAt
			items = append(items, chat.ExtractMessageItems(m.com.Styles, msg, toolResultMap)...)
		case message.Assistant:
			m.updateLatestProposedPlan(*msg)
			items = append(items, chat.ExtractMessageItems(m.com.Styles, msg, toolResultMap)...)
			if msg.FinishPart() != nil && msg.FinishPart().Reason == message.FinishReasonEndTurn {
				infoItem := chat.NewAssistantInfoItem(m.com.Styles, msg, m.com.Config(), time.Unix(m.lastUserMessageTime, 0))
				items = append(items, infoItem)
			}
		default:
			items = append(items, chat.ExtractMessageItems(m.com.Styles, msg, toolResultMap)...)
		}
	}

	// Load nested tool calls for agent/agentic_fetch tools.
	m.loadNestedToolCalls(items)

	// Restore TaskNodeItems for agent tool calls with task graphs.
	// During live streaming these are created by ensureTaskNodes in
	// appendSessionMessage, but when restoring a session we must recreate
	// them from the stored message data.
	items = m.restoreTaskNodes(items, toolResultMap)

	// Load nested tool calls for each TaskNodeItem from its child session.
	m.loadTaskNodeNestedTools(items)

	// Restored sessions can contain incomplete assistant/tool messages left
	// behind by interruption. Keep their content, but suppress loading UI only
	// when there is no active or queued work left for the session.
	if !m.hasLiveSessionActivity() {
		m.setLoadingStateVisible(items, false)
	}

	// If the user switches between sessions while the agent still has active or
	// queued work, keep the loading animations visible.
	if m.hasLiveSessionActivity() {
		if cmd := m.startAnimations(items); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}

	m.chat.SetMessages(items...)
	if m.session != nil && m.com != nil && m.com.App != nil && m.com.App.GetToolRuntime() != nil {
		for _, state := range m.com.App.GetToolRuntime().ListBySession(m.session.ID) {
			item := m.chat.MessageItem(state.ToolCallID)
			if toolItem, ok := item.(chat.ToolMessageItem); ok {
				toolItem.SetRuntimeState(&state)
			}
		}
	}
	if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	m.chat.SelectLast()
	return tea.Sequence(cmds...)
}

func (m *UI) setLoadingStateVisible(items []chat.MessageItem, visible bool) {
	for _, item := range items {
		if controllable, ok := item.(chat.LoadingStateControllable); ok {
			controllable.SetLoadingStateVisible(visible)
		}

		nested, ok := item.(chat.NestedToolContainer)
		if !ok {
			continue
		}

		nestedTools := nested.NestedTools()
		nestedItems := make([]chat.MessageItem, 0, len(nestedTools))
		for _, tool := range nestedTools {
			nestedItems = append(nestedItems, tool)
		}
		m.setLoadingStateVisible(nestedItems, visible)
	}
}

// loadNestedToolCalls recursively loads nested tool calls for agent/agentic_fetch tools.
func (m *UI) loadNestedToolCalls(items []chat.MessageItem) {
	for _, item := range items {
		nestedContainer, ok := item.(chat.NestedToolContainer)
		if !ok {
			continue
		}
		toolItem, ok := item.(chat.ToolMessageItem)
		if !ok {
			continue
		}

		tc := toolItem.ToolCall()
		messageID := toolItem.MessageID()

		// Get the agent tool session ID.
		agentSessionID := m.com.App.Sessions.CreateAgentToolSessionID(messageID, tc.ID)

		// Fetch nested messages from the base session and any child sessions
		// with :: suffix (e.g. task graph sessions like "msg$$tc::task").
		var nestedMsgs []message.Message
		parentSessionID := ""
		if m.session != nil {
			parentSessionID = m.session.ID
		}
		for _, sessionID := range m.taskNodeChildSessionIDs(parentSessionID, agentSessionID) {
			msgs, err := m.com.App.Messages.List(context.Background(), sessionID)
			if err != nil {
				continue
			}
			nestedMsgs = append(nestedMsgs, msgs...)
		}
		if len(nestedMsgs) == 0 {
			continue
		}

		// Build tool result map for nested messages.
		nestedMsgPtrs := make([]*message.Message, len(nestedMsgs))
		for i := range nestedMsgs {
			nestedMsgPtrs[i] = &nestedMsgs[i]
		}
		nestedToolResultMap := chat.BuildToolResultMap(nestedMsgPtrs)

		// Extract nested tool items.
		var nestedTools []chat.ToolMessageItem
		for _, nestedMsg := range nestedMsgPtrs {
			nestedItems := chat.ExtractMessageItems(m.com.Styles, nestedMsg, nestedToolResultMap)
			for _, nestedItem := range nestedItems {
				if nestedToolItem, ok := nestedItem.(chat.ToolMessageItem); ok {
					// Mark nested tools as simple (compact) rendering.
					if simplifiable, ok := nestedToolItem.(chat.Compactable); ok {
						simplifiable.SetCompact(true)
					}
					nestedTools = append(nestedTools, nestedToolItem)
				}
			}
		}

		// Recursively load nested tool calls for any agent tools within.
		nestedMessageItems := make([]chat.MessageItem, len(nestedTools))
		for i, nt := range nestedTools {
			nestedMessageItems[i] = nt
		}
		m.loadNestedToolCalls(nestedMessageItems)

		// Set nested tools on the parent.
		nestedContainer.SetNestedTools(nestedTools)
	}
}

// restoreTaskNodes recreates TaskNodeItems for agent tool calls that contain a
// task graph. During live streaming these are created by ensureTaskNodes in
// appendSessionMessage, but when restoring a session (e.g. returning from a
// child session) we must recreate them from the stored message data.
// It also propagates completion status from the agent tool result to each node.
func (m *UI) restoreTaskNodes(items []chat.MessageItem, toolResultMap map[string]message.ToolResult) []chat.MessageItem {
	var result []chat.MessageItem
	for _, item := range items {
		result = append(result, item)

		toolItem, ok := item.(chat.ToolMessageItem)
		if !ok || toolItem.ToolCall().Name != agent.AgentToolName {
			continue
		}

		var params agentToolParams
		if err := json.Unmarshal([]byte(toolItem.ToolCall().Input), &params); err != nil || len(params.Tasks) <= 1 {
			continue
		}

		// Mark the parent agent item so the inline task list renders summary only.
		if agentItem, ok := item.(*chat.AgentToolMessageItem); ok {
			agentItem.SetHasTaskNodes(true)
		}

		// Parse task completion statuses from the agent tool result.
		var statuses map[string]message.ToolResultSubtaskStatus
		if tr, exists := toolResultMap[toolItem.ToolCall().ID]; exists {
			statuses = chat.ParseTaskStatusesFromAgentResult(&tr)
		}
		if statuses == nil {
			statuses = make(map[string]message.ToolResultSubtaskStatus)
		}

		tc := toolItem.ToolCall()
		messageID := toolItem.MessageID()
		for i, task := range params.Tasks {
			taskID := strings.TrimSpace(task.Name)
			if taskID == "" {
				taskID = strings.TrimSpace(task.ID)
			}
			if taskID == "" {
				continue
			}
			childSessionID := m.com.App.Sessions.CreateAgentToolSessionID(
				messageID,
				fmt.Sprintf("%s::%s", tc.ID, taskID),
			)
			node := chat.NewTaskNodeItem(
				m.com.Styles,
				tc.ID,
				taskID,
				strings.TrimSpace(task.Description),
				strings.TrimSpace(task.Assignment),
				task.SubagentType,
				childSessionID,
			)
			if status, ok := statuses[taskID]; ok {
				node.SetCompletionStatus(status)
			}
			node.SetTaskRef(agent.SubagentTaskRef(i, taskID, tc.ID))
			result = append(result, node)
		}
	}
	return result
}

// propagateTaskStatusesToNodes updates TaskNodeItems with completion statuses
// parsed from an agent tool result (lines like "- task_id: completed").
func (m *UI) propagateTaskStatusesToNodes(toolCallID string, result *message.ToolResult) {
	statuses := chat.ParseTaskStatusesFromAgentResult(result)
	for taskID, status := range statuses {
		nodeID := chat.TaskNodeItemID(toolCallID, taskID)
		if nodeItem := m.chat.MessageItem(nodeID); nodeItem != nil {
			if taskNode, ok := nodeItem.(*chat.TaskNodeItem); ok {
				taskNode.SetCompletionStatus(status)
			}
		}
	}
}

// loadTaskNodeNestedTools loads nested tool calls from child sessions into
// each TaskNodeItem. This provides the third-level collapsible operations
// inside each sub-task.
func (m *UI) loadTaskNodeNestedTools(items []chat.MessageItem) {
	parentSessionID := ""
	if m.session != nil {
		parentSessionID = m.session.ID
	}

	for _, item := range items {
		taskNode, ok := item.(*chat.TaskNodeItem)
		if !ok {
			continue
		}
		childSessionID := taskNode.ChildSessionID()
		if childSessionID == "" {
			continue
		}

		var nestedTools []chat.ToolMessageItem
		var restoredStatus message.ToolResultSubtaskStatus
		var hasRestoredStatus bool
		for _, sessionID := range m.taskNodeChildSessionIDs(parentSessionID, childSessionID) {
			nestedMsgs, err := m.com.App.Messages.List(context.Background(), sessionID)
			if err != nil || len(nestedMsgs) == 0 {
				continue
			}
			if status, ok := taskNodeCompletionStatusFromMessages(nestedMsgs); ok {
				restoredStatus = status
				hasRestoredStatus = true
			}

			nestedMsgPtrs := make([]*message.Message, len(nestedMsgs))
			for i := range nestedMsgs {
				nestedMsgPtrs[i] = &nestedMsgs[i]
			}
			nestedToolResultMap := chat.BuildToolResultMap(nestedMsgPtrs)

			for _, nestedMsg := range nestedMsgPtrs {
				nestedItems := chat.ExtractMessageItems(m.com.Styles, nestedMsg, nestedToolResultMap)
				for _, nestedItem := range nestedItems {
					if nestedToolItem, ok := nestedItem.(chat.ToolMessageItem); ok {
						if compactable, ok := nestedToolItem.(chat.Compactable); ok {
							compactable.SetCompact(true)
						}
						nestedTools = append(nestedTools, nestedToolItem)
					}
				}
			}
		}

		if len(nestedTools) > 0 {
			taskNode.SetNestedTools(nestedTools)
		}
		if taskNode.CompletionStatus() == "" && hasRestoredStatus {
			taskNode.SetCompletionStatus(restoredStatus)
		}
	}
}

func taskNodeCompletionStatusFromMessages(msgs []message.Message) (message.ToolResultSubtaskStatus, bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == message.Tool {
			for _, result := range msgs[i].ToolResults() {
				if subtask, ok := result.SubtaskResult(); ok && subtask.Status != "" {
					return subtask.Status, true
				}
			}
		}
		if msgs[i].Role != message.Assistant {
			continue
		}
		finish := msgs[i].FinishPart()
		if finish == nil {
			continue
		}
		switch finish.Reason {
		case message.FinishReasonEndTurn:
			return message.ToolResultSubtaskStatusCompleted, true
		case message.FinishReasonError, message.FinishReasonPermissionDenied:
			return message.ToolResultSubtaskStatusFailed, true
		case message.FinishReasonCanceled:
			return message.ToolResultSubtaskStatusCanceled, true
		}
	}
	return "", false
}

func (m *UI) taskNodeChildSessionIDs(parentSessionID, childSessionID string) []string {
	childSessionID = strings.TrimSpace(childSessionID)
	if childSessionID == "" {
		return nil
	}

	sessionIDs := []string{childSessionID}
	if strings.TrimSpace(parentSessionID) == "" || m.com == nil || m.com.App == nil || m.com.App.Sessions == nil {
		return sessionIDs
	}

	children, err := m.com.App.Sessions.ListChildren(context.Background(), parentSessionID)
	if err != nil {
		return sessionIDs
	}

	prefix := childSessionID + "::"
	var retries []string
	for _, child := range children {
		if strings.HasPrefix(child.ID, prefix) {
			retries = append(retries, child.ID)
		}
	}
	slices.Reverse(retries)
	return append(sessionIDs, retries...)
}

// appendSessionMessage appends a new message to the current session in the chat
// if the message is a tool result it will update the corresponding tool call message
func (m *UI) appendSessionMessage(msg message.Message) tea.Cmd {
	var cmds []tea.Cmd

	existing := m.chat.MessageItem(msg.ID)
	if existing != nil {
		// message already exists, skip
		return nil
	}
	m.appendCurrentSessionMessage(msg)

	switch msg.Role {
	case message.User:
		m.lastUserMessageTime = msg.CreatedAt
		items := chat.ExtractMessageItems(m.com.Styles, &msg, nil)
		if cmd := m.startAnimations(items); cmd != nil {
			cmds = append(cmds, cmd)
		}
		m.chat.AppendMessages(items...)
		if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case message.Assistant:
		m.updateLatestProposedPlan(msg)
		items := chat.ExtractMessageItems(m.com.Styles, &msg, nil)
		if cmd := m.startAnimations(items); cmd != nil {
			cmds = append(cmds, cmd)
		}
		m.chat.AppendMessages(items...)
		if m.chat.Follow() {
			if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		if msg.FinishPart() != nil && msg.FinishPart().Reason == message.FinishReasonEndTurn {
			infoItem := chat.NewAssistantInfoItem(m.com.Styles, &msg, m.com.Config(), time.Unix(m.lastUserMessageTime, 0))
			m.chat.AppendMessages(infoItem)
			if cmd := m.maybeOpenProposedPlanDialog(msg); cmd != nil {
				cmds = append(cmds, cmd)
			}
			if m.chat.Follow() {
				if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			}
		}
	case message.Tool:
		for _, tr := range msg.ToolResults() {
			toolItem := m.chat.MessageItem(tr.ToolCallID)
			if toolItem == nil {
				// we should have an item!
				continue
			}
			if toolMsgItem, ok := toolItem.(chat.ToolMessageItem); ok {
				toolMsgItem.SetResult(&tr)
				// Propagate task statuses to TaskNodeItems when the agent result arrives.
				if toolMsgItem.ToolCall().Name == agent.AgentToolName {
					m.propagateTaskStatusesToNodes(tr.ToolCallID, &tr)
				}
				if m.chat.Follow() {
					if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
						cmds = append(cmds, cmd)
					}
				}
			}
		}
	}
	return tea.Sequence(cmds...)
}

func (m *UI) handleClickFocus(msg tea.MouseClickMsg) (cmd tea.Cmd) {
	switch {
	case m.state != uiChat:
		return nil
	case image.Pt(msg.X, msg.Y).In(m.layout.sidebar):
		return nil
	case m.focus != uiFocusEditor && image.Pt(msg.X, msg.Y).In(m.layout.editor):
		m.focus = uiFocusEditor
		cmd = m.textarea.Focus()
		m.chat.Blur()
	case m.focus != uiFocusMain && image.Pt(msg.X, msg.Y).In(m.layout.main):
		m.focus = uiFocusMain
		m.textarea.Blur()
		m.chat.Focus()
	}
	return cmd
}

// updateSessionMessage updates an existing message in the current session in the chat
// when an assistant message is updated it may include updated tool calls as well
// that is why we need to handle creating/updating each tool call message too
func (m *UI) updateSessionMessage(msg message.Message) tea.Cmd {
	var cmds []tea.Cmd
	m.updateCurrentSessionMessage(msg)

	if msg.Role == message.Tool {
		for _, tr := range msg.ToolResults() {
			toolItem := m.chat.MessageItem(tr.ToolCallID)
			if toolItem != nil {
				if toolMsgItem, ok := toolItem.(chat.ToolMessageItem); ok {
					toolMsgItem.SetResult(&tr)
				}
			}
		}
		return nil
	}

	existingItem := m.chat.MessageItem(msg.ID)
	shouldRenderAssistant := chat.ShouldRenderAssistantMessage(&msg)
	var toolCallIDs map[string]struct{}
	if tcs := msg.ToolCalls(); len(tcs) > 0 {
		toolCallIDs = make(map[string]struct{}, len(tcs))
		for _, tc := range tcs {
			toolCallIDs[tc.ID] = struct{}{}
		}
	}

	if existingItem != nil {
		if assistantItem, ok := existingItem.(*chat.AssistantMessageItem); ok {
			assistantItem.SetMessage(&msg)
		}
	} else if shouldRenderAssistant {
		assistantItem := chat.NewAssistantMessageItem(m.com.Styles, &msg)
		inserted := false
		if toolCalls := msg.ToolCalls(); len(toolCalls) > 0 {
			inserted = m.chat.InsertMessagesBefore(toolCalls[0].ID, assistantItem)
		}
		if !inserted {
			m.chat.AppendMessages(assistantItem)
		}
		if cmd := m.startAnimations([]chat.MessageItem{assistantItem}); cmd != nil {
			cmds = append(cmds, cmd)
		}
		if m.chat.Follow() {
			if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		existingItem = assistantItem
	}
	m.updateLatestProposedPlan(msg)

	// if the message of the assistant does not have any  response just tool calls we need to remove it
	if !shouldRenderAssistant && len(msg.ToolCalls()) > 0 && existingItem != nil {
		m.chat.RemoveMessage(msg.ID)
		if infoItem := m.chat.MessageItem(chat.AssistantInfoID(msg.ID)); infoItem != nil {
			m.chat.RemoveMessage(chat.AssistantInfoID(msg.ID))
		}
	}

	m.removeToolItemsForMessage(msg.ID, toolCallIDs)

	if shouldRenderAssistant && msg.FinishPart() != nil && msg.FinishPart().Reason == message.FinishReasonEndTurn {
		if infoItem := m.chat.MessageItem(chat.AssistantInfoID(msg.ID)); infoItem == nil {
			newInfoItem := chat.NewAssistantInfoItem(m.com.Styles, &msg, m.com.Config(), time.Unix(m.lastUserMessageTime, 0))
			m.chat.AppendMessages(newInfoItem)
		}
		if cmd := m.maybeOpenProposedPlanDialog(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}

	isCanceled := msg.FinishReason() == message.FinishReasonCanceled
	var items []chat.MessageItem
	for _, tc := range msg.ToolCalls() {
		existingToolItem := m.chat.MessageItem(tc.ID)
		if toolItem, ok := existingToolItem.(chat.ToolMessageItem); ok {
			existingToolCall := toolItem.ToolCall()
			// only update if finished state changed or input changed
			// to avoid clearing the cache
			if (tc.Finished && !existingToolCall.Finished) || tc.Input != existingToolCall.Input {
				toolItem.SetToolCall(tc)
			}
			if isCanceled && toolItem.Status() != chat.ToolStatusCanceled {
				toolItem.SetStatus(chat.ToolStatusCanceled)
			}
		}
		if existingToolItem == nil {
			newItem := chat.NewToolMessageItem(m.com.Styles, msg.ID, tc, nil, isCanceled)
			items = append(items, newItem)
		}
		// Create TaskNodeItems for agent tool calls with a tasks array.
		// This runs for both new and existing tool items because during
		// streaming the tasks JSON may arrive after the initial tool call.
		if tc.Name == agent.AgentToolName {
			items = append(items, m.ensureTaskNodes(msg.ID, tc, existingToolItem)...)
		}
	}

	if cmd := m.startAnimations(items); cmd != nil {
		cmds = append(cmds, cmd)
	}

	m.chat.AppendMessages(items...)
	if m.chat.Follow() {
		if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		m.chat.SelectLast()
	}

	return tea.Sequence(cmds...)
}

// ensureTaskNodes creates TaskNodeItems for an agent tool call with tasks.
// It also marks the parent AgentToolMessageItem so it only shows the summary.
func (m *UI) ensureTaskNodes(messageID string, tc message.ToolCall, existing chat.MessageItem) []chat.MessageItem {
	var params agentToolParams
	if err := json.Unmarshal([]byte(tc.Input), &params); err != nil || len(params.Tasks) <= 1 {
		return nil
	}

	// Resolve the agent tool item — either from the existing item or from
	// the chat list (if ensureTaskNodes is called for a newly created item
	// that hasn't been appended yet, existing is nil, but the item was just
	// appended in the same batch so check the chat too).
	agentItem, _ := existing.(*chat.AgentToolMessageItem)
	if agentItem == nil {
		if item := m.chat.MessageItem(tc.ID); item != nil {
			agentItem, _ = item.(*chat.AgentToolMessageItem)
		}
	}
	if agentItem != nil {
		agentItem.SetHasTaskNodes(true)
	}

	var items []chat.MessageItem
	for i, task := range params.Tasks {
		taskID := strings.TrimSpace(task.Name)
		if taskID == "" {
			continue
		}
		nodeID := chat.TaskNodeItemID(tc.ID, taskID)
		if m.chat.MessageItem(nodeID) != nil {
			continue
		}
		childSessionID := m.com.App.Sessions.CreateAgentToolSessionID(
			messageID,
			fmt.Sprintf("%s::%s", tc.ID, taskID),
		)
		node := chat.NewTaskNodeItem(
			m.com.Styles,
			tc.ID,
			taskID,
			strings.TrimSpace(task.Description),
			strings.TrimSpace(task.Assignment),
			task.SubagentType,
			childSessionID,
		)
		node.SetTaskRef(agent.SubagentTaskRef(i, taskID, tc.ID))
		items = append(items, node)
	}
	return items
}

func (m *UI) removeToolItemsForMessage(messageID string, keepToolCallIDs map[string]struct{}) {
	// Use the secondary index to find tool calls for this message instead
	// of scanning the entire list O(n). This is critical during streaming
	// updates in long conversations where the list can have hundreds of items.
	allToolCallIDs := m.chat.ToolCallIDsForMessage(messageID)
	if len(allToolCallIDs) == 0 {
		return
	}

	var toolItemIDs []string
	for toolCallID := range allToolCallIDs {
		if _, keep := keepToolCallIDs[toolCallID]; keep {
			continue
		}
		toolItemIDs = append(toolItemIDs, toolCallID)
	}

	removedToolCallIDs := make(map[string]struct{}, len(toolItemIDs))
	for _, toolItemID := range toolItemIDs {
		removedToolCallIDs[toolItemID] = struct{}{}
		m.chat.RemoveMessage(toolItemID)
	}
	m.chat.RemoveTaskNodesForRemovedToolCalls(removedToolCallIDs)
}

func syncNestedToolsForMessage(
	nestedTools []chat.ToolMessageItem,
	messageID string,
	keepToolCallIDs map[string]struct{},
) []chat.ToolMessageItem {
	filtered := make([]chat.ToolMessageItem, 0, len(nestedTools))
	for _, nestedTool := range nestedTools {
		if nestedTool.MessageID() != messageID {
			filtered = append(filtered, nestedTool)
			continue
		}
		if _, keep := keepToolCallIDs[nestedTool.ToolCall().ID]; keep {
			filtered = append(filtered, nestedTool)
		}
	}
	return filtered
}

// handleChildSessionMessage handles messages from child sessions (agent tools).
func (m *UI) handleChildSessionMessage(event pubsub.Event[message.Message]) tea.Cmd {
	var cmds []tea.Cmd

	// Check if this is an agent tool session and parse it.
	childSessionID := event.Payload.SessionID
	_, toolCallID, ok := m.com.App.Sessions.ParseAgentToolSessionID(childSessionID)
	if !ok {
		return nil
	}

	// If this is a task-graph task session (toolCallID = "tc.ID::taskID"),
	// update the corresponding TaskNodeItem status directly.
	if tcPart, taskPart, found := strings.Cut(toolCallID, "::"); found && taskPart != "" {
		baseTaskID, _, _ := strings.Cut(taskPart, "::")
		nodeID := chat.TaskNodeItemID(tcPart, baseTaskID)
		if nodeItem := m.chat.MessageItem(nodeID); nodeItem != nil {
			if taskNode, ok := nodeItem.(*chat.TaskNodeItem); ok {
				if statusText, isError, ok := childSessionStatus(event.Payload); ok {
					if event.Type == pubsub.DeletedEvent {
						taskNode.ClearChildSessionStatus()
					} else {
						taskNode.SetChildSessionStatus(statusText, isError)
					}
				} else {
					taskNode.ClearChildSessionStatus()
				}
				if status, ok := taskNodeCompletionStatusFromMessages([]message.Message{event.Payload}); ok {
					taskNode.SetCompletionStatus(status)
					if status == message.ToolResultSubtaskStatusCompleted || status == message.ToolResultSubtaskStatus("completed_with_warnings") {
						taskNode.ClearChildSessionStatus()
					}
				}
			}
		}
	}

	// Resolve nested tool target: task node first for task graph sessions,
	// otherwise fall back to the parent agent tool item.
	var nestedTarget chat.NestedToolContainer
	if tcPart, taskPart, found := strings.Cut(toolCallID, "::"); found && taskPart != "" {
		baseTaskID, _, _ := strings.Cut(taskPart, "::")
		nodeID := chat.TaskNodeItemID(tcPart, baseTaskID)
		if nodeItem := m.chat.MessageItem(nodeID); nodeItem != nil {
			if target, ok := nodeItem.(chat.NestedToolContainer); ok {
				nestedTarget = target
			}
		}
	}

	resolveParentToolCallID := func(candidate string) string {
		for candidate != "" {
			if item := m.chat.MessageItem(candidate); item != nil {
				if nested, ok := item.(chat.NestedToolContainer); ok {
					if toolMessageItem, ok := item.(chat.ToolMessageItem); ok && toolMessageItem.ToolCall().ID == candidate {
						nestedTarget = nested
						return candidate
					}
				}
			}
			next, _, found := strings.Cut(candidate, "::")
			if !found || next == candidate {
				break
			}
			candidate = next
		}
		return ""
	}
	if nestedTarget == nil && resolveParentToolCallID(toolCallID) == "" {
		return nil
	}

	if nestedTarget == nil {
		return nil
	}

	if statusItem, ok := nestedTarget.(chat.ChildSessionStatusSetter); ok {
		if statusText, isError, ok := childSessionStatus(event.Payload); ok {
			if event.Type == pubsub.DeletedEvent {
				statusItem.ClearChildSessionStatus()
			} else {
				statusItem.SetChildSessionStatus(statusText, isError)
			}
		} else {
			statusItem.ClearChildSessionStatus()
		}
	}

	// Get existing nested tools.
	nestedTools := nestedTarget.NestedTools()

	// Capture old nested tool IDs before any modifications for incremental
	// ID index map updates. This avoids a full rebuildIDIndexMap() scan of
	// all chat items on every child session message (e.g. every streamed
	// token from a sub-agent).
	containerID := ""
	if item, ok := nestedTarget.(chat.MessageItem); ok {
		containerID = item.ID()
	}
	oldNestedIDs := make([]string, 0, len(nestedTools))
	for _, nt := range nestedTools {
		oldNestedIDs = append(oldNestedIDs, nt.ID())
	}

	if event.Payload.Role == message.Assistant {
		toolCallIDs := make(map[string]struct{}, len(event.Payload.ToolCalls()))
		for _, tc := range event.Payload.ToolCalls() {
			toolCallIDs[tc.ID] = struct{}{}
		}
		nestedTools = syncNestedToolsForMessage(nestedTools, event.Payload.ID, toolCallIDs)
	}

	// Only process tool call and result updates below this point.
	if len(event.Payload.ToolCalls()) == 0 && len(event.Payload.ToolResults()) == 0 {
		nestedTarget.SetNestedTools(nestedTools)
		m.chat.UpdateNestedToolIDsIncremental(containerID, oldNestedIDs)
		return nil
	}

	if event.Type == pubsub.DeletedEvent {
		nestedTarget.SetNestedTools(nestedTools)
		m.chat.UpdateNestedToolIDsIncremental(containerID, oldNestedIDs)
		return nil
	}

	// Update or create nested tool calls.
	for _, tc := range event.Payload.ToolCalls() {
		found := false
		for _, existingTool := range nestedTools {
			if existingTool.ToolCall().ID == tc.ID {
				existingTool.SetToolCall(tc)
				found = true
				break
			}
		}
		if !found {
			// Create a new nested tool item.
			nestedItem := chat.NewToolMessageItem(m.com.Styles, event.Payload.ID, tc, nil, false)
			if simplifiable, ok := nestedItem.(chat.Compactable); ok {
				simplifiable.SetCompact(true)
			}
			if animatable, ok := nestedItem.(chat.Animatable); ok {
				if animatable.IsAnimating() {
					m.chat.RegisterAnimation(nestedItem.ID())
				}
			}
			nestedTools = append(nestedTools, nestedItem)
		}
	}

	// Update nested tool results.
	for _, tr := range event.Payload.ToolResults() {
		for _, nestedTool := range nestedTools {
			if nestedTool.ToolCall().ID == tr.ToolCallID {
				nestedTool.SetResult(&tr)
				break
			}
		}
	}

	if !m.hasLiveSessionActivity() {
		for _, nestedTool := range nestedTools {
			if controllable, ok := nestedTool.(chat.LoadingStateControllable); ok {
				controllable.SetLoadingStateVisible(false)
			}
		}
	}

	// Update the agent item with the new nested tools.
	nestedTarget.SetNestedTools(nestedTools)

	// Incrementally update the ID map instead of a full rebuild.
	m.chat.UpdateNestedToolIDsIncremental(containerID, oldNestedIDs)

	if m.chat.Follow() {
		if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		m.chat.SelectLast()
	}

	return tea.Sequence(cmds...)
}

func (m *UI) handleDialogMsg(msg tea.Msg) tea.Cmd {
	var cmds []tea.Cmd
	action := m.dialog.Update(msg)
	if action == nil {
		return tea.Batch(cmds...)
	}

	isOnboarding := m.state == uiOnboarding

	switch msg := action.(type) {
	// Generic dialog messages
	case dialog.ActionClose:
		if isOnboarding && m.dialog.ContainsDialog(dialog.ModelsID) {
			break
		}

		if m.dialog.ContainsDialog(dialog.FilePickerID) {
			defer fimage.ResetCache()
		}

		m.dialog.CloseFrontDialog()

		if isOnboarding {
			if cmd := m.openModelsDialog(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}

		if m.focus == uiFocusEditor {
			cmds = append(cmds, m.textarea.Focus())
		}
	case dialog.ActionCmd:
		if msg.Cmd != nil {
			cmds = append(cmds, msg.Cmd)
		}

	// Session dialog messages.
	case dialog.ActionSelectSession:
		m.dialog.CloseDialog(dialog.SessionsID)
		cmds = append(cmds, m.loadSession(msg.Session.ID))

	// Open dialog message.
	case dialog.ActionOpenDialog:
		m.dialog.CloseDialog(dialog.CommandsID)
		if cmd := m.openDialog(msg.DialogID); cmd != nil {
			cmds = append(cmds, cmd)
		}

	// Command dialog messages.
	case dialog.ActionTogglePlanMode:
		if m.isAgentBusy() {
			cmds = append(cmds, util.ReportWarn("Agent is busy, please wait before changing Plan Mode..."))
			break
		}
		cmds = append(cmds, func() tea.Msg {
			ctx := context.Background()
			sessionID := msg.SessionID
			if sessionID == "" {
				if msg.NextMode != session.CollaborationModePlan {
					return util.ReportError(errors.New("cannot exit Plan Mode without an active session"))()
				}
				newSession, err := m.com.App.Sessions.Create(ctx, "New Session")
				if err != nil {
					return util.ReportError(err)()
				}
				sessionID = newSession.ID
			}
			_, err := m.com.App.Sessions.UpdateCollaborationMode(ctx, sessionID, msg.NextMode)
			if err != nil {
				return util.ReportError(err)()
			}
			status := "Plan Mode disabled"
			if msg.NextMode == session.CollaborationModePlan {
				status = "Plan Mode enabled"
			}
			return planModeChangedMsg{SessionID: sessionID, Status: status, Mode: msg.NextMode}
		})
		m.dialog.CloseDialog(dialog.CommandsID)
	case dialog.ActionToggleOrchestrateMode:
		if m.isAgentBusy() {
			cmds = append(cmds, util.ReportWarn("Agent is busy, please wait before changing Orchestrate Mode..."))
			break
		}
		cmds = append(cmds, func() tea.Msg {
			ctx := context.Background()
			sessionID := msg.SessionID
			if sessionID == "" {
				if msg.NextMode != session.CollaborationModeOrchestrate {
					return util.ReportError(errors.New("cannot exit Orchestrate Mode without an active session"))()
				}
				newSession, err := m.com.App.Sessions.Create(ctx, "New Session")
				if err != nil {
					return util.ReportError(err)()
				}
				sessionID = newSession.ID
			}
			_, err := m.com.App.Sessions.UpdateCollaborationMode(ctx, sessionID, msg.NextMode)
			if err != nil {
				return util.ReportError(err)()
			}
			status := "Orchestrate Mode disabled"
			if msg.NextMode == session.CollaborationModeOrchestrate {
				status = "Orchestrate Mode enabled"
			}
			return planModeChangedMsg{SessionID: sessionID, Status: status, Mode: msg.NextMode}
		})
		m.dialog.CloseDialog(dialog.CommandsID)
	case dialog.ActionToggleAutoMode:
		if m.isAgentBusy() {
			cmds = append(cmds, util.ReportWarn("Agent is busy, please wait before changing execution mode..."))
			break
		}
		targetMode := executionModeAsk
		switch msg.NextMode {
		case session.PermissionModeAuto:
			targetMode = executionModeAuto
		case session.PermissionModeYolo:
			targetMode = executionModeYolo
		}
		if cmd := m.setExecutionMode(targetMode); cmd != nil {
			cmds = append(cmds, cmd)
		}
		m.dialog.CloseDialog(dialog.CommandsID)
	case dialog.ActionExecuteProposedPlan:
		if m.isAgentBusy() {
			cmds = append(cmds, util.ReportWarn("Agent is busy, please wait before executing the plan..."))
			break
		}
		if strings.TrimSpace(msg.Plan) == "" {
			cmds = append(cmds, util.ReportWarn("No proposed plan found in this session."))
			break
		}
		cmds = append(cmds, m.executeApprovedPlan(msg.SessionID, msg.Plan))
		m.dialog.CloseDialog(dialog.CommandsID)
		m.dialog.CloseDialog(dialog.ProposedPlanID)
	case dialog.ActionSubmitPlanFeedback:
		feedback := strings.TrimSpace(msg.Feedback)
		if feedback == "" {
			cmds = append(cmds, util.ReportWarn("Please enter feedback for the plan revision."))
			break
		}
		cmds = append(cmds, m.sendMessage(feedback))
		m.dialog.CloseDialog(dialog.ProposedPlanID)
	case dialog.ActionToggleNotifications:
		cfg := m.com.Config()
		if cfg != nil && cfg.Options != nil {
			disabled := !cfg.Options.DisableNotifications
			cfg.Options.DisableNotifications = disabled
			// Persist the notification setting to config asynchronously.
			cmds = append(cmds, func() tea.Msg {
				if err := m.com.Store().SetConfigField(config.ScopeGlobal, "options.disable_notifications", disabled); err != nil {
					slog.Error("Failed to persist notification setting", "error", err)
				}
				status := "enabled"
				if disabled {
					status = "disabled"
				}
				return util.NewInfoMsg("Notifications " + status)
			})
		}
		m.dialog.CloseDialog(dialog.CommandsID)
	case dialog.ActionNewSession:
		if m.isAgentBusy() {
			cmds = append(cmds, util.ReportWarn("Agent is busy, please wait before starting a new session..."))
			break
		}
		if cmd := m.newSession(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		m.dialog.CloseDialog(dialog.CommandsID)
	case dialog.ActionSummarize:
		if m.isAgentBusy() {
			cmds = append(cmds, util.ReportWarn("Agent is busy, please wait before summarizing session..."))
			break
		}
		cmds = append(cmds, func() tea.Msg {
			err := m.com.App.AgentCoordinator.Summarize(context.Background(), msg.SessionID, nil)
			if err != nil {
				return util.ReportError(err)()
			}
			return nil
		})
		m.dialog.CloseDialog(dialog.CommandsID)
	case dialog.ActionGenerateHandoff:
		if m.isAgentBusy() {
			cmds = append(cmds, util.ReportWarn("Agent is busy, please wait before creating a handoff..."))
			break
		}
		if m.com.App.AgentCoordinator == nil {
			cmds = append(cmds, util.ReportWarn("Agent is not configured."))
			break
		}
		if cmd := m.dialog.StartLoading(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		cmds = append(cmds, func() tea.Msg {
			draft, err := m.com.App.AgentCoordinator.GenerateHandoff(context.Background(), msg.SessionID, msg.Goal)
			if err != nil {
				return handoffGeneratedMsg{err: err}
			}
			handoffSession, err := m.com.App.Sessions.CreateHandoffSession(context.Background(), msg.SessionID, draft.Title, msg.Goal, draft.Prompt, draft.RelevantFiles)
			if err != nil {
				return handoffGeneratedMsg{err: err}
			}
			return handoffGeneratedMsg{
				sessionID: handoffSession.ID,
				title:     handoffSession.Title,
			}
		})
	case dialog.ActionToggleHelp:
		if shortcuts, err := dialog.NewKeyboardShortcuts(m.com); err == nil {
			m.dialog.OpenDialog(shortcuts)
		}
		m.dialog.CloseDialog(dialog.CommandsID)
	case dialog.ActionExternalEditor:
		if m.isAgentBusy() {
			cmds = append(cmds, util.ReportWarn("Agent is working, please wait..."))
			break
		}
		cmds = append(cmds, m.openEditor(m.textarea.Value()))
		m.dialog.CloseDialog(dialog.CommandsID)
	case dialog.ActionToggleCompactMode:
		cmds = append(cmds, m.toggleCompactMode())
		m.dialog.CloseDialog(dialog.CommandsID)
	case dialog.ActionTogglePills:
		if cmd := m.togglePillsExpanded(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		m.dialog.CloseDialog(dialog.CommandsID)
	case dialog.ActionPauseQueue:
		if cmd := m.pauseQueue(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		m.dialog.CloseDialog(dialog.CommandsID)
	case dialog.ActionResumeQueue:
		if cmd := m.resumeQueue(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		m.dialog.CloseDialog(dialog.CommandsID)
	case dialog.ActionToggleThinking:
		cmds = append(cmds, func() tea.Msg {
			if m.isAgentBusy() {
				return util.ReportWarn("Agent is busy, please wait...")()
			}
			cfg := m.com.Config()
			if cfg == nil {
				return util.ReportError(errors.New("configuration not found"))()
			}

			agentCfg, ok := cfg.Agents[config.AgentCoder]
			if !ok {
				return util.ReportError(errors.New("agent configuration not found"))()
			}

			currentModel := cfg.Models[agentCfg.Model]
			thinkingCurrentlyEnabled := currentModel.Think == nil || *currentModel.Think
			newThinkValue := !thinkingCurrentlyEnabled
			currentModel.Think = &newThinkValue
			if err := m.com.Store().UpdatePreferredModel(config.ScopeGlobal, agentCfg.Model, currentModel); err != nil {
				return util.ReportError(err)()
			}
			if err := m.com.App.UpdateAgentModel(context.TODO()); err != nil {
				return util.ReportError(err)()
			}
			status := "disabled"
			if newThinkValue {
				status = "enabled"
			}
			return util.NewInfoMsg("Thinking mode " + status)
		})
		m.dialog.CloseDialog(dialog.CommandsID)
	case dialog.ActionToggleTransparentBackground:
		cmds = append(cmds, func() tea.Msg {
			cfg := m.com.Config()
			if cfg == nil {
				return util.ReportError(errors.New("configuration not found"))()
			}

			isTransparent := cfg.Options != nil && cfg.Options.TUI.Transparent != nil && *cfg.Options.TUI.Transparent
			newValue := !isTransparent
			if err := m.com.Store().SetTransparentBackground(config.ScopeGlobal, newValue); err != nil {
				return util.ReportError(err)()
			}
			m.isTransparent = newValue

			status := "disabled"
			if newValue {
				status = "enabled"
			}
			return util.NewInfoMsg("Transparent background " + status)
		})
		m.dialog.CloseDialog(dialog.CommandsID)
	case dialog.ActionAuthenticateMCP:
		cmds = append(cmds, m.authenticateMCP(msg.Name))
	case dialog.ActionReconnectMCP:
		cmds = append(cmds, m.reconnectMCP(msg.Name))
	case dialog.ActionOpenMCPDetail:
		mcpCfg, ok := m.com.Config().MCP[msg.Name]
		if !ok {
			cmds = append(cmds, util.ReportWarn("MCP server not found: "+msg.Name))
			break
		}
		state := m.mcpStates[msg.Name]
		detailDialog := dialog.NewMCPDetail(m.com, msg.Name, state, mcpCfg)
		m.dialog.OpenDialog(detailDialog)
	case dialog.ActionToggleMCP:
		cmds = append(cmds, m.toggleMCP(msg.Name, msg.Enable))
	case dialog.ActionQuit:
		cmds = append(cmds, tea.Quit)
	case dialog.ActionEnableDockerMCP:
		m.dialog.CloseDialog(dialog.CommandsID)
		cmds = append(cmds, m.enableDockerMCP)
	case dialog.ActionDisableDockerMCP:
		m.dialog.CloseDialog(dialog.CommandsID)
		cmds = append(cmds, m.disableDockerMCP)
	case dialog.ActionInitializeProject:
		if m.isAgentBusy() {
			cmds = append(cmds, util.ReportWarn("Agent is busy, please wait before summarizing session..."))
			break
		}
		cmds = append(cmds, m.initializeProject())
		m.dialog.CloseDialog(dialog.CommandsID)

	case dialog.ActionSelectModel:
		if m.isAgentBusy() {
			cmds = append(cmds, util.ReportWarn("Agent is busy, please wait..."))
			break
		}

		cfg := m.com.Config()
		if cfg == nil {
			cmds = append(cmds, util.ReportError(errors.New("configuration not found")))
			break
		}

		var (
			providerID   = msg.Model.Provider
			isCopilot    = providerID == string(catwalk.InferenceProviderCopilot)
			isConfigured = func() bool { _, ok := cfg.Providers.Get(providerID); return ok }
		)

		// Attempt to import GitHub Copilot tokens from VSCode if available.
		if isCopilot && !isConfigured() && !msg.ReAuthenticate {
			m.com.Store().ImportCopilot()
		}

		if !isConfigured() || msg.ReAuthenticate {
			m.dialog.CloseDialog(dialog.ModelsID)
			if cmd := m.openAuthenticationDialog(msg.Provider, msg.Model, msg.ModelType); cmd != nil {
				cmds = append(cmds, cmd)
			}
			break
		}

		m.dialog.CloseDialog(dialog.APIKeyInputID)
		m.dialog.CloseDialog(dialog.OAuthID)

		sessionID := ""
		if m.session != nil {
			sessionID = m.session.ID
		}

		if cmd := m.dialog.StartLoading(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		cmds = append(cmds, m.prepareModelSwitchCmd(msg, sessionID, isOnboarding))
	case dialog.ActionSelectReasoningEffort:
		if m.isAgentBusy() {
			cmds = append(cmds, util.ReportWarn("Agent is busy, please wait..."))
			break
		}

		cfg := m.com.Config()
		if cfg == nil {
			cmds = append(cmds, util.ReportError(errors.New("configuration not found")))
			break
		}

		agentCfg, ok := cfg.Agents[config.AgentCoder]
		if !ok {
			cmds = append(cmds, util.ReportError(errors.New("agent configuration not found")))
			break
		}

		currentModel := cfg.Models[agentCfg.Model]
		if err := m.com.Store().UpdatePreferredModel(config.ScopeGlobal, agentCfg.Model, currentModel); err != nil {
			cmds = append(cmds, util.ReportError(err))
			break
		}
		providerKey := fmt.Sprintf("models.%s.provider_options.reasoning_effort", agentCfg.Model)
		if err := m.com.Store().SetConfigField(config.ScopeGlobal, providerKey, msg.Effort); err != nil {
			cmds = append(cmds, util.ReportError(err))
			break
		}

		cmds = append(cmds, func() tea.Msg {
			if err := m.com.App.UpdateAgentModel(context.TODO()); err != nil {
				return util.ReportError(err)
			}
			return util.NewInfoMsg("Reasoning effort set to " + msg.Effort)
		})
		m.dialog.CloseDialog(dialog.ReasoningID)
	case dialog.ActionPermissionResponse:
		m.dialog.CloseDialog(dialog.PermissionsID)
		switch msg.Action {
		case dialog.PermissionAllow:
			m.com.App.Permissions.Grant(msg.Permission)
		case dialog.PermissionAllowForSession:
			m.com.App.Permissions.GrantPersistent(msg.Permission)
		case dialog.PermissionDeny:
			m.com.App.Permissions.Deny(msg.Permission)
		}

	case dialog.ActionApproveDenial:
		// User approved a previously denied action from the denial queue.
		// Grant the permission for session so it can be retried without blocking.
		m.dialog.CloseDialog(dialog.DenialsID)
		if m.session != nil {
			if editor := m.com.App.Permissions.GetDenialQueueEditor(m.session.ID); editor != nil {
				if entry := editor.Take(msg.EntryID); entry != nil {
					// Grant persistent permission for this action so it won't block again
					m.com.App.Permissions.GrantPersistent(entry.Request)
					cmds = append(cmds, util.ReportInfo(fmt.Sprintf("Approved: %s %s. You can retry the action.", entry.Request.ToolName, entry.Request.Action)))
				} else {
					cmds = append(cmds, util.ReportInfo("Denial entry not found. It may have been already processed."))
				}
			} else {
				cmds = append(cmds, util.ReportInfo("Could not access denial queue."))
			}
		}

	case dialog.ActionFilePickerSelected:
		cmds = append(cmds, tea.Sequence(
			msg.Cmd(),
			func() tea.Msg {
				m.dialog.CloseDialog(dialog.FilePickerID)
				return nil
			},
			func() tea.Msg {
				fimage.ResetCache()
				return nil
			},
		))

	case dialog.ActionRunCustomCommand:
		if len(msg.Arguments) > 0 && msg.Args == nil {
			m.dialog.CloseFrontDialog()
			argsDialog := dialog.NewArguments(
				m.com,
				"Custom Command Arguments",
				"",
				msg.Arguments,
				msg, // Pass the action as the result
			)
			m.dialog.OpenDialog(argsDialog)
			break
		}
		content := msg.Content
		if msg.Args != nil {
			content = substituteArgs(content, msg.Args)
		}
		cmds = append(cmds, m.sendMessage(content))
		m.dialog.CloseFrontDialog()
	case dialog.ActionRunMCPPrompt:
		if len(msg.Arguments) > 0 && msg.Args == nil {
			m.dialog.CloseFrontDialog()
			title := cmp.Or(msg.Title, "MCP Prompt Arguments")
			argsDialog := dialog.NewArguments(
				m.com,
				title,
				msg.Description,
				msg.Arguments,
				msg, // Pass the action as the result
			)
			m.dialog.OpenDialog(argsDialog)
			break
		}
		cmds = append(cmds, m.runMCPPrompt(msg.ClientID, msg.PromptID, msg.Args))
	case dialog.ActionResolveUserInput:
		m.com.App.UserInput.Resolve(msg.Response)
		m.dialog.CloseDialog(dialog.RequestUserInputID)
	default:
		cmds = append(cmds, util.CmdHandler(msg))
	}

	return tea.Batch(cmds...)
}

func childSessionStatus(msg message.Message) (text string, isError bool, ok bool) {
	const retryPrefix = "Service temporarily unavailable. Retrying in "

	if content := strings.TrimSpace(msg.Content().Text); strings.HasPrefix(content, retryPrefix) {
		return content, false, true
	}

	for _, result := range msg.ToolResults() {
		if subtask, ok := result.SubtaskResult(); ok {
			switch subtask.Status {
			case message.ToolResultSubtaskStatus("blocked"):
				if content := strings.TrimSpace(result.Content); content != "" {
					return content, true, true
				}
				return "Blocked", true, true
			case message.ToolResultSubtaskStatus("completed_with_warnings"):
				if content := strings.TrimSpace(result.Content); content != "" {
					return content, false, true
				}
				return "Completed with warnings", false, true
			}
		}
	}

	if finish := msg.FinishPart(); finish != nil {
		switch finish.Reason {
		case message.FinishReasonError, message.FinishReasonPermissionDenied:
			switch {
			case strings.TrimSpace(finish.Details) != "":
				return strings.TrimSpace(finish.Details), true, true
			case strings.TrimSpace(finish.Message) != "":
				return strings.TrimSpace(finish.Message), true, true
			}
		}
	}

	return "", false, false
}

func (m *UI) prepareModelSwitchCmd(action dialog.ActionSelectModel, sessionID string, isOnboarding bool) tea.Cmd {
	return func() tea.Msg {
		if m.com.App.AgentCoordinator != nil {
			if err := m.com.App.AgentCoordinator.PrepareModelSwitch(context.Background(), sessionID, action.ModelType, action.Model); err != nil {
				return modelSwitchPreparedMsg{
					action:       action,
					isOnboarding: isOnboarding,
					err:          err,
				}
			}
		}
		return modelSwitchPreparedMsg{
			action:       action,
			isOnboarding: isOnboarding,
		}
	}
}

func (m *UI) completeModelSwitchCmd(action dialog.ActionSelectModel, defaultSmallModel *config.SelectedModel, isOnboarding bool) tea.Cmd {
	modelType := action.ModelType
	modelName := action.Model.Model
	selectedModel := action.Model
	closeDialog := action.CloseDialog

	return func() tea.Msg {
		if err := m.com.Store().UpdatePreferredModel(config.ScopeGlobal, modelType, selectedModel); err != nil {
			return modelSwitchCompletedMsg{
				modelType:    modelType,
				modelName:    modelName,
				isOnboarding: isOnboarding,
				closeDialog:  closeDialog,
				err:          err,
			}
		}

		if defaultSmallModel != nil {
			if err := m.com.Store().UpdatePreferredModel(config.ScopeGlobal, config.SelectedModelTypeSmall, *defaultSmallModel); err != nil {
				return modelSwitchCompletedMsg{
					modelType:    modelType,
					modelName:    modelName,
					isOnboarding: isOnboarding,
					closeDialog:  closeDialog,
					err:          err,
				}
			}
		}

		if isOnboarding {
			if err := m.com.App.InitCoderAgent(context.Background()); err != nil {
				return modelSwitchCompletedMsg{
					modelType:    modelType,
					modelName:    modelName,
					isOnboarding: isOnboarding,
					closeDialog:  closeDialog,
					err:          err,
				}
			}
		} else {
			if err := m.com.App.UpdateAgentModel(context.Background()); err != nil {
				return modelSwitchCompletedMsg{
					modelType:    modelType,
					modelName:    modelName,
					isOnboarding: isOnboarding,
					closeDialog:  closeDialog,
					err:          err,
				}
			}
		}

		return modelSwitchCompletedMsg{
			modelType:    modelType,
			modelName:    modelName,
			isOnboarding: isOnboarding,
			closeDialog:  closeDialog,
		}
	}
}

// substituteArgs replaces $ARG_NAME placeholders in content with actual values.
func substituteArgs(content string, args map[string]string) string {
	for name, value := range args {
		placeholder := "$" + name
		content = strings.ReplaceAll(content, placeholder, value)
	}
	return content
}

func (m *UI) updateLatestProposedPlan(msg message.Message) {
	if msg.Role != message.Assistant {
		return
	}
	if m.session == nil || m.session.CollaborationMode != session.CollaborationModePlan {
		return
	}
	if !hasToolCall(msg, agenttools.PlanExitToolName) {
		return
	}
	plan, ok := planmode.ExtractProposedPlan(msg.Content().Text)
	if !ok {
		return
	}
	m.latestProposedPlan = plan
}

func (m *UI) maybeOpenProposedPlanDialog(msg message.Message) tea.Cmd {
	if m.session == nil || m.session.CollaborationMode != session.CollaborationModePlan {
		return nil
	}
	if msg.FinishPart() == nil || msg.FinishPart().Reason != message.FinishReasonEndTurn {
		return nil
	}
	plan, ok := planmode.ExtractProposedPlan(msg.Content().Text)
	if !ok || strings.TrimSpace(plan) == "" {
		return nil
	}
	if !hasToolCall(msg, agenttools.PlanExitToolName) {
		return nil
	}
	if m.lastPromptedPlanMsg == msg.ID {
		return nil
	}
	m.lastPromptedPlanMsg = msg.ID
	if m.dialog.ContainsDialog(dialog.ProposedPlanID) {
		m.dialog.CloseDialog(dialog.ProposedPlanID)
	}
	m.dialog.OpenDialog(dialog.NewProposedPlan(m.com, msg.SessionID, plan))
	return nil
}

func hasToolCall(msg message.Message, toolName string) bool {
	for _, tc := range msg.ToolCalls() {
		if tc.Name == toolName {
			return true
		}
	}
	return false
}

func (m *UI) openRequestUserInputDialog(request userinput.Request) tea.Cmd {
	if m.dialog.ContainsDialog(dialog.RequestUserInputID) {
		m.dialog.CloseDialog(dialog.RequestUserInputID)
	}
	m.dialog.OpenDialog(dialog.NewRequestUserInput(m.com, request))
	return nil
}

func (m *UI) openAuthenticationDialog(provider catwalk.Provider, model config.SelectedModel, modelType config.SelectedModelType) tea.Cmd {
	var (
		dlg dialog.Dialog
		cmd tea.Cmd

		isOnboarding = m.state == uiOnboarding
	)

	switch provider.ID {
	case "hyper":
		dlg, cmd = dialog.NewOAuthHyper(m.com, isOnboarding, provider, model, modelType)
	case catwalk.InferenceProviderCopilot:
		dlg, cmd = dialog.NewOAuthCopilot(m.com, isOnboarding, provider, model, modelType)
	default:
		dlg, cmd = dialog.NewAPIKeyInput(m.com, isOnboarding, provider, model, modelType)
	}

	if m.dialog.ContainsDialog(dlg.ID()) {
		m.dialog.BringToFront(dlg.ID())
		return nil
	}

	m.dialog.OpenDialog(dlg)
	return cmd
}

func (m *UI) preferredExecutionMode() executionMode {
	if m.com == nil || m.com.App == nil {
		return executionModeAuto
	}
	cfg := m.com.Config()
	if cfg == nil || cfg.Options == nil {
		return executionModeAuto
	}
	switch session.NormalizePermissionMode(cfg.Options.PreferredPermissionMode) {
	case session.PermissionModeDefault:
		return executionModeAsk
	case session.PermissionModeYolo:
		return executionModeYolo
	}
	return executionModeAuto
}

func (m *UI) currentExecutionMode() executionMode {
	if m.com != nil && m.com.App != nil && m.com.App.Permissions.SkipRequests() {
		return executionModeYolo
	}
	if m.session != nil {
		switch m.session.PermissionMode {
		case session.PermissionModeAuto:
			return executionModeAuto
		case session.PermissionModeYolo:
			return executionModeYolo
		default:
			return executionModeAsk
		}
	}
	return m.preferredExecutionMode()
}

func nextExecutionMode(mode executionMode) executionMode {
	switch mode {
	case executionModeAsk:
		return executionModeAuto
	case executionModeAuto:
		return executionModeYolo
	default:
		return executionModeAsk
	}
}

func (m *UI) refreshEditorPlaceholder() {
	if m.isAgentBusy() {
		m.textarea.Placeholder = m.workingPlaceholder
		return
	}
	if m.session != nil && m.session.CollaborationMode == session.CollaborationModePlan {
		m.textarea.Placeholder = "Plan Mode: explore, clarify, and propose a plan"
		return
	}
	if m.session != nil && m.session.CollaborationMode == session.CollaborationModeOrchestrate {
		m.textarea.Placeholder = "Orchestrate Mode: coordinating multi-agent tasks"
		return
	}

	switch m.currentExecutionMode() {
	case executionModeAuto:
		m.textarea.Placeholder = "Auto Mode: work autonomously with guarded approvals"
	case executionModeYolo:
		m.textarea.Placeholder = "Yolo mode!"
	default:
		m.textarea.Placeholder = m.readyPlaceholder
	}
}

func (m *UI) cycleExecutionMode() tea.Cmd {
	if m.isAgentBusy() {
		return util.ReportWarn("Agent is busy, please wait before changing execution mode...")
	}
	if m.session != nil && m.session.CollaborationMode == session.CollaborationModePlan {
		return util.ReportWarn("Exit Plan Mode before cycling Ask/Auto/Yolo.")
	}
	return m.setExecutionMode(nextExecutionMode(m.currentExecutionMode()))
}

func (m *UI) cycleCollaborationMode() tea.Cmd {
	if m.isAgentBusy() {
		return util.ReportWarn("Agent is busy, please wait before changing collaboration mode...")
	}
	if m.com == nil || m.com.App == nil {
		return nil
	}

	currentMode := session.CollaborationModeDefault
	if m.session != nil {
		currentMode = m.session.CollaborationMode
	}

	nextMode := session.CollaborationModePlan
	status := "Plan Mode enabled"
	switch currentMode {
	case session.CollaborationModePlan:
		nextMode = session.CollaborationModeOrchestrate
		status = "Orchestrate Mode enabled"
	case session.CollaborationModeOrchestrate:
		nextMode = session.CollaborationModeDefault
		status = "Orchestrate Mode disabled"
	}

	return func() tea.Msg {
		ctx := context.Background()
		sessionID := ""
		if m.session != nil {
			sessionID = m.session.ID
		}
		if sessionID == "" {
			if nextMode == session.CollaborationModeDefault {
				return util.ReportError(errors.New("cannot exit collaboration mode without an active session"))()
			}
			newSession, err := m.com.App.Sessions.Create(ctx, "New Session")
			if err != nil {
				return util.ReportError(err)()
			}
			sessionID = newSession.ID
		}
		if _, err := m.com.App.Sessions.UpdateCollaborationMode(ctx, sessionID, nextMode); err != nil {
			return util.ReportError(err)()
		}
		return planModeChangedMsg{SessionID: sessionID, Status: status, Mode: nextMode}
	}
}

func (m *UI) setExecutionMode(mode executionMode) tea.Cmd {
	if m.com == nil || m.com.App == nil {
		return nil
	}

	status := "Ask mode enabled"
	preferredMode := session.PermissionModeDefault

	switch mode {
	case executionModeAuto:
		status = "Auto Mode enabled"
		preferredMode = session.PermissionModeAuto
	case executionModeYolo:
		status = "Yolo mode enabled"
		preferredMode = session.PermissionModeYolo
	}

	if preferredMode != session.PermissionModeYolo {
		m.com.App.Permissions.SetSkipRequests(false)
	}
	m.setEditorPrompt(preferredMode == session.PermissionModeYolo)

	var transition session.ModeTransition
	sessionID := ""
	if m.session != nil {
		sessionID = m.session.ID
		transition = session.NewPermissionModeTransition(*m.session, preferredMode)
	}

	if cfg := m.com.Config(); cfg != nil && cfg.Options != nil {
		cfg.Options.PreferredPermissionMode = string(preferredMode)
	}
	if m.session != nil {
		m.session.PermissionMode = transition.Current.PermissionMode
	}
	m.refreshEditorPlaceholder()

	return func() tea.Msg {
		if preferredMode != session.PermissionModeYolo {
			if err := m.com.Store().SetSkipRequests(config.ScopeGlobal, false); err != nil {
				return util.ReportError(err)()
			}
		}
		if err := m.com.Store().SetPreferredPermissionMode(config.ScopeGlobal, string(preferredMode)); err != nil {
			return util.ReportError(err)()
		}
		m.com.App.Sessions.SetDefaultPermissionMode(preferredMode)
		if sessionID != "" {
			if preferredMode == session.PermissionModeAuto {
				m.com.App.Permissions.ClearPersistentPermissions(sessionID)
			}
			if _, err := m.com.App.Sessions.UpdatePermissionMode(context.Background(), sessionID, transition.Current.PermissionMode); err != nil {
				return util.ReportError(err)()
			}
			if transition.ExitedAutoMode() {
				if _, err := m.com.App.Messages.Create(context.Background(), sessionID, message.NewAutoModePromptMessage(message.AutoModePromptTypeExit)); err != nil {
					return util.ReportError(err)()
				}
			}
		}
		return executionModeChangedMsg{SessionID: sessionID, Status: status}
	}
}

func (m *UI) handleKeyPressMsg(msg tea.KeyPressMsg) tea.Cmd {
	var cmds []tea.Cmd

	handleGlobalKeys := func(msg tea.KeyPressMsg) bool {
		switch {
		case key.Matches(msg, m.keyMap.Help):
			if shortcuts, err := dialog.NewKeyboardShortcuts(m.com); err == nil {
				m.dialog.OpenDialog(shortcuts)
			}
			return true
		case key.Matches(msg, m.keyMap.Commands):
			if cmd := m.openCommandsDialog(); cmd != nil {
				cmds = append(cmds, cmd)
			}
			return true
		case key.Matches(msg, m.keyMap.Models):
			if cmd := m.openModelsDialog(); cmd != nil {
				cmds = append(cmds, cmd)
			}
			return true
		case key.Matches(msg, m.keyMap.Sessions):
			if cmd := m.openSessionsDialog(); cmd != nil {
				cmds = append(cmds, cmd)
			}
			return true
		case key.Matches(msg, m.keyMap.Chat.Details) && m.isCompact:
			m.detailsOpen = !m.detailsOpen
			m.updateLayoutAndSize()
			return true
		case key.Matches(msg, m.keyMap.Chat.TogglePills):
			if m.state == uiChat && m.hasSession() {
				if cmd := m.togglePillsExpanded(); cmd != nil {
					cmds = append(cmds, cmd)
				}
				return true
			}
		case key.Matches(msg, m.keyMap.Chat.PillLeft):
			if m.state == uiChat && m.hasSession() && m.pillsExpanded && m.focus != uiFocusEditor {
				if cmd := m.switchPillSection(-1); cmd != nil {
					cmds = append(cmds, cmd)
				}
				return true
			}
		case key.Matches(msg, m.keyMap.Chat.PillRight):
			if m.state == uiChat && m.hasSession() && m.pillsExpanded && m.focus != uiFocusEditor {
				if cmd := m.switchPillSection(1); cmd != nil {
					cmds = append(cmds, cmd)
				}
				return true
			}
		case key.Matches(msg, m.keyMap.Chat.SessionParent):
			if m.state == uiChat && m.hasSession() && m.focus != uiFocusEditor {
				if cmd := m.openParentSession(); cmd != nil {
					cmds = append(cmds, cmd)
				}
				return true
			}
		case key.Matches(msg, m.keyMap.Chat.SessionChild):
			if m.state == uiChat && m.hasSession() && m.focus != uiFocusEditor {
				if cmd := m.openSelectedChildSession(); cmd != nil {
					cmds = append(cmds, cmd)
				}
				return true
			}
		case key.Matches(msg, m.keyMap.Chat.SessionNext):
			if m.state == uiChat && m.hasSession() && m.focus != uiFocusEditor {
				if cmd := m.cycleSiblingChildSession(1); cmd != nil {
					cmds = append(cmds, cmd)
				}
				return true
			}
		case key.Matches(msg, m.keyMap.Chat.SessionPrev):
			if m.state == uiChat && m.hasSession() && m.focus != uiFocusEditor {
				if cmd := m.cycleSiblingChildSession(-1); cmd != nil {
					cmds = append(cmds, cmd)
				}
				return true
			}
		case key.Matches(msg, m.keyMap.Suspend):
			if m.isAgentBusy() {
				cmds = append(cmds, util.ReportWarn("Agent is busy, please wait..."))
				return true
			}
			cmds = append(cmds, tea.Suspend)
			return true
		}
		return false
	}

	if key.Matches(msg, m.keyMap.Quit) && !m.dialog.ContainsDialog(dialog.QuitID) {
		// In editor mode with text, clear the input instead of quitting.
		if m.focus == uiFocusEditor && m.textarea.Value() != "" {
			m.textarea.Reset()
			return tea.Batch(cmds...)
		}
		// Otherwise, open quit dialog.
		if cmd := m.openQuitDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		return tea.Batch(cmds...)
	}

	// Route all messages to dialog if one is open.
	if m.dialog.HasDialogs() {
		return m.handleDialogMsg(msg)
	}

	// Handle cancel key when the current session has an active request or queue.
	if key.Matches(msg, m.keyMap.Chat.Cancel) {
		if m.isAgentBusy() || m.hasQueuedPrompts() {
			if cmd := m.cancelAgent(); cmd != nil {
				cmds = append(cmds, cmd)
			}
			return tea.Batch(cmds...)
		}
	}

	switch m.state {
	case uiOnboarding:
		return tea.Batch(cmds...)
	case uiInitialize:
		cmds = append(cmds, m.updateInitializeView(msg)...)
		return tea.Batch(cmds...)
	case uiChat, uiLanding:
		switch m.focus {
		case uiFocusEditor:
			// Handle completions if open.
			if m.completionsOpen {
				if msg, ok := m.completions.Update(msg); ok {
					switch msg := msg.(type) {
					case completions.SelectionMsg[completions.FileCompletionValue]:
						cmds = append(cmds, m.insertFileCompletion(msg.Value.Path))
						if !msg.KeepOpen {
							m.closeCompletions()
						}
					case completions.SelectionMsg[completions.ResourceCompletionValue]:
						cmds = append(cmds, m.insertMCPResourceCompletion(msg.Value))
						if !msg.KeepOpen {
							m.closeCompletions()
						}
					case completions.ClosedMsg:
						m.completionsOpen = false
					}
					return tea.Batch(cmds...)
				}
			}

			if ok := m.attachments.Update(msg); ok {
				return tea.Batch(cmds...)
			}

			switch {
			case key.Matches(msg, m.keyMap.Editor.AddImage):
				if cmd := m.openFilesDialog(); cmd != nil {
					cmds = append(cmds, cmd)
				}

			case key.Matches(msg, m.keyMap.Editor.PasteImage):
				if m.modelSupportsImages() {
					m.lastClipboardPasteShortcut = time.Now()
					// Pass an empty PasteMsg for keyboard shortcut - fallback is not needed
					// since there's no text content to paste anyway.
					cmds = append(cmds, m.pasteImageFromClipboard(tea.PasteMsg{}))
				}

			case m.attachments.HasAny() && m.textarea.Value() == "" && key.Matches(msg, m.keyMap.Editor.RemoveLastAttachment):
				if m.attachments.DeleteLast() {
					m.updateLayoutAndSize()
				}

			case m.attachments.HasAny() && m.textarea.Value() == "" && key.Matches(msg, m.keyMap.Editor.ClearAttachments):
				if m.attachments.Clear() {
					m.updateLayoutAndSize()
				}

			case key.Matches(msg, m.keyMap.Editor.SendMessage):
				prevHeight := m.textarea.Height()
				value := m.textarea.Value()
				if before, ok := strings.CutSuffix(value, "\\"); ok {
					// If the last character is a backslash, remove it and add a newline.
					m.textarea.SetValue(before)
					if cmd := m.handleTextareaHeightChange(prevHeight); cmd != nil {
						cmds = append(cmds, cmd)
					}
					break
				}

				// Otherwise, send the message
				m.textarea.Reset()
				if cmd := m.handleTextareaHeightChange(prevHeight); cmd != nil {
					cmds = append(cmds, cmd)
				}

				value = strings.TrimSpace(value)
				if value == "exit" || value == "quit" {
					return m.openQuitDialog()
				}

				attachments := m.attachments.List()
				m.attachments.Reset()
				if len(value) == 0 && !message.ContainsTextAttachment(attachments) {
					return nil
				}

				m.randomizePlaceholders()
				m.historyReset()

				return tea.Batch(m.sendMessage(value, attachments...), m.loadPromptHistory())
			case key.Matches(msg, m.keyMap.Chat.NewSession):
				if !m.hasSession() {
					break
				}
				if m.isAgentBusy() {
					cmds = append(cmds, util.ReportWarn("Agent is busy, please wait before starting a new session..."))
					break
				}
				if cmd := m.newSession(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			case key.Matches(msg, m.keyMap.Tab):
				if m.state != uiLanding {
					m.setState(m.state, uiFocusMain)
					m.textarea.Blur()
					m.chat.Focus()
					m.chat.SetSelected(m.chat.Len() - 1)
				}
			case key.Matches(msg, m.keyMap.Editor.OpenEditor):
				if m.isAgentBusy() {
					cmds = append(cmds, util.ReportWarn("Agent is working, please wait..."))
					break
				}
				cmds = append(cmds, m.openEditor(m.textarea.Value()))
			case key.Matches(msg, m.keyMap.Editor.CycleExecutionMode):
				if cmd := m.cycleExecutionMode(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			case key.Matches(msg, m.keyMap.Editor.CycleCollaborationMode):
				if cmd := m.cycleCollaborationMode(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			case key.Matches(msg, m.keyMap.Editor.PromptEnhance):
				if m.isEnhancingPrompt {
					break
				}
				if strings.TrimSpace(m.textarea.Value()) == "" {
					cmds = append(cmds, util.ReportWarn("Type something first to enhance."))
					break
				}
				m.isEnhancingPrompt = true
				cmds = append(cmds,
					util.ReportInfo("Enhancing prompt…"),
					m.enhancePromptCmd(),
				)
			case key.Matches(msg, m.keyMap.Editor.Newline):
				prevHeight := m.textarea.Height()
				m.textarea.InsertRune('\n')
				m.closeCompletions()
				cmds = append(cmds, m.updateTextareaWithPrevHeight(msg, prevHeight))
			case key.Matches(msg, m.keyMap.Editor.HistoryPrev):
				cmd := m.handleHistoryUp(msg)
				if cmd != nil {
					cmds = append(cmds, cmd)
				}
			case key.Matches(msg, m.keyMap.Editor.HistoryNext):
				cmd := m.handleHistoryDown(msg)
				if cmd != nil {
					cmds = append(cmds, cmd)
				}
			case key.Matches(msg, m.keyMap.Editor.Escape):
				cmd := m.handleHistoryEscape(msg)
				if cmd != nil {
					cmds = append(cmds, cmd)
				}
			case key.Matches(msg, m.keyMap.Editor.Commands) && m.textarea.Value() == "":
				if cmd := m.openCommandsDialog(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			default:
				if handleGlobalKeys(msg) {
					// Handle global keys first before passing to textarea.
					break
				}

				// Check for @ trigger before passing to textarea.
				curValue := m.textarea.Value()
				curIdx := len(curValue)

				// Trigger completions on @.
				if msg.String() == "@" && !m.completionsOpen {
					// Only show if beginning of prompt or after whitespace.
					if curIdx == 0 || (curIdx > 0 && isWhitespace(curValue[curIdx-1])) {
						m.completionsOpen = true
						m.completionsQuery = ""
						m.completionsStartIndex = curIdx
						m.completionsPositionStart = m.completionsPosition()
						depth, limit := m.com.Config().Options.TUI.Completions.Limits()
						cmds = append(cmds, m.completions.Open(depth, limit))
					}
				}

				// remove the details if they are open when user starts typing
				if m.detailsOpen {
					m.detailsOpen = false
					m.updateLayoutAndSize()
				}

				prevHeight := m.textarea.Height()
				cmds = append(cmds, m.updateTextareaWithPrevHeight(msg, prevHeight))

				// Any text modification becomes the current draft.
				m.updateHistoryDraft(curValue)

				// After updating textarea, check if we need to filter completions.
				// Skip filtering on the initial @ keystroke since items are loading async.
				if m.completionsOpen && msg.String() != "@" {
					newValue := m.textarea.Value()
					newIdx := len(newValue)

					// Close completions if cursor moved before start.
					if newIdx <= m.completionsStartIndex {
						m.closeCompletions()
					} else if msg.String() == "space" {
						// Close on space.
						m.closeCompletions()
					} else {
						// Extract current word and filter.
						word := m.textareaWord()
						if strings.HasPrefix(word, "@") {
							m.completionsQuery = word[1:]
							m.completions.Filter(m.completionsQuery)
						} else if m.completionsOpen {
							m.closeCompletions()
						}
					}
				}
			}
		case uiFocusMain:
			switch {
			case key.Matches(msg, m.keyMap.Tab):
				// Block focus switch to editor in subagent sessions (read-only).
				if m.isSubagentSession() {
					break
				}
				m.focus = uiFocusEditor
				cmds = append(cmds, m.textarea.Focus())
				m.chat.Blur()
			case key.Matches(msg, m.keyMap.Chat.NewSession):
				if !m.hasSession() {
					break
				}
				if m.isAgentBusy() {
					cmds = append(cmds, util.ReportWarn("Agent is busy, please wait before starting a new session..."))
					break
				}
				m.focus = uiFocusEditor
				if cmd := m.newSession(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			case m.queueSelectionActive() && key.Matches(msg, m.keyMap.Chat.QueueDelete):
				if cmd := m.removeSelectedQueuedPrompt(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			case m.queueSelectionActive() && key.Matches(msg, m.keyMap.Chat.QueueClear):
				if cmd := m.clearQueuedPrompts(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			case m.queueSelectionActive() && key.Matches(msg, m.keyMap.Chat.QueuePrioritize):
				if cmd := m.prioritizeSelectedQueuedPrompt(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			case m.queueSelectionActive() && key.Matches(msg, m.keyMap.Chat.Up):
				m.moveQueueSelection(-1)
			case m.queueSelectionActive() && key.Matches(msg, m.keyMap.Chat.Down):
				m.moveQueueSelection(1)
			case key.Matches(msg, m.keyMap.Chat.Expand):
				m.chat.ToggleExpandedSelectedItem()
			case key.Matches(msg, m.keyMap.Chat.Up):
				if cmd := m.chat.ScrollByAndAnimate(-1); cmd != nil {
					cmds = append(cmds, cmd)
				}
				if !m.chat.SelectedItemInView() {
					m.chat.SelectPrev()
					if cmd := m.chat.ScrollToSelectedAndAnimate(); cmd != nil {
						cmds = append(cmds, cmd)
					}
				}
			case key.Matches(msg, m.keyMap.Chat.Down):
				if cmd := m.chat.ScrollByAndAnimate(1); cmd != nil {
					cmds = append(cmds, cmd)
				}
				if !m.chat.SelectedItemInView() {
					m.chat.SelectNext()
					if cmd := m.chat.ScrollToSelectedAndAnimate(); cmd != nil {
						cmds = append(cmds, cmd)
					}
				}
			case key.Matches(msg, m.keyMap.Chat.UpOneItem):
				m.chat.SelectPrev()
				if cmd := m.chat.ScrollToSelectedAndAnimate(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			case key.Matches(msg, m.keyMap.Chat.DownOneItem):
				m.chat.SelectNext()
				if cmd := m.chat.ScrollToSelectedAndAnimate(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			case key.Matches(msg, m.keyMap.Chat.HalfPageUp):
				if cmd := m.chat.ScrollByAndAnimate(-m.chat.Height() / 2); cmd != nil {
					cmds = append(cmds, cmd)
				}
				m.chat.SelectFirstInView()
			case key.Matches(msg, m.keyMap.Chat.HalfPageDown):
				if cmd := m.chat.ScrollByAndAnimate(m.chat.Height() / 2); cmd != nil {
					cmds = append(cmds, cmd)
				}
				m.chat.SelectLastInView()
			case key.Matches(msg, m.keyMap.Chat.PageUp):
				if cmd := m.chat.ScrollByAndAnimate(-m.chat.Height()); cmd != nil {
					cmds = append(cmds, cmd)
				}
				m.chat.SelectFirstInView()
			case key.Matches(msg, m.keyMap.Chat.PageDown):
				if cmd := m.chat.ScrollByAndAnimate(m.chat.Height()); cmd != nil {
					cmds = append(cmds, cmd)
				}
				m.chat.SelectLastInView()
			case key.Matches(msg, m.keyMap.Chat.Home):
				if cmd := m.chat.ScrollToTopAndAnimate(); cmd != nil {
					cmds = append(cmds, cmd)
				}
				m.chat.SelectFirst()
			case key.Matches(msg, m.keyMap.Chat.End):
				if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
					cmds = append(cmds, cmd)
				}
				m.chat.SelectLast()
			default:
				if ok, cmd := m.chat.HandleKeyMsg(msg); ok {
					cmds = append(cmds, cmd)
				} else {
					handleGlobalKeys(msg)
				}
			}
		default:
			handleGlobalKeys(msg)
		}
	default:
		handleGlobalKeys(msg)
	}

	return tea.Sequence(cmds...)
}

// drawHeader draws the header section of the UI.
func (m *UI) drawHeader(scr uv.Screen, area uv.Rectangle) {
	// Use the per-frame cached snapshot to avoid a second walk through
	// the message list.
	m.header.drawHeader(
		scr,
		area,
		m.session,
		m.frameUsageSnapshotCached(),
		m.isCompact,
		m.detailsOpen,
		area.Dx(),
	)
}

// Draw implements [uv.Drawable] and draws the UI model.
func (m *UI) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	layout := m.generateLayout(area.Dx(), area.Dy())

	if m.layout != layout {
		m.layout = layout
		m.updateSize()
	}

	// Compute the usage snapshot once for the entire frame so that both
	// drawHeader and modelInfo (sidebar) share the same value without
	// re-walking the message list.
	m.frameUsageSnapshot = m.currentContextUsageSnapshot()
	m.frameUsageSnapshotValid = true
	defer func() { m.frameUsageSnapshotValid = false }()

	// Clear the screen first
	screen.Clear(scr)

	switch m.state {
	case uiOnboarding:
		m.drawHeader(scr, layout.header)

		// NOTE: Onboarding flow will be rendered as dialogs below, but
		// positioned at the bottom left of the screen.

	case uiInitialize:
		m.drawHeader(scr, layout.header)

		main := uv.NewStyledString(m.initializeView())
		main.Draw(scr, layout.main)

	case uiLanding:
		m.drawHeader(scr, layout.header)
		main := uv.NewStyledString(m.landingView())
		main.Draw(scr, layout.main)

		editor := uv.NewStyledString(m.renderEditorView(scr.Bounds().Dx()))
		editor.Draw(scr, layout.editor)

	case uiChat:
		if m.isCompact {
			m.drawHeader(scr, layout.header)
		} else {
			m.drawSidebar(scr, layout.sidebar)
		}

		m.chat.Draw(scr, layout.main)
		if layout.pills.Dy() > 0 && m.pillsView != "" {
			uv.NewStyledString(m.pillsView).Draw(scr, layout.pills)
		}

		if m.isSubagentSession() {
			// Show a read-only banner instead of the editor.
			bannerWidth := scr.Bounds().Dx()
			if !m.isCompact {
				bannerWidth -= layout.sidebar.Dx()
			}
			banner := uv.NewStyledString(m.renderSubagentBanner(bannerWidth))
			banner.Draw(scr, layout.editor)
		} else {
			editorWidth := scr.Bounds().Dx()
			if !m.isCompact {
				editorWidth -= layout.sidebar.Dx()
			}
			editor := uv.NewStyledString(m.renderEditorView(editorWidth))
			editor.Draw(scr, layout.editor)
		}

		// Draw details overlay in compact mode when open
		if m.isCompact && m.detailsOpen {
			m.drawSessionDetails(scr, layout.sessionDetails)
		}
	}

	isOnboarding := m.state == uiOnboarding

	// Add status and help layer
	m.status.SetHideHelp(isOnboarding)
	m.status.Draw(scr, layout.status)

	// Draw completions popup if open
	if !isOnboarding && m.completionsOpen && m.completions.HasItems() {
		w, h := m.completions.Size()
		x := m.completionsPositionStart.X
		y := m.completionsPositionStart.Y - h

		screenW := area.Dx()
		if x+w > screenW {
			x = screenW - w
		}
		x = max(0, x)
		y = max(0, y+m.editorTopMarginRows()) // Offset for attachments row when present

		completionsView := uv.NewStyledString(m.completions.Render())
		completionsView.Draw(scr, image.Rectangle{
			Min: image.Pt(x, y),
			Max: image.Pt(x+w, y+h),
		})
	}

	// Debugging rendering (visually see when the tui rerenders)
	if os.Getenv("CRUSH_UI_DEBUG") == "true" {
		debugView := lipgloss.NewStyle().Background(lipgloss.ANSIColor(rand.Intn(256))).Width(4).Height(2)
		debug := uv.NewStyledString(debugView.String())
		debug.Draw(scr, image.Rectangle{
			Min: image.Pt(4, 1),
			Max: image.Pt(8, 3),
		})
	}

	// This needs to come last to overlay on top of everything. We always pass
	// the full screen bounds because the dialogs will position themselves
	// accordingly.
	if m.dialog.HasDialogs() {
		return m.dialog.Draw(scr, scr.Bounds())
	}

	switch m.focus {
	case uiFocusEditor:
		if m.layout.editor.Dy() <= 0 {
			// Don't show cursor if editor is not visible
			return nil
		}
		if m.detailsOpen && m.isCompact {
			// Don't show cursor if details overlay is open
			return nil
		}

		if m.textarea.Focused() {
			cur := m.textarea.Cursor()
			cur.X++ // Adjust for app margins
			cur.Y += m.layout.editor.Min.Y + m.editorTopMarginRows()
			return cur
		}
	}
	return nil
}

// View renders the UI model's view.
func (m *UI) View() tea.View {
	var v tea.View
	v.AltScreen = true
	if !m.isTransparent {
		v.BackgroundColor = m.com.Styles.Background
	}
	v.MouseMode = tea.MouseModeCellMotion
	v.ReportFocus = m.caps.ReportFocusEvents
	v.WindowTitle = "crush " + home.Short(m.com.Store().WorkingDir())

	canvas := uv.NewScreenBuffer(m.width, m.height)
	v.Cursor = m.Draw(canvas, canvas.Bounds())

	content := canvas.Render()
	// Single-pass post-processing: normalize \r\n and trim trailing spaces
	// per line. Avoids allocating a full string slice for every line (the
	// old Split/TrimRight/Join approach allocated O(height) strings).
	var buf strings.Builder
	buf.Grow(len(content))
	first := true
	for line := range strings.SplitSeq(content, "\n") {
		if !first {
			buf.WriteByte('\n')
		}
		first = false
		line = strings.TrimSuffix(line, "\r")
		buf.WriteString(strings.TrimRight(line, " "))
	}
	content = buf.String()

	v.Content = content
	if m.progressBarEnabled && m.sendProgressBar && m.isAgentBusy() {
		// HACK: use a random percentage to prevent ghostty from hiding it
		// after a timeout.
		v.ProgressBar = tea.NewProgressBar(tea.ProgressBarIndeterminate, rand.Intn(100))
	}

	return v
}

// ShortHelp implements [help.KeyMap].
func (m *UI) ShortHelp() []key.Binding {
	var binds []key.Binding
	k := &m.keyMap
	tab := k.Tab
	commands := k.Commands
	if m.focus == uiFocusEditor && m.textarea.Value() == "" {
		commands.SetHelp("ctrl+/", "commands")
	}

	switch m.state {
	case uiInitialize:
		binds = append(binds, k.Quit)
	case uiChat:
		if m.isAgentBusy() {
			cancelBinding := k.Chat.Cancel
			if m.isCanceling {
				cancelBinding.SetHelp("esc", "press again to cancel")
			} else if m.com.App.AgentCoordinator.QueuedPrompts(m.session.ID) > 0 && !m.queuePaused {
				cancelBinding.SetHelp("esc", "pause queue")
			}
			binds = append(binds, cancelBinding)
		}

		if m.focus == uiFocusEditor {
			tab.SetHelp("tab", "focus chat")
		} else {
			tab.SetHelp("tab", "focus editor")
		}

		binds = append(binds,
			tab,
			commands,
		)

		switch m.focus {
		case uiFocusEditor:
			binds = append(binds,
				k.Editor.CycleExecutionMode,
				k.Editor.CycleCollaborationMode,
				k.Editor.PromptEnhance,
			)
		case uiFocusMain:
			binds = append(binds, k.Chat.UpDown)
			if m.selectedHasChildSession() {
				binds = append(binds, k.Chat.SessionChild)
			}
		}
	default:
		binds = append(binds,
			commands,
			k.Editor.PromptEnhance,
		)
	}

	help := k.Help
	help.SetHelp("ctrl+g", "more shortcuts")
	binds = append(binds,
		k.Quit,
		help,
	)

	return binds
}

// FullHelp implements [help.KeyMap].
func (m *UI) FullHelp() [][]key.Binding {
	var binds [][]key.Binding
	k := &m.keyMap
	help := k.Help
	help.SetHelp("ctrl+g", "shortcuts")
	hasSession := m.hasSession()
	commands := k.Commands
	if m.focus == uiFocusEditor && m.textarea.Value() == "" {
		commands.SetHelp("ctrl+/", "commands")
	}

	switch m.state {
	case uiInitialize:
		binds = append(binds,
			[]key.Binding{
				k.Quit,
			})
	case uiChat:
		// Show cancel binding if agent is busy.
		if m.isAgentBusy() {
			cancelBinding := k.Chat.Cancel
			if m.isCanceling {
				cancelBinding.SetHelp("esc", "press again to cancel")
			} else if m.com.App.AgentCoordinator.QueuedPrompts(m.session.ID) > 0 && !m.queuePaused {
				cancelBinding.SetHelp("esc", "pause queue")
			}
			binds = append(binds, []key.Binding{cancelBinding})
		}

		mainBinds := []key.Binding{}
		tab := k.Tab
		if m.focus == uiFocusEditor {
			tab.SetHelp("tab", "focus chat")
		} else {
			tab.SetHelp("tab", "focus editor")
		}

		mainBinds = append(mainBinds,
			tab,
			commands,
			k.Models,
			k.Sessions,
		)
		if hasSession {
			mainBinds = append(mainBinds, k.Chat.NewSession)
		}

		binds = append(binds, mainBinds)

		switch m.focus {
		case uiFocusEditor:
			binds = append(binds,
				[]key.Binding{
					k.Editor.CycleExecutionMode,
					k.Editor.CycleCollaborationMode,
					k.Editor.PromptEnhance,
					k.Editor.Newline,
					k.Editor.AddImage,
					k.Editor.PasteImage,
					k.Editor.MentionFile,
					k.Editor.OpenEditor,
				},
			)
			binds = append(binds,
				[]key.Binding{
					k.Editor.RemoveLastAttachment,
					k.Editor.ClearAttachments,
					k.Editor.AttachmentDeleteMode,
					k.Editor.DeleteAllAttachments,
					k.Editor.Escape,
				},
			)
		case uiFocusMain:
			binds = append(binds,
				[]key.Binding{
					k.Chat.UpDown,
					k.Chat.UpDownOneItem,
					k.Chat.PageUp,
					k.Chat.PageDown,
				},
				[]key.Binding{
					k.Chat.HalfPageUp,
					k.Chat.HalfPageDown,
					k.Chat.Home,
					k.Chat.End,
				},
				[]key.Binding{
					k.Chat.Copy,
					k.Chat.ClearHighlight,
					k.Chat.SessionNav,
				},
				[]key.Binding{
					k.Chat.SessionPrev,
					k.Chat.SessionNext,
				},
			)
			if m.pillsExpanded && hasIncompleteTodos(m.session.Todos) && m.promptQueue > 0 {
				binds = append(binds, []key.Binding{k.Chat.PillLeft})
			}
		}
	default:
		if m.session == nil {
			// no session selected
			binds = append(binds,
				[]key.Binding{
					commands,
					k.Models,
					k.Sessions,
				},
				[]key.Binding{
					k.Editor.CycleExecutionMode,
					k.Editor.PromptEnhance,
					k.Editor.Newline,
					k.Editor.AddImage,
					k.Editor.PasteImage,
					k.Editor.MentionFile,
					k.Editor.OpenEditor,
				},
			)
			binds = append(binds,
				[]key.Binding{
					k.Editor.RemoveLastAttachment,
					k.Editor.ClearAttachments,
					k.Editor.AttachmentDeleteMode,
					k.Editor.DeleteAllAttachments,
					k.Editor.Escape,
				},
			)
			binds = append(binds,
				[]key.Binding{
					help,
				},
			)
		}
	}

	// Always show help and quit in the last group
	binds = append(binds,
		[]key.Binding{
			help,
			k.Quit,
		},
	)

	return binds
}

// toggleCompactMode toggles compact mode between uiChat and uiChatCompact states.
func (m *UI) toggleCompactMode() tea.Cmd {
	m.forceCompactMode = !m.forceCompactMode

	err := m.com.Store().SetCompactMode(config.ScopeGlobal, m.forceCompactMode)
	if err != nil {
		return util.ReportError(err)
	}

	m.updateLayoutAndSize()

	return nil
}

// updateLayoutAndSize updates the layout and sizes of UI components.
func (m *UI) updateLayoutAndSize() {
	// Determine if we should be in compact mode
	if m.state == uiChat {
		if m.forceCompactMode {
			m.isCompact = true
		} else if m.width < compactModeWidthBreakpoint || m.height < compactModeHeightBreakpoint {
			m.isCompact = true
		} else {
			m.isCompact = false
		}
	}

	// First pass sizes components from the current textarea height.
	m.layout = m.generateLayout(m.width, m.height)
	prevHeight := m.textarea.Height()
	m.updateSize()

	// SetWidth can change textarea height due to soft-wrap recalculation.
	// If that happens, run one reconciliation pass with the new height.
	if m.textarea.Height() != prevHeight {
		m.layout = m.generateLayout(m.width, m.height)
		m.updateSize()
	}
}

// handleTextareaHeightChange recalculates layout after the textarea grows or
// shrinks. When the chat is following new output it keeps the viewport pinned
// to the bottom.
func (m *UI) handleTextareaHeightChange(prevHeight int) tea.Cmd {
	if m.textarea.Height() == prevHeight {
		return nil
	}
	m.updateLayoutAndSize()
	if m.state == uiChat && m.chat.Follow() {
		return m.chat.ScrollToBottomAndAnimate()
	}
	return nil
}

// updateTextarea updates the textarea and reconciles layout if its height
// changed as a result.
func (m *UI) updateTextarea(msg tea.Msg) tea.Cmd {
	return m.updateTextareaWithPrevHeight(msg, m.textarea.Height())
}

// updateTextareaWithPrevHeight updates the textarea after callers have already
// mutated its content and need height reconciliation against the previous
// height.
func (m *UI) updateTextareaWithPrevHeight(msg tea.Msg, prevHeight int) tea.Cmd {
	ta, cmd := m.textarea.Update(msg)
	m.textarea = ta
	return tea.Batch(cmd, m.handleTextareaHeightChange(prevHeight))
}

// startAnimations registers animatable items with the chat's animation set
// and ensures the global animation ticker is running. This replaces the old
// per-animation timer approach with a single shared ticker.
func (m *UI) startAnimations(items []chat.MessageItem) tea.Cmd {
	var needTick bool
	for _, item := range items {
		if animatable, ok := item.(chat.Animatable); ok {
			if animatable.IsAnimating() {
				m.chat.RegisterAnimation(item.ID())
				needTick = true
			}
		}
	}
	if needTick && !m.globalTickActive {
		m.globalTickActive = true
		return anim.GlobalTick()
	}
	return nil
}

// updateSize updates the sizes of UI components based on the current layout.
func (m *UI) updateSize() {
	// Set status width
	m.status.SetWidth(m.layout.status.Dx())

	m.chat.SetSize(m.layout.main.Dx(), m.layout.main.Dy())
	m.textarea.MaxHeight = TextareaMaxHeight
	m.textarea.SetWidth(m.layout.editor.Dx())
	m.renderPills()

	// Handle different app states
	switch m.state {
	case uiChat:
		if !m.isCompact {
			m.cacheSidebarLogo(m.layout.sidebar.Dx())
		}
		// Sidebar dimensions may have changed; invalidate cache.
		m.invalidateSidebarCache()
	}
}

// generateLayout calculates the layout rectangles for all UI components based
// on the current UI state and terminal dimensions.
func (m *UI) generateLayout(w, h int) uiLayout {
	// The screen area we're working with
	area := image.Rect(0, 0, w, h)

	// The help height
	helpHeight := 1
	// The editor height includes the dynamic textarea, optional attachments row,
	// and bottom spacing.
	editorHeight := m.textarea.Height() + editorBottomMargin + m.editorTopMarginRows()
	// The sidebar width
	sidebarWidth := 30
	// The header height
	const landingHeaderHeight = 4

	var helpKeyMap help.KeyMap = m
	if m.status != nil && m.status.ShowingAll() {
		for _, row := range helpKeyMap.FullHelp() {
			helpHeight = max(helpHeight, len(row))
		}
	}

	// Add app margins
	appRect, helpRect := layout.SplitVertical(area, layout.Fixed(area.Dy()-helpHeight))
	appRect.Min.Y += 1
	appRect.Max.Y -= 1
	helpRect.Min.Y -= 1
	appRect.Min.X += 1
	appRect.Max.X -= 1

	if slices.Contains([]uiState{uiOnboarding, uiInitialize, uiLanding}, m.state) {
		// extra padding on left and right for these states
		appRect.Min.X += 1
		appRect.Max.X -= 1
	}

	uiLayout := uiLayout{
		area:   area,
		status: helpRect,
	}

	// Handle different app states
	switch m.state {
	case uiOnboarding, uiInitialize:
		// Layout
		//
		// header
		// ------
		// main
		// ------
		// help

		headerRect, mainRect := layout.SplitVertical(appRect, layout.Fixed(landingHeaderHeight))
		uiLayout.header = headerRect
		uiLayout.main = mainRect

	case uiLanding:
		// Layout
		//
		// header
		// ------
		// main
		// ------
		// editor
		// ------
		// help
		headerRect, mainRect := layout.SplitVertical(appRect, layout.Fixed(landingHeaderHeight))
		mainRect, editorRect := layout.SplitVertical(mainRect, layout.Fixed(mainRect.Dy()-editorHeight))
		// Remove extra padding from editor (but keep it for header and main)
		editorRect.Min.X -= 1
		editorRect.Max.X += 1
		uiLayout.header = headerRect
		uiLayout.main = mainRect
		uiLayout.editor = editorRect

	case uiChat:
		// Hide the editor when viewing a subagent session (read-only).
		showEditor := !m.isSubagentSession()

		if m.isCompact {
			// Layout
			//
			// compact-header
			// ------
			// (subagent-banner — only in subagent sessions)
			// ------
			// main
			// ------
			// editor (hidden in subagent)
			// ------
			// help
			const compactHeaderHeight = 1
			headerRect, mainRect := layout.SplitVertical(appRect, layout.Fixed(compactHeaderHeight))
			detailsHeight := min(sessionDetailsMaxHeight, area.Dy()-1) // One row for the header
			sessionDetailsArea, _ := layout.SplitVertical(appRect, layout.Fixed(detailsHeight))
			uiLayout.sessionDetails = sessionDetailsArea
			uiLayout.sessionDetails.Min.Y += compactHeaderHeight // adjust for header
			// Add one line gap between header and main content
			mainRect.Min.Y += 1
			if showEditor {
				mainRect, editorRect := layout.SplitVertical(mainRect, layout.Fixed(mainRect.Dy()-editorHeight))
				uiLayout.editor = editorRect
				mainRect.Max.X -= 1 // Add padding right
				uiLayout.header = headerRect
				pillsHeight := m.pillsAreaHeight()
				if pillsHeight > 0 {
					pillsHeight = min(pillsHeight, mainRect.Dy())
					chatRect, pillsRect := layout.SplitVertical(mainRect, layout.Fixed(mainRect.Dy()-pillsHeight))
					uiLayout.main = chatRect
					uiLayout.pills = pillsRect
				} else {
					uiLayout.main = mainRect
				}
			} else {
				mainRect.Max.X -= 1 // Add padding right
				uiLayout.header = headerRect
				// Reserve one line for the subagent banner at the bottom.
				mainRect, bannerRect := layout.SplitVertical(mainRect, layout.Fixed(mainRect.Dy()-1))
				uiLayout.editor = bannerRect // reuse editor slot for banner
				pillsHeight := m.pillsAreaHeight()
				if pillsHeight > 0 {
					pillsHeight = min(pillsHeight, mainRect.Dy())
					chatRect, pillsRect := layout.SplitVertical(mainRect, layout.Fixed(mainRect.Dy()-pillsHeight))
					uiLayout.main = chatRect
					uiLayout.pills = pillsRect
				} else {
					uiLayout.main = mainRect
				}
			}
			// Add bottom margin to main
			uiLayout.main.Max.Y -= 1
		} else {
			// Layout
			//
			// ------|---
			// main  |
			// ------| side
			// editor|  (hidden in subagent, replaced by banner)
			// ----------
			// help

			mainRect, sideRect := layout.SplitHorizontal(appRect, layout.Fixed(appRect.Dx()-sidebarWidth))
			// Add padding left
			sideRect.Min.X += 1
			if showEditor {
				mainRect, editorRect := layout.SplitVertical(mainRect, layout.Fixed(mainRect.Dy()-editorHeight))
				uiLayout.editor = editorRect
				mainRect.Max.X -= 1 // Add padding right
				uiLayout.sidebar = sideRect
				pillsHeight := m.pillsAreaHeight()
				if pillsHeight > 0 {
					pillsHeight = min(pillsHeight, mainRect.Dy())
					chatRect, pillsRect := layout.SplitVertical(mainRect, layout.Fixed(mainRect.Dy()-pillsHeight))
					uiLayout.main = chatRect
					uiLayout.pills = pillsRect
				} else {
					uiLayout.main = mainRect
				}
			} else {
				// Reserve one line for the subagent banner at the bottom.
				mainRect, bannerRect := layout.SplitVertical(mainRect, layout.Fixed(mainRect.Dy()-1))
				uiLayout.editor = bannerRect // reuse editor slot for banner
				mainRect.Max.X -= 1          // Add padding right
				uiLayout.sidebar = sideRect
				pillsHeight := m.pillsAreaHeight()
				if pillsHeight > 0 {
					pillsHeight = min(pillsHeight, mainRect.Dy())
					chatRect, pillsRect := layout.SplitVertical(mainRect, layout.Fixed(mainRect.Dy()-pillsHeight))
					uiLayout.main = chatRect
					uiLayout.pills = pillsRect
				} else {
					uiLayout.main = mainRect
				}
			}
			// Add bottom margin to main
			uiLayout.main.Max.Y -= 1
		}
	}

	return uiLayout
}

// uiLayout defines the positioning of UI elements.
type uiLayout struct {
	// area is the overall available area.
	area uv.Rectangle

	// header is the header shown in special cases
	// e.x when the sidebar is collapsed
	// or when in the landing page
	// or in init/config
	header uv.Rectangle

	// main is the area for the main pane. (e.x chat, configure, landing)
	main uv.Rectangle

	// pills is the area for the pills panel.
	pills uv.Rectangle

	// editor is the area for the editor pane.
	editor uv.Rectangle

	// sidebar is the area for the sidebar.
	sidebar uv.Rectangle

	// status is the area for the status view.
	status uv.Rectangle

	// session details is the area for the session details overlay in compact mode.
	sessionDetails uv.Rectangle
}

func (m *UI) openEditor(value string) tea.Cmd {
	tmpfile, err := os.CreateTemp("", "msg_*.md")
	if err != nil {
		return util.ReportError(err)
	}
	tmpPath := tmpfile.Name()
	defer tmpfile.Close() //nolint:errcheck
	if _, err := tmpfile.WriteString(value); err != nil {
		return util.ReportError(err)
	}
	cmd, err := editor.Command(
		"crush",
		tmpPath,
		editor.AtPosition(
			m.textarea.Line()+1,
			m.textarea.Column()+1,
		),
	)
	if err != nil {
		return util.ReportError(err)
	}
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		defer func() {
			_ = os.Remove(tmpPath)
		}()
		if err != nil {
			return util.ReportError(err)
		}
		content, err := os.ReadFile(tmpPath)
		if err != nil {
			return util.ReportError(err)
		}
		if len(content) == 0 {
			return util.ReportWarn("Message is empty")
		}
		return openEditorMsg{
			Text: strings.TrimSpace(string(content)),
		}
	})
}

// setEditorPrompt configures the textarea prompt function based on whether
// yolo mode is enabled.
func (m *UI) setEditorPrompt(yolo bool) {
	if yolo {
		m.textarea.SetPromptFunc(4, m.yoloPromptFunc)
		return
	}
	m.textarea.SetPromptFunc(4, m.normalPromptFunc)
}

// normalPromptFunc returns the normal editor prompt style ("  > " on first
// line, "::: " on subsequent lines).
func (m *UI) normalPromptFunc(info textarea.PromptInfo) string {
	t := m.com.Styles
	if info.LineNumber == 0 {
		if info.Focused {
			return "  > "
		}
		return "::: "
	}
	if info.Focused {
		return t.EditorPromptNormalFocused.Render()
	}
	return t.EditorPromptNormalBlurred.Render()
}

// yoloPromptFunc returns the yolo mode editor prompt style with warning icon
// and colored dots.
func (m *UI) yoloPromptFunc(info textarea.PromptInfo) string {
	t := m.com.Styles
	if info.LineNumber == 0 {
		if info.Focused {
			return t.EditorPromptYoloIconFocused.Render()
		} else {
			return t.EditorPromptYoloIconBlurred.Render()
		}
	}
	if info.Focused {
		return t.EditorPromptYoloDotsFocused.Render()
	}
	return t.EditorPromptYoloDotsBlurred.Render()
}

// closeCompletions closes the completions popup and resets state.
func (m *UI) closeCompletions() {
	m.completionsOpen = false
	m.completionsQuery = ""
	m.completionsStartIndex = 0
	m.completions.Close()
}

// insertCompletionText replaces the @query in the textarea with the given text.
// Returns false if the replacement cannot be performed.
func (m *UI) insertCompletionText(text string) bool {
	value := m.textarea.Value()
	if m.completionsStartIndex > len(value) {
		return false
	}

	word := m.textareaWord()
	endIdx := min(m.completionsStartIndex+len(word), len(value))
	newValue := value[:m.completionsStartIndex] + text + value[endIdx:]
	m.textarea.SetValue(newValue)
	m.textarea.MoveToEnd()
	m.textarea.InsertRune(' ')
	return true
}

// insertFileCompletion inserts the selected file path into the textarea,
// replacing the @query, and adds the file as an attachment.
func (m *UI) insertFileCompletion(path string) tea.Cmd {
	prevHeight := m.textarea.Height()
	if !m.insertCompletionText(path) {
		return nil
	}
	heightCmd := m.handleTextareaHeightChange(prevHeight)

	fileCmd := func() tea.Msg {
		absPath, _ := filepath.Abs(path)

		if m.hasSession() {
			// Skip attachment if file was already read and hasn't been modified.
			lastRead := m.com.App.FileTracker.LastReadTime(context.Background(), m.session.ID, absPath)
			if !lastRead.IsZero() {
				if info, err := os.Stat(path); err == nil && !info.ModTime().After(lastRead) {
					return nil
				}
			}
		} else if slices.Contains(m.sessionFileReads, absPath) {
			return nil
		}

		m.sessionFileReads = append(m.sessionFileReads, absPath)

		// Add file as attachment.
		content, err := os.ReadFile(path)
		if err != nil {
			// If it fails, let the LLM handle it later.
			return nil
		}

		return message.Attachment{
			FilePath: path,
			FileName: filepath.Base(path),
			MimeType: mimeOf(content),
			Content:  content,
		}
	}
	return tea.Batch(heightCmd, fileCmd)
}

// insertMCPResourceCompletion inserts the selected resource into the textarea,
// replacing the @query, and adds the resource as an attachment.
func (m *UI) insertMCPResourceCompletion(item completions.ResourceCompletionValue) tea.Cmd {
	displayText := cmp.Or(item.Title, item.URI)

	prevHeight := m.textarea.Height()
	if !m.insertCompletionText(displayText) {
		return nil
	}
	heightCmd := m.handleTextareaHeightChange(prevHeight)

	resourceCmd := func() tea.Msg {
		contents, err := mcp.ReadResource(
			context.Background(),
			m.com.Store(),
			item.MCPName,
			item.URI,
		)
		if err != nil {
			slog.Warn("Failed to read MCP resource", "uri", item.URI, "error", err)
			return nil
		}
		if len(contents) == 0 {
			return nil
		}

		content := contents[0]
		var data []byte
		if content.Text != "" {
			data = []byte(content.Text)
		} else if len(content.Blob) > 0 {
			data = content.Blob
		}
		if len(data) == 0 {
			return nil
		}

		mimeType := item.MIMEType
		if mimeType == "" && content.MIMEType != "" {
			mimeType = content.MIMEType
		}
		if mimeType == "" {
			mimeType = "text/plain"
		}

		return message.Attachment{
			FilePath: item.URI,
			FileName: displayText,
			MimeType: mimeType,
			Content:  data,
		}
	}
	return tea.Batch(heightCmd, resourceCmd)
}

// completionsPosition returns the X and Y position for the completions popup.
func (m *UI) completionsPosition() image.Point {
	cur := m.textarea.Cursor()
	if cur == nil {
		return image.Point{
			X: m.layout.editor.Min.X,
			Y: m.layout.editor.Min.Y,
		}
	}
	return image.Point{
		X: cur.X + m.layout.editor.Min.X,
		Y: m.layout.editor.Min.Y + cur.Y,
	}
}

// textareaWord returns the current word at the cursor position.
func (m *UI) textareaWord() string {
	return m.textarea.Word()
}

// isWhitespace returns true if the byte is a whitespace character.
func isWhitespace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// isAgentBusy returns true if the agent coordinator exists and is currently
// busy processing a request.
func (m *UI) isAgentBusy() bool {
	return m.hasSession() &&
		m.com.App != nil &&
		m.com.App.AgentCoordinator != nil &&
		m.com.App.AgentCoordinator.IsSessionBusy(m.session.ID)
}

func (m *UI) hasLiveSessionActivity() bool {
	return m.isAgentBusy() || m.hasQueuedPrompts()
}

func (m *UI) stopStaleLoadingIndicators() {
	m.setLoadingStateVisible(m.chat.MessageItems(), false)
}

func (m *UI) editorTopMarginRows() int {
	if m.attachments != nil && len(m.attachments.List()) > 0 {
		return 1
	}
	return 0
}

// hasSession returns true if there is an active session with a valid ID.
func (m *UI) hasSession() bool {
	return m.session != nil && m.session.ID != ""
}

// isSubagentSession returns true when the current session is a child of
// another session (i.e. the user navigated into a subagent view).
func (m *UI) isSubagentSession() bool {
	return m.session != nil && m.session.ParentSessionID != ""
}

func (m *UI) shouldRefreshSessionUsage(eventType pubsub.EventType, msg message.Message) bool {
	if eventType != pubsub.UpdatedEvent || msg.Role != message.Assistant {
		return false
	}
	finish := msg.FinishPart()
	return finish != nil && finish.Time > 0
}

func (m *UI) refreshCurrentSessionUsage() tea.Cmd {
	if !m.hasSession() {
		return nil
	}
	sessionID := m.session.ID
	return func() tea.Msg {
		refreshed, err := m.com.App.Sessions.Get(context.Background(), sessionID)
		if err != nil {
			return util.ReportError(err)()
		}
		return sessionUsageRefreshedMsg{session: &refreshed}
	}
}

// mimeOf detects the MIME type of the given content.
func mimeOf(content []byte) string {
	mimeBufferSize := min(512, len(content))
	return http.DetectContentType(content[:mimeBufferSize])
}

var readyPlaceholders = [...]string{
	"Ready!",
	"Ready...",
	"Ready?",
	"Ready for instructions",
}

var workingPlaceholders = [...]string{
	"Working!",
	"Working...",
	"Brrrrr...",
	"Prrrrrrrr...",
	"Processing...",
	"Thinking...",
}

// randomizePlaceholders selects random placeholder text for the textarea's
// ready and working states.
func (m *UI) randomizePlaceholders() {
	m.workingPlaceholder = workingPlaceholders[rand.Intn(len(workingPlaceholders))]
	m.readyPlaceholder = readyPlaceholders[rand.Intn(len(readyPlaceholders))]
}

// renderSubagentBanner renders a read-only banner shown instead of the editor
// when the user is viewing a subagent session.
func (m *UI) renderSubagentBanner(width int) string {
	t := m.com.Styles
	roleLabel := strings.ToUpper(m.sessionRoleLabel(m.session))
	bannerText := "SUBAGENT"
	if roleLabel != "" {
		bannerText += " " + roleLabel
	}
	tag := t.Tool.SubagentBanner.Render(bannerText)
	hint := t.Muted.Render("  read-only  [ back")
	line := lipgloss.JoinHorizontal(lipgloss.Left, tag, hint)
	return lipgloss.NewStyle().Width(width).PaddingLeft(1).Render(line)
}

// renderEditorView renders the editor view with attachments if any.
func (m *UI) renderEditorView(width int) string {
	parts := make([]string, 0, 3)
	if m.attachments != nil && len(m.attachments.List()) > 0 {
		parts = append(parts, m.attachments.Render(width))
	}
	parts = append(parts, m.textarea.View(), "") // margin at bottom of editor
	return strings.Join(parts, "\n")
}

// cacheSidebarLogo renders and caches the sidebar logo at the specified width.
func (m *UI) cacheSidebarLogo(width int) {
	m.sidebarLogo = renderLogo(m.com.Styles, true, width)
}

func (m *UI) sendMessage(content string, attachments ...message.Attachment) tea.Cmd {
	if m.com.App.AgentCoordinator == nil {
		return util.ReportError(fmt.Errorf("coder agent is not initialized"))
	}

	if m.status != nil && m.status.msg.Type == util.InfoTypeError {
		m.statusMsgSeq++
		m.status.ClearInfoMsg()
	}

	trimmedContent := strings.TrimSpace(content)

	if m.hasSession() && m.session != nil && len(m.pendingSubagentNotifications[m.session.ID]) > 0 {
		var sb strings.Builder
		for _, pending := range m.pendingSubagentNotifications[m.session.ID] {
			sb.WriteString(pending)
			sb.WriteString("\n\n")
		}
		sb.WriteString(content)
		content = sb.String()
		delete(m.pendingSubagentNotifications, m.session.ID)
		trimmedContent = strings.TrimSpace(content)
	}

	if !m.hasSession() {
		newSession, err := m.com.App.Sessions.Create(context.Background(), "New Session")
		if err != nil {
			return util.ReportError(err)
		}
		if m.forceCompactMode {
			m.isCompact = true
		}
		if newSession.ID != "" {
			m.session = &newSession
			// Load session to initialize LSP, file tracking, and prompt history
			return tea.Batch(m.loadSession(newSession.ID), func() tea.Msg {
				return sendMessageMsg{Content: content, Attachments: attachments}
			})
		}
		m.setState(uiChat, m.focus)
	}

	trimmedContent = strings.TrimSpace(content)
	if m.session != nil &&
		m.session.CollaborationMode == session.CollaborationModePlan &&
		strings.TrimSpace(m.latestProposedPlan) != "" &&
		len(attachments) == 0 &&
		trimmedContent != "" {
		if isPlanApprovalMessage(trimmedContent) {
			return m.executeApprovedPlan(m.session.ID, m.latestProposedPlan)
		}
		content = buildPlanRevisionPrompt(m.latestProposedPlan, trimmedContent)
	}

	return m.runAgentMessage(content, attachments...)
}

func (m *UI) executeApprovedPlan(sessionID, plan string) tea.Cmd {
	return tea.Sequence(
		func() tea.Msg {
			_, err := m.com.App.Sessions.UpdateCollaborationMode(context.Background(), sessionID, session.CollaborationModeDefault)
			if err != nil {
				return util.ReportError(err)()
			}
			if m.session != nil && m.session.ID == sessionID {
				m.session.CollaborationMode = session.CollaborationModeDefault
			}
			return planModeChangedMsg{SessionID: sessionID, Status: "Plan approved. Starting implementation.", Mode: session.CollaborationModeDefault}
		},
		m.runAgentMessage(planmode.BuildExecutionPrompt(plan)),
	)
}

func (m *UI) runAgentMessage(content string, attachments ...message.Attachment) tea.Cmd {
	ctx := context.Background()

	preRunCmd := func() tea.Msg {
		for _, path := range m.sessionFileReads {
			m.com.App.FileTracker.RecordRead(ctx, m.session.ID, path)
			m.com.App.LSPManager.Start(ctx, path)
		}
		return nil
	}

	// Capture session ID to avoid race with main goroutine updating m.session.
	sessionID := m.session.ID
	runCmd := func() tea.Msg {
		_, err := m.com.App.AgentCoordinator.Run(context.Background(), sessionID, content, attachments...)
		if err != nil {
			isCancelErr := errors.Is(err, context.Canceled)
			isPermissionErr := permission.IsPermissionError(err)
			if isCancelErr || isPermissionErr {
				return nil
			}
			return util.InfoMsg{
				Type: util.InfoTypeError,
				Msg:  err.Error(),
			}
		}
		return nil
	}

	refreshUsageCmd := func() tea.Msg {
		refreshed, err := m.com.App.Sessions.Get(context.Background(), sessionID)
		if err != nil {
			return util.ReportError(err)()
		}
		return sessionUsageRefreshedMsg{session: &refreshed}
	}

	return tea.Sequence(preRunCmd, runCmd, refreshUsageCmd)
}

func isPlanApprovalMessage(content string) bool {
	switch strings.ToLower(strings.TrimSpace(content)) {
	case "y", "yes", "ok", "okay", "sure", "start", "run", "execute", "implement", "go",
		"\u662f", "\u597d\u7684", "\u597d", "\u5f00\u59cb", "\u5f00\u59cb\u5427", "\u6267\u884c", "\u6267\u884c\u5427", "\u5b9e\u73b0", "\u5b9e\u73b0\u5427", "\u53ef\u4ee5", "\u786e\u8ba4", "\u7ee7\u7eed":
		return true
	default:
		return false
	}
}

func buildPlanRevisionPrompt(plan, feedback string) string {
	return fmt.Sprintf(
		"Revise the proposed plan below using the user's feedback. Stay in Plan Mode. Explore further if needed, then respond with exactly one <proposed_plan>...</proposed_plan> block.\n\nCurrent plan:\n%s\n\nUser feedback:\n%s",
		planmode.WrapProposedPlan(plan),
		feedback,
	)
}

const cancelTimerDuration = 2 * time.Second

// cancelTimerCmd creates a command that expires the cancel timer.
func cancelTimerCmd() tea.Cmd {
	return tea.Tick(cancelTimerDuration, func(time.Time) tea.Msg {
		return cancelTimerExpiredMsg{}
	})
}

// cancelAgent handles the cancel key press. The first press sets isCanceling to true
// and starts a timer. The second press (before the timer expires) actually
// cancels the agent.
func (m *UI) hasQueuedPrompts() bool {
	return m.hasSession() &&
		m.com.App != nil &&
		m.com.App.AgentCoordinator != nil &&
		m.com.App.AgentCoordinator.QueuedPrompts(m.session.ID) > 0
}

func (m *UI) syncPromptQueue() bool {
	if !m.hasSession() || m.com.App == nil || m.com.App.AgentCoordinator == nil {
		return false
	}

	queueSize := m.com.App.AgentCoordinator.QueuedPrompts(m.session.ID)
	queuePaused := m.com.App.AgentCoordinator.IsQueuePaused(m.session.ID)
	changed := queueSize != m.promptQueue || queuePaused != m.queuePaused
	m.promptQueue = queueSize
	m.queuePaused = queuePaused
	if queueSize <= 0 {
		m.selectedQueueIndex = 0
		return changed
	}
	if m.selectedQueueIndex >= queueSize {
		m.selectedQueueIndex = queueSize - 1
	}
	if m.selectedQueueIndex < 0 {
		m.selectedQueueIndex = 0
	}
	return changed
}

func (m *UI) queueSelectionActive() bool {
	return m.state == uiChat &&
		m.focus == uiFocusMain &&
		m.pillsExpanded &&
		m.focusedPillSection == pillSectionQueue &&
		m.promptQueue > 0
}

func (m *UI) moveQueueSelection(delta int) {
	if m.promptQueue <= 0 {
		m.selectedQueueIndex = 0
		m.renderPills()
		return
	}
	m.selectedQueueIndex = min(max(m.selectedQueueIndex+delta, 0), m.promptQueue-1)
	m.renderPills()
}

func (m *UI) removeSelectedQueuedPrompt() tea.Cmd {
	if !m.hasSession() || m.com.App == nil || m.com.App.AgentCoordinator == nil || m.promptQueue == 0 {
		return nil
	}
	m.com.App.AgentCoordinator.RemoveQueuedPrompt(m.session.ID, m.selectedQueueIndex)
	m.syncPromptQueue()
	m.renderPills()
	return nil
}

func (m *UI) clearQueuedPrompts() tea.Cmd {
	if !m.hasSession() || m.com.App == nil || m.com.App.AgentCoordinator == nil || m.promptQueue == 0 {
		return nil
	}
	m.com.App.AgentCoordinator.ClearQueue(m.session.ID)
	m.queuePaused = false
	m.syncPromptQueue()
	m.renderPills()
	return nil
}

func (m *UI) prioritizeSelectedQueuedPrompt() tea.Cmd {
	if !m.hasSession() || m.com.App == nil || m.com.App.AgentCoordinator == nil || m.promptQueue == 0 {
		return nil
	}
	m.com.App.AgentCoordinator.PrioritizeQueuedPrompt(m.session.ID, m.selectedQueueIndex)
	m.syncPromptQueue()
	m.renderPills()
	return nil
}

// pauseQueue pauses automatic processing of queued prompts.
// The current request continues, but subsequent queued items won't start.
func (m *UI) pauseQueue() tea.Cmd {
	if !m.hasSession() || m.com.App == nil || m.com.App.AgentCoordinator == nil {
		return nil
	}
	m.com.App.AgentCoordinator.PauseQueue(m.session.ID)
	m.queuePaused = true
	m.renderPills()
	return nil
}

// resumeQueue resumes automatic processing of queued prompts.
func (m *UI) resumeQueue() tea.Cmd {
	if !m.hasSession() || m.com.App == nil || m.com.App.AgentCoordinator == nil {
		return nil
	}
	m.com.App.AgentCoordinator.ResumeQueue(m.session.ID)
	m.queuePaused = false
	m.renderPills()
	return nil
}

func (m *UI) cancelAgent() tea.Cmd {
	if !m.hasSession() {
		return nil
	}

	coordinator := m.com.App.AgentCoordinator
	if coordinator == nil {
		return nil
	}

	if m.isCanceling {
		// Second escape press - actually cancel the agent.
		m.isCanceling = false
		coordinator.Cancel(m.session.ID)
		// Stop the spinning todo indicator.
		m.todoIsSpinning = false
		m.renderPills()
		return nil
	}

	// If there's a queue that's not yet paused, pause it first.
	// Once paused (or if already paused), ESC proceeds to the normal cancel flow.
	if coordinator.QueuedPrompts(m.session.ID) > 0 && !m.queuePaused {
		return m.pauseQueue()
	}

	// First escape press - set canceling state and start timer.
	m.isCanceling = true
	return cancelTimerCmd()
}

// openDialog opens a dialog by its ID.
func (m *UI) openDialog(id string) tea.Cmd {
	var cmds []tea.Cmd
	switch id {
	case dialog.SessionsID:
		if cmd := m.openSessionsDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case dialog.ModelsID:
		if cmd := m.openModelsDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case dialog.CommandsID:
		if cmd := m.openCommandsDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case dialog.HandoffID:
		if cmd := m.openHandoffDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case dialog.MCPID:
		if cmd := m.openMCPDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case dialog.ReasoningID:
		if cmd := m.openReasoningDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case dialog.FilePickerID:
		if cmd := m.openFilesDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case dialog.DenialsID:
		if cmd := m.openDenialsDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case dialog.QuitID:
		if cmd := m.openQuitDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	default:
		// Unknown dialog
		break
	}
	return tea.Batch(cmds...)
}

// openQuitDialog opens the quit confirmation dialog.
func (m *UI) openQuitDialog() tea.Cmd {
	if m.dialog.ContainsDialog(dialog.QuitID) {
		// Bring to front
		m.dialog.BringToFront(dialog.QuitID)
		return nil
	}

	quitDialog := dialog.NewQuit(m.com)
	m.dialog.OpenDialog(quitDialog)
	return nil
}

// openDenialsDialog opens the denials review dialog.
func (m *UI) openDenialsDialog() tea.Cmd {
	if m.dialog.ContainsDialog(dialog.DenialsID) {
		// Bring to front
		m.dialog.BringToFront(dialog.DenialsID)
		return nil
	}

	var entries []*permission.DenialQueueEntry
	if m.session != nil {
		if dq := m.com.App.Permissions.GetDenialQueue(m.session.ID); dq != nil {
			entries = dq.Entries()
		}
	}

	denialsDialog := dialog.NewDenialsDialog(m.com, entries)
	m.dialog.OpenDialog(denialsDialog)
	return nil
}

// openModelsDialog opens the models dialog.
func (m *UI) openModelsDialog() tea.Cmd {
	if m.dialog.ContainsDialog(dialog.ModelsID) {
		// Bring to front
		m.dialog.BringToFront(dialog.ModelsID)
		return nil
	}

	isOnboarding := m.state == uiOnboarding
	modelsDialog, err := dialog.NewModels(m.com, isOnboarding)
	if err != nil {
		return util.ReportError(err)
	}

	m.dialog.OpenDialog(modelsDialog)

	return nil
}

// openCommandsDialog opens the commands dialog.
func (m *UI) openCommandsDialog() tea.Cmd {
	if m.dialog.ContainsDialog(dialog.CommandsID) {
		// Bring to front
		m.dialog.BringToFront(dialog.CommandsID)
		return nil
	}

	var sessionID string
	mode := session.CollaborationModeDefault
	hasSession := m.session != nil
	if hasSession {
		sessionID = m.session.ID
		mode = m.session.CollaborationMode
	}
	hasTodos := hasSession && hasIncompleteTodos(m.session.Todos)
	hasQueue := m.promptQueue > 0
	queuePaused := m.queuePaused

	permissionMode := session.PermissionModeDefault
	if m.session != nil {
		permissionMode = m.session.PermissionMode
	}
	// Get denial queue count for auto mode
	denialCount := 0
	if m.session != nil {
		if dq := m.com.App.Permissions.GetDenialQueue(m.session.ID); dq != nil {
			denialCount = dq.Size()
		}
	}
	commands, err := dialog.NewCommands(m.com, sessionID, hasSession, hasTodos, hasQueue, queuePaused, mode, permissionMode, m.latestProposedPlan, denialCount, m.customCommands, m.mcpPrompts)
	if err != nil {
		return util.ReportError(err)
	}

	m.dialog.OpenDialog(commands)

	return commands.InitialCmd()
}

func (m *UI) openHandoffDialog() tea.Cmd {
	if m.dialog.ContainsDialog(dialog.HandoffID) {
		m.dialog.BringToFront(dialog.HandoffID)
		return nil
	}
	if m.session == nil {
		return util.ReportWarn("Open a session before creating a handoff.")
	}

	handoffDialog := dialog.NewHandoff(m.com, m.session.ID)
	m.dialog.OpenDialog(handoffDialog)
	return nil
}

// openMCPDialog opens the MCP management dialog.
func (m *UI) openMCPDialog() tea.Cmd {
	if m.dialog.ContainsDialog(dialog.MCPID) {
		m.dialog.BringToFront(dialog.MCPID)
		return nil
	}

	mcpDialog, err := dialog.NewMCP(m.com, m.mcpStates)
	if err != nil {
		return util.ReportError(err)
	}

	m.dialog.OpenDialog(mcpDialog)
	return nil
}

// openReasoningDialog opens the reasoning effort dialog.
func (m *UI) openReasoningDialog() tea.Cmd {
	if m.dialog.ContainsDialog(dialog.ReasoningID) {
		m.dialog.BringToFront(dialog.ReasoningID)
		return nil
	}

	reasoningDialog, err := dialog.NewReasoning(m.com)
	if err != nil {
		return util.ReportError(err)
	}

	m.dialog.OpenDialog(reasoningDialog)
	return nil
}

// openSessionsDialog opens the sessions dialog. If the dialog is already open,
// it brings it to the front. Otherwise, it will list all the sessions and open
// the dialog.
func (m *UI) openSessionsDialog() tea.Cmd {
	if m.dialog.ContainsDialog(dialog.SessionsID) {
		// Bring to front
		m.dialog.BringToFront(dialog.SessionsID)
		return nil
	}

	selectedSessionID := ""
	if m.session != nil {
		selectedSessionID = m.session.ID
	}

	dialog, err := dialog.NewSessions(m.com, selectedSessionID)
	if err != nil {
		return util.ReportError(err)
	}

	m.dialog.OpenDialog(dialog)
	return nil
}

// modelSupportsImages returns whether the currently configured coder model
// supports image inputs. Defaults to true when the configuration is
// unavailable to avoid blocking users unexpectedly.
func (m *UI) modelSupportsImages() bool {
	cfg := m.com.Config()
	if cfg == nil {
		return true
	}
	agentCfg, ok := cfg.Agents[config.AgentCoder]
	if !ok {
		return true
	}
	model := cfg.GetModelByType(agentCfg.Model)
	return model == nil || model.SupportsImages
}

// openFilesDialog opens the file picker dialog.
func (m *UI) openFilesDialog() tea.Cmd {
	if m.dialog.ContainsDialog(dialog.FilePickerID) {
		// Bring to front
		m.dialog.BringToFront(dialog.FilePickerID)
		return nil
	}

	filePicker, cmd := dialog.NewFilePicker(m.com)
	filePicker.SetImageCapabilities(&m.caps)
	m.dialog.OpenDialog(filePicker)

	return cmd
}

// openPermissionsDialog opens the permissions dialog for a permission request.
func (m *UI) openPermissionsDialog(perm permission.PermissionRequest) tea.Cmd {
	// Close any existing permissions dialog first.
	m.dialog.CloseDialog(dialog.PermissionsID)

	// Get diff mode from config.
	var opts []dialog.PermissionsOption
	if diffMode := m.com.Config().Options.TUI.DiffMode; diffMode != "" {
		opts = append(opts, dialog.WithDiffMode(diffMode == "split"))
	}

	permDialog := dialog.NewPermissions(m.com, perm, opts...)
	m.dialog.OpenDialog(permDialog)
	return nil
}

// handlePermissionNotification updates tool items when permission state changes.
func (m *UI) handlePermissionNotification(notification permission.PermissionNotification) {
	toolItem := m.chat.MessageItem(notification.ToolCallID)
	if toolItem == nil {
		return
	}

	if permItem, ok := toolItem.(chat.ToolMessageItem); ok {
		if notification.Granted {
			permItem.SetStatus(chat.ToolStatusRunning)
		} else {
			permItem.SetStatus(chat.ToolStatusAwaitingPermission)
		}
	}
}

// handleAgentNotification translates domain agent events into desktop
// notifications using the UI notification backend.
func (m *UI) handleAgentNotification(n notify.Notification) tea.Cmd {
	switch n.Type {
	case notify.TypeAgentFinished:
		cmd := m.sendNotification(notification.Notification{
			Title:   "Crush finished turn",
			Message: fmt.Sprintf("Agent's turn completed in \"%s\"", n.SessionTitle),
		})

		// Check if there are queued subagent notifications for this session that can now be sent.
		if m.hasSession() && m.session != nil &&
			len(m.pendingSubagentNotifications[m.session.ID]) > 0 &&
			!m.dialog.HasDialogs() &&
			m.state == uiChat &&
			(m.focus != uiFocusEditor || strings.TrimSpace(m.textarea.Value()) == "") {
			var sb strings.Builder
			for _, pending := range m.pendingSubagentNotifications[m.session.ID] {
				sb.WriteString(pending)
				sb.WriteString("\n\n")
			}
			delete(m.pendingSubagentNotifications, m.session.ID)
			return tea.Batch(
				cmd,
				m.sendNotification(notification.Notification{
					Title:   "Crush background task finished",
					Message: "Background task completed. Resuming session.",
				}),
				m.sendMessage(sb.String()),
			)
		}
		return cmd

	case notify.TypeSubagentFinished:
		if n.SessionID == "" {
			return nil
		}

		isCurrentSession := m.hasSession() && m.session != nil && n.SessionID == m.session.ID

		canAutoWakeup := isCurrentSession &&
			!m.isAgentBusy() &&
			!m.hasQueuedPrompts() &&
			m.state == uiChat &&
			!m.dialog.HasDialogs() &&
			(m.focus != uiFocusEditor || strings.TrimSpace(m.textarea.Value()) == "")

		if canAutoWakeup {
			return tea.Batch(
				m.sendNotification(notification.Notification{
					Title:   "Crush background task finished",
					Message: fmt.Sprintf("Subagent task %s completed.", n.SubagentID),
				}),
				m.sendMessage(n.Summary),
			)
		} else {
			if m.pendingSubagentNotifications == nil {
				m.pendingSubagentNotifications = make(map[string][]string)
			}
			m.pendingSubagentNotifications[n.SessionID] = append(m.pendingSubagentNotifications[n.SessionID], n.Summary)
			return m.sendNotification(notification.Notification{
				Title:   "Crush background task finished (queued)",
				Message: fmt.Sprintf("Subagent task %s completed. Notification has been queued.", n.SubagentID),
			})
		}

	default:
		return nil
	}
}

// newSession clears the current session state and prepares for a new session.
// The actual session creation happens when the user sends their first message.
// Returns a command to reload prompt history.
func (m *UI) newSession() tea.Cmd {
	if !m.hasSession() {
		return nil
	}

	m.session = nil
	m.sessionFiles = nil
	m.sessionFileReads = nil
	m.setState(uiLanding, uiFocusEditor)
	m.textarea.Focus()
	m.chat.Blur()
	m.chat.ClearMessages()
	m.pillsExpanded = false
	m.promptQueue = 0
	m.pillsView = ""
	m.historyReset()
	agenttools.ResetCache()
	return tea.Batch(
		func() tea.Msg {
			m.com.App.LSPManager.StopAll(context.Background())
			return nil
		},
		m.loadPromptHistory(),
	)
}

// handlePasteMsg handles a paste message.
func (m *UI) handlePasteMsg(msg tea.PasteMsg) tea.Cmd {
	if m.dialog.HasDialogs() {
		return m.handleDialogMsg(msg)
	}

	if m.focus != uiFocusEditor {
		return nil
	}

	if time.Since(m.lastClipboardPasteShortcut) <= 500*time.Millisecond {
		return nil
	}

	// If the terminal already provided text content, handle it directly.
	// This avoids slow PowerShell calls on Windows when pasting plain text.
	// When clipboard contains an image, terminals typically send empty Content,
	// so we only check for image/file when Content is empty.
	if msg.Content != "" {
		return m.handleClipboardFallback(clipboardFallbackMsg{pasteMsg: msg})
	}

	// Only attempt image paste when the current model supports images.
	if !m.modelSupportsImages() {
		return m.handleClipboardFallback(clipboardFallbackMsg{pasteMsg: msg})
	}
	// Try to paste image/file from clipboard first.
	return m.pasteImageFromClipboard(msg)
}

func hasPasteExceededThreshold(msg tea.PasteMsg) bool {
	var (
		lineCount = 0
		colCount  = 0
	)
	for line := range strings.SplitSeq(msg.Content, "\n") {
		lineCount++
		colCount = max(colCount, len(line))

		if lineCount > pasteLinesThreshold || colCount > pasteColsThreshold {
			return true
		}
	}
	return false
}

// handleClipboardFallback handles the case when clipboard has no image/file content.
// It processes the original paste event as a text paste.
// If pasteMsg is empty (keyboard shortcut case), there's nothing to do.
func (m *UI) handleClipboardFallback(msg clipboardFallbackMsg) tea.Cmd {
	// If there's no text content to fall back to, just return nil.
	if msg.pasteMsg.Content == "" {
		return nil
	}
	if hasPasteExceededThreshold(msg.pasteMsg) {
		pasteIdx := m.pasteIdx()
		return func() tea.Msg {
			content := []byte(msg.pasteMsg.Content)
			if int64(len(content)) > common.MaxAttachmentSize {
				return util.ReportWarn("Paste is too big (>5mb)")
			}
			name := fmt.Sprintf("paste_%d.txt", pasteIdx)
			mimeBufferSize := min(512, len(content))
			mimeType := http.DetectContentType(content[:mimeBufferSize])
			return message.Attachment{
				FileName: name,
				FilePath: name,
				MimeType: mimeType,
				Content:  content,
			}
		}
	}

	// Attempt to parse pasted content as file paths. If possible to parse,
	// all files exist and are valid, add as attachments.
	// Otherwise, paste as text.
	paths := fsext.ParsePastedFiles(msg.pasteMsg.Content)
	allExistsAndValid := func() bool {
		if len(paths) == 0 {
			return false
		}
		for _, path := range paths {
			if _, err := os.Stat(path); os.IsNotExist(err) {
				return false
			}

			lowerPath := strings.ToLower(path)
			isValid := false
			for _, ext := range common.AllowedImageTypes {
				if strings.HasSuffix(lowerPath, ext) {
					isValid = true
					break
				}
			}
			if !isValid {
				return false
			}
		}
		return true
	}
	if !allExistsAndValid() {
		prevHeight := m.textarea.Height()
		return m.updateTextareaWithPrevHeight(msg.pasteMsg, prevHeight)
	}

	var cmds []tea.Cmd
	for _, path := range paths {
		cmds = append(cmds, m.handleFilePathPaste(path))
	}
	return tea.Batch(cmds...)
}

// handleFilePathPaste handles a pasted file path.
func (m *UI) handleFilePathPaste(path string) tea.Cmd {
	return func() tea.Msg {
		fileInfo, err := os.Stat(path)
		if err != nil {
			return util.ReportError(err)
		}
		if fileInfo.IsDir() {
			return util.ReportWarn("Cannot attach a directory")
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return util.ReportError(err)
		}

		mimeBufferSize := min(512, len(content))
		mimeType := http.DetectContentType(content[:mimeBufferSize])
		fileName := filepath.Base(path)

		// Compress image if it exceeds 1MB.
		if strings.HasPrefix(mimeType, "image/") {
			config := imageutil.DefaultCompressionConfig()
			result, compressErr := imageutil.CompressImage(content, mimeType, config)
			if compressErr != nil {
				slog.Warn("Failed to compress pasted image", "error", compressErr, "path", path)
				// Fall through with original data.
			} else if result.WasCompressed {
				content = result.Data
				mimeType = result.MimeType
			}
		}
		if int64(len(content)) > common.MaxAttachmentSize {
			return util.ReportWarn("File is too big (>5mb)")
		}

		return message.Attachment{
			FilePath: path,
			FileName: fileName,
			MimeType: mimeType,
			Content:  content,
		}
	}
}

func clipboardAttachmentSignature(att message.Attachment) string {
	sum := sha256.Sum256(att.Content)
	return att.MimeType + ":" + hex.EncodeToString(sum[:])
}

func (m *UI) shouldSkipClipboardAttachment(att message.Attachment) bool {
	sig := clipboardAttachmentSignature(att)
	if sig == m.lastClipboardAttachmentSig && time.Since(m.lastClipboardAttachmentAt) <= 200*time.Millisecond {
		return true
	}
	// Update state - this is safe because we're in the Update loop, not in a goroutine.
	m.lastClipboardAttachmentSig = sig
	m.lastClipboardAttachmentAt = time.Now()
	return false
}

// pasteImageFromClipboard returns a command that reads image data from the
// system clipboard and creates an attachment. All IO operations are performed
// inside the returned tea.Cmd to avoid blocking the Update loop.
// The original PasteMsg is passed so we can fall back to text paste if needed.
func (m *UI) pasteImageFromClipboard(originalMsg tea.PasteMsg) tea.Cmd {
	// Return a command that performs all clipboard IO asynchronously.
	return func() tea.Msg {
		// First, try to read image data from clipboard.
		imageData, err := readClipboard(clipboardFormatImage)
		if err == nil && len(imageData) > 0 {
			return clipboardImageMsg{imageData: imageData, err: nil}
		}

		// Try to read file list from clipboard.
		paths, pathsErr := readClipboardFileList()
		if pathsErr == nil && len(paths) > 0 {
			return clipboardPathsMsg{paths: paths}
		}

		// No clipboard image/file content found; fall back to text paste handling.
		// The fallback handler properly validates file paths and falls through
		// to textarea text paste when content is not valid file paths.
		return clipboardFallbackMsg{pasteMsg: originalMsg}
	}
}

// handleClipboardImageMsg processes clipboard image data returned by the command.
func (m *UI) handleClipboardImageMsg(msg clipboardImageMsg) tea.Cmd {
	if msg.err != nil || len(msg.imageData) == 0 {
		return nil
	}

	// Return a command that compresses the image.
	pasteIdx := m.pasteIdx()
	return func() tea.Msg {
		// Compress image if it exceeds 1MB.
		imageData := msg.imageData
		mimeType := mimeOf(imageData)
		config := imageutil.DefaultCompressionConfig()
		result, compressErr := imageutil.CompressImage(imageData, mimeType, config)
		if compressErr != nil {
			slog.Warn("Failed to compress clipboard image", "error", compressErr)
			// Fall through with original data.
		} else if result.WasCompressed {
			imageData = result.Data
			mimeType = result.MimeType
		}
		if int64(len(imageData)) > common.MaxAttachmentSize {
			return util.InfoMsg{
				Type: util.InfoTypeError,
				Msg:  "File too large, max 5MB",
			}
		}

		// Determine file extension based on MIME type.
		ext := ".png"
		if mimeType == "image/jpeg" {
			ext = ".jpg"
		}
		name := fmt.Sprintf("paste_%d%s", pasteIdx, ext)
		return message.Attachment{
			FilePath: name,
			FileName: name,
			MimeType: mimeType,
			Content:  imageData,
		}
	}
}

// handleClipboardPathsMsg processes clipboard file paths returned by the command.
func (m *UI) handleClipboardPathsMsg(msg clipboardPathsMsg) tea.Cmd {
	if len(msg.paths) == 0 {
		return nil
	}
	return m.attachmentFromClipboardPaths(msg.paths)
}

func (m *UI) attachmentFromClipboardPaths(paths []string) tea.Cmd {
	// Return a command that performs all IO asynchronously.
	return func() tea.Msg {
		for _, path := range paths {
			attachment, err := attachmentFromClipboardPath(path)
			if err == nil {
				return attachment
			}
		}
		return nil
	}
}

func attachmentFromClipboardPath(rawPath string) (message.Attachment, error) {
	path := normalizeClipboardPath(rawPath)
	if path == "" {
		return message.Attachment{}, fmt.Errorf("clipboard path is empty")
	}
	if _, statErr := os.Stat(path); statErr != nil {
		return message.Attachment{}, statErr
	}

	lowerPath := strings.ToLower(path)
	isAllowed := false
	for _, ext := range common.AllowedImageTypes {
		if strings.HasSuffix(lowerPath, ext) {
			isAllowed = true
			break
		}
	}
	if !isAllowed {
		return message.Attachment{}, fmt.Errorf("clipboard path is not a supported image")
	}

	content, readErr := os.ReadFile(path)
	if readErr != nil {
		return message.Attachment{}, readErr
	}

	// Compress image if it exceeds 1MB.
	mimeType := mimeOf(content)
	config := imageutil.DefaultCompressionConfig()
	result, compressErr := imageutil.CompressImage(content, mimeType, config)
	if compressErr != nil {
		slog.Warn("Failed to compress clipboard image", "error", compressErr, "path", path)
		// Fall through with original data.
	} else if result.WasCompressed {
		content = result.Data
		mimeType = result.MimeType
	}
	if int64(len(content)) > common.MaxAttachmentSize {
		return message.Attachment{}, fmt.Errorf("file too large")
	}

	return message.Attachment{
		FilePath: path,
		FileName: filepath.Base(path),
		MimeType: mimeType,
		Content:  content,
	}, nil
}

func clipboardPathCandidates(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	parts := strings.FieldsFunc(text, func(r rune) bool {
		return r == '\n' || r == 0
	})
	paths := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			paths = append(paths, part)
		}
	}
	return paths
}

func normalizeClipboardPath(path string) string {
	path = strings.TrimSpace(path)
	path = strings.Trim(path, "\"")
	path = strings.ReplaceAll(path, "\\ ", " ")
	if strings.HasPrefix(strings.ToLower(path), "file://") {
		parsed, err := url.Parse(path)
		if err == nil {
			path = parsed.Path
			if decoded, decodeErr := url.PathUnescape(path); decodeErr == nil {
				path = decoded
			}
			if len(path) >= 3 && path[0] == '/' && path[2] == ':' {
				path = path[1:]
			}
		}
	}
	if len(path) >= 2 && path[1] == ':' {
		path = strings.ReplaceAll(path, "/", "\\")
	}
	return filepath.Clean(path)
}

// pasteSkippedMsg is returned by pasteImageFromClipboard when the same image
// was pasted too recently and was silently skipped to prevent duplicates.
type pasteSkippedMsg struct{}

// clipboardImageMsg is returned by the clipboard read command.
type clipboardImageMsg struct {
	imageData []byte
	err       error
}

// clipboardPathsMsg is returned by the clipboard file list read command.
type clipboardPathsMsg struct {
	paths []string
}

// clipboardFallbackMsg is returned when clipboard has no image/file content,
// signaling that the original paste event should be handled as text paste.
type clipboardFallbackMsg struct {
	pasteMsg tea.PasteMsg
}

var pasteRE = regexp.MustCompile(`paste_(\d+)\.[^.]+`)

func (m *UI) pasteIdx() int {
	result := 0
	for _, at := range m.attachments.List() {
		found := pasteRE.FindStringSubmatch(at.FileName)
		if len(found) == 0 {
			continue
		}
		idx, err := strconv.Atoi(found[1])
		if err == nil {
			result = max(result, idx)
		}
	}
	return result + 1
}

// drawSessionDetails draws the session details in compact mode.
func (m *UI) drawSessionDetails(scr uv.Screen, area uv.Rectangle) {
	if m.session == nil {
		return
	}

	s := m.com.Styles

	width := area.Dx() - s.CompactDetails.View.GetHorizontalFrameSize()
	height := area.Dy() - s.CompactDetails.View.GetVerticalFrameSize()

	title := s.CompactDetails.Title.Width(width).MaxHeight(2).Render(m.session.Title)
	blocks := []string{
		title,
		"",
		m.modelInfo(width),
		"",
	}

	detailsHeader := lipgloss.JoinVertical(
		lipgloss.Left,
		blocks...,
	)

	version := s.CompactDetails.Version.Foreground(s.Border).Width(width).AlignHorizontal(lipgloss.Right).Render(version.Version)

	remainingHeight := height - lipgloss.Height(detailsHeader) - lipgloss.Height(version)

	const maxSectionWidth = 50
	sectionWidth := min(maxSectionWidth, width/2-1) // account for 1 space
	maxItemsPerSection := remainingHeight - 3       // Account for section title and spacing

	filesSection := m.filesInfo(m.com.Store().WorkingDir(), sectionWidth, maxItemsPerSection, false)
	lspSection := m.lspInfo(sectionWidth, maxItemsPerSection, false)
	mcpSection := m.mcpInfo(sectionWidth, maxItemsPerSection, false)
	timelineSection := m.timelineInfo(sectionWidth, maxItemsPerSection, false)
	upperSections := lipgloss.JoinHorizontal(lipgloss.Top, filesSection, " ", lspSection)
	lowerSections := lipgloss.JoinHorizontal(lipgloss.Top, mcpSection, " ", timelineSection)
	uv.NewStyledString(
		s.CompactDetails.View.
			Width(area.Dx()).
			Render(
				lipgloss.JoinVertical(
					lipgloss.Left,
					detailsHeader,
					upperSections,
					"",
					lowerSections,
					version,
				),
			),
	).Draw(scr, area)
}

func (m *UI) runMCPPrompt(clientID, promptID string, arguments map[string]string) tea.Cmd {
	load := func() tea.Msg {
		prompt, err := commands.GetMCPPrompt(m.com.Store(), clientID, promptID, arguments)
		if err != nil {
			// TODO: make this better
			return util.ReportError(err)()
		}

		if prompt == "" {
			return nil
		}
		return sendMessageMsg{
			Content: prompt,
		}
	}

	var cmds []tea.Cmd
	if cmd := m.dialog.StartLoading(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	cmds = append(cmds, load, func() tea.Msg {
		return closeDialogMsg{}
	})

	return tea.Sequence(cmds...)
}

func (m *UI) handleStateChanged() tea.Cmd {
	return func() tea.Msg {
		m.com.App.UpdateAgentModel(context.Background())
		return mcpStateChangedMsg{
			states: mcp.GetStates(),
		}
	}
}

func handleMCPPromptsEvent(name string) tea.Cmd {
	return func() tea.Msg {
		mcp.RefreshPrompts(context.Background(), name)
		return nil
	}
}

func handleMCPToolsEvent(cfg *config.ConfigStore, name string) tea.Cmd {
	return func() tea.Msg {
		mcp.RefreshTools(
			context.Background(),
			cfg,
			name,
		)
		return nil
	}
}

func handleMCPResourcesEvent(name string) tea.Cmd {
	return func() tea.Msg {
		mcp.RefreshResources(context.Background(), name)
		return nil
	}
}

func (m *UI) authenticateMCP(name string) tea.Cmd {
	return func() tea.Msg {
		if err := mcp.Authenticate(context.Background(), m.com.Store(), name); err != nil {
			return util.ReportError(err)()
		}
		if err := mcp.Reconnect(context.Background(), m.com.Store(), name); err != nil {
			return util.ReportError(err)()
		}
		return util.NewInfoMsg("MCP " + name + " authenticated")
	}
}

func (m *UI) reconnectMCP(name string) tea.Cmd {
	return func() tea.Msg {
		if err := mcp.Reconnect(context.Background(), m.com.Store(), name); err != nil {
			return util.ReportError(err)()
		}
		return util.NewInfoMsg("MCP " + name + " reconnected")
	}
}

func (m *UI) toggleMCP(name string, enable bool) tea.Cmd {
	return func() tea.Msg {
		// Find the MCP config key
		var mcpKey string
		for k := range m.com.Config().MCP {
			if k == name {
				mcpKey = k
				break
			}
		}
		if mcpKey == "" {
			return util.ReportError(errors.New("MCP server not found: " + name))()
		}

		// Update the disabled field in memory and persist to disk
		if err := m.com.Store().SetMCPDisabled(config.ScopeWorkspace, mcpKey, !enable); err != nil {
			return util.ReportError(err)()
		}

		// Reconnect so the MCP layer picks up the new disabled state
		// and publishes a state-changed event for the UI to refresh.
		if err := mcp.Reconnect(context.Background(), m.com.Store(), name); err != nil {
			return util.ReportError(err)()
		}

		if enable {
			return util.NewInfoMsg("MCP " + name + " enabled and reconnected")
		}
		return util.NewInfoMsg("MCP " + name + " disabled")
	}
}

func (m *UI) copyChatHighlight() tea.Cmd {
	text := m.chat.HighlightContent()
	return common.CopyToClipboardWithCallback(
		text,
		"Selected text copied to clipboard",
		func() tea.Msg {
			m.chat.ClearMouse()
			return nil
		},
	)
}

func (m *UI) enableDockerMCP() tea.Msg {
	store := m.com.Store()
	// Stage Docker MCP in memory first so startup and persistence can be atomic.
	mcpConfig, err := store.PrepareDockerMCPConfig()
	if err != nil {
		return util.ReportError(err)()
	}

	ctx := context.Background()
	if err := mcp.InitializeSingle(ctx, config.DockerMCPName, store); err != nil {
		// Roll back runtime and in-memory state when startup fails.
		disableErr := mcp.DisableSingle(store, config.DockerMCPName)
		delete(store.Config().MCP, config.DockerMCPName)
		return util.ReportError(fmt.Errorf("failed to start docker MCP: %w", errors.Join(err, disableErr)))()
	}

	if err := store.PersistDockerMCPConfig(mcpConfig); err != nil {
		// Roll back runtime and in-memory state if persistence fails.
		disableErr := mcp.DisableSingle(store, config.DockerMCPName)
		delete(store.Config().MCP, config.DockerMCPName)
		return util.ReportError(fmt.Errorf("docker MCP started but failed to persist configuration: %w", errors.Join(err, disableErr)))()
	}

	// Refresh agent tools to include the new Docker MCP tools.
	if m.com.App.AgentCoordinator != nil {
		if err := m.com.App.AgentCoordinator.RefreshTools(ctx); err != nil {
			slog.Warn("Failed to refresh agent tools after enabling Docker MCP", "error", err)
		}
	}

	return util.NewInfoMsg("Docker MCP enabled and started successfully")
}

func (m *UI) disableDockerMCP() tea.Msg {
	store := m.com.Store()
	// Close the Docker MCP client.
	if err := mcp.DisableSingle(store, config.DockerMCPName); err != nil {
		return util.ReportError(fmt.Errorf("failed to disable docker MCP: %w", err))()
	}

	// Remove from config and persist.
	if err := store.DisableDockerMCP(); err != nil {
		return util.ReportError(err)()
	}

	// Refresh agent tools to remove the Docker MCP tools.
	if m.com.App.AgentCoordinator != nil {
		if err := m.com.App.AgentCoordinator.RefreshTools(context.Background()); err != nil {
			slog.Warn("Failed to refresh agent tools after disabling Docker MCP", "error", err)
		}
	}

	return util.NewInfoMsg("Docker MCP disabled successfully")
}

// renderLogo renders the Crush logo with the given styles and dimensions.
func renderLogo(t *styles.Styles, compact bool, width int) string {
	return logo.Render(t, version.Version, compact, logo.Opts{
		FieldColor:   t.LogoFieldColor,
		TitleColorA:  t.LogoTitleColorA,
		TitleColorB:  t.LogoTitleColorB,
		CharmColor:   t.LogoCharmColor,
		VersionColor: t.LogoVersionColor,
		Width:        width,
	})
}

// enhancePromptCmd returns a tea.Cmd that uses the small LLM model to rewrite
// the current prompt text to be clearer and more specific (Ctrl+P feature,
// ported from Augment's V0o template).
// The current session ID is passed so the LLM can use conversation history as context.
func (m *UI) enhancePromptCmd() tea.Cmd {
	prompt := m.textarea.Value()
	coordinator := m.com.App.AgentCoordinator
	if coordinator == nil {
		return func() tea.Msg {
			return promptEnhanceResultMsg{err: fmt.Errorf("coder agent is not initialized")}
		}
	}
	var sessionID string
	if m.session != nil {
		sessionID = m.session.ID
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		enhanced, err := coordinator.EnhancePrompt(ctx, sessionID, prompt)
		return promptEnhanceResultMsg{enhanced: enhanced, err: err}
	}
}

func (m *UI) handleToolRuntimeEvent(event pubsub.Event[toolruntime.State]) tea.Cmd {
	if m.session == nil {
		return nil
	}
	state := event.Payload
	if state.SessionID != m.session.ID {
		return nil
	}

	item := m.chat.MessageItem(state.ToolCallID)
	toolItem, ok := item.(chat.ToolMessageItem)
	if !ok {
		return nil
	}

	switch event.Type {
	case pubsub.DeletedEvent:
		toolItem.SetRuntimeState(nil)
	default:
		toolItem.SetRuntimeState(&state)
	}

	if m.chat.Follow() {
		return m.chat.ScrollToBottomAndAnimate()
	}
	return nil
}
