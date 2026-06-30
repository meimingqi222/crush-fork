package chat

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/anim"
	"github.com/charmbracelet/crush/internal/ui/attachments"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/x/ansi"
)

// userMessageSpinnerFrames are the Braille characters used for a lightweight
// spinner shown while an image attachment is being processed.
var userMessageSpinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// UserMessageItem represents a user message in the chat UI.
type UserMessageItem struct {
	*highlightableMessageItem
	*cachedMessageItem
	*focusableMessageItem

	attachments      *attachments.Renderer
	message          *message.Message
	sty              *styles.Styles
	showLoadingState bool
	spinnerFrame     int
}

// NewUserMessageItem creates a new UserMessageItem.
func NewUserMessageItem(sty *styles.Styles, message *message.Message, attachments *attachments.Renderer) MessageItem {
	return &UserMessageItem{
		highlightableMessageItem: defaultHighlighter(sty),
		cachedMessageItem:        &cachedMessageItem{},
		focusableMessageItem:     &focusableMessageItem{},
		attachments:              attachments,
		message:                  message,
		sty:                      sty,
	}
}

// RawRender implements [MessageItem].
func (m *UserMessageItem) RawRender(width int) string {
	cappedWidth := cappedMessageWidth(width)

	content, height, ok := m.getCachedRender(cappedWidth)
	// cache hit
	if ok {
		return m.renderHighlighted(content, cappedWidth, height)
	}

	renderer := common.MarkdownRenderer(m.sty, cappedWidth)

	msgContent := strings.TrimSpace(m.message.Content().Text)
	result, err := renderer.Render(msgContent)
	if err != nil {
		content = msgContent
	} else {
		content = strings.TrimSuffix(result, "\n")
	}

	if len(m.message.BinaryContent()) > 0 {
		attachmentsStr := m.renderAttachments(cappedWidth)
		if content == "" {
			content = attachmentsStr
		} else {
			content = strings.Join([]string{content, "", attachmentsStr}, "\n")
		}
	}

	if m.showLoadingState {
		frame := userMessageSpinnerFrames[m.spinnerFrame%len(userMessageSpinnerFrames)]
		spinner := m.sty.Base.Faint(true).Render(frame + " analyzing image...")
		if content == "" {
			content = spinner
		} else {
			content = strings.Join([]string{content, spinner}, "\n")
		}
	}

	height = lipgloss.Height(content)
	m.setCachedRender(content, cappedWidth, height)
	return m.renderHighlighted(content, cappedWidth, height)
}

// Render implements MessageItem.
func (m *UserMessageItem) Render(width int) string {
	var prefix string
	if m.focused {
		prefix = m.sty.Chat.Message.UserFocused.Render()
	} else {
		prefix = m.sty.Chat.Message.UserBlurred.Render()
	}
	return applyLinePrefix(m.RawRender(width), prefix)
}

// ID implements MessageItem.
func (m *UserMessageItem) ID() string {
	return m.message.ID
}

// renderAttachments renders attachments.
func (m *UserMessageItem) renderAttachments(width int) string {
	var attachments []message.Attachment
	for _, at := range m.message.BinaryContent() {
		attachments = append(attachments, message.Attachment{
			FileName: at.Path,
			MimeType: at.MIMEType,
		})
	}
	return m.attachments.Render(attachments, false, width)
}

// HandleKeyEvent implements KeyEventHandler.
func (m *UserMessageItem) HandleKeyEvent(key tea.KeyMsg) (bool, tea.Cmd) {
	k := key.String()
	if k == "c" || k == "y" {
		text := m.message.Content().Text
		return true, common.CopyToClipboard(text, "Message copied to clipboard")
	}
	if k == "enter" && m.hasImageAttachment() {
		return true, m.openImageAttachment()
	}
	return false, nil
}

// HandleMouseClick implements list.MouseClickable.
func (m *UserMessageItem) HandleMouseClick(btn ansi.MouseButton, x, y int) (bool, tea.Cmd) {
	if btn != ansi.MouseButton1 {
		return false, nil
	}
	if !m.hasImageAttachment() {
		return false, nil
	}
	// Any click inside the message item opens the first image attachment.
	// This keeps the interaction simple while still being useful.
	return true, m.openImageAttachment()
}

func (m *UserMessageItem) openImageAttachment() tea.Cmd {
	for _, bc := range m.message.BinaryContent() {
		if strings.HasPrefix(bc.MIMEType, "image/") {
			filename := bc.Path
			if filename == "" {
				filename = "image" + imageExtensionForMIMEType(bc.MIMEType)
			}
			return common.OpenAttachment(bc.Data, filename)
		}
	}
	return nil
}

func imageExtensionForMIMEType(mimeType string) string {
	switch mimeType {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/bmp":
		return ".bmp"
	default:
		return ""
	}
}

func (m *UserMessageItem) hasImageAttachment() bool {
	for _, bc := range m.message.BinaryContent() {
		if strings.HasPrefix(bc.MIMEType, "image/") {
			return true
		}
	}
	return false
}

// SetLoadingStateVisible controls whether the loading spinner is rendered
// below the user message while image attachments are being processed.
func (m *UserMessageItem) SetLoadingStateVisible(visible bool) {
	if m.showLoadingState == visible {
		return
	}
	m.showLoadingState = visible
	m.clearCache()
}

// StartAnimation implements chat.Animatable.
func (m *UserMessageItem) StartAnimation() tea.Cmd {
	return nil
}

// Animate implements chat.Animatable.
func (m *UserMessageItem) Animate(anim.StepMsg) tea.Cmd {
	return nil
}

// TickAnimation implements chat.Animatable.
func (m *UserMessageItem) TickAnimation() {
	if !m.showLoadingState {
		return
	}
	m.spinnerFrame = (m.spinnerFrame + 1) % len(userMessageSpinnerFrames)
	m.clearCache()
}

// IsAnimating implements chat.Animatable.
func (m *UserMessageItem) IsAnimating() bool {
	return m.showLoadingState
}
