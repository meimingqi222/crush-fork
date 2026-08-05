package dialog

import (
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/list"
	"github.com/charmbracelet/crush/internal/ui/styles"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/dustin/go-humanize"
	"github.com/sahilm/fuzzy"
)

// ForkID is the identifier for the fork session dialog.
const ForkID = "fork"

// Fork is a dialog that lets the user pick a user message to fork the session
// from. The selected message and everything before it is copied to a new
// session.
type Fork struct {
	com       *common.Common
	help      help.Model
	list      *list.FilterableList
	input     textinput.Model
	sessionID string

	keyMap struct {
		Select   key.Binding
		Next     key.Binding
		Previous key.Binding
		UpDown   key.Binding
		Close    key.Binding
	}
}

var _ Dialog = (*Fork)(nil)

// NewFork creates a new fork dialog for the given session and user messages.
func NewFork(com *common.Common, sessionID string, userMessages []message.Message) *Fork {
	f := &Fork{
		com:       com,
		sessionID: sessionID,
	}

	help := help.New()
	help.Styles = com.Styles.DialogHelpStyles()
	f.help = help

	f.list = list.NewFilterableList(forkItems(com.Styles, userMessages)...)
	f.list.Focus()
	f.list.SetSelected(len(userMessages) - 1)

	f.input = textinput.New()
	f.input.SetVirtualCursor(false)
	f.input.Placeholder = "Type to filter"
	f.input.SetStyles(com.Styles.TextInput)
	f.input.Focus()

	f.keyMap.Select = key.NewBinding(
		key.WithKeys("enter", "ctrl+y"),
		key.WithHelp("enter", "fork here"),
	)
	f.keyMap.Previous = key.NewBinding(
		key.WithKeys("up", "ctrl+p"),
		key.WithHelp("↑", "previous item"),
	)
	f.keyMap.Next = key.NewBinding(
		key.WithKeys("down", "ctrl+n"),
		key.WithHelp("↓", "next item"),
	)
	f.keyMap.UpDown = key.NewBinding(
		key.WithKeys("up", "down"),
		key.WithHelp("↑↓", "choose"),
	)
	f.keyMap.Close = CloseKey

	return f
}

// ID implements Dialog.
func (f *Fork) ID() string {
	return ForkID
}

// HandleMsg implements Dialog.
func (f *Fork) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, f.keyMap.Close):
			return ActionClose{}
		case key.Matches(msg, f.keyMap.Previous):
			f.list.Focus()
			if f.list.IsSelectedFirst() {
				f.list.SelectLast()
			} else {
				f.list.SelectPrev()
			}
			f.list.ScrollToSelected()
		case key.Matches(msg, f.keyMap.Next):
			f.list.Focus()
			if f.list.IsSelectedLast() {
				f.list.SelectFirst()
			} else {
				f.list.SelectNext()
			}
			f.list.ScrollToSelected()
		case key.Matches(msg, f.keyMap.Select):
			if item := f.list.SelectedItem(); item != nil {
				if forkItem, ok := item.(*ForkItem); ok {
					return ActionForkSession{
						SessionID: f.sessionID,
						MessageID: forkItem.Message.ID,
					}
				}
			}
		default:
			var cmd tea.Cmd
			f.input, cmd = f.input.Update(msg)
			value := f.input.Value()
			f.list.SetFilter(value)
			f.list.ScrollToTop()
			f.list.SetSelected(0)
			return ActionCmd{cmd}
		}
	case tea.PasteMsg:
		var cmd tea.Cmd
		f.input, cmd = f.input.Update(msg)
		value := f.input.Value()
		f.list.SetFilter(value)
		f.list.ScrollToTop()
		f.list.SetSelected(0)
		return ActionCmd{cmd}
	}
	return nil
}

// Cursor returns the cursor position relative to the dialog.
func (f *Fork) Cursor() *tea.Cursor {
	return InputCursor(f.com.Styles, f.input.Cursor())
}

// Draw implements Dialog.
func (f *Fork) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := f.com.Styles
	width := max(0, min(defaultDialogMaxWidth+20, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
	height := max(0, min(defaultDialogHeight, area.Dy()-t.Dialog.View.GetVerticalBorderSize()))
	innerWidth := width - t.Dialog.View.GetHorizontalFrameSize()
	heightOffset := t.Dialog.Title.GetVerticalFrameSize() + titleContentHeight +
		t.Dialog.InputPrompt.GetVerticalFrameSize() + inputContentHeight +
		t.Dialog.HelpView.GetVerticalFrameSize() +
		t.Dialog.View.GetVerticalFrameSize()

	f.input.SetWidth(max(0, innerWidth-t.Dialog.InputPrompt.GetHorizontalFrameSize()-1))
	f.list.SetSize(innerWidth, height-heightOffset)
	f.help.SetWidth(innerWidth)

	rc := NewRenderContext(t, width)
	rc.Title = "Fork Session"
	rc.TitleInfo = t.HalfMuted.Render("choose a user message")
	inputView := t.Dialog.InputPrompt.Render(f.input.View())
	rc.AddPart(inputView)
	listView := t.Dialog.List.Height(f.list.Height()).Render(f.list.Render())
	rc.AddPart(listView)
	rc.Help = f.help.View(f)

	view := rc.Render()
	cur := f.Cursor()
	DrawCenterCursor(scr, area, view, cur)
	return cur
}

// ShortHelp implements [help.KeyMap].
func (f *Fork) ShortHelp() []key.Binding {
	return []key.Binding{
		f.keyMap.UpDown,
		f.keyMap.Select,
		f.keyMap.Close,
	}
}

// FullHelp implements [help.KeyMap].
func (f *Fork) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{f.keyMap.Select, f.keyMap.Next, f.keyMap.Previous},
		{f.keyMap.Close},
	}
}

// ForkItem represents a user message that can be selected as a fork point.
type ForkItem struct {
	Message message.Message
	t       *styles.Styles
	focused bool
	m       fuzzy.Match
	cache   map[int]string
}

var _ list.FilterableItem = (*ForkItem)(nil)

// Filter returns the filterable text of the item.
func (f *ForkItem) Filter() string {
	return f.Message.Content().Text
}

// ID returns the message ID.
func (f *ForkItem) ID() string {
	return f.Message.ID
}

// SetFocused sets the focus state.
func (f *ForkItem) SetFocused(focused bool) {
	if f.focused != focused {
		f.cache = nil
	}
	f.focused = focused
}

// SetMatch sets the fuzzy match.
func (f *ForkItem) SetMatch(m fuzzy.Match) {
	f.cache = nil
	f.m = m
}

// Render renders the fork item.
func (f *ForkItem) Render(width int) string {
	if f.cache == nil {
		f.cache = make(map[int]string)
	}
	if cached, ok := f.cache[width]; ok {
		return cached
	}

	styles := ListItemStyles{
		ItemBlurred:     f.t.Dialog.NormalItem,
		ItemFocused:     f.t.Dialog.SelectedItem,
		InfoTextBlurred: f.t.Subtle,
		InfoTextFocused: f.t.Base,
	}

	title := strings.TrimSpace(f.Message.Content().Text)
	if title == "" {
		title = "(empty message)"
	}
	title = ansi.Strip(title)
	title = strings.Join(strings.Fields(title), " ")

	meta := humanize.Time(time.Unix(f.Message.CreatedAt, 0))

	rendered := renderItem(styles, title, meta, f.focused, width, f.cache, &f.m)
	f.cache[width] = rendered
	return rendered
}

func forkItems(t *styles.Styles, messages []message.Message) []list.FilterableItem {
	items := make([]list.FilterableItem, len(messages))
	for i, msg := range messages {
		items[i] = &ForkItem{
			Message: msg,
			t:       t,
		}
	}
	return items
}
