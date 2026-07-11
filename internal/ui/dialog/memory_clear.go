package dialog

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/ui/common"
	uv "github.com/charmbracelet/ultraviolet"
)

// MemoryClearID is the identifier for the memory clear confirmation dialog.
const MemoryClearID = "memory_clear"

// MemoryClear is a yes/no confirmation dialog for the "Memory: Clear"
// command (docs/refactor-memory.md Phase 4). It never clears memory itself
// -- confirming returns ActionMemoryClearConfirmed, and the caller decides
// how to invoke Backend.Clear.
type MemoryClear struct {
	com        *common.Common
	selectedNo bool
	keyMap     struct {
		LeftRight,
		EnterSpace,
		Yes,
		No,
		Tab,
		Close key.Binding
	}
}

var _ Dialog = (*MemoryClear)(nil)

// NewMemoryClear creates a new memory clear confirmation dialog.
func NewMemoryClear(com *common.Common) *MemoryClear {
	c := &MemoryClear{
		com:        com,
		selectedNo: true,
	}
	c.keyMap.LeftRight = key.NewBinding(
		key.WithKeys("left", "right"),
		key.WithHelp("←/→", "switch options"),
	)
	c.keyMap.EnterSpace = key.NewBinding(
		key.WithKeys("enter", " "),
		key.WithHelp("enter/space", "confirm"),
	)
	c.keyMap.Yes = key.NewBinding(
		key.WithKeys("y", "Y"),
		key.WithHelp("y/Y", "yes"),
	)
	c.keyMap.No = key.NewBinding(
		key.WithKeys("n", "N"),
		key.WithHelp("n/N", "no"),
	)
	c.keyMap.Tab = key.NewBinding(
		key.WithKeys("tab"),
		key.WithHelp("tab", "switch options"),
	)
	c.keyMap.Close = CloseKey
	return c
}

// ID implements [Dialog].
func (*MemoryClear) ID() string { return MemoryClearID }

// HandleMsg implements [Dialog].
func (c *MemoryClear) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, c.keyMap.Close, c.keyMap.No):
			return ActionClose{}
		case key.Matches(msg, c.keyMap.LeftRight, c.keyMap.Tab):
			c.selectedNo = !c.selectedNo
		case key.Matches(msg, c.keyMap.EnterSpace):
			if !c.selectedNo {
				return ActionMemoryClearConfirmed{}
			}
			return ActionClose{}
		case key.Matches(msg, c.keyMap.Yes):
			return ActionMemoryClearConfirmed{}
		}
	}
	return nil
}

// Draw implements [Dialog].
func (c *MemoryClear) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	const question = "Clear all memory state? This deletes events, materialized\nviews, and knowledge-graph data. This cannot be undone."
	baseStyle := c.com.Styles.Base
	buttonOpts := []common.ButtonOpts{
		{Text: "Yes, clear it", Selected: !c.selectedNo, Padding: 3},
		{Text: "Cancel", Selected: c.selectedNo, Padding: 3},
	}
	buttons := common.ButtonGroup(c.com.Styles, buttonOpts, " ")
	content := baseStyle.Render(
		lipgloss.JoinVertical(
			lipgloss.Center,
			question,
			"",
			buttons,
		),
	)

	view := c.com.Styles.BorderFocus.Render(content)
	DrawCenter(scr, area, view)
	return nil
}

// ShortHelp implements [help.KeyMap].
func (c *MemoryClear) ShortHelp() []key.Binding {
	return []key.Binding{
		c.keyMap.LeftRight,
		c.keyMap.EnterSpace,
	}
}

// FullHelp implements [help.KeyMap].
func (c *MemoryClear) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{c.keyMap.LeftRight, c.keyMap.EnterSpace, c.keyMap.Yes, c.keyMap.No},
		{c.keyMap.Tab, c.keyMap.Close},
	}
}
