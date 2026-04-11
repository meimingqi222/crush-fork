package model

import (
	"strconv"
	"strings"
	"testing"

	"charm.land/bubbles/v2/textarea"
	"github.com/charmbracelet/crush/internal/ui/chat"
	"github.com/charmbracelet/crush/internal/ui/common"
)

// testMessageItem is a minimal chat item used to exercise layout behavior
// without depending on full message rendering.
type testMessageItem struct {
	id   string
	text string
}

func (m testMessageItem) ID() string           { return m.id }
func (m testMessageItem) Render(int) string    { return m.text }
func (m testMessageItem) RawRender(int) string { return m.text }

var _ chat.MessageItem = testMessageItem{}

func newTestUI() *UI {
	com := common.DefaultCommon(nil)

	ta := textarea.New()
	ta.SetStyles(com.Styles.TextArea)
	ta.ShowLineNumbers = false
	ta.CharLimit = -1
	ta.SetVirtualCursor(false)
	ta.DynamicHeight = true
	ta.MinHeight = TextareaMinHeight
	ta.MaxHeight = TextareaMaxHeight
	ta.Focus()

	return &UI{
		com:      com,
		status:   NewStatus(com, nil),
		chat:     NewChat(com),
		textarea: ta,
		state:    uiChat,
		focus:    uiFocusEditor,
		width:    140,
		height:   45,
	}
}

func TestUpdateLayoutAndSize_EditorGrowthShrinksChat(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	u.updateLayoutAndSize()

	initialEditorHeight := u.layout.editor.Dy()
	initialChatHeight := u.layout.main.Dy()

	prevHeight := u.textarea.Height()
	u.textarea.SetValue(strings.Repeat("line\n", 8))
	u.textarea.MoveToEnd()
	_ = u.handleTextareaHeightChange(prevHeight)

	if got := u.layout.editor.Dy(); got <= initialEditorHeight {
		t.Fatalf("expected editor to grow: got %d, want > %d", got, initialEditorHeight)
	}
	if got := u.layout.main.Dy(); got >= initialChatHeight {
		t.Fatalf("expected chat to shrink: got %d, want < %d", got, initialChatHeight)
	}
}

func TestHandleTextareaHeightChange_FollowModeStaysAtBottom(t *testing.T) {
	t.Parallel()

	u := newTestUI()

	msgs := make([]chat.MessageItem, 0, 60)
	for i := range 60 {
		msgs = append(msgs, testMessageItem{
			id:   "m-" + strconv.Itoa(i),
			text: "message " + strconv.Itoa(i),
		})
	}
	u.chat.SetMessages(msgs...)
	u.updateLayoutAndSize()

	u.chat.ScrollToBottom()
	if !u.chat.AtBottom() {
		t.Fatal("expected chat to start at bottom")
	}

	prevHeight := u.textarea.Height()
	u.textarea.SetValue(strings.Repeat("line\n", 10))
	u.textarea.MoveToEnd()
	_ = u.handleTextareaHeightChange(prevHeight)

	if !u.chat.Follow() {
		t.Fatal("expected follow mode to remain enabled")
	}
	if !u.chat.AtBottom() {
		t.Fatal("expected chat to remain at bottom after editor resize in follow mode")
	}
}
