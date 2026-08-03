package tools

import (
	"path/filepath"
	"testing"

	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
	"github.com/stretchr/testify/require"
)

func TestFormatCodeActions(t *testing.T) {
	t.Parallel()

	output := formatCodeActions([]protocol.CodeAction{
		{Title: "Apply quick fix", Kind: protocol.QuickFix, Edit: &protocol.WorkspaceEdit{}},
		{Title: "Run command", Command: &protocol.Command{Title: "Run command", Command: "go.test"}},
		{Title: "Disabled action", Disabled: &protocol.CodeActionDisabled{Reason: "unsupported context"}},
	})

	require.Contains(t, output, "Found 3 code action(s):")
	require.Contains(t, output, "1. Apply quick fix [quickfix] (edit)")
	require.Contains(t, output, "2. Run command (command)")
	require.Contains(t, output, "3. Disabled action (disabled: unsupported context)")
}

func TestWorkspaceEditHelpers(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	fileA := filepath.Join(tempDir, "a.go")
	fileB := filepath.Join(tempDir, "b.go")
	fileC := filepath.Join(tempDir, "c.go")
	fileD := filepath.Join(tempDir, "d.go")
	fileE := filepath.Join(tempDir, "e.go")

	edit := protocol.WorkspaceEdit{
		Changes: map[protocol.DocumentURI][]protocol.TextEdit{
			protocol.URIFromPath(fileA): {},
			protocol.URIFromPath(fileB): {},
		},
		DocumentChanges: []protocol.DocumentChange{
			{
				TextDocumentEdit: &protocol.TextDocumentEdit{
					TextDocument: protocol.OptionalVersionedTextDocumentIdentifier{
						Version: 1,
						TextDocumentIdentifier: protocol.TextDocumentIdentifier{
							URI: protocol.URIFromPath(fileC),
						},
					},
					Edits: []protocol.Or_TextDocumentEdit_edits_Elem{{
						Value: protocol.TextEdit{},
					}},
				},
			},
			{CreateFile: &protocol.CreateFile{URI: protocol.URIFromPath(fileD)}},
			{RenameFile: &protocol.RenameFile{OldURI: protocol.URIFromPath(fileA), NewURI: protocol.URIFromPath(fileE)}},
		},
	}

	require.False(t, workspaceEditEmpty(edit))
	paths := workspaceEditPaths(edit)
	require.Equal(t, []string{fileA, fileB, fileC, fileD, fileE}, paths)
	require.True(t, workspaceEditEmpty(protocol.WorkspaceEdit{}))
}
