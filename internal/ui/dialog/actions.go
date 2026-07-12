package dialog

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/commands"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/oauth"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/util"
	"github.com/charmbracelet/crush/internal/userinput"
)

// ActionClose is a message to close the current dialog.
type ActionClose struct{}

// ActionQuit is a message to quit the application.
type ActionQuit = tea.QuitMsg

// ActionOpenDialog is a message to open a dialog.
type ActionOpenDialog struct {
	DialogID string
}

// ActionSelectSession is a message indicating a session has been selected.
type ActionSelectSession struct {
	Session session.Session
}

// ActionSelectModel is a message indicating a model has been selected.
type ActionSelectModel struct {
	Provider       catwalk.Provider
	Model          config.SelectedModel
	ModelType      config.SelectedModelType
	ReAuthenticate bool
	CloseDialog    bool
}

// Messages for commands
type (
	ActionNewSession                  struct{}
	// ActionForkSession starts a new session seeded with messages up to and
	// including the turn ending at the given message ID.
	ActionForkSession struct {
		SessionID string
		MessageID string
	}
	ActionToggleHelp struct{}
	ActionToggleCompactMode           struct{}
	ActionToggleThinking              struct{}
	ActionTogglePills                 struct{}
	ActionPauseQueue                  struct{}
	ActionResumeQueue                 struct{}
	ActionExternalEditor              struct{}
	ActionToggleNotifications         struct{}
	ActionToggleTransparentBackground struct{}
	ActionToggleAutoMode              struct {
		SessionID string
		NextMode  session.PermissionMode
	}
	ActionTogglePlanMode struct {
		SessionID string
		NextMode  session.CollaborationMode
	}
	ActionToggleOrchestrateMode struct {
		SessionID string
		NextMode  session.CollaborationMode
	}
	ActionExecuteProposedPlan struct {
		SessionID string
		Plan      string
	}
	ActionSubmitPlanFeedback struct {
		SessionID string
		Feedback  string
	}
	// ActionExecuteWithCompact approves a plan and compacts the context before
	// execution. The conversation is summarized, then the plan is executed in the
	// compacted session.
	ActionExecuteWithCompact struct {
		SessionID string
		Plan      string
	}
	// ActionExecuteKeepContext approves a plan and executes it without
	// compacting, preserving the full exploration history.
	ActionExecuteKeepContext struct {
		SessionID string
		Plan      string
	}
	// ActionSetGoal sets or replaces the session's goal objective.
	ActionSetGoal struct {
		SessionID string
		Goal      string
		Budget    int64
	}
	// ActionSetGoalBudget updates the current goal token budget.
	ActionSetGoalBudget struct {
		SessionID string
		Budget    int64
	}
	// ActionStartGuidedGoal starts an agent-led goal refinement flow.
	ActionStartGuidedGoal struct {
		SessionID string
		RoughGoal string
	}
	// ActionPauseGoal pauses the current goal.
	ActionPauseGoal struct {
		SessionID string
	}
	// ActionResumeGoal resumes a paused goal.
	ActionResumeGoal struct {
		SessionID string
	}
	// ActionDropGoal drops the current goal.
	ActionDropGoal struct {
		SessionID string
	}
	ActionInitializeProject struct{}
	ActionSummarize         struct {
		SessionID string
	}
	ActionGenerateHandoff struct {
		SessionID string
		Goal      string
	}
	// ActionSelectReasoningEffort is a message indicating a reasoning effort
	// has been selected.
	ActionSelectReasoningEffort struct {
		Effort string
	}
	ActionPermissionResponse struct {
		Permission permission.PermissionRequest
		Action     PermissionAction
	}
	// ActionRunCustomCommand is a message to run a custom command.
	ActionRunCustomCommand struct {
		Content   string
		Arguments []commands.Argument
		Args      map[string]string // Actual argument values
	}
	// ActionRunMCPPrompt is a message to run a custom command.
	ActionRunMCPPrompt struct {
		Title       string
		Description string
		PromptID    string
		ClientID    string
		Arguments   []commands.Argument
		Args        map[string]string // Actual argument values
	}
	ActionResolveUserInput struct {
		Response userinput.Response
	}
	ActionAuthenticateMCP struct {
		Name string
	}
	ActionReconnectMCP struct {
		Name string
	}
	ActionOpenMCPDetail struct {
		Name string
	}
	ActionToggleMCP struct {
		Name   string
		Enable bool
	}
	// ActionEnableDockerMCP is a message to enable Docker MCP.
	ActionEnableDockerMCP struct{}
	// ActionDisableDockerMCP is a message to disable Docker MCP.
	ActionDisableDockerMCP struct{}
	// ActionMemoryStatus requests a one-line summary of the memory backend
	// status (the "Memory: Status" command).
	ActionMemoryStatus struct{}
	// ActionMemorySearch runs a read-only memory retrieval query (the
	// "Memory: Search" command).
	ActionMemorySearch struct {
		Query string
	}
	// ActionMemoryConsolidate triggers an on-demand consolidation +
	// materialization pass (the "Memory: Consolidate Now" command).
	ActionMemoryConsolidate struct{}
	// ActionMemoryClearConfirmed is sent after the user confirms clearing
	// all memory state via the MemoryClear dialog (the "Memory: Clear"
	// command).
	ActionMemoryClearConfirmed struct{}
)

// Messages for API key input dialog.
type (
	ActionChangeAPIKeyState struct {
		State APIKeyInputState
	}
)

// Messages for OAuth2 device flow dialog.
type (
	// ActionInitiateOAuth is sent when the device auth is initiated
	// successfully.
	ActionInitiateOAuth struct {
		DeviceCode      string
		UserCode        string
		ExpiresIn       int
		VerificationURL string
		Interval        int
	}

	// ActionCompleteOAuth is sent when the device flow completes successfully.
	ActionCompleteOAuth struct {
		Token *oauth.Token
	}

	// ActionOAuthErrored is sent when the device flow encounters an error.
	ActionOAuthErrored struct {
		Error error
	}
)

// ActionCmd represents an action that carries a [tea.Cmd] to be passed to the
// Bubble Tea program loop.
type ActionCmd struct {
	Cmd tea.Cmd
}

// ActionFilePickerSelected is a message indicating a file has been selected in
// the file picker dialog.
type ActionFilePickerSelected struct {
	Path string
}

// Cmd returns a command that reads the file at path and sends a
// [message.Attachement] to the program.
func (a ActionFilePickerSelected) Cmd() tea.Cmd {
	path := a.Path
	if path == "" {
		return nil
	}
	return func() tea.Msg {
		isFileLarge, err := common.IsFileTooBig(path, common.MaxAttachmentSize)
		if err != nil {
			return util.InfoMsg{
				Type: util.InfoTypeError,
				Msg:  fmt.Sprintf("unable to read the image: %v", err),
			}
		}
		if isFileLarge {
			return util.InfoMsg{
				Type: util.InfoTypeError,
				Msg:  "file too large, max 5MB",
			}
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return util.InfoMsg{
				Type: util.InfoTypeError,
				Msg:  fmt.Sprintf("unable to read the image: %v", err),
			}
		}

		mimeBufferSize := min(512, len(content))
		mimeType := http.DetectContentType(content[:mimeBufferSize])
		fileName := filepath.Base(path)

		return message.Attachment{
			FilePath: path,
			FileName: fileName,
			MimeType: mimeType,
			Content:  content,
		}
	}
}
