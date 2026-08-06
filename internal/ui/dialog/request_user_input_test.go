package dialog

import (
	"testing"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/userinput"
	"github.com/rivo/uniseg"
	"github.com/stretchr/testify/require"
)

func TestRequestUserInputTextInputCursorXUsesDisplayWidth(t *testing.T) {
	input := textinput.New()
	input.SetVirtualCursor(false)
	input.SetWidth(80)
	input.SetValue("你觉 怎么弄比较好？")
	input.CursorEnd()
	input.Focus()

	want := uniseg.StringWidth(input.Prompt) + uniseg.StringWidth(input.Value())
	require.Equal(t, want, textInputCursorX(input))
	require.Greater(t, textInputCursorX(input), input.Cursor().X)
}

func TestRequestUserInputTextInputCursorXHandlesHorizontalScroll(t *testing.T) {
	input := textinput.New()
	input.SetVirtualCursor(false)
	input.SetWidth(6)
	input.SetValue("abc你觉怎么")
	input.CursorEnd()

	require.Equal(t, uniseg.StringWidth(input.Prompt)+6, textInputCursorX(input))
}

func TestRequestUserInputTextInputCursorXAfterDeletingWideCharacter(t *testing.T) {
	input := textinput.New()
	input.SetVirtualCursor(false)
	input.SetWidth(80)
	input.Focus()
	input.SetValue("你觉 怎么弄比较好？")
	input.SetCursor(2)

	var cmd tea.Cmd
	input, cmd = input.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyBackspace}))
	require.Nil(t, cmd)

	require.Equal(t, "你 怎么弄比较好？", input.Value())
	want := uniseg.StringWidth(input.Prompt) + uniseg.StringWidth("你")
	require.Equal(t, want, textInputCursorX(input))
	require.Greater(t, textInputCursorX(input), input.Cursor().X)
}

func TestRequestUserInputMultiSelect(t *testing.T) {
	com := testCommon(t, `{"options":{"disable_provider_auto_update":true},"tools":{}}`)
	req := userinput.Request{
		ID:         "req-1",
		SessionID:  "session-1",
		ToolCallID: "call-1",
		Questions: []userinput.Question{{
			Header:      "Features",
			ID:          "features",
			Question:    "Which features?",
			MultiSelect: true,
			Options: []userinput.Option{
				{Label: "A", Description: "Option A"},
				{Label: "B", Description: "Option B"},
				{Label: "C", Description: "Option C"},
			},
		}},
	}
	dlg := NewRequestUserInput(com, req)

	// Empty Enter does not advance/submit.
	require.Nil(t, dlg.HandleMsg(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter})))
	require.Equal(t, 0, dlg.current)
	require.Len(t, dlg.answers, 0)

	// Toggle option A and B.
	require.Nil(t, dlg.HandleMsg(tea.KeyPressMsg(tea.Key{Code: tea.KeySpace})))
	dlg.HandleMsg(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	require.Nil(t, dlg.HandleMsg(tea.KeyPressMsg(tea.Key{Code: tea.KeySpace})))

	// Confirm submits both selections.
	action := dlg.HandleMsg(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	resolve, ok := action.(ActionResolveUserInput)
	require.True(t, ok)
	require.Equal(t, userinput.ResponseStatusSubmitted, resolve.Response.Status)
	require.Len(t, resolve.Response.Answers, 1)
	require.Equal(t, []string{"A", "B"}, resolve.Response.Answers[0].SelectedOptions)
}
