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

const GoalID = "goal"

type Goal struct {
	com       *common.Common
	sessionID string
	objective textinput.Model
	budget    textinput.Model
	focus     int
	help      help.Model
	keyMap    struct {
		Confirm key.Binding
		Tab     key.Binding
		Close   key.Binding
	}
}

func NewGoal(com *common.Common, sessionID string) *Goal {
	objective := textinput.New()
	objective.SetVirtualCursor(false)
	objective.Placeholder = "Describe the autonomous goal"
	objective.SetStyles(com.Styles.TextInput)
	objective.Focus()

	budget := textinput.New()
	budget.SetVirtualCursor(false)
	budget.Placeholder = "Optional token budget, or blank for unlimited"
	budget.SetStyles(com.Styles.TextInput)

	h := help.New()
	h.Styles = com.Styles.DialogHelpStyles()

	return &Goal{
		com:       com,
		sessionID: sessionID,
		objective: objective,
		budget:    budget,
		help:      h,
		keyMap: struct {
			Confirm key.Binding
			Tab     key.Binding
			Close   key.Binding
		}{
			Confirm: key.NewBinding(key.WithKeys("enter", "ctrl+y"), key.WithHelp("enter", "set")),
			Tab:     key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "next field")),
			Close:   CloseKey,
		},
	}
}

func (*Goal) ID() string { return GoalID }

func (g *Goal) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, g.keyMap.Close):
			return ActionClose{}
		case key.Matches(msg, g.keyMap.Tab):
			g.toggleFocus()
			return nil
		case key.Matches(msg, g.keyMap.Confirm):
			objective := strings.TrimSpace(g.objective.Value())
			if objective == "" {
				return nil
			}
			var budget int64
			budgetText := strings.TrimSpace(g.budget.Value())
			if budgetText != "" {
				parsed, err := strconv.ParseInt(budgetText, 10, 64)
				if err != nil || parsed < 0 {
					return nil
				}
				budget = parsed
			}
			return ActionSetGoal{SessionID: g.sessionID, Goal: objective, Budget: budget}
		default:
			var cmd tea.Cmd
			if g.focus == 0 {
				g.objective, cmd = g.objective.Update(msg)
			} else {
				g.budget, cmd = g.budget.Update(msg)
			}
			return ActionCmd{Cmd: cmd}
		}
	}
	return nil
}

func (g *Goal) toggleFocus() {
	if g.focus == 0 {
		g.focus = 1
		g.objective.Blur()
		g.budget.Focus()
	} else {
		g.focus = 0
		g.budget.Blur()
		g.objective.Focus()
	}
}

func (g *Goal) Cursor() *tea.Cursor {
	input := g.objective
	// The description line sits between the title and the first input field.
	lineOffset := titleContentHeight + 1
	if g.focus == 1 {
		input = g.budget
		// The objective input block is three lines tall (margin top, content,
		// margin bottom) before the budget input's top margin.
		lineOffset += 3
	}
	cur := InputCursor(g.com.Styles, input.Cursor())
	if cur != nil {
		cur.Y += lineOffset
	}
	return cur
}

func (g *Goal) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := g.com.Styles
	width := max(0, min(defaultDialogMaxWidth, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
	innerWidth := width - t.Dialog.View.GetHorizontalFrameSize()
	g.objective.SetWidth(max(0, innerWidth-t.Dialog.InputPrompt.GetHorizontalFrameSize()-1))
	g.budget.SetWidth(max(0, innerWidth-t.Dialog.InputPrompt.GetHorizontalFrameSize()-1))
	g.help.SetWidth(innerWidth)

	rc := NewRenderContext(t, width)
	rc.Title = "Set Goal"
	rc.AddPart("Set a persistent autonomous goal for this session. Leave budget blank for no token limit.")
	rc.AddPart(t.Dialog.InputPrompt.Render(g.objective.View()))
	rc.AddPart(t.Dialog.InputPrompt.Render(g.budget.View()))
	rc.Help = g.help.View(g)
	// DrawCenterCursor translates the cursor into screen coordinates in
	// place, so the same value must be returned. Calling Cursor() twice
	// yields a second, dialog-relative cursor that the UI would then treat
	// as absolute -- parking the terminal caret up in the chat area.
	cur := g.Cursor()
	DrawCenterCursor(scr, area, rc.Render(), cur)
	return cur
}

func (g *Goal) ShortHelp() []key.Binding {
	return []key.Binding{g.keyMap.Confirm, g.keyMap.Tab, g.keyMap.Close}
}

func (g *Goal) FullHelp() [][]key.Binding { return [][]key.Binding{g.ShortHelp()} }
