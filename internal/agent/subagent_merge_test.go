package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/message"
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
	result := c.mergeBackWorktree(t.Context(), repo, worktreeDir)

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
	result := c.mergeBackWorktree(t.Context(), repo, worktreeDir)

	require.True(t, result.Success)
	require.Contains(t, result.Message, "no changes")
}

func TestMergeBackPreservesParentDirtyState(t *testing.T) {
	repo := setupGitRepo(t)
	worktreeDir := setupWorktree(t, repo, "crush-agent-test-stash")

	// Subagent change.
	require.NoError(t, os.WriteFile(filepath.Join(worktreeDir, "sub.txt"), []byte("sub\n"), 0o644))

	// Parent has uncommitted dirty state that should be preserved.
	require.NoError(t, os.WriteFile(filepath.Join(repo, "parent.txt"), []byte("parent dirty\n"), 0o644))

	c := &coordinator{}
	result := c.mergeBackWorktree(t.Context(), repo, worktreeDir)

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

// TestHasWorktreeChangesDetectsCommittedChange guards against the data-loss
// bug where a subagent worktree with committed changes had a clean working
// tree and was deleted before merge-back could see those commits.
func TestHasWorktreeChangesDetectsCommittedChange(t *testing.T) {
	repo := setupGitRepo(t)
	worktreeDir := setupWorktree(t, repo, "crush-agent-test-committed")

	// Subagent makes a change and commits it inside the worktree.
	require.NoError(t, os.WriteFile(filepath.Join(worktreeDir, "sub.txt"), []byte("sub committed\n"), 0o644))
	runGit(t, worktreeDir, "add", "-A")
	runGit(t, worktreeDir, "commit", "-m", "subagent commit")

	c := &coordinator{}
	hasChanges, err := c.hasWorktreeChanges(repo, worktreeDir)
	require.NoError(t, err)
	require.True(t, hasChanges, "hasWorktreeChanges should detect committed changes ahead of parent HEAD")
}

// TestCleanupWorktreeIfNeededMergesCommittedChanges ensures a subagent that
// commits inside its worktree has those changes merged back into the parent
// and the worktree is cleaned up.
func TestCleanupWorktreeIfNeededMergesCommittedChanges(t *testing.T) {
	repo := setupGitRepo(t)
	worktreeDir := setupWorktree(t, repo, "crush-agent-test-cleanup")

	require.NoError(t, os.WriteFile(filepath.Join(worktreeDir, "sub.txt"), []byte("sub committed\n"), 0o644))
	runGit(t, worktreeDir, "add", "-A")
	runGit(t, worktreeDir, "commit", "-m", "subagent commit")

	c := &coordinator{}
	result := c.cleanupWorktreeIfNeeded(t.Context(), repo, worktreeDir)
	require.True(t, result.Success, "cleanup merge-back should succeed: %s", result.Message)

	data, err := os.ReadFile(filepath.Join(repo, "sub.txt"))
	require.NoError(t, err)
	require.Equal(t, "sub committed", strings.TrimRight(string(data), "\r\n"))

	_, err = os.Stat(worktreeDir)
	require.True(t, os.IsNotExist(err), "worktree should be removed after successful merge-back")
}

// TestApplyMergeBackResultMarksResponseAsErrorOnFailure ensures the parent
// model sees a tool error when merge-back fails, instead of a silent success.
func TestApplyMergeBackResultMarksResponseAsErrorOnFailure(t *testing.T) {
	c := &coordinator{}
	resp := fantasy.NewTextResponse("subagent succeeded")
	var runErr error
	mergeResult := MergeBackResult{
		Success: false,
		Message: "merge conflict in main.go",
	}
	params := subAgentParams{
		ToolCallID:      "tc-1",
		ParentMessageID: "msg-1",
	}

	c.applyMergeBackResult(&resp, &runErr, mergeResult, params, "child-1")

	require.True(t, resp.IsError, "merge-back failure should turn the response into an error")
	require.Contains(t, resp.Content, "merge conflict in main.go")
	require.Contains(t, resp.Content, "merge_back")
	parsed, ok := message.ParseToolResultSubtaskResult(resp.Metadata)
	require.True(t, ok)
	require.Equal(t, message.ToolResultSubtaskStatusFailed, parsed.Status)
}

// TestRepoLockForCanonicalizesCaseAndSymlinks guards against isolation
// bypasses where the same repository accessed through different case or a
// symlink gets a different repo lock.
func TestRepoLockForCanonicalizesCaseAndSymlinks(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "repo")
	link := filepath.Join(dir, "link")
	require.NoError(t, os.MkdirAll(target, 0o755))

	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlinks on this filesystem: %v", err)
	}

	mu1 := repoLockFor(target)
	mu2 := repoLockFor(link)
	require.Same(t, mu1, mu2, "repo lock must be the same for symlink-equivalent paths")

	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		mu3 := repoLockFor(filepath.Join(dir, "REPO"))
		require.Same(t, mu1, mu3, "repo lock must be case-insensitive on windows/darwin")
	}
}
