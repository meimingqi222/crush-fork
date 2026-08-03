package dialog

import (
	"cmp"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	agentmcp "github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/crush/internal/ui/util"
	uv "github.com/charmbracelet/ultraviolet"
)

// MCPDetailID is the identifier for the MCP detail dialog.
const MCPDetailID = "mcp-detail"

// MCPDetail shows detailed information about a single MCP server.
type MCPDetail struct {
	com    *common.Common
	help   help.Model
	name   string
	state  agentmcp.ClientInfo
	config config.MCPConfig

	viewport      viewport.Model
	viewportDirty bool
	viewportWidth int

	keyMap struct {
		Back         key.Binding
		Authenticate key.Binding
		Reconnect    key.Binding
		Toggle       key.Binding
		ScrollUp     key.Binding
		ScrollDown   key.Binding
		PageUp       key.Binding
		PageDown     key.Binding
	}
}

var _ Dialog = (*MCPDetail)(nil)

// NewMCPDetail creates a new MCP detail dialog.
func NewMCPDetail(com *common.Common, name string, state agentmcp.ClientInfo, cfg config.MCPConfig) *MCPDetail {
	d := &MCPDetail{
		com:    com,
		name:   name,
		state:  state,
		config: cfg,
	}

	helpView := help.New()
	helpView.Styles = com.Styles.DialogHelpStyles()
	d.help = helpView

	vp := viewport.New()
	vp.KeyMap = viewport.KeyMap{
		Up:   key.NewBinding(key.WithKeys("up", "k")),
		Down: key.NewBinding(key.WithKeys("down", "j")),
		PageUp: key.NewBinding(
			key.WithKeys("shift+up", "pgup"),
		),
		PageDown: key.NewBinding(
			key.WithKeys("shift+down", "pgdown"),
		),
		HalfPageUp:   key.NewBinding(key.WithKeys()),
		HalfPageDown: key.NewBinding(key.WithKeys()),
	}
	d.viewport = vp

	d.keyMap.Back = key.NewBinding(
		key.WithKeys("esc", "q"),
		key.WithHelp("esc", "back"),
	)
	d.keyMap.Authenticate = key.NewBinding(
		key.WithKeys("a"),
		key.WithHelp("a", "authenticate"),
	)
	d.keyMap.Reconnect = key.NewBinding(
		key.WithKeys("ctrl+r"),
		key.WithHelp("ctrl+r", "reconnect"),
	)
	d.keyMap.Toggle = key.NewBinding(
		key.WithKeys("ctrl+d"),
		key.WithHelp("ctrl+d", "toggle enable/disable"),
	)
	d.keyMap.ScrollUp = key.NewBinding(
		key.WithKeys("up", "k"),
		key.WithHelp("↑/k", "scroll up"),
	)
	d.keyMap.ScrollDown = key.NewBinding(
		key.WithKeys("down", "j"),
		key.WithHelp("↓/j", "scroll down"),
	)
	d.keyMap.PageUp = key.NewBinding(
		key.WithKeys("shift+up", "pgup"),
		key.WithHelp("shift+↑/pgup", "page up"),
	)
	d.keyMap.PageDown = key.NewBinding(
		key.WithKeys("shift+down", "pgdown"),
		key.WithHelp("shift+↓/pgdown", "page down"),
	)

	return d
}

// ID implements Dialog.
func (d *MCPDetail) ID() string {
	return MCPDetailID
}

// HandleMsg implements Dialog.
func (d *MCPDetail) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, d.keyMap.Back):
			return ActionClose{}
		case key.Matches(msg, d.keyMap.Authenticate):
			if !d.config.SupportsInteractiveAuth() || d.config.Disabled {
				return ActionCmd{Cmd: util.ReportWarn("This MCP server cannot authenticate interactively.")}
			}
			return ActionAuthenticateMCP{Name: d.name}
		case key.Matches(msg, d.keyMap.Reconnect):
			if d.config.Disabled {
				return ActionCmd{Cmd: util.ReportWarn("Cannot reconnect a disabled MCP server.")}
			}
			return ActionReconnectMCP{Name: d.name}
		case key.Matches(msg, d.keyMap.Toggle):
			return ActionToggleMCP{Name: d.name, Enable: d.config.Disabled}
		case key.Matches(msg, d.keyMap.ScrollUp, d.keyMap.ScrollDown, d.keyMap.PageUp, d.keyMap.PageDown):
			d.viewport, _ = d.viewport.Update(msg)
		}
	}
	return nil
}

// Cursor returns the cursor position relative to the dialog.
func (d *MCPDetail) Cursor() *tea.Cursor {
	return nil
}

// Draw implements Dialog.
func (d *MCPDetail) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := d.com.Styles
	width := max(0, min(defaultDialogMaxWidth+20, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
	// Cap the dialog height to 75% of the screen so it never dominates the terminal.
	maxTotalHeight := min(area.Dy(), (area.Dy()*3)/4)
	maxInnerHeight := maxTotalHeight - t.Dialog.View.GetVerticalBorderSize()
	height := max(0, min(defaultDialogHeight+4, maxInnerHeight))
	innerWidth := width - t.Dialog.View.GetHorizontalFrameSize()
	heightOffset := t.Dialog.Title.GetVerticalFrameSize() + titleContentHeight +
		t.Dialog.HelpView.GetVerticalFrameSize() +
		t.Dialog.View.GetVerticalFrameSize()
	contentHeight := max(0, height-heightOffset)

	d.help.SetWidth(innerWidth)

	renderedContent := d.buildContent(t, innerWidth)
	contentLines := strings.Count(renderedContent, "\n") + 1
	needsScrollbar := contentLines > contentHeight
	viewportWidth := innerWidth
	if needsScrollbar {
		viewportWidth = max(0, innerWidth-1) // Reserve space for scrollbar.
	}

	if d.viewport.Width() != viewportWidth {
		d.viewportDirty = true
		renderedContent = d.buildContent(t, viewportWidth)
	}

	d.viewport.SetWidth(viewportWidth)
	d.viewport.SetHeight(contentHeight)
	if d.viewportDirty {
		d.viewport.SetContent(renderedContent)
		d.viewportWidth = d.viewport.Width()
		d.viewportDirty = false
	}

	contentView := d.viewport.View()
	if needsScrollbar {
		scrollbar := common.Scrollbar(t, contentHeight, d.viewport.TotalLineCount(), contentHeight, d.viewport.YOffset())
		if scrollbar != "" {
			contentView = lipgloss.JoinHorizontal(lipgloss.Top, contentView, scrollbar)
		}
	}

	rc := NewRenderContext(t, width)
	rc.Title = "MCP: " + d.name
	rc.AddPart(contentView)
	rc.Help = d.help.View(d)

	view := rc.Render()
	cur := d.Cursor()
	DrawCenterCursor(scr, area, view, cur)
	return cur
}

// ShortHelp implements help.KeyMap.
func (d *MCPDetail) ShortHelp() []key.Binding {
	return []key.Binding{
		d.keyMap.Back,
		d.keyMap.Authenticate,
		d.keyMap.Reconnect,
		d.keyMap.Toggle,
		d.keyMap.ScrollDown,
	}
}

// FullHelp implements help.KeyMap.
func (d *MCPDetail) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{d.keyMap.Back, d.keyMap.Authenticate, d.keyMap.Reconnect, d.keyMap.Toggle},
		{d.keyMap.ScrollUp, d.keyMap.ScrollDown, d.keyMap.PageUp, d.keyMap.PageDown},
	}
}

func (d *MCPDetail) buildContent(t *styles.Styles, width int) string {
	var sections []string

	// Status section
	statusIcon := d.statusIcon(t)
	statusText := d.statusText()
	statusLine := lipgloss.JoinHorizontal(lipgloss.Left, statusIcon, " ", statusText)
	sections = append(sections, t.Base.Render("Status: "+statusLine))

	// Type section
	sections = append(sections, t.Base.Render(fmt.Sprintf("Type: %s", d.config.Type)))

	// Command/URL section
	if d.config.Command != "" {
		sections = append(sections, t.Base.Render(fmt.Sprintf("Command: %s", d.config.Command)))
		if len(d.config.Args) > 0 {
			sections = append(sections, t.Base.Render(fmt.Sprintf("Args: %s", strings.Join(d.config.Args, " "))))
		}
	}
	if d.config.URL != "" {
		sections = append(sections, t.Base.Render(fmt.Sprintf("URL: %s", d.config.URL)))
	}

	// OAuth section
	if d.config.OAuth != nil {
		oauthStatus := "Enabled"
		if d.config.OAuth.ClientID == "" {
			oauthStatus = "Configured (no client ID)"
		}
		sections = append(sections, t.Base.Render(fmt.Sprintf("OAuth: %s", oauthStatus)))
	}

	// Tools section
	var tools []*agentmcp.Tool
	for name, ts := range agentmcp.Tools() {
		if name == d.name {
			tools = ts
			break
		}
	}
	if len(tools) > 0 {
		sections = append(sections, "")
		sections = append(sections, t.Dialog.TitleText.Render(fmt.Sprintf("Tools (%d)", len(tools))))
		for _, tool := range tools {
			toolLine := fmt.Sprintf("  • %s", tool.Name)
			if tool.Description != "" {
				desc := tool.Description
				if len(desc) > 60 {
					desc = desc[:57] + "..."
				}
				toolLine += t.Subtle.Render(" - " + desc)
			}
			sections = append(sections, t.Base.Render(toolLine))
		}
	}

	// Resources section
	var resources []*agentmcp.Resource
	for name, rs := range agentmcp.Resources() {
		if name == d.name {
			resources = rs
			break
		}
	}
	if len(resources) > 0 {
		sections = append(sections, "")
		sections = append(sections, t.Dialog.TitleText.Render(fmt.Sprintf("Resources (%d)", len(resources))))
		for _, res := range resources {
			resName := cmp.Or(res.Title, res.Name, res.URI)
			sections = append(sections, t.Base.Render(fmt.Sprintf("  • %s", resName)))
		}
	}

	// Prompts section
	var prompts []*agentmcp.Prompt
	for name, ps := range agentmcp.Prompts() {
		if name == d.name {
			prompts = ps
			break
		}
	}
	if len(prompts) > 0 {
		sections = append(sections, "")
		sections = append(sections, t.Dialog.TitleText.Render(fmt.Sprintf("Prompts (%d)", len(prompts))))
		for _, prompt := range prompts {
			promptLine := fmt.Sprintf("  • %s", prompt.Name)
			if prompt.Description != "" {
				promptLine += t.Subtle.Render(" - " + prompt.Description)
			}
			sections = append(sections, t.Base.Render(promptLine))
		}
	}

	// Error section
	if d.state.Error != nil && d.state.State != agentmcp.StateNeedsAuth {
		sections = append(sections, "")
		sections = append(sections, t.Dialog.TitleError.Render("Error"))
		sections = append(sections, t.Base.Render(d.state.Error.Error()))
	}

	return lipgloss.JoinVertical(lipgloss.Left, sections...)
}

func (d *MCPDetail) statusIcon(t *styles.Styles) string {
	switch d.state.State {
	case agentmcp.StateStarting:
		return t.ResourceBusyIcon.String()
	case agentmcp.StateConnected:
		return t.ResourceOnlineIcon.String()
	case agentmcp.StateNeedsAuth:
		return t.ResourceBusyIcon.String()
	case agentmcp.StateError:
		return t.ResourceErrorIcon.String()
	case agentmcp.StateCached:
		return t.ResourceOfflineIcon.String()
	case agentmcp.StateCircuitOpen:
		return t.ResourceErrorIcon.String()
	case agentmcp.StateDisabled:
		return t.ResourceOfflineIcon.String()
	default:
		return t.ResourceOfflineIcon.String()
	}
}

func (d *MCPDetail) statusText() string {
	if d.config.Disabled {
		return "Disabled"
	}
	switch d.state.State {
	case agentmcp.StateStarting:
		return "Starting"
	case agentmcp.StateConnected:
		return "Connected"
	case agentmcp.StateNeedsAuth:
		if d.config.SupportsInteractiveAuth() {
			return "Needs authentication. Press a to authenticate."
		}
		return "Needs authentication"
	case agentmcp.StateCached:
		return "Cached (offline)"
	case agentmcp.StateCircuitOpen:
		return "Circuit open. Press ctrl+r to reconnect."
	case agentmcp.StateError:
		if d.state.Error != nil {
			return fmt.Sprintf("Error: %s", d.state.Error.Error())
		}
		return "Error"
	default:
		return "Unknown"
	}
}

// Name returns the MCP server name shown by this detail dialog.
func (d *MCPDetail) Name() string {
	return d.name
}

// SetState updates the runtime state shown in the detail dialog.
func (d *MCPDetail) SetState(state agentmcp.ClientInfo) {
	d.state = state
	d.viewportDirty = true
}

// SetConfig updates the config shown in the detail dialog (e.g. after toggling disabled).
func (d *MCPDetail) SetConfig(cfg config.MCPConfig) {
	d.config = cfg
	d.viewportDirty = true
}
