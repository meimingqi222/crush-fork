package dialog

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/rivo/uniseg"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/userinput"
	uv "github.com/charmbracelet/ultraviolet"
)

const (
	RequestUserInputID       = "request_user_input"
	requestUserInputMaxWidth = 84
	customAnswerLabel        = "Other"
	customAnswerDescription  = "Provide a custom answer"
)

type RequestUserInput struct {
	com *common.Common

	request          userinput.Request
	current          int
	selected         int
	customMode       bool // true when "Other" row has focus and textinput is active
	customInput      textinput.Model
	multiSelected    map[string]map[string]bool // question ID -> set of selected option labels
	answers          map[string]userinput.Answer
	help             help.Model
	keyMap           requestUserInputKeyMap
	dialogInnerWidth int
}

type requestUserInputKeyMap struct {
	Select   key.Binding
	Toggle   key.Binding
	Next     key.Binding
	Previous key.Binding
	Close    key.Binding
}

func (k requestUserInputKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Select, k.Close}
}

func (k requestUserInputKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{{k.Select, k.Toggle, k.Close}, {k.Previous, k.Next}}
}

func NewRequestUserInput(com *common.Common, request userinput.Request) *RequestUserInput {
	input := textinput.New()
	input.SetVirtualCursor(false)
	input.Placeholder = "Type your answer"
	input.SetStyles(com.Styles.TextInput)

	helpModel := help.New()
	helpModel.Styles = com.Styles.DialogHelpStyles()

	multiSelected := make(map[string]map[string]bool, len(request.Questions))
	for _, q := range request.Questions {
		if q.MultiSelect {
			multiSelected[q.ID] = make(map[string]bool)
		}
	}

	return &RequestUserInput{
		com:           com,
		request:       request,
		answers:       make(map[string]userinput.Answer, len(request.Questions)),
		customInput:   input,
		multiSelected: multiSelected,
		help:          helpModel,
		keyMap: requestUserInputKeyMap{
			Select:   key.NewBinding(key.WithKeys("enter", "ctrl+y"), key.WithHelp("enter", "confirm")),
			Toggle:   key.NewBinding(key.WithKeys("space"), key.WithHelp("space", "toggle")),
			Next:     key.NewBinding(key.WithKeys("down", "ctrl+n"), key.WithHelp("↓", "next")),
			Previous: key.NewBinding(key.WithKeys("up", "ctrl+p"), key.WithHelp("↑", "previous")),
			Close:    CloseKey,
		},
	}
}

func (*RequestUserInput) ID() string {
	return RequestUserInputID
}

func (r *RequestUserInput) Cursor() *tea.Cursor {
	if !r.customMode {
		return nil
	}
	return r.customInput.Cursor()
}

func (r *RequestUserInput) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch {
		case key.Matches(msg, r.keyMap.Close):
			if r.customMode {
				r.customMode = false
				r.customInput.Blur()
				return nil
			}
			return ActionResolveUserInput{Response: userinput.Response{
				RequestID:  r.request.ID,
				SessionID:  r.request.SessionID,
				ToolCallID: r.request.ToolCallID,
				Status:     userinput.ResponseStatusCanceled,
				Answers:    r.collectedAnswers(),
			}}
		case key.Matches(msg, r.keyMap.Previous):
			if !r.customMode {
				options := optionsWithOther(r.currentQuestion())
				r.selected = (r.selected + len(options) - 1) % len(options)
				r.syncCustomFocus()
			}
			return nil
		case key.Matches(msg, r.keyMap.Next):
			if !r.customMode {
				options := optionsWithOther(r.currentQuestion())
				r.selected = (r.selected + 1) % len(options)
				r.syncCustomFocus()
			}
			return nil
		case key.Matches(msg, r.keyMap.Toggle):
			if r.customMode {
				// In custom mode, space inserts a space character.
				var cmd tea.Cmd
				r.customInput, cmd = r.customInput.Update(msg)
				return ActionCmd{Cmd: cmd}
			}
			question := r.currentQuestion()
			if question.MultiSelect && r.selected < len(question.Options) {
				label := question.Options[r.selected].Label
				selectedSet := r.multiSelected[question.ID]
				selectedSet[label] = !selectedSet[label]
			}
			return nil
		case key.Matches(msg, r.keyMap.Select):
			if r.customMode {
				value := strings.TrimSpace(r.customInput.Value())
				if value == "" {
					return nil
				}
				r.answers[r.currentQuestion().ID] = userinput.Answer{
					QuestionID:  r.currentQuestion().ID,
					CustomInput: value,
				}
				return r.advance()
			}
			question := r.currentQuestion()
			if r.selected == len(question.Options) {
				// "Other" row selected — focus the inline textinput.
				r.customMode = true
				r.customInput.SetValue("")
				r.customInput.Focus()
				return nil
			}
			if question.MultiSelect {
				// Multi-select: Enter confirms current selections and advances.
				selectedLabels := r.collectMultiSelected(question)
				if len(selectedLabels) == 0 {
					// Don't allow an empty multi-select submission.
					return nil
				}
				r.answers[question.ID] = userinput.Answer{
					QuestionID:      question.ID,
					SelectedOptions: selectedLabels,
				}
				return r.advance()
			}
			// Single-select: select and advance.
			selected := question.Options[r.selected]
			r.answers[question.ID] = userinput.Answer{
				QuestionID:     question.ID,
				SelectedOption: selected.Label,
			}
			return r.advance()
		}
	}

	if r.customMode {
		var cmd tea.Cmd
		r.customInput, cmd = r.customInput.Update(msg)
		return ActionCmd{Cmd: cmd}
	}

	return nil
}

// syncCustomFocus is called when navigating onto or off the "Other" row.
// When landing on "Other" without a prior custom value, we do NOT auto-focus
// the textinput — the user presses Enter to activate it. This keeps
// navigation smooth. If a custom value was already typed for this question,
// we preserve and focus it so editing is seamless.
func (r *RequestUserInput) syncCustomFocus() {
	question := r.currentQuestion()
	if r.selected == len(question.Options) {
		// Navigated onto "Other" row.
		if existing, ok := r.answers[question.ID]; ok && existing.CustomInput != "" {
			r.customMode = true
			r.customInput.SetValue(existing.CustomInput)
			r.customInput.Focus()
		}
	} else {
		// Navigated off "Other" row.
		if r.customMode {
			r.customMode = false
			r.customInput.Blur()
		}
	}
}

// collectMultiSelected returns the sorted list of selected option labels for
// a multi-select question.
func (r *RequestUserInput) collectMultiSelected(question requestQuestion) []string {
	selectedSet := r.multiSelected[question.ID]
	result := make([]string, 0, len(selectedSet))
	for _, opt := range question.Options {
		if selectedSet[opt.Label] {
			result = append(result, opt.Label)
		}
	}
	return result
}

func (r *RequestUserInput) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	question := r.currentQuestion()
	title := fmt.Sprintf("%s (%d/%d)", question.Header, r.current+1, len(r.request.Questions))
	if title == fmt.Sprintf(" (%d/%d)", r.current+1, len(r.request.Questions)) {
		title = fmt.Sprintf("Question %d/%d", r.current+1, len(r.request.Questions))
	}
	dialogWidth := min(area.Dx(), requestUserInputMaxWidth)
	r.dialogInnerWidth = max(0, dialogWidth-r.com.Styles.Dialog.View.GetHorizontalFrameSize())
	r.customInput.SetWidth(max(0, r.dialogInnerWidth-r.com.Styles.Dialog.InputPrompt.GetHorizontalFrameSize()-1))

	var parts []string
	parts = append(parts, r.com.Styles.Dialog.Title.Render(title))
	parts = append(parts, "")
	parts = append(parts, question.Question)
	parts = append(parts, "")

	allOpts := optionsWithOther(question)
	for idx, option := range allOpts {
		isOther := idx == len(question.Options)
		isHighlighted := idx == r.selected && !r.customMode
		isActive := idx == r.selected && (r.customMode || isOther)

		var prefix string
		if question.MultiSelect && !isOther {
			prefix = "☐"
			if r.multiSelected[question.ID][option.Label] {
				prefix = "☑"
			}
		} else {
			prefix = "○"
		}
		if isHighlighted || isActive {
			prefix = "▶ " + prefix
		} else {
			prefix = "  " + prefix
		}

		parts = append(parts, fmt.Sprintf("%s %s", prefix, option.Label))
		parts = append(parts, r.com.Styles.Dialog.SecondaryText.Render("    "+option.Description))
		parts = append(parts, "")

		// Inline custom input row — rendered directly under "Other".
		if isOther && r.customMode {
			parts = append(parts, "      "+r.com.Styles.Dialog.InputPrompt.Render(r.customInput.View()))
			parts = append(parts, "")
		}
	}

	// Help hint adapts to mode.
	if question.MultiSelect {
		parts = append(parts, r.com.Styles.Dialog.SecondaryText.Render("Space to toggle, Enter to confirm."))
	} else {
		parts = append(parts, r.com.Styles.Dialog.SecondaryText.Render("Enter to select."))
	}
	parts = append(parts, "")
	parts = append(parts, r.help.View(r.keyMap))

	content := strings.Join(parts, "\n")
	rendered := r.com.Styles.Dialog.View.Width(dialogWidth).Render(content)

	var cur *tea.Cursor
	if r.customMode {
		cur = realTextInputCursor(r.customInput)
		if cur != nil {
			dialogStyle := r.com.Styles.Dialog.View
			inputStyle := r.com.Styles.Dialog.InputPrompt
			// Count the visual lines of content that appear above the InputPrompt part.
			// Use lipgloss rendering at the dialog inner width to correctly handle
			// word-wrapped content (e.g. long question text).
			aboveInput := strings.Join(parts[:len(parts)-4], "\n")
			linesAbove := lipgloss.Height(lipgloss.NewStyle().Width(r.dialogInnerWidth).Render(aboveInput))
			cur.X += dialogStyle.GetBorderLeftSize() +
				dialogStyle.GetPaddingLeft() +
				dialogStyle.GetMarginLeft() +
				inputStyle.GetBorderLeftSize() +
				inputStyle.GetMarginLeft() +
				inputStyle.GetPaddingLeft() +
				6 // indent for inline input ("      " prefix)
			cur.Y += dialogStyle.GetBorderTopSize() +
				dialogStyle.GetPaddingTop() +
				dialogStyle.GetMarginTop() +
				linesAbove +
				inputStyle.GetBorderTopSize() +
				inputStyle.GetMarginTop() +
				inputStyle.GetPaddingTop()
			cur = CenterCursor(area, rendered, cur)
		}
	}
	DrawCenter(scr, area, rendered)
	return cur
}

func realTextInputCursor(input textinput.Model) *tea.Cursor {
	cur := input.Cursor()
	if cur == nil {
		return nil
	}
	cur.X = textInputCursorX(input)
	return cur
}

func textInputCursorX(input textinput.Model) int {
	promptWidth := lipgloss.Width(input.Prompt)
	value := []rune(input.Value())
	pos := max(0, min(input.Position(), len(value)))
	inputWidth := input.Width()
	if inputWidth <= 0 || uniseg.StringWidth(string(value)) <= inputWidth {
		return promptWidth + uniseg.StringWidth(string(value[:pos]))
	}

	prefixWidth := uniseg.StringWidth(string(value[:pos]))
	if prefixWidth <= inputWidth {
		return promptWidth + prefixWidth
	}

	visibleWidth := 0
	for i := pos - 1; i >= 0; i-- {
		charWidth := uniseg.StringWidth(string(value[i : i+1]))
		if visibleWidth+charWidth > inputWidth {
			break
		}
		visibleWidth += charWidth
	}
	return promptWidth + visibleWidth
}

func (r *RequestUserInput) advance() Action {
	r.customMode = false
	r.customInput.Blur()
	r.selected = 0
	if r.current == len(r.request.Questions)-1 {
		return ActionResolveUserInput{Response: userinput.Response{
			RequestID:  r.request.ID,
			SessionID:  r.request.SessionID,
			ToolCallID: r.request.ToolCallID,
			Status:     userinput.ResponseStatusSubmitted,
			Answers:    r.collectedAnswers(),
		}}
	}
	r.current++
	return nil
}

func (r *RequestUserInput) collectedAnswers() []userinput.Answer {
	answers := make([]userinput.Answer, 0, len(r.request.Questions))
	for _, question := range r.request.Questions {
		answer, ok := r.answers[question.ID]
		if !ok {
			continue
		}
		answers = append(answers, answer)
	}
	return answers
}

func (r *RequestUserInput) currentQuestion() requestQuestion {
	return requestQuestion(r.request.Questions[r.current])
}

type requestQuestion userinput.Question

func optionsWithOther(question requestQuestion) []userinput.Option {
	q := userinput.Question(question)
	options := make([]userinput.Option, 0, len(question.Options)+1)
	options = append(options, q.Options...)
	options = append(options, userinput.Option{Label: customAnswerLabel, Description: customAnswerDescription})
	return options
}
