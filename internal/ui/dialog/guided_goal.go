package dialog

import (
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/ui/common"
	uv "github.com/charmbracelet/ultraviolet"
)

const GuidedGoalID = "guided_goal"

type GuidedGoal struct {
	com       *common.Common
	sessionID string
	input     textinput.Model
	help      help.Model
	keyMap    struct {
		Confirm key.Binding
		Close   key.Binding
	}
}

func NewGuidedGoal(com *common.Common, sessionID string) *GuidedGoal {
	input := textinput.New()
	input.SetVirtualCursor(false)
	input.Placeholder = "Describe the rough goal you want help refining"
	input.SetStyles(com.Styles.TextInput)
	input.Focus()
	h := help.New()
	h.Styles = com.Styles.DialogHelpStyles()
	return &GuidedGoal{
		com:       com,
		sessionID: sessionID,
		input:     input,
		help:      h,
		keyMap: struct {
			Confirm key.Binding
			Close   key.Binding
		}{
			Confirm: key.NewBinding(key.WithKeys("enter", "ctrl+y"), key.WithHelp("enter", "start")),
			Close:   CloseKey,
		},
	}
}

func (*GuidedGoal) ID() string { return GuidedGoalID }

func (g *GuidedGoal) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, g.keyMap.Close):
			return ActionClose{}
		case key.Matches(msg, g.keyMap.Confirm):
			rough := strings.TrimSpace(g.input.Value())
			if rough == "" {
				return nil
			}
			return ActionStartGuidedGoal{SessionID: g.sessionID, RoughGoal: rough}
		default:
			var cmd tea.Cmd
			g.input, cmd = g.input.Update(msg)
			return ActionCmd{Cmd: cmd}
		}
	case tea.PasteMsg:
		var cmd tea.Cmd
		g.input, cmd = g.input.Update(msg)
		return ActionCmd{Cmd: cmd}
	}
	return nil
}

func (g *GuidedGoal) Cursor() *tea.Cursor {
	cur := InputCursor(g.com.Styles, realTextInputCursor(g.input))
	if cur != nil {
		// The description line sits between the title and the input field.
		cur.Y += titleContentHeight + 1
	}
	return cur
}

func (g *GuidedGoal) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := g.com.Styles
	width := max(0, min(defaultDialogMaxWidth, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
	innerWidth := width - t.Dialog.View.GetHorizontalFrameSize()
	g.input.SetWidth(max(0, innerWidth-t.Dialog.InputPrompt.GetHorizontalFrameSize()-1))
	g.help.SetWidth(innerWidth)
	rc := NewRenderContext(t, width)
	rc.Title = "Guided Goal"
	rc.AddPart("Start a short agent-led conversation to turn a rough intent into a bounded, verifiable goal.")
	rc.AddPart(t.Dialog.InputPrompt.Render(g.input.View()))
	rc.Help = g.help.View(g)
	// DrawCenterCursor translates the cursor into screen coordinates in
	// place, so the same value must be returned. Calling Cursor() twice
	// yields a second, dialog-relative cursor that the UI would then treat
	// as absolute -- parking the terminal caret up in the chat area.
	cur := g.Cursor()
	DrawCenterCursor(scr, area, rc.Render(), cur)
	return cur
}

func (g *GuidedGoal) ShortHelp() []key.Binding {
	return []key.Binding{g.keyMap.Confirm, g.keyMap.Close}
}

func (g *GuidedGoal) FullHelp() [][]key.Binding { return [][]key.Binding{g.ShortHelp()} }
