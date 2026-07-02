package skills

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// writeSkillFile creates a valid skill directory named `name` inside dir with
// a minimal SKILL.md file. The directory name must match the skill name to
// satisfy validation.
func writeSkillFile(t *testing.T, dir, name, description string) {
	t.Helper()
	skillDir := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	content := "---\nname: " + name + "\ndescription: " + description + "\n---\n# " + name + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644))
}

func TestDiscoverCachedEmptyPaths(t *testing.T) {
	t.Parallel()

	require.Nil(t, DiscoverCached(nil))
	require.Nil(t, DiscoverCached([]string{}))
}

// TestDiscoverCachedCaches verifies that repeated calls with the same paths
// return the cached result without re-scanning the filesystem. This is
// proven by adding a new skill on disk after the first call and asserting
// the cached result does not reflect it.
func TestDiscoverCachedCaches(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeSkillFile(t, dir, "skill-a", "Skill A.")

	first := DiscoverCached([]string{dir})
	require.Contains(t, skillNames(first), "skill-a")
	require.NotContains(t, skillNames(first), "skill-b")

	// Add a second skill on disk after the cache is populated.
	writeSkillFile(t, dir, "skill-b", "Skill B.")

	// The cached result must not reflect the newly added skill.
	second := DiscoverCached([]string{dir})
	require.Contains(t, skillNames(second), "skill-a")
	require.NotContains(t, skillNames(second), "skill-b")
}

// TestDiscoverCachedInvalidate verifies that Invalidate with a specific paths
// key forces a re-scan on the next DiscoverCached call.
func TestDiscoverCachedInvalidate(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeSkillFile(t, dir, "skill-a", "Skill A.")

	first := DiscoverCached([]string{dir})
	require.Contains(t, skillNames(first), "skill-a")
	require.NotContains(t, skillNames(first), "skill-b")

	// Add a second skill and invalidate only this path.
	writeSkillFile(t, dir, "skill-b", "Skill B.")
	Invalidate([]string{dir})

	second := DiscoverCached([]string{dir})
	require.Contains(t, skillNames(second), "skill-a")
	require.Contains(t, skillNames(second), "skill-b")
}

// TestDiscoverCachedInvalidateAll verifies that Invalidate(nil) clears all
// cached entries, forcing a re-scan. This test is intentionally not run in
// parallel because Invalidate(nil) wipes the shared package-level cache,
// which could evict entries other parallel tests rely on for staleness
// assertions. Non-parallel tests complete before any parallel test resumes,
// so this isolation is guaranteed.
func TestDiscoverCachedInvalidateAll(t *testing.T) {
	dir := t.TempDir()
	writeSkillFile(t, dir, "skill-a", "Skill A.")

	first := DiscoverCached([]string{dir})
	require.Contains(t, skillNames(first), "skill-a")
	require.NotContains(t, skillNames(first), "skill-b")

	// Add a second skill and clear the entire cache.
	writeSkillFile(t, dir, "skill-b", "Skill B.")
	Invalidate(nil)

	second := DiscoverCached([]string{dir})
	require.Contains(t, skillNames(second), "skill-a")
	require.Contains(t, skillNames(second), "skill-b")
}

// TestDiscoverCachedConcurrent verifies that concurrent DiscoverCached calls
// with the same paths do not panic and return consistent results.
func TestDiscoverCachedConcurrent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeSkillFile(t, dir, "skill-a", "Skill A.")

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	results := make([][]*Skill, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = DiscoverCached([]string{dir})
		}(i)
	}
	close(start)
	wg.Wait()

	for i, r := range results {
		require.Containsf(t, skillNames(r), "skill-a", "goroutine %d missing skill-a", i)
		require.NotContainsf(t, skillNames(r), "skill-b", "goroutine %d unexpectedly has skill-b", i)
	}
}

// TestDiscoverCachedReturnsCopy verifies that the returned slice is a copy:
// mutating it does not affect subsequent cached results.
func TestDiscoverCachedReturnsCopy(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeSkillFile(t, dir, "skill-a", "Skill A.")

	first := DiscoverCached([]string{dir})
	require.Contains(t, skillNames(first), "skill-a")

	// Mutate the returned slice header; the cache must not be affected.
	first[0] = &Skill{Name: "mutated", Description: "should not leak"}

	second := DiscoverCached([]string{dir})
	require.Contains(t, skillNames(second), "skill-a")
	require.NotContains(t, skillNames(second), "mutated")
}
