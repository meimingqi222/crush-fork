package dialog

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/planmode"
	"github.com/charmbracelet/crush/internal/ui/common"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

const PlanReviewID = "plan_review"

// planReviewFocus identifies the focused region.
type planReviewFocus int

const (
	focusBody    planReviewFocus = iota
	focusActions                 // approval action buttons
)

// planReviewAction represents an approval action choice.
type planReviewAction int

const (
	actionExecute planReviewAction = iota
	actionCompact
	actionKeepContext
	actionRevise
)

// PlanReview is a fullscreen plan review dialog with scrollable content,
// optional TOC sidebar, and approval actions.
type PlanReview struct {
	com       *common.Common
	sessionID string
	plan      string
	title     string

	// Parsed content.
	sections []planmode.Section
	toc      []planmode.TOCEntry

	// Viewport state.
	scrollY   int
	contentH  int
	viewportH int

	// TOC state.
	showTOC bool
	tocSel  int

	// Action bar state.
	focus       planReviewFocus
	selectedAct planReviewAction

	// Revision input.
	inputMode bool
	feedback  textinput.Model

	// Annotation state: section index -> annotation text.
	annotations map[int]string
	annotating  int // section being annotated, -1 when inactive.

	help   help.Model
	keyMap struct {
		Up       key.Binding
		Down     key.Binding
		PageUp   key.Binding
		PageDown key.Binding
		Home     key.Binding
		End      key.Binding
		Left     key.Binding
		Right    key.Binding
		Tab      key.Binding
		Toggle   key.Binding
		Annotate key.Binding
		Select   key.Binding
		Close    key.Binding
	}
}

// NewPlanReview creates a new plan review dialog.
func NewPlanReview(com *common.Common, sessionID, plan, title string) *PlanReview {
	input := textinput.New()
	input.SetVirtualCursor(false)
	input.Placeholder = "Describe what to change in this section"
	input.SetStyles(com.Styles.TextInput)

	h := help.New()
	h.Styles = com.Styles.DialogHelpStyles()

	plan = strings.TrimSpace(plan)
	sections := planmode.SplitSections(plan, 2)
	toc := planmode.ParseTOC(plan)

	p := &PlanReview{
		com:         com,
		sessionID:   sessionID,
		plan:        plan,
		title:       title,
		sections:    sections,
		toc:         toc,
		showTOC:     len(toc) >= 2,
		tocSel:      0,
		focus:       focusBody,
		selectedAct: actionExecute,
		annotations: make(map[int]string),
		annotating:  -1,
		feedback:    input,
		help:        h,
	}

	p.keyMap = struct {
		Up       key.Binding
		Down     key.Binding
		PageUp   key.Binding
		PageDown key.Binding
		Home     key.Binding
		End      key.Binding
		Left     key.Binding
		Right    key.Binding
		Tab      key.Binding
		Toggle   key.Binding
		Annotate key.Binding
		Select   key.Binding
		Close    key.Binding
	}{
		Up:       key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:     key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		PageUp:   key.NewBinding(key.WithKeys("pgup", "b"), key.WithHelp("pgup", "page up")),
		PageDown: key.NewBinding(key.WithKeys("pgdown", "f", " "), key.WithHelp("pgdn", "page down")),
		Home:     key.NewBinding(key.WithKeys("home", "g"), key.WithHelp("home", "top")),
		End:      key.NewBinding(key.WithKeys("end", "G"), key.WithHelp("end", "bottom")),
		Left:     key.NewBinding(key.WithKeys("left", "h"), key.WithHelp("←", "left")),
		Right:    key.NewBinding(key.WithKeys("right", "l"), key.WithHelp("→", "right")),
		Tab:      key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "switch focus")),
		Toggle:   key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "toggle TOC")),
		Annotate: key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "annotate section")),
		Select:   key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "select")),
		Close:    CloseKey,
	}

	return p
}

func (*PlanReview) ID() string { return PlanReviewID }

func (p *PlanReview) Cursor() *tea.Cursor {
	if !p.inputMode {
		return nil
	}
	cur := InputCursor(p.com.Styles, p.feedback.Cursor())
	if cur == nil {
		return nil
	}
	// Offset for the title and content area above the input.
	cur.Y += titleContentHeight + 3
	return cur
}

func (p *PlanReview) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		// Annotation input mode.
		if p.inputMode {
			return p.handleInputMsg(msg)
		}
		return p.handleNormalMsg(msg)
	}
	return nil
}

func (p *PlanReview) handleInputMsg(msg tea.KeyPressMsg) Action {
	switch {
	case key.Matches(msg, p.keyMap.Close):
		p.inputMode = false
		p.feedback.Blur()
		return nil
	case key.Matches(msg, p.keyMap.Select):
		feedback := strings.TrimSpace(p.feedback.Value())
		if feedback == "" {
			return nil
		}
		if p.annotating >= 0 {
			p.annotations[p.annotating] = feedback
			p.annotating = -1
		}
		p.inputMode = false
		p.feedback.Blur()
		// If annotating a section, collect all annotations and send as feedback.
		if len(p.annotations) > 0 {
			return ActionSubmitPlanFeedback{
				SessionID: p.sessionID,
				Feedback:  p.formatAnnotations(),
			}
		}
		return ActionSubmitPlanFeedback{
			SessionID: p.sessionID,
			Feedback:  feedback,
		}
	}
	var cmd tea.Cmd
	p.feedback, cmd = p.feedback.Update(msg)
	return ActionCmd{Cmd: cmd}
}

func (p *PlanReview) handleNormalMsg(msg tea.KeyPressMsg) Action {
	switch {
	case key.Matches(msg, p.keyMap.Close):
		return ActionClose{}

	case key.Matches(msg, p.keyMap.Tab):
		if p.focus == focusBody {
			p.focus = focusActions
		} else {
			p.focus = focusBody
		}
		return nil

	case key.Matches(msg, p.keyMap.Toggle):
		p.showTOC = !p.showTOC
		p.scrollY = 0
		return nil

	// Body scrolling (only when body is focused).
	case p.focus == focusBody && key.Matches(msg, p.keyMap.Up):
		if p.scrollY > 0 {
			p.scrollY--
		}
		return nil
	case p.focus == focusBody && key.Matches(msg, p.keyMap.Down):
		p.scrollDown(1)
		return nil
	case p.focus == focusBody && key.Matches(msg, p.keyMap.PageUp):
		p.scrollY = max(0, p.scrollY-p.viewportH)
		return nil
	case p.focus == focusBody && key.Matches(msg, p.keyMap.PageDown):
		p.scrollDown(p.viewportH)
		return nil
	case p.focus == focusBody && key.Matches(msg, p.keyMap.Home):
		p.scrollY = 0
		return nil
	case p.focus == focusBody && key.Matches(msg, p.keyMap.End):
		p.scrollToEnd()
		return nil

	// TOC navigation (when TOC is visible and body is focused).
	case p.focus == focusBody && key.Matches(msg, p.keyMap.Annotate):
		p.startAnnotation()
		return nil

	// Action bar navigation (when actions are focused).
	case p.focus == focusActions && key.Matches(msg, p.keyMap.Left):
		p.selectedAct = (p.selectedAct + 3) % 4
		return nil
	case p.focus == focusActions && key.Matches(msg, p.keyMap.Right):
		p.selectedAct = (p.selectedAct + 1) % 4
		return nil
	case p.focus == focusActions && key.Matches(msg, p.keyMap.Select):
		return p.executeAction()
	}
	return nil
}

func (p *PlanReview) startAnnotation() {
	p.annotating = p.sectionIndexAtViewport()
	p.inputMode = true
	p.feedback.SetValue("")
	p.feedback.Focus()
	p.feedback.Placeholder = "Describe what to change in the plan"
}

func (p *PlanReview) sectionIndexAtViewport() int {
	if len(p.sections) == 0 {
		return -1
	}
	if p.contentH <= 0 {
		return 0
	}
	planLines := strings.Split(p.plan, "\n")
	if len(planLines) == 0 {
		return 0
	}
	approxLine := (p.scrollY * len(planLines)) / max(1, p.contentH)
	for i, section := range p.sections {
		if approxLine >= section.LineStart && approxLine <= section.LineEnd {
			return i
		}
	}
	return 0
}

func (p *PlanReview) executeAction() Action {
	switch p.selectedAct {
	case actionExecute:
		return ActionExecuteProposedPlan{
			SessionID: p.sessionID,
			Plan:      p.plan,
		}
	case actionCompact:
		return ActionExecuteWithCompact{
			SessionID: p.sessionID,
			Plan:      p.plan,
		}
	case actionKeepContext:
		return ActionExecuteKeepContext{
			SessionID: p.sessionID,
			Plan:      p.plan,
		}
	case actionRevise:
		p.inputMode = true
		p.feedback.SetValue("")
		p.feedback.Focus()
		p.feedback.Placeholder = "Describe what to change in the plan"
		p.annotating = -1
		return nil
	}
	return nil
}

func (p *PlanReview) formatAnnotations() string {
	var parts []string
	for idx, note := range p.annotations {
		if idx < len(p.sections) {
			parts = append(parts, fmt.Sprintf("Section \"%s\": %s", p.sections[idx].Heading, note))
		} else {
			parts = append(parts, note)
		}
	}
	return strings.Join(parts, "\n")
}

func (p *PlanReview) scrollDown(n int) {
	p.scrollY += n
	maxScroll := max(0, p.contentH-p.viewportH)
	if p.scrollY > maxScroll {
		p.scrollY = maxScroll
	}
}

func (p *PlanReview) scrollToEnd() {
	p.scrollY = max(0, p.contentH-p.viewportH)
}

func (p *PlanReview) effectiveContentWidth(width int) int {
	w := min(120, max(60, width-4))
	dialogFrameW := p.com.Styles.Dialog.View.GetHorizontalFrameSize()
	w -= dialogFrameW
	if p.showTOC {
		w -= 28 // TOC sidebar width.
	}
	return max(30, w)
}

func (p *PlanReview) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	width := min(120, max(60, area.Dx()-4))
	dialogFrameW := p.com.Styles.Dialog.View.GetHorizontalFrameSize()
	dialogFrameH := p.com.Styles.Dialog.View.GetVerticalFrameSize()
	innerW := width - dialogFrameW
	p.viewportH = max(5, area.Dy()-dialogFrameH-titleContentHeight-8)

	rc := NewRenderContext(p.com.Styles, width)
	if p.title != "" {
		rc.Title = p.title
	} else {
		rc.Title = "Plan Review"
	}

	if p.inputMode {
		p.drawInputMode(rc)
		view := rc.Render()
		cur := p.Cursor()
		DrawCenterCursor(scr, area, view, cur)
		return cur
	}

	// Render content area.
	contentW := innerW
	tocStr := ""
	if p.showTOC && innerW > 58 {
		tocW := 26
		contentW = innerW - tocW - 2
		tocStr = p.renderTOC(tocW)
	}

	contentStr := p.renderContent(contentW)
	p.contentH = lipgloss.Height(contentStr)

	// Apply scroll.
	contentLines := strings.Split(contentStr, "\n")
	visibleLines := p.viewportLines(contentLines)
	scrolledContent := strings.Join(visibleLines, "\n")

	// Scroll indicator.
	scrollInfo := p.scrollIndicator()

	// Combine TOC and content.
	var body string
	if tocStr != "" {
		body = lipgloss.JoinHorizontal(lipgloss.Top,
			tocStr,
			lipgloss.NewStyle().PaddingLeft(1).Render(scrolledContent),
		)
	} else {
		body = scrolledContent
	}

	// Action bar.
	actionBar := p.renderActions(innerW)

	rc.AddPart(body)
	if scrollInfo != "" {
		rc.AddPart(p.com.Styles.Dialog.SecondaryText.Render(scrollInfo))
	}
	rc.AddPart(actionBar)
	rc.Help = p.help.View(p)

	view := rc.Render()
	DrawCenter(scr, area, view)
	return nil
}

func (p *PlanReview) drawInputMode(rc *RenderContext) {
	if p.annotating >= 0 && p.annotating < len(p.sections) {
		rc.AddPart(fmt.Sprintf("Annotating section: %s", p.sections[p.annotating].Heading))
	} else {
		rc.AddPart("Describe what should change in the plan.")
	}
	p.feedback.SetWidth(max(0, rc.Width-
		p.com.Styles.Dialog.View.GetHorizontalFrameSize()-
		p.com.Styles.Dialog.InputPrompt.GetHorizontalFrameSize()-1))
	rc.AddPart(p.com.Styles.Dialog.InputPrompt.Render(p.feedback.View()))
	rc.Help = p.help.View(p)
}

func (p *PlanReview) viewportLines(lines []string) []string {
	if p.scrollY >= len(lines) {
		return nil
	}
	end := p.scrollY + p.viewportH
	if end > len(lines) {
		end = len(lines)
	}
	return lines[p.scrollY:end]
}

func (p *PlanReview) scrollIndicator() string {
	if p.contentH <= p.viewportH {
		return ""
	}
	pct := 0
	if p.contentH > 0 {
		pct = (p.scrollY + p.viewportH) * 100 / p.contentH
		if pct > 100 {
			pct = 100
		}
	}
	return fmt.Sprintf("Lines %d-%d of %d (%d%%)",
		p.scrollY+1,
		min(p.scrollY+p.viewportH, p.contentH),
		p.contentH,
		pct,
	)
}

func (p *PlanReview) renderContent(width int) string {
	renderer := common.MarkdownRenderer(p.com.Styles, width)
	if renderer == nil {
		return p.plan
	}
	rendered, err := renderer.Render(p.plan)
	if err != nil {
		// Fallback to plain text.
		return ansi.Truncate(p.plan, width, "…")
	}
	return rendered
}

func (p *PlanReview) renderTOC(width int) string {
	if len(p.toc) == 0 {
		return ""
	}
	var sb strings.Builder
	titleStyle := p.com.Styles.Dialog.Title.Copy().Bold(true).Underline(true)
	sb.WriteString(titleStyle.Width(width).Render("Contents"))
	sb.WriteString("\n")

	normalStyle := p.com.Styles.Dialog.SecondaryText
	selStyle := p.com.Styles.Subtle

	for i, entry := range p.toc {
		indent := strings.Repeat("  ", max(0, entry.Level-1))
		line := indent + entry.Title
		truncated := ansi.Truncate(line, width-1, "…")
		if i == p.tocSel {
			sb.WriteString(selStyle.Width(width).Render(truncated))
		} else {
			sb.WriteString(normalStyle.Render(truncated))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func (p *PlanReview) renderActions(width int) string {
	actions := []struct {
		label string
		act   planReviewAction
	}{
		{"Execute", actionExecute},
		{"Compact & Execute", actionCompact},
		{"Keep Context", actionKeepContext},
		{"Revise", actionRevise},
	}

	buttons := make([]common.ButtonOpts, len(actions))
	for i, a := range actions {
		buttons[i] = common.ButtonOpts{
			Text:     a.label,
			Selected: p.focus == focusActions && p.selectedAct == a.act,
			Padding:  1,
		}
	}
	return common.ButtonGroup(p.com.Styles, buttons, " ")
}

func (p *PlanReview) ShortHelp() []key.Binding {
	if p.inputMode {
		return []key.Binding{p.keyMap.Select, p.keyMap.Close}
	}
	if p.focus == focusBody {
		return []key.Binding{p.keyMap.Up, p.keyMap.Down, p.keyMap.Toggle, p.keyMap.Annotate, p.keyMap.Tab}
	}
	return []key.Binding{p.keyMap.Left, p.keyMap.Right, p.keyMap.Select, p.keyMap.Tab}
}

func (p *PlanReview) FullHelp() [][]key.Binding {
	if p.inputMode {
		return [][]key.Binding{{p.keyMap.Select, p.keyMap.Close}}
	}
	return [][]key.Binding{
		{p.keyMap.Up, p.keyMap.Down, p.keyMap.PageUp, p.keyMap.PageDown, p.keyMap.Home, p.keyMap.End},
		{p.keyMap.Toggle, p.keyMap.Annotate, p.keyMap.Tab, p.keyMap.Select, p.keyMap.Close},
	}
}
