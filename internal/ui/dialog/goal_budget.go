package dialog

import (
	"strconv"
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/ui/common"
	uv "github.com/charmbracelet/ultraviolet"
)

const GoalBudgetID = "goal_budget"

type GoalBudget struct {
	com       *common.Common
	sessionID string
	input     textinput.Model
	help      help.Model
	keyMap    struct {
		Confirm key.Binding
		Close   key.Binding
	}
}

func NewGoalBudget(com *common.Common, sessionID string) *GoalBudget {
	input := textinput.New()
	input.SetVirtualCursor(false)
	input.Placeholder = "Token budget, or off/0 for unlimited"
	input.SetStyles(com.Styles.TextInput)
	input.Focus()
	h := help.New()
	h.Styles = com.Styles.DialogHelpStyles()
	return &GoalBudget{
		com:       com,
		sessionID: sessionID,
		input:     input,
		help:      h,
		keyMap: struct {
			Confirm key.Binding
			Close   key.Binding
		}{
			Confirm: key.NewBinding(key.WithKeys("enter", "ctrl+y"), key.WithHelp("enter", "set")),
			Close:   CloseKey,
		},
	}
}

func (*GoalBudget) ID() string { return GoalBudgetID }

func (g *GoalBudget) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, g.keyMap.Close):
			return ActionClose{}
		case key.Matches(msg, g.keyMap.Confirm):
			value := strings.TrimSpace(strings.ToLower(g.input.Value()))
			var budget int64
			if value != "" && value != "off" {
				parsed, err := strconv.ParseInt(value, 10, 64)
				if err != nil || parsed < 0 {
					return nil
				}
				budget = parsed
			}
			return ActionSetGoalBudget{SessionID: g.sessionID, Budget: budget}
		default:
			var cmd tea.Cmd
			g.input, cmd = g.input.Update(msg)
			return ActionCmd{Cmd: cmd}
		}
	}
	return nil
}

func (g *GoalBudget) Cursor() *tea.Cursor {
	cur := InputCursor(g.com.Styles, g.input.Cursor())
	if cur != nil {
		// The description line sits between the title and the input field.
		cur.Y += 1
	}
	return cur
}

func (g *GoalBudget) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := g.com.Styles
	width := max(0, min(defaultDialogMaxWidth, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
	innerWidth := width - t.Dialog.View.GetHorizontalFrameSize()
	g.input.SetWidth(max(0, innerWidth-t.Dialog.InputPrompt.GetHorizontalFrameSize()-1))
	g.help.SetWidth(innerWidth)
	rc := NewRenderContext(t, width)
	rc.Title = "Set Goal Budget"
	rc.AddPart("Set a token budget for the current goal. Use off or 0 for unlimited.")
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

func (g *GoalBudget) ShortHelp() []key.Binding {
	return []key.Binding{g.keyMap.Confirm, g.keyMap.Close}
}

func (g *GoalBudget) FullHelp() [][]key.Binding { return [][]key.Binding{g.ShortHelp()} }
