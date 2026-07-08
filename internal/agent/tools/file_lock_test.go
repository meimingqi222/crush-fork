package tools

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCanonicalLockKeyLowercasesOnCaseInsensitiveFilesystems ensures that
// paths differing only in case resolve to the same key on Windows and macOS.
func TestCanonicalLockKeyLowercasesOnCaseInsensitiveFilesystems(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	lower := filepath.Join(dir, "foo", "bar.txt")
	upper := filepath.Join(dir, "FOO", "BAR.TXT")

	lowerKey := CanonicalLockKey(lower)
	upperKey := CanonicalLockKey(upper)

	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		require.Equal(t, lowerKey, upperKey, "paths differing only in case must share a lock key")
	} else {
		require.NotEqual(t, lowerKey, upperKey, "case-sensitive filesystems should preserve case in lock keys")
	}
}

// TestCanonicalLockKeyResolvesSymlinks ensures that a path through a symlink
// resolves to the same lock key as the real path.
func TestCanonicalLockKeyResolvesSymlinks(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	link := filepath.Join(dir, "link")
	require.NoError(t, os.MkdirAll(target, 0o755))

	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlinks on this filesystem: %v", err)
	}

	targetFile := filepath.Join(target, "file.txt")
	linkFile := filepath.Join(link, "file.txt")

	require.Equal(t, CanonicalLockKey(targetFile), CanonicalLockKey(linkFile))
}

// TestFilePathLockForSameMutexForCanonicalEquivalentPaths ensures that
// different spellings of the same actual file get the same mutex.
func TestFilePathLockForSameMutexForCanonicalEquivalentPaths(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	link := filepath.Join(dir, "link")
	require.NoError(t, os.MkdirAll(target, 0o755))

	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlinks on this filesystem: %v", err)
	}

	targetFile := filepath.Join(target, "file.txt")
	linkFile := filepath.Join(link, "file.txt")

	mu1 := FilePathLockFor(targetFile)
	mu2 := FilePathLockFor(linkFile)
	require.Same(t, mu1, mu2)
}
