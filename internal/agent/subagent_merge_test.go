package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// setupGitRepo creates a temporary git repo with an initial commit and
// returns its path. It skips the test if git is not available.
func setupGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
		)
		require.NoError(t, cmd.Run(), "git %s", strings.Join(args, " "))
	}
	run("init")
	run("config", "user.name", "test")
	run("config", "user.email", "test@test.com")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test\n"), 0o644))
	run("add", "-A")
	run("commit", "-m", "initial")
	return dir
}

// setupWorktree creates a worktree branch from the repo for testing.
func setupWorktree(t *testing.T, repo, branchName string) string {
	t.Helper()
	worktreeDir := filepath.Join(t.TempDir(), branchName)
	cmd := exec.Command("git", "-C", repo, "worktree", "add", "-B", branchName, worktreeDir, "HEAD")
	require.NoError(t, cmd.Run(), "git worktree add")
	return worktreeDir
}

func TestMergeBackPatchModeAppliesDiffToParent(t *testing.T) {
	repo := setupGitRepo(t)

	// Create a worktree and make changes there.
	worktreeDir := setupWorktree(t, repo, "crush-agent-test-patch")
	require.NoError(t, os.WriteFile(filepath.Join(worktreeDir, "file.txt"), []byte("subagent change\n"), 0o644))

	// Merge back in patch mode.
	c := &coordinator{}
	result := c.mergeBackWorktree(t.Context(), repo, worktreeDir, MergeBackModePatch)

	require.True(t, result.Success, "message: %s", result.Message)
	require.Equal(t, MergeBackModePatch, result.Mode)

	// The parent repo should now contain the subagent's change.
	data, err := os.ReadFile(filepath.Join(repo, "file.txt"))
	require.NoError(t, err)
	require.Equal(t, "subagent change", strings.TrimRight(string(data), "\r\n"))
}

func TestMergeBackPatchModeNoChanges(t *testing.T) {
	repo := setupGitRepo(t)
	worktreeDir := setupWorktree(t, repo, "crush-agent-test-empty")

	c := &coordinator{}
	result := c.mergeBackWorktree(t.Context(), repo, worktreeDir, MergeBackModePatch)

	require.True(t, result.Success)
	require.Contains(t, result.Message, "no changes")
}

func TestMergeBackBranchModeCherryPicksCommits(t *testing.T) {
	repo := setupGitRepo(t)
	worktreeDir := setupWorktree(t, repo, "crush-agent-test-branch")

	// Make and commit changes in the worktree.
	require.NoError(t, os.WriteFile(filepath.Join(worktreeDir, "a.txt"), []byte("a\n"), 0o644))
	commitInWorktree(t, worktreeDir, "add a")
	require.NoError(t, os.WriteFile(filepath.Join(worktreeDir, "b.txt"), []byte("b\n"), 0o644))
	commitInWorktree(t, worktreeDir, "add b")

	c := &coordinator{}
	result := c.mergeBackWorktree(t.Context(), repo, worktreeDir, MergeBackModeBranch)

	require.True(t, result.Success, "message: %s", result.Message)
	require.Equal(t, MergeBackModeBranch, result.Mode)
	require.Equal(t, 2, result.TotalCommits)
	require.Equal(t, 2, result.LandedCommits)

	// Parent repo should have both files.
	_, err := os.Stat(filepath.Join(repo, "a.txt"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(repo, "b.txt"))
	require.NoError(t, err)
}

func TestMergeBackPreservesParentDirtyState(t *testing.T) {
	repo := setupGitRepo(t)
	worktreeDir := setupWorktree(t, repo, "crush-agent-test-stash")

	// Subagent change.
	require.NoError(t, os.WriteFile(filepath.Join(worktreeDir, "sub.txt"), []byte("sub\n"), 0o644))

	// Parent has uncommitted dirty state that should be preserved.
	require.NoError(t, os.WriteFile(filepath.Join(repo, "parent.txt"), []byte("parent dirty\n"), 0o644))

	c := &coordinator{}
	result := c.mergeBackWorktree(t.Context(), repo, worktreeDir, MergeBackModePatch)

	require.True(t, result.Success, "message: %s", result.Message)

	// Both the subagent's change and the parent's dirty state should be present.
	data, err := os.ReadFile(filepath.Join(repo, "sub.txt"))
	require.NoError(t, err)
	require.Equal(t, "sub", strings.TrimRight(string(data), "\r\n"))
	data, err = os.ReadFile(filepath.Join(repo, "parent.txt"))
	require.NoError(t, err)
	require.Equal(t, "parent dirty", strings.TrimRight(string(data), "\r\n"))
}

func TestRepoLockForReturnsSameMutexForSameRoot(t *testing.T) {
	mu1 := repoLockFor("/some/path")
	mu2 := repoLockFor("/some/path")
	require.Same(t, mu1, mu2)
}

func TestRepoLockForReturnsDifferentMutexForDifferentRoot(t *testing.T) {
	mu1 := repoLockFor("/some/path")
	mu2 := repoLockFor("/other/path")
	require.NotSame(t, mu1, mu2)
}

func TestWorktreeBranchName(t *testing.T) {
	if runtime.GOOS == "windows" {
		require.Equal(t, "crush-agent-abc", worktreeBranchName(`C:\data\worktrees\crush-agent-abc`))
	} else {
		require.Equal(t, "crush-agent-abc", worktreeBranchName("/data/worktrees/crush-agent-abc"))
	}
}

func TestShortHash(t *testing.T) {
	require.Equal(t, "abcdef12", shortHash("abcdef1234567890"))
	require.Equal(t, "short", shortHash("short"))
}

// commitInWorktree commits all changes in the given worktree directory.
func commitInWorktree(t *testing.T, dir, message string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
		)
		require.NoError(t, cmd.Run(), "git %s", strings.Join(args, " "))
	}
	run("add", "-A")
	run("commit", "-m", message)
}
