package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/filetracker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeFileTracker is a minimal filetracker.Service that just returns a fixed
// list of paths, so buildRecentFileContext can be exercised without a DB.
type fakeFileTracker struct {
	paths []string
}

func (f *fakeFileTracker) RecordRead(ctx context.Context, sessionID, path string) {}

func (f *fakeFileTracker) LastReadTime(ctx context.Context, sessionID, path string) time.Time {
	return time.Time{}
}

func (f *fakeFileTracker) ListReadFiles(ctx context.Context, sessionID string) ([]string, error) {
	return f.paths, nil
}

var _ filetracker.Service = (*fakeFileTracker)(nil)

// writeTestFile writes content of the given size (in runes, all ASCII 'a')
// to a new file under dir and returns its absolute path.
func writeTestFile(t *testing.T, dir, name string, size int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	content := strings.Repeat("a", size)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

func TestBuildRecentFileContext_InjectsUpToMaxFilesWithinBudget(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// 8 files, each far bigger than postCompactMaxPerFile (5,000 chars), so
	// each one gets truncated individually, but there's easily enough of the
	// 50,000 char total budget for postCompactMaxFiles (5) of them
	// (5 * 5,000 = 25,000 < 50,000).
	var paths []string
	for i := range 8 {
		paths = append(paths, writeTestFile(t, dir, fmt.Sprintf("file%d.txt", i), 20_000))
	}

	a := &sessionAgent{
		filetracker: &fakeFileTracker{paths: paths},
		workingDir:  dir,
	}

	result := a.buildRecentFileContext(context.Background(), "sess", 0)

	// Only the most-recently-read postCompactMaxFiles files are considered
	// at all (ListReadFiles order is oldest-to-newest by convention; the
	// function keeps the tail).
	require.Len(t, result, postCompactMaxFiles)

	for _, entry := range result {
		// Each injected file's *displayed* text (excluding the wrapping
		// "Recently read file `...`:\n```\n...\n```" markup) must respect
		// postCompactMaxPerFile - it should never inject a whole 20,000-char
		// file verbatim.
		assert.LessOrEqual(t, len([]rune(entry)), postCompactMaxPerFile+200,
			"injected entry should be truncated to roughly postCompactMaxPerFile, not the full file")
		assert.Contains(t, entry, "truncated")
	}
}

func TestBuildRecentFileContext_TotalCharsReflectsActualInjectedText(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Every file is oversized, so every one of the postCompactMaxFiles
	// selected files gets truncated to (approximately) postCompactMaxPerFile
	// runes. Before the fix, totalChars accumulated the *full* file size
	// (20,000 runes each), which alone would exceed postCompactMaxTotalChars
	// (50,000) after just 2-3 files and starve the rest of the budget even
	// though only a few thousand characters were actually injected.
	var paths []string
	for i := range postCompactMaxFiles {
		paths = append(paths, writeTestFile(t, dir, fmt.Sprintf("big%d.txt", i), 20_000))
	}

	a := &sessionAgent{
		filetracker: &fakeFileTracker{paths: paths},
		workingDir:  dir,
	}

	result := a.buildRecentFileContext(context.Background(), "sess", 0)
	require.Len(t, result, postCompactMaxFiles)

	// Reproduce the internal totalChars accounting to assert it matches the
	// actual injected content length, not the full file lengths.
	totalInjectedChars := 0
	for _, entry := range result {
		totalInjectedChars += len([]rune(entry))
	}

	// With correct accounting, postCompactMaxFiles files each truncated to
	// ~postCompactMaxPerFile chars sum to well under postCompactMaxTotalChars,
	// so all of them are injected (none dropped due to a bogus early budget
	// exhaustion).
	assert.Less(t, totalInjectedChars, postCompactMaxTotalChars,
		"actual injected content must fit under the total budget")

	// Sanity: with the old (buggy) accounting using full file length
	// (20,000 runes) per file, a single file would already exceed
	// postCompactMaxTotalChars (50,000), so only 1 of the 5 files would ever
	// have been injected. Assert we get all of them now.
	require.Len(t, result, postCompactMaxFiles)
}

func TestBuildRecentFileContext_SmallFilesNotTruncated(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	paths := []string{
		writeTestFile(t, dir, "small1.txt", 100),
		writeTestFile(t, dir, "small2.txt", 200),
	}

	a := &sessionAgent{
		filetracker: &fakeFileTracker{paths: paths},
		workingDir:  dir,
	}

	result := a.buildRecentFileContext(context.Background(), "sess", 0)
	require.Len(t, result, 2)
	for _, entry := range result {
		assert.NotContains(t, entry, "truncated")
	}
}
