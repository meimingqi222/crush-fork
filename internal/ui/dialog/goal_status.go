package dialog

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/ui/common"
	uv "github.com/charmbracelet/ultraviolet"
)

const GoalStatusID = "goal_status"

type GoalStatus struct {
	com    *common.Common
	goal   session.Goal
	help   help.Model
	keyMap struct{ Close key.Binding }
}

func NewGoalStatus(com *common.Common, goal session.Goal) *GoalStatus {
	h := help.New()
	h.Styles = com.Styles.DialogHelpStyles()
	return &GoalStatus{
		com:  com,
		goal: goal,
		help: h,
		keyMap: struct{ Close key.Binding }{
			Close: CloseKey,
		},
	}
}

func (*GoalStatus) ID() string { return GoalStatusID }

func (g *GoalStatus) HandleMsg(msg tea.Msg) Action {
	if msg, ok := msg.(tea.KeyPressMsg); ok && key.Matches(msg, g.keyMap.Close) {
		return ActionClose{}
	}
	return nil
}

func (g *GoalStatus) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := g.com.Styles
	width := max(0, min(defaultDialogMaxWidth, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
	innerWidth := width - t.Dialog.View.GetHorizontalFrameSize()
	g.help.SetWidth(innerWidth)

	rc := NewRenderContext(t, width)
	rc.Title = "Goal Status"
	if g.goal.Status == "" {
		rc.AddPart("No goal is currently set.")
	} else {
		rc.AddPart(fmt.Sprintf("Status: %s", g.goal.Status))
		rc.AddPart(fmt.Sprintf("Objective:\n%s", strings.TrimSpace(g.goal.Text)))
		if g.goal.HasBudget() {
			rc.AddPart(fmt.Sprintf("Budget: %d / %d tokens (%d remaining)", g.goal.TokensUsed, g.goal.TokenBudget, g.goal.RemainingTokens()))
		} else if g.goal.TokensUsed > 0 {
			rc.AddPart(fmt.Sprintf("Tokens used: %d", g.goal.TokensUsed))
		}
		if g.goal.TimeSeconds > 0 {
			rc.AddPart(fmt.Sprintf("Elapsed: %s", (time.Duration(g.goal.TimeSeconds) * time.Second).String()))
		}
	}
	rc.Help = g.help.View(g)
	DrawCenterCursor(scr, area, rc.Render(), nil)
	return nil
}

func (g *GoalStatus) ShortHelp() []key.Binding { return []key.Binding{g.keyMap.Close} }

func (g *GoalStatus) FullHelp() [][]key.Binding { return [][]key.Binding{g.ShortHelp()} }
