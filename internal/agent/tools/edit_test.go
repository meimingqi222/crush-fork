package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestApplyEditToContentPartialSuccess(t *testing.T) {
	t.Parallel()

	content := "line 1\nline 2\nline 3\n"

	newContent, err := applyEntryToContent(content, EditEntry{
		OldString: "line 1",
		NewString: "LINE 1",
	}, 0.92)
	require.NoError(t, err)
	require.Contains(t, newContent, "LINE 1")
	require.Contains(t, newContent, "line 2")

	_, err = applyEntryToContent(content, EditEntry{
		OldString: "line 99",
		NewString: "LINE 99",
	}, 0.92)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestEditSequentialApplication(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")

	content := "line 1\nline 2\nline 3\nline 4\n"
	err := os.WriteFile(testFile, []byte(content), 0o644)
	require.NoError(t, err)

	currentContent := content

	edits := []EditEntry{
		{OldString: "line 1", NewString: "LINE 1"},
		{OldString: "line 99", NewString: "LINE 99"},
		{OldString: "line 3", NewString: "LINE 3"},
		{OldString: "line 2", NewString: "LINE 2"},
	}

	var failedEdits []FailedEdit
	successCount := 0

	for i, edit := range edits {
		newContent, err := applyEntryToContent(currentContent, edit, 0.92)
		if err != nil {
			failedEdits = append(failedEdits, FailedEdit{
				Index: i + 1,
				Error: err.Error(),
				Edit:  edit,
			})
			continue
		}
		currentContent = newContent
		successCount++
	}

	require.Equal(t, 3, successCount, "Expected 3 successful edits")
	require.Len(t, failedEdits, 1, "Expected 1 failed edit")
	require.Equal(t, 2, failedEdits[0].Index)
	require.Contains(t, failedEdits[0].Error, "not found")
	require.Contains(t, currentContent, "LINE 1")
	require.Contains(t, currentContent, "LINE 2")
	require.Contains(t, currentContent, "LINE 3")
	require.Contains(t, currentContent, "line 4")
	require.NotContains(t, currentContent, "LINE 99")
}

func TestEditAllEditsSucceed(t *testing.T) {
	t.Parallel()

	content := "line 1\nline 2\nline 3\n"

	edits := []EditEntry{
		{OldString: "line 1", NewString: "LINE 1"},
		{OldString: "line 2", NewString: "LINE 2"},
		{OldString: "line 3", NewString: "LINE 3"},
	}

	currentContent := content
	successCount := 0

	for _, edit := range edits {
		newContent, err := applyEntryToContent(currentContent, edit, 0.92)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		currentContent = newContent
		successCount++
	}

	require.Equal(t, 3, successCount)
	require.Contains(t, currentContent, "LINE 1")
	require.Contains(t, currentContent, "LINE 2")
	require.Contains(t, currentContent, "LINE 3")
}

func TestEditAllEditsFail(t *testing.T) {
	t.Parallel()

	content := "line 1\nline 2\n"

	edits := []EditEntry{
		{OldString: "line 99", NewString: "LINE 99"},
		{OldString: "line 100", NewString: "LINE 100"},
	}

	currentContent := content
	var failedEdits []FailedEdit

	for i, edit := range edits {
		newContent, err := applyEntryToContent(currentContent, edit, 0.92)
		if err != nil {
			failedEdits = append(failedEdits, FailedEdit{
				Index: i + 1,
				Error: err.Error(),
				Edit:  edit,
			})
			continue
		}
		currentContent = newContent
	}

	require.Len(t, failedEdits, 2)
	require.Equal(t, content, currentContent, "Content should be unchanged")
}

func TestMemoryFileReadCache(t *testing.T) {
	t.Parallel()

	cache := &FileCache{cache: make(map[string]map[string][]string)}
	sessionID := "session-1"
	filePath := "/path/to/file.txt"
	lines := []string{"line1", "line2"}

	cache.Put(sessionID, filePath, lines)

	cachedLines, ok := cache.Get(sessionID, filePath)
	require.True(t, ok)
	require.Equal(t, lines, cachedLines)

	// test concurrency
	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func(val int) {
			cache.Put(sessionID, filePath, []string{string(rune(val))})
			_, _ = cache.Get(sessionID, filePath)
			done <- true
		}(i)
	}
	for i := 0; i < 10; i++ {
		<-done
	}
}

func TestAlignLinesLCS(t *testing.T) {
	t.Parallel()

	base := []string{"A", "B", "C", "D", "E"}
	ours := []string{"A", "X", "B", "C", "Y", "E"}

	alignment := alignLinesLCS(base, ours)

	require.Equal(t, 1, alignment[1])
	require.Equal(t, 3, alignment[2])
	require.Equal(t, 4, alignment[3])
	require.Equal(t, 0, alignment[4])
	require.Equal(t, 6, alignment[5])
}

func TestTryRecoverHashline(t *testing.T) {
	t.Parallel()

	sessionID := "session-test"
	filePath := "/path/to/recover.txt"

	baseLines := []string{
		"line 1",
		"line 2",
		"line 3",
		"line 4",
		"line 5",
	}

	GlobalFileCache.Put(sessionID, filePath, baseLines)
	defer GlobalFileCache.Delete(sessionID, filePath)

	oursLines := []string{
		"unrelated inserted line",
		"line 1",
		"line 2",
		"line 3",
		"line 4",
		"another unrelated line",
		"line 5",
	}

	// 动态计算 line 3 的 hash
	hash := computeHashlineID(3, "line 3")

	ops := []HashlineEditOperation{
		{
			Operation: "replace_line",
			Line:      fmt.Sprintf("3#%s", hash),
			Content:   "LINE 3 MODIFIED",
		},
	}

	result, err := tryRecoverHashline(sessionID, filePath, oursLines, ops)
	require.NoError(t, err)

	expected := []string{
		"unrelated inserted line",
		"line 1",
		"line 2",
		"LINE 3 MODIFIED",
		"line 4",
		"another unrelated line",
		"line 5",
	}
	require.Equal(t, expected, result)
}

func TestFileCacheFIFO(t *testing.T) {
	t.Parallel()

	cache := &FileCache{}
	sessionID := "session-fifo"

	// Add 35 items
	for i := 0; i < 35; i++ {
		filePath := fmt.Sprintf("/path/to/file%d.txt", i)
		cache.Put(sessionID, filePath, []string{fmt.Sprintf("content %d", i)})
	}

	// The first 5 should be evicted, leaving only 5 to 34
	for i := 0; i < 5; i++ {
		filePath := fmt.Sprintf("/path/to/file%d.txt", i)
		_, ok := cache.Get(sessionID, filePath)
		require.False(t, ok, "File %d should have been evicted", i)
	}

	for i := 5; i < 35; i++ {
		filePath := fmt.Sprintf("/path/to/file%d.txt", i)
		_, ok := cache.Get(sessionID, filePath)
		require.True(t, ok, "File %d should still be in cache", i)
	}
}

func TestFileCacheHistory(t *testing.T) {
	t.Parallel()

	cache := &FileCache{}
	cache.Put("session", "/file", []string{"one"})
	cache.Put("session", "/file", []string{"two"})

	history, ok := cache.GetHistory("session", "/file")
	require.True(t, ok)
	require.Equal(t, [][]string{{"one"}, {"two"}}, history)
}

func TestAlignLinesGreedyForLargeFiles(t *testing.T) {
	t.Parallel()

	base := make([]string, maxLCSMatrixSize/1000+1)
	ours := append([]string{"inserted"}, base...)
	alignment := alignLinesLCS(base, ours)
	require.Equal(t, 2, alignment[1])
}

func TestPatchEmptyLineMatch(t *testing.T) {
	t.Parallel()

	original := []string{
		"line 1",
		"",
		"line 3",
	}

	patchText := `--- a/test.txt
+++ b/test.txt
@@ -1,3 +1,3 @@
 line 1
-
+line 2
 line 3
`

	patches, err := ParseUnifiedPatch(patchText)
	require.NoError(t, err)
	require.Len(t, patches, 1)

	updated, err := ApplyPatchToLines(original, patches[0].Hunks)
	require.NoError(t, err)
	require.Equal(t, []string{"line 1", "line 2", "line 3"}, updated)
}

func TestPatchOffsetShiftAssignment(t *testing.T) {
	t.Parallel()

	original := []string{
		"line 1",
		"line 2",
		"line 3",
		"line 4",
		"line 5",
	}

	// hunk 1: replace line 2 with newline (offset shift)
	// hunk 2: replace line 4
	patchText := `--- a/test.txt
+++ b/test.txt
@@ -2,1 +2,2 @@
-line 2
+line 2a
+line 2b
@@ -4,1 +5,1 @@
-line 4
+line 4a
`

	patches, err := ParseUnifiedPatch(patchText)
	require.NoError(t, err)
	require.Len(t, patches, 1)

	updated, err := ApplyPatchToLines(original, patches[0].Hunks)
	require.NoError(t, err)
	require.Equal(t, []string{"line 1", "line 2a", "line 2b", "line 3", "line 4a", "line 5"}, updated)
}

func TestNegativeLimitDefense(t *testing.T) {
	t.Parallel()

	lines := []string{"1", "2", "3"}
	res, err := extractReadResultFromLines(lines, 0, -5)
	require.NoError(t, err)
	require.Len(t, res.Lines, 0)
}

func TestFuzzyReplaceCommentStrip(t *testing.T) {
	t.Parallel()

	content := "func main() {\n    // this is a comment\n    doSomething()\n}"
	// LLM 忽略了注释，或者用了不同的注释前缀
	oldStr := "func main() {\n    # this is a comment\n    doSomething()\n}"
	newStr := "func main() {\n    doSomething()\n}"

	res, ok := fuzzyReplace(content, oldStr, newStr, false, 0.92)
	require.True(t, ok)
	require.NotContains(t, res, "comment")
}

func TestFuzzyReplaceLevenshteinSimilarity(t *testing.T) {
	t.Parallel()

	content := "func processData(data string) {\n    validate(data)\n    save(data)\n}"
	// LLM 输入的 validate 稍微拼错，例如 valdate，应该仍能基于相似度匹配
	oldStr := "func processData(data string) {\n    valdate(data)\n    save(data)\n}"
	newStr := "func processData(data string) {\n    validate(data)\n    // saved!\n    save(data)\n}"

	res, ok := fuzzyReplace(content, oldStr, newStr, false, 0.92)
	require.True(t, ok)
	require.Contains(t, res, "saved!")
}

func TestDetailedMatchErrorFormat(t *testing.T) {
	t.Parallel()

	content := "line 1\nline 2\nline 3\n"
	oldStr := "line 1\nline 2a\nline 3\n"

	errText := buildDetailedMatchError(content, oldStr)
	require.Contains(t, errText, "Closest match (95% similar) found at line 1")
	require.Contains(t, errText, "Expected (LLM):")
	require.Contains(t, errText, "Actual (File):")
}

func TestFuzzyReplaceCommentStripGuard(t *testing.T) {
	t.Parallel()

	// Content has one commented line and one uncommented line.
	// If Strategy 4 is NOT guarded, searching for "return x + y" (without comment)
	// would mistakenly match the commented line as well, leading to 2 matches (ambiguous).
	// With the guard, the commented line is ignored under Strategy 4,
	// allowing the exact match on the second line to succeed.
	content := "func main() {\n    // return x + y\n    return x + y\n}"
	oldStr := "return x + y"
	newStr := "return a + b"

	res, ok := fuzzyReplace(content, oldStr, newStr, false, 0.92)
	require.True(t, ok)
	require.Contains(t, res, "return a + b")
	require.Contains(t, res, "// return x + y") // the comment line should remain untouched

	// Verify the helper isCommentLine
	require.True(t, isCommentLine("// return x + y"))
	require.False(t, isCommentLine("return x + y"))
}

func TestLevenshteinRuneAware(t *testing.T) {
	t.Parallel()

	s := "你好世界"
	t1 := "你好"

	// Rune-based Levenshtein distance: "你好" to "你好世界" is 2 runes insertion.
	// Byte-based distance would be 6 bytes (Chinese characters are 3 bytes each in UTF-8).
	dist := levenshteinDistance(s, t1)
	require.Equal(t, 2, dist)

	sim := lineSimilarity(s, t1)
	// Similarity = 1 - 2/4 = 0.5 (50% similarity)
	require.InDelta(t, 0.5, sim, 0.01)
}
