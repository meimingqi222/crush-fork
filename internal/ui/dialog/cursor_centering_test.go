package dialog

import (
	"go/ast"
	"go/parser"
	"go/token"
	"image"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
)

// TestDrawCenterCursorTranslatesInPlace pins the contract that the Draw
// methods rely on: DrawCenterCursor mutates the cursor it is handed, turning
// dialog-relative coordinates into screen coordinates.
func TestDrawCenterCursorTranslatesInPlace(t *testing.T) {
	t.Parallel()

	cur := &tea.Cursor{Position: tea.Position{X: 3, Y: 4}}
	area := image.Rect(0, 0, 120, 40)
	got := CenterCursor(area, "line one\nline two\nline three", cur)

	require.Same(t, cur, got, "CenterCursor returns the same cursor it translated")
	require.Greater(t, got.X, 3, "X must be offset by the centered view origin")
	require.Greater(t, got.Y, 4, "Y must be offset by the centered view origin")
}

// TestDrawMethodsReturnTheTranslatedCursor catches a bug that was copy-pasted
// across all three goal dialogs: passing Cursor() to DrawCenterCursor and then
// calling Cursor() *again* for the return value. The drawn copy gets the
// centering offset while the returned one keeps dialog-relative coordinates,
// which the UI then treats as absolute -- parking the terminal caret in the
// chat area instead of the dialog's input.
//
// The check is source-level because the faulty and correct forms are
// behaviourally identical except for where the caret lands, which needs a real
// terminal to observe.
func TestDrawMethodsReturnTheTranslatedCursor(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err, name)

		ast.Inspect(file, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "Draw" || fn.Body == nil {
				return true
			}
			if !callsCursorInArgs(fn) {
				return true
			}
			checked++
			t.Errorf(
				"%s: %s passes a fresh Cursor() call to DrawCenterCursor; "+
					"store it in a variable and return that same value, or the "+
					"returned cursor keeps dialog-relative coordinates",
				filepath.Base(name), fn.Name.Name)
			return false
		})
	}

	require.Zero(t, checked, "no dialog may inline Cursor() into DrawCenterCursor")
}

// callsCursorInArgs reports whether the function passes a Cursor() call
// directly as an argument to DrawCenterCursor.
func callsCursorInArgs(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		ident, ok := call.Fun.(*ast.Ident)
		if !ok || ident.Name != "DrawCenterCursor" {
			return true
		}
		for _, arg := range call.Args {
			inner, ok := arg.(*ast.CallExpr)
			if !ok {
				continue
			}
			sel, ok := inner.Fun.(*ast.SelectorExpr)
			if ok && sel.Sel.Name == "Cursor" {
				found = true
				return false
			}
		}
		return true
	})
	return found
}
