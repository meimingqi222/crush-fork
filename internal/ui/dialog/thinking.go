package dialog

import (
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/common"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

const (
	// ThinkingID is the identifier for the thinking/reasoning dialog.
	ThinkingID = "thinking"

	// thinkingDialogMaxWidth is the maximum width of the thinking dialog.
	thinkingDialogMaxWidth = 80
	// thinkingDialogMaxHeight is the maximum height of the thinking dialog.
	thinkingDialogMaxHeight = 24
)

// Thinking represents a dialog that displays the reasoning/thinking content of
// an assistant message.
type Thinking struct {
	com      *common.Common
	msg      *message.Message
	help     help.Model
	viewport viewport.Model

	keyMap struct {
		Close,
		ScrollUp,
		ScrollDown,
		PageUp,
		PageDown key.Binding
	}

	windowWidth  int
	windowHeight int
	content      string
}

var _ Dialog = (*Thinking)(nil)

// NewThinking creates a new thinking dialog for the given message.
func NewThinking(com *common.Common, msg *message.Message) *Thinking {
	t := &Thinking{
		com: com,
		msg: msg,
	}

	t.help = help.New()
	t.help.Styles = com.Styles.DialogHelpStyles()

	t.keyMap.Close = key.NewBinding(
		key.WithKeys("esc", "alt+esc", "q"),
		key.WithHelp("esc/q", "close"),
	)
	t.keyMap.ScrollUp = key.NewBinding(
		key.WithKeys("up", "k"),
		key.WithHelp("↑/k", "scroll up"),
	)
	t.keyMap.ScrollDown = key.NewBinding(
		key.WithKeys("down", "j"),
		key.WithHelp("↓/j", "scroll down"),
	)
	t.keyMap.PageUp = key.NewBinding(
		key.WithKeys("pgup", "b"),
		key.WithHelp("pgup", "page up"),
	)
	t.keyMap.PageDown = key.NewBinding(
		key.WithKeys("pgdown", "space", "f"),
		key.WithHelp("pgdn", "page down"),
	)

	vp := viewport.New()
	vp.KeyMap = viewport.KeyMap{
		Up:           t.keyMap.ScrollUp,
		Down:         t.keyMap.ScrollDown,
		Left:         key.NewBinding(key.WithDisabled()),
		Right:        key.NewBinding(key.WithDisabled()),
		PageUp:       t.keyMap.PageUp,
		PageDown:     t.keyMap.PageDown,
		HalfPageUp:   key.NewBinding(key.WithDisabled()),
		HalfPageDown: key.NewBinding(key.WithDisabled()),
	}
	t.viewport = vp

	return t
}

// ID implements Dialog.
func (t *Thinking) ID() string {
	return ThinkingID
}

// HandleMsg implements Dialog.
func (t *Thinking) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, t.keyMap.Close):
			return ActionClose{}
		case key.Matches(msg, t.keyMap.ScrollUp, t.keyMap.ScrollDown, t.keyMap.PageUp, t.keyMap.PageDown):
			var cmd tea.Cmd
			t.viewport, cmd = t.viewport.Update(msg)
			return ActionCmd{cmd}
		}
	}
	return nil
}

// Cursor returns the cursor position relative to the dialog.
func (t *Thinking) Cursor() *tea.Cursor {
	return nil
}

// Draw implements Dialog.
func (t *Thinking) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	sty := t.com.Styles

	width := max(0, min(thinkingDialogMaxWidth, area.Dx()-sty.Dialog.View.GetHorizontalBorderSize()))
	height := max(0, min(thinkingDialogMaxHeight, area.Dy()-sty.Dialog.View.GetVerticalBorderSize()))

	if area.Dx() != t.windowWidth || area.Dy() != t.windowHeight {
		t.windowWidth = area.Dx()
		t.windowHeight = area.Dy()

		innerWidth := width - sty.Dialog.View.GetHorizontalFrameSize()
		contentHeight := height - sty.Dialog.Title.GetVerticalFrameSize() -
			titleContentHeight - sty.Dialog.HelpView.GetVerticalFrameSize() -
			sty.Dialog.View.GetVerticalFrameSize()
		t.viewport.SetWidth(max(0, innerWidth))
		t.viewport.SetHeight(max(0, contentHeight))
	}

	t.help.SetWidth(width - sty.Dialog.View.GetHorizontalFrameSize())

	if t.content == "" {
		t.content = t.buildContent(width - sty.Dialog.View.GetHorizontalFrameSize())
		t.viewport.SetContent(t.content)
		t.viewport.GotoBottom()
	}

	rc := NewRenderContext(sty, width)
	rc.Title = "Reasoning"
	rc.TitleInfo = t.titleInfo()

	contentView := sty.Dialog.List.Height(t.viewport.Height()).Render(t.viewport.View())
	rc.AddPart(contentView)
	rc.Help = t.help.View(t)

	view := rc.Render()
	cur := t.Cursor()
	DrawCenterCursor(scr, area, view, cur)
	return cur
}

// titleInfo returns a small subtitle showing the message thinking duration.
func (t *Thinking) titleInfo() string {
	duration := t.msg.ThinkingDuration()
	if duration.String() == "0s" {
		return ""
	}
	return t.com.Styles.Dialog.SecondaryText.Render("(" + duration.String() + ")")
}

// buildContent renders the raw thinking text into markdown for display.
func (t *Thinking) buildContent(innerWidth int) string {
	rawThinking := t.msg.ReasoningContent().Thinking
	thinking, _ := message.StripTextualToolCallProtocol(rawThinking)
	thinking = strings.TrimSpace(thinking)
	if thinking == "" {
		return t.com.Styles.Dialog.SecondaryText.Render("No reasoning content available.")
	}

	renderer := common.PlainMarkdownRenderer(t.com.Styles, innerWidth)
	rendered, err := renderer.Render(thinking)
	if err != nil {
		return thinking
	}
	return ansi.Truncate(strings.TrimSpace(rendered), innerWidth, "")
}

// ShortHelp implements help.KeyMap.
func (t *Thinking) ShortHelp() []key.Binding {
	return []key.Binding{
		t.keyMap.ScrollUp,
		t.keyMap.ScrollDown,
		t.keyMap.Close,
	}
}

// FullHelp implements help.KeyMap.
func (t *Thinking) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{t.keyMap.ScrollUp, t.keyMap.ScrollDown},
		{t.keyMap.PageUp, t.keyMap.PageDown},
		{t.keyMap.Close},
	}
}
