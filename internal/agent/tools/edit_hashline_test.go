package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// helper to compute the hash for a line and format as "LINE#HASH".
func testHashlineRef(lineNum int, content string) string {
	return formatHashlineRef(lineNum, content)
}

func formatHashlineRef(lineNum int, content string) string {
	h := computeHashlineID(lineNum, content)
	return formatRef(lineNum, h)
}

func formatRef(lineNum int, h string) string {
	return jsonIntString(lineNum) + "#" + h
}

func jsonIntString(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestClipboardNamedRegisters(t *testing.T) {
	t.Parallel()

	sessionID := "test-clipboard-session"
	cb := &Clipboard{
		named: make(map[string]map[string]*ClipboardRegister),
		anon:  make(map[string]*ClipboardRegister),
	}

	// Put named
	cb.PutNamed(sessionID, "reg1", []string{"a", "b", "c"})

	// Get named
	lines, ok := cb.GetNamed(sessionID, "reg1")
	require.True(t, ok)
	require.Equal(t, []string{"a", "b", "c"}, lines)

	// Get nonexistent
	_, ok = cb.GetNamed(sessionID, "reg2")
	require.False(t, ok)

	// Anonymous
	cb.PutAnonymous(sessionID, []string{"x", "y"})
	lines, ok = cb.GetAnonymous(sessionID)
	require.True(t, ok)
	require.Equal(t, []string{"x", "y"}, lines)

	// ClearAnonymous
	cb.ClearAnonymous(sessionID)
	_, ok = cb.GetAnonymous(sessionID)
	require.False(t, ok)

	// Named still present after clearing anonymous
	lines, ok = cb.GetNamed(sessionID, "reg1")
	require.True(t, ok)
	require.Equal(t, []string{"a", "b", "c"}, lines)

	// Clear all
	cb.Clear(sessionID)
	_, ok = cb.GetNamed(sessionID, "reg1")
	require.False(t, ok)
	_, ok = cb.GetAnonymous(sessionID)
	require.False(t, ok)
}

func TestClipboardReturnsCopies(t *testing.T) {
	t.Parallel()

	sessionID := "test-clipboard-copy"
	cb := &Clipboard{
		named: make(map[string]map[string]*ClipboardRegister),
		anon:  make(map[string]*ClipboardRegister),
	}

	original := []string{"line1", "line2"}
	cb.PutNamed(sessionID, "reg", original)

	got1, _ := cb.GetNamed(sessionID, "reg")
	got1[0] = "MUTATED"

	got2, _ := cb.GetNamed(sessionID, "reg")
	require.Equal(t, "line1", got2[0], "mutation of returned slice should not affect stored data")
}

func TestHashlineCutPasteInFile(t *testing.T) {
	t.Parallel()

	lines := []string{
		"alpha",
		"beta",
		"gamma",
		"delta",
		"epsilon",
	}

	// Parse: cut lines 2-3 (beta, gamma), paste after line 1 (alpha)
	cutStart := formatHashlineRef(2, "beta")
	cutEnd := formatHashlineRef(3, "gamma")
	pasteLine := formatHashlineRef(1, "alpha")

	ops := []HashlineEditOperation{
		{Operation: "cut", Start: cutStart, End: cutEnd, Register: "move"},
		{Operation: "paste", Line: pasteLine, Register: "move"},
	}

	parsed, err := parseHashlineOperations(ops, lines)
	require.NoError(t, err)

	// Simulate the cut pre-pass
	sessionID := "test-cut-paste"
	for i, op := range parsed {
		if op.Operation == hashlineEditOpCut {
			startLine := op.Start.Line
			endLine := op.End.Line
			captured := make([]string, endLine-startLine+1)
			copy(captured, lines[startLine-1:endLine])
			GlobalClipboard.PutNamed(sessionID, op.Register, captured)
			parsed[i].Operation = hashlineEditOpReplaceRange
			parsed[i].ContentLines = nil
		}
	}

	// Resolve paste
	for i, op := range parsed {
		if op.Operation == hashlineEditOpPaste && len(op.ContentLines) == 0 {
			captured, found := GlobalClipboard.GetNamed(sessionID, op.Register)
			require.True(t, found)
			parsed[i].ContentLines = captured
		}
	}

	result, err := applyHashlineOperations(lines, parsed)
	require.NoError(t, err)

	// Expected: alpha, beta, gamma, delta, epsilon (cut from 2-3, paste after 1 = no net change)
	require.Equal(t, []string{"alpha", "beta", "gamma", "delta", "epsilon"}, result)

	GlobalClipboard.Clear(sessionID)
}

func TestHashlineCutPasteCrossFile(t *testing.T) {
	t.Parallel()

	sessionID := "test-cross-file"

	fileALines := []string{
		"file_a_line1",
		"file_a_line2",
		"file_a_line3",
		"file_a_line4",
	}
	fileBLines := []string{
		"file_b_line1",
		"file_b_line2",
		"file_b_line3",
	}

	// Cut lines 2-3 from file A
	cutStart := formatHashlineRef(2, "file_a_line2")
	cutEnd := formatHashlineRef(3, "file_a_line3")

	opsA := []HashlineEditOperation{
		{Operation: "cut", Start: cutStart, End: cutEnd, Register: "xfer"},
	}

	parsedA, err := parseHashlineOperations(opsA, fileALines)
	require.NoError(t, err)

	// Execute cut pre-pass on file A
	for i, op := range parsedA {
		if op.Operation == hashlineEditOpCut {
			captured := make([]string, op.End.Line-op.Start.Line+1)
			copy(captured, fileALines[op.Start.Line-1:op.End.Line])
			GlobalClipboard.PutNamed(sessionID, op.Register, captured)
			parsedA[i].Operation = hashlineEditOpReplaceRange
			parsedA[i].ContentLines = nil
		}
	}

	resultA, err := applyHashlineOperations(fileALines, parsedA)
	require.NoError(t, err)
	require.Equal(t, []string{"file_a_line1", "file_a_line4"}, resultA)

	// Now paste into file B after line 1
	pasteLine := formatHashlineRef(1, "file_b_line1")
	opsB := []HashlineEditOperation{
		{Operation: "paste", Line: pasteLine, Register: "xfer"},
	}

	parsedB, err := parseHashlineOperations(opsB, fileBLines)
	require.NoError(t, err)

	// Resolve paste from the named register
	for i, op := range parsedB {
		if op.Operation == hashlineEditOpPaste && len(op.ContentLines) == 0 {
			captured, found := GlobalClipboard.GetNamed(sessionID, op.Register)
			require.True(t, found)
			require.Equal(t, []string{"file_a_line2", "file_a_line3"}, captured)
			parsedB[i].ContentLines = captured
		}
	}

	resultB, err := applyHashlineOperations(fileBLines, parsedB)
	require.NoError(t, err)
	require.Equal(t, []string{"file_b_line1", "file_a_line2", "file_a_line3", "file_b_line2", "file_b_line3"}, resultB)

	GlobalClipboard.Clear(sessionID)
}

func TestHashlinePasteBefore(t *testing.T) {
	t.Parallel()

	sessionID := "test-paste-before"

	lines := []string{
		"line1",
		"line2",
		"line3",
	}

	// Put some lines in a register
	GlobalClipboard.PutNamed(sessionID, "reg", []string{"INSERTED1", "INSERTED2"})

	pasteLine := formatHashlineRef(2, "line2")
	ops := []HashlineEditOperation{
		{Operation: "paste", Line: pasteLine, Register: "reg", PasteBefore: true},
	}

	parsed, err := parseHashlineOperations(ops, lines)
	require.NoError(t, err)

	// Resolve paste
	for i, op := range parsed {
		if op.Operation == hashlineEditOpPaste && len(op.ContentLines) == 0 {
			captured, found := GlobalClipboard.GetNamed(sessionID, op.Register)
			require.True(t, found)
			parsed[i].ContentLines = captured
		}
	}

	result, err := applyHashlineOperations(lines, parsed)
	require.NoError(t, err)
	require.Equal(t, []string{"line1", "INSERTED1", "INSERTED2", "line2", "line3"}, result)

	GlobalClipboard.Clear(sessionID)
}

func TestApplyFileHashlineOperationsBasic(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	fileA := filepath.Join(root, "a.txt")
	fileB := filepath.Join(root, "b.txt")

	contentA := "line1\nline2\nline3\nline4\n"
	contentB := "aaa\nbbb\nccc\n"

	require.NoError(t, os.WriteFile(fileA, []byte(contentA), 0o644))
	require.NoError(t, os.WriteFile(fileB, []byte(contentB), 0o644))

	permissions := &mockWritePermissionService{
		Broker: pubsub.NewBroker[permission.PermissionRequest](), granted: true,
	}
	historyService := &mockHistoryService{Broker: pubsub.NewBroker[history.File]()}

	editCtx := editContext{
		ctx:            newNonPlanModeContext("test-multi-file"),
		permissions:    permissions,
		files:          historyService,
		filetracker:    &mockFileTracker{},
		workingDir:     root,
		fuzzyThreshold: 0.92,
	}

	// Replace line 2 in file A, replace line 1 in file B
	refA2 := formatHashlineRef(2, "line2")
	refB1 := formatHashlineRef(1, "aaa")

	fileOps := []FileHashlineOperations{
		{
			FilePath: fileA,
			Operations: []HashlineEditOperation{
				{Operation: "replace_line", Line: refA2, Content: "LINE2_MODIFIED"},
			},
		},
		{
			FilePath: fileB,
			Operations: []HashlineEditOperation{
				{Operation: "replace_line", Line: refB1, Content: "AAA_MODIFIED"},
			},
		},
	}

	response, err := applyFileHashlineOperations(editCtx, fileOps, fantasy.ToolCall{ID: "test-call", Name: EditToolName})
	require.NoError(t, err)
	require.False(t, response.IsError, "expected success, got: %s", response.Content)

	// Verify file A
	newContentA, err := os.ReadFile(fileA)
	require.NoError(t, err)
	require.Contains(t, string(newContentA), "LINE2_MODIFIED")
	require.Contains(t, string(newContentA), "line1")
	require.Contains(t, string(newContentA), "line3")

	// Verify file B
	newContentB, err := os.ReadFile(fileB)
	require.NoError(t, err)
	require.Contains(t, string(newContentB), "AAA_MODIFIED")
	require.Contains(t, string(newContentB), "bbb")

	// Verify metadata is an array
	var meta []EditResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(response.Metadata), &meta))
	require.Len(t, meta, 2)
}

func TestApplyFileHashlineOperationsCrossFileCutPaste(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	fileA := filepath.Join(root, "source.go")
	fileB := filepath.Join(root, "dest.go")

	contentA := "func oldFunc() {\n\treturn\n}\nfunc keep() {\n\treturn\n}\n"
	contentB := "package main\n\nfunc main() {}\n"

	require.NoError(t, os.WriteFile(fileA, []byte(contentA), 0o644))
	require.NoError(t, os.WriteFile(fileB, []byte(contentB), 0o644))

	permissions := &mockWritePermissionService{
		Broker: pubsub.NewBroker[permission.PermissionRequest](), granted: true,
	}
	historyService := &mockHistoryService{Broker: pubsub.NewBroker[history.File]()}

	editCtx := editContext{
		ctx:            newNonPlanModeContext("test-cross-file-ops"),
		permissions:    permissions,
		files:          historyService,
		filetracker:    &mockFileTracker{},
		workingDir:     root,
		fuzzyThreshold: 0.92,
	}

	// Cut lines 1-3 from file A (the oldFunc), paste after line 1 in file B
	linesA := []string{
		"func oldFunc() {",
		"\treturn",
		"}",
		"func keep() {",
		"\treturn",
		"}",
	}
	linesB := []string{
		"package main",
		"",
		"func main() {}",
	}

	cutStart := formatHashlineRef(1, linesA[0])
	cutEnd := formatHashlineRef(3, linesA[2])
	pasteLine := formatHashlineRef(1, linesB[0])

	fileOps := []FileHashlineOperations{
		{
			FilePath: fileA,
			Operations: []HashlineEditOperation{
				{Operation: "cut", Start: cutStart, End: cutEnd, Register: "move"},
			},
		},
		{
			FilePath: fileB,
			Operations: []HashlineEditOperation{
				{Operation: "paste", Line: pasteLine, Register: "move"},
			},
		},
	}

	response, err := applyFileHashlineOperations(editCtx, fileOps, fantasy.ToolCall{ID: "test-call", Name: EditToolName})
	require.NoError(t, err)
	require.False(t, response.IsError, "expected success, got: %s", response.Content)

	// File A should only have keep()
	newContentA, err := os.ReadFile(fileA)
	require.NoError(t, err)
	require.NotContains(t, string(newContentA), "oldFunc")
	require.Contains(t, string(newContentA), "func keep() {")

	// File B should have oldFunc pasted after line 1
	newContentB, err := os.ReadFile(fileB)
	require.NoError(t, err)
	require.Contains(t, string(newContentB), "func oldFunc() {")
	require.Contains(t, string(newContentB), "package main")
	require.Contains(t, string(newContentB), "func main() {}")
}

func TestApplyFileHashlineOperationsFileNotFound(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	permissions := &mockWritePermissionService{
		Broker: pubsub.NewBroker[permission.PermissionRequest](), granted: true,
	}
	historyService := &mockHistoryService{Broker: pubsub.NewBroker[history.File]()}

	editCtx := editContext{
		ctx:            newNonPlanModeContext("test-not-found"),
		permissions:    permissions,
		files:          historyService,
		filetracker:    &mockFileTracker{},
		workingDir:     root,
		fuzzyThreshold: 0.92,
	}

	fileOps := []FileHashlineOperations{
		{
			FilePath: filepath.Join(root, "nonexistent.txt"),
			Operations: []HashlineEditOperation{
				{Operation: "replace_line", Line: "1#XX", Content: "test"},
			},
		},
	}

	response, err := applyFileHashlineOperations(editCtx, fileOps, fantasy.ToolCall{ID: "test-call", Name: EditToolName})
	require.NoError(t, err)
	require.True(t, response.IsError, "should return error for nonexistent file")
	require.Contains(t, response.Content, "not found")
}

func TestEditToolFileOperationsDispatch(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	fileA := filepath.Join(root, "a.txt")
	fileB := filepath.Join(root, "b.txt")

	require.NoError(t, os.WriteFile(fileA, []byte("hello\nworld\n"), 0o644))
	require.NoError(t, os.WriteFile(fileB, []byte("foo\nbar\n"), 0o644))

	permissions := &mockWritePermissionService{
		Broker: pubsub.NewBroker[permission.PermissionRequest](), granted: true,
	}
	historyService := &mockHistoryService{Broker: pubsub.NewBroker[history.File]()}
	editTool := NewEditTool(nil, permissions, historyService, &mockFileTracker{}, root, 0.92)

	ctx := newNonPlanModeContext("test-dispatch")

	refA1 := formatHashlineRef(1, "hello")
	refB1 := formatHashlineRef(1, "foo")

	input, err := json.Marshal(EditParams{
		FilePath: fileA, // fallback path
		FileOperations: []FileHashlineOperations{
			{
				FilePath: fileA,
				Operations: []HashlineEditOperation{
					{Operation: "replace_line", Line: refA1, Content: "HELLO"},
				},
			},
			{
				FilePath: fileB,
				Operations: []HashlineEditOperation{
					{Operation: "replace_line", Line: refB1, Content: "FOO"},
				},
			},
		},
	})
	require.NoError(t, err)

	resp, err := editTool.Run(ctx, fantasy.ToolCall{
		ID: "edit-1", Name: EditToolName, Input: string(input),
	})
	require.NoError(t, err)
	require.False(t, resp.IsError, "edit should succeed, got: %s", resp.Content)

	// Both files should be modified
	contentA, err := os.ReadFile(fileA)
	require.NoError(t, err)
	require.Contains(t, string(contentA), "HELLO")

	contentB, err := os.ReadFile(fileB)
	require.NoError(t, err)
	require.Contains(t, string(contentB), "FOO")
}

func TestFuzzyThresholdDisabled(t *testing.T) {
	t.Parallel()

	// Use a typo that won't match via strategies 1-4 (whitespace/comment)
	// but will match via strategy 5 (Levenshtein similarity).
	content := "func processData(data string) {\n    valdate(data)\n    save(data)\n}"
	oldStr := "func processData(data string) {\n    valdate(data)\n    save(data)\n}"
	newStr := "func processData(data string) {\n    validate(data)\n    save(data)\n}"

	// oldStr == content here, so strategies 1-4 will match exactly.
	// To truly test threshold=0 disabling similarity matching, use a slightly
	// different oldStr that only similarity matching could catch.
	content2 := "func processData(data string) {\n    valdate(data)\n    save(data)\n}"
	oldStr2 := "func processData(data string) {\n    valdate2(data)\n    save(data)\n}"

	// With threshold 0, strategy 5 is skipped. Strategies 1-4 won't match
	// because "valdate2" != "valdate" under any whitespace/comment normalization.
	_, ok := fuzzyReplace(content2, oldStr2, newStr, false, 0)
	require.False(t, ok, "fuzzy similarity matching should be disabled when threshold is 0")

	// Sanity check: with threshold > 0, it should match.
	res, ok := fuzzyReplace(content2, oldStr2, newStr, false, 0.92)
	require.True(t, ok, "fuzzy similarity matching should succeed with threshold > 0")
	require.Contains(t, res, "validate")

	// Also verify that exact match still works with threshold=0 (strategies 1-4).
	res2, ok := fuzzyReplace(content, oldStr, newStr, false, 0)
	require.True(t, ok, "exact match should still work even with threshold=0")
	require.Contains(t, res2, "validate")
}

func TestFuzzyThresholdCustom(t *testing.T) {
	t.Parallel()

	content := "func processData(data string) {\n    valdate(data)\n    save(data)\n}"
	oldStr := "func processData(data string) {\n    valdate(data)\n    save(data)\n}"
	newStr := "func processData(data string) {\n    validate(data)\n    save(data)\n}"

	// With a lower threshold (0.80), the typo should match
	res, ok := fuzzyReplace(content, oldStr, newStr, false, 0.80)
	require.True(t, ok, "fuzzy matching should succeed with lower threshold")
	require.Contains(t, res, "validate")
}
