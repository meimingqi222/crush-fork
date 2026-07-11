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

// MemorySearchID is the identifier for the memory search dialog.
const MemorySearchID = "memory_search"

// MemorySearch is a single-input dialog for the "Memory: Search" command
// (docs/refactor-memory.md Phase 4). It only collects the query text; the
// actual Retriever.Retrieve call and read-only result rendering happen in
// the caller after ActionMemorySearch is returned, since the retriever
// itself lives on the memory backend rather than in the dialog layer.
type MemorySearch struct {
	com    *common.Common
	query  textinput.Model
	help   help.Model
	keyMap struct {
		Confirm key.Binding
		Close   key.Binding
	}
}

// NewMemorySearch creates a new memory search dialog.
func NewMemorySearch(com *common.Common) *MemorySearch {
	query := textinput.New()
	query.SetVirtualCursor(false)
	query.Placeholder = "Search memory (free text)"
	query.SetStyles(com.Styles.TextInput)
	query.Focus()

	h := help.New()
	h.Styles = com.Styles.DialogHelpStyles()

	s := &MemorySearch{
		com:   com,
		query: query,
		help:  h,
	}
	s.keyMap.Confirm = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "search"))
	s.keyMap.Close = CloseKey
	return s
}

// ID implements [Dialog].
func (*MemorySearch) ID() string { return MemorySearchID }

// HandleMsg implements [Dialog].
func (s *MemorySearch) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, s.keyMap.Close):
			return ActionClose{}
		case key.Matches(msg, s.keyMap.Confirm):
			query := strings.TrimSpace(s.query.Value())
			if query == "" {
				return nil
			}
			return ActionMemorySearch{Query: query}
		default:
			var cmd tea.Cmd
			s.query, cmd = s.query.Update(msg)
			return ActionCmd{Cmd: cmd}
		}
	}
	return nil
}

// Cursor implements [Dialog].
func (s *MemorySearch) Cursor() *tea.Cursor {
	return InputCursor(s.com.Styles, s.query.Cursor())
}

// Draw implements [Dialog].
func (s *MemorySearch) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := s.com.Styles
	width := max(0, min(defaultDialogMaxWidth, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
	innerWidth := width - t.Dialog.View.GetHorizontalFrameSize()
	s.query.SetWidth(max(0, innerWidth-t.Dialog.InputPrompt.GetHorizontalFrameSize()-1))
	s.help.SetWidth(innerWidth)

	rc := NewRenderContext(t, width)
	rc.Title = "Memory: Search"
	rc.AddPart("Search materialized memory summaries and events. Results are read-only.")
	rc.AddPart(t.Dialog.InputPrompt.Render(s.query.View()))
	rc.Help = s.help.View(s)

	view := rc.Render()
	cur := s.Cursor()
	DrawCenterCursor(scr, area, view, cur)
	return cur
}

// ShortHelp implements [help.KeyMap].
func (s *MemorySearch) ShortHelp() []key.Binding {
	return []key.Binding{s.keyMap.Confirm, s.keyMap.Close}
}

// FullHelp implements [help.KeyMap].
func (s *MemorySearch) FullHelp() [][]key.Binding {
	return [][]key.Binding{s.ShortHelp()}
}
