package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

func writeCtxFile(t *testing.T, dir, name, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
}

// TestBudgetSharedWhenPathsProcessedSequentially verifies the aggregate
// budget is shared across context paths: one budget consumed by the first
// path must affect the second path, producing a marker instead of a second
// full file.
func TestBudgetSharedWhenPathsProcessedSequentially(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeCtxFile(t, dir, "AGENTS.md", strings.Repeat("a", 4000))
	writeCtxFile(t, dir, "CLAUDE.md", strings.Repeat("b", 4000))

	store := &config.ConfigStore{}
	budget := newContextFileBudget(6000)
	ag := processContextPath(filepath.Join(dir, "AGENTS.md"), store, 6000, budget)
	require.Len(t, ag, 1)

	cl := processContextPath(filepath.Join(dir, "CLAUDE.md"), store, 6000, budget)
	require.Len(t, cl, 1)
	require.Equal(t, contextFileBudgetExhaustedMarker, cl[0].Content)
}

// TestOversizedFileGetsMarker verifies bug #2: a file that cannot fit within
// the remaining aggregate budget must produce a marker, not vanish silently.
func TestOversizedFileGetsMarker(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeCtxFile(t, dir, "AGENTS.md", strings.Repeat("a", 1000))
	writeCtxFile(t, dir, "CLAUDE.md", strings.Repeat("b", 8000))

	store := &config.ConfigStore{}
	budget := newContextFileBudget(3000)
	ag := processContextPath(filepath.Join(dir, "AGENTS.md"), store, 6000, budget)
	require.Len(t, ag, 1)

	cl := processContextPath(filepath.Join(dir, "CLAUDE.md"), store, 6000, budget)
	require.Len(t, cl, 1)
	require.Equal(t, contextFileBudgetExhaustedMarker, cl[0].Content)
}

// TestRuneBudgetConsistency verifies bug #3: CJK files (1 rune = 3 bytes)
// must be budgeted in runes, so a 4000-rune CJK file (12000 bytes) fits a
// 6000-rune budget and is not wrongly dropped.
func TestRuneBudgetConsistency(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeCtxFile(t, dir, "AGENTS.md", strings.Repeat("中", 4000))

	store := &config.ConfigStore{}
	budget := newContextFileBudget(6000)
	ag := processContextPath(filepath.Join(dir, "AGENTS.md"), store, 6000, budget)
	require.Len(t, ag, 1)
	require.Contains(t, ag[0].Content, strings.Repeat("中", 4000))
	require.NotEqual(t, contextFileBudgetExhaustedMarker, ag[0].Content)
}

// TestZeroDisablesCaps verifies bug #4: maxTotalChars <= 0 disables the
// aggregate cap (unbounded), and maxFileChars <= 0 disables per-file truncation.
func TestZeroDisablesCaps(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeCtxFile(t, dir, "AGENTS.md", strings.Repeat("a", 10000))

	store := &config.ConfigStore{}
	// 0 per-file cap + 0 total cap => file injected in full, no truncation.
	ag := processContextPath(filepath.Join(dir, "AGENTS.md"), store, 0, newContextFileBudget(0))
	require.Len(t, ag, 1)
	require.Contains(t, ag[0].Content, strings.Repeat("a", 10000))
	require.NotContains(t, ag[0].Content, contextFileTruncateMarker)
	require.NotEqual(t, contextFileBudgetExhaustedMarker, ag[0].Content)
}

// TestPerFileTruncation verifies the per-file rune cap truncates correctly
// and appends the truncation marker.
func TestPerFileTruncation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeCtxFile(t, dir, "AGENTS.md", strings.Repeat("a", 8000))

	store := &config.ConfigStore{}
	ag := processContextPath(filepath.Join(dir, "AGENTS.md"), store, 6000, newContextFileBudget(0))
	require.Len(t, ag, 1)
	require.Contains(t, ag[0].Content, contextFileTruncateMarker)
}

// TestGlobalContextFileGetsMarkerWhenCrowdedOut verifies the global AGENTS.md
// is not dropped silently when project context paths have already consumed
// the aggregate budget: the user's global instructions must leave a trace.
func TestGlobalContextFileGetsMarkerWhenCrowdedOut(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeCtxFile(t, dir, "AGENTS.md", strings.Repeat("p", 5000))
	writeCtxFile(t, dir, "global.md", strings.Repeat("g", 5000))

	store := &config.ConfigStore{}
	budget := newContextFileBudget(6000)

	project := processContextPath(filepath.Join(dir, "AGENTS.md"), store, 6000, budget)
	require.Len(t, project, 1)
	require.NotEqual(t, contextFileBudgetExhaustedMarker, project[0].Content)

	global := appendGlobalContextFile(nil, filepath.Join(dir, "global.md"), 6000, budget)
	require.Len(t, global, 1, "global AGENTS.md must not vanish")
	require.Equal(t, contextFileBudgetExhaustedMarker, global[0].Content)
	require.Equal(t, filepath.Join(dir, "global.md"), global[0].Path)
}

// TestGlobalContextFileFitsWithinBudget verifies the normal path still
// injects the global file in full when the budget allows.
func TestGlobalContextFileFitsWithinBudget(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeCtxFile(t, dir, "global.md", strings.Repeat("g", 1000))

	global := appendGlobalContextFile(nil, filepath.Join(dir, "global.md"), 6000, newContextFileBudget(6000))
	require.Len(t, global, 1)
	require.Contains(t, global[0].Content, strings.Repeat("g", 1000))
}
