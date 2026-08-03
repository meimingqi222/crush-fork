# TUI Dialog Cursor Coordinates

## The Problem

Text-input cursors in dialogs can be vertically or horizontally misaligned with
the visible input, especially after a description wraps or the input contains
CJK characters.

## Root Cause

The cursor passes through several coordinate spaces:

1. `textinput.Cursor()` returns a position relative to the text input.
2. Dialog styles add borders, margins, and padding.
3. The dialog adds title/description/input layout rows.
4. `DrawCenterCursor` translates the result into screen coordinates.

The bug recurs when each layer estimates part of the layout independently:

- `textinput.Cursor().X` is a logical character position, not always the
  terminal display-column position. Wide characters such as Chinese text occupy
  two columns.
- `InputCursor` accounts for input and dialog styles, while individual dialogs
  add hand-written `+1`/`+2` Y offsets. This can double-count margins or miss
  wrapped description lines.
- Dialog content is rendered dynamically by `RenderContext`, but cursor offsets
  are often calculated from constants rather than from the rendered layout.
- `DrawCenterCursor` mutates the cursor it receives. Passing one cursor for
  drawing and returning a separately calculated cursor produces different
  coordinate spaces.

## Symptoms

- The caret appears one row above or below the input text.
- The caret drifts left as the user types CJK or other wide characters.
- The first field is aligned but the second field is not.
- The layout breaks when terminal width changes and description text wraps.

## The Fix Pattern

- Convert input positions to display columns using `uniseg.StringWidth` (or the
  shared `textInputCursorX` helper), not rune count alone.
- Apply style/frame offsets exactly once.
- Derive offsets from the same rendered layout used by `Draw`, including the
  actual visual height of wrapped content.
- Store the cursor in a local variable, pass that same pointer to
  `DrawCenterCursor`, and return it after translation.
- Add tests for ASCII, CJK, horizontal scrolling, wrapped descriptions, and
  focus changes between fields.

Do not fix a new symptom by blindly adding another constant Y offset. First
identify which coordinate space the cursor currently represents and which style
or rendered rows have already been included.

## Prevention Checklist

1. Read `internal/ui/AGENTS.md` before changing dialog layout code.
2. Reuse `InputCursor` and `textInputCursorX` instead of duplicating width math.
3. Treat lipgloss margins, borders, and padding as part of the layout contract.
4. Test at multiple terminal widths so wrapped content is covered.
5. Check cursor identity and translation when using `DrawCenterCursor`.

## Affected Paths

- `internal/ui/dialog/common.go` — shared style/frame cursor offsets.
- `internal/ui/dialog/dialog.go` — centered cursor translation.
- `internal/ui/dialog/goal.go` and `guided_goal.go` — multi-part dialog offsets.
- `internal/ui/dialog/request_user_input.go` — display-width cursor helper.
- `internal/ui/dialog/request_user_input_test.go` — CJK and scrolling coverage.
