package dialog

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/ui/common"
	uv "github.com/charmbracelet/ultraviolet"
)

// DenialsID is the identifier for the denials dialog.
const DenialsID = "denials"

// DenialsDialog shows a list of Guardian-denied actions that can be reviewed.
type DenialsDialog struct {
	com        *common.Common
	entries    []*permission.DenialQueueEntry
	selected   int
	scroll     int
	maxVisible int
	keyMap     denialsKeyMap
	help       help.Model
}

type denialsKeyMap struct {
	Up, Down, Select, Close key.Binding
}

func defaultDenialsKeyMap() denialsKeyMap {
	return denialsKeyMap{
		Up: key.NewBinding(
			key.WithKeys("up", "k"),
			key.WithHelp("↑/k", "up"),
		),
		Down: key.NewBinding(
			key.WithKeys("down", "j"),
			key.WithHelp("↓/j", "down"),
		),
		Select: key.NewBinding(
			key.WithKeys("enter", "y"),
			key.WithHelp("enter/y", "approve"),
		),
		Close: key.NewBinding(
			key.WithKeys("esc", "q"),
			key.WithHelp("esc/q", "close"),
		),
	}
}

var _ Dialog = (*DenialsDialog)(nil)

// NewDenialsDialog creates a new denials dialog.
func NewDenialsDialog(com *common.Common, entries []*permission.DenialQueueEntry) *DenialsDialog {
	help := help.New()
	help.Styles = com.Styles.DialogHelpStyles()

	return &DenialsDialog{
		com:      com,
		entries:  entries,
		selected: 0,
		scroll:   0,
		keyMap:   defaultDenialsKeyMap(),
		help:     help,
	}
}

func (d *DenialsDialog) ID() string {
	return DenialsID
}

func (d *DenialsDialog) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, d.keyMap.Close):
			return ActionClose{}
		case key.Matches(msg, d.keyMap.Up):
			if d.selected > 0 {
				d.selected--
				if d.selected < d.scroll {
					d.scroll = d.selected
				}
			}
		case key.Matches(msg, d.keyMap.Down):
			if d.selected < len(d.entries)-1 {
				d.selected++
				if d.selected >= d.scroll+d.maxVisible {
					d.scroll = d.selected - d.maxVisible + 1
				}
			}
		case key.Matches(msg, d.keyMap.Select):
			if len(d.entries) > 0 && d.entries[d.selected].Retryable {
				entry := d.entries[d.selected]
				return ActionApproveDenial{EntryID: entry.ID}
			}
		}
	}
	return nil
}

func (d *DenialsDialog) SetSize(width, height int) {
	d.maxVisible = max(1, (height-10)/3)
}

func (d *DenialsDialog) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := d.com.Styles
	width := max(0, min(defaultDialogMaxWidth, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
	innerWidth := width - t.Dialog.View.GetHorizontalFrameSize()
	d.help.SetWidth(innerWidth)

	rc := NewRenderContext(t, width)
	rc.Title = "Guardian Denials"

	var parts []string

	if len(d.entries) == 0 {
		parts = append(parts, t.Dialog.PrimaryText.Render("No denied actions to review."))
	} else {
		parts = append(parts, t.Dialog.PrimaryText.Render(fmt.Sprintf("%d action(s) blocked by Guardian:", len(d.entries))))

		// Render entries
		visibleEnd := min(d.scroll+d.maxVisible, len(d.entries))
		for i := d.scroll; i < visibleEnd; i++ {
			entry := d.entries[i]
			isSelected := i == d.selected

			// Format time
			timeStr := formatDenialTime(entry.Timestamp)

			// Tool name and action
			toolInfo := fmt.Sprintf("%s %s", entry.Request.ToolName, entry.Request.Action)

			// Reason (truncated)
			reason := entry.Reason
			if len(reason) > 40 {
				reason = reason[:37] + "..."
			}

			// Status indicator
			status := "  "
			if entry.Retryable {
				status = "↻ "
			} else {
				status = "✗ "
			}

			line := fmt.Sprintf("  %s%s %s [%s]", status, toolInfo, reason, timeStr)
			if isSelected {
				parts = append(parts, t.Dialog.SelectedItem.Render(line))
			} else {
				parts = append(parts, t.Dialog.NormalItem.Render(line))
			}
		}

		if len(d.entries) > d.maxVisible {
			remaining := len(d.entries) - d.scroll - d.maxVisible
			if remaining > 0 {
				parts = append(parts, t.Dialog.SecondaryText.Render(fmt.Sprintf("\n  ... %d more", remaining)))
			}
		}
	}

	rc.AddPart(strings.Join(parts, "\n"))
	rc.Help = d.help.View(d)

	view := rc.Render()
	DrawCenterCursor(scr, area, view, nil)
	return nil
}

// ShortHelp implements help.KeyMap.
func (d *DenialsDialog) ShortHelp() []key.Binding {
	return []key.Binding{d.keyMap.Up, d.keyMap.Down, d.keyMap.Select, d.keyMap.Close}
}

// FullHelp implements help.KeyMap.
func (d *DenialsDialog) FullHelp() [][]key.Binding {
	return [][]key.Binding{d.ShortHelp()}
}

func formatDenialTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// ActionApproveDenial is sent when the user approves a denied action.
type ActionApproveDenial struct {
	EntryID string
}
