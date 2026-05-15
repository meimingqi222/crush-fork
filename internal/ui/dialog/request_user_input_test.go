package dialog

import (
	"testing"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
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
