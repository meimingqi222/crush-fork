package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// MergeBackMode controls how subagent worktree changes are merged back
// into the parent's working tree after a worktree-isolated subagent
// completes.
type MergeBackMode string

const (
	// MergeBackModePatch (default) computes the subagent's full diff
	// (committed + working-tree changes) relative to the branch point and
	// applies it onto the parent's working tree via git apply. Does not
	// preserve subagent commit history.
	MergeBackModePatch MergeBackMode = "patch"
	// MergeBackModeBranch cherry-picks the subagent's commits onto the
	// parent's current branch, preserving commit history and authorship.
	// Aborts on the first conflicting commit and leaves the branch
	// preserved for manual resolution. Falls back to patch mode when the
	// subagent made no commits.
	MergeBackModeBranch MergeBackMode = "branch"
)

// MergeBackResult describes the outcome of a merge-back operation.
type MergeBackResult struct {
	Mode          MergeBackMode
	Success       bool
	Message       string
	ConflictFile  string // first conflicting file, best-effort extraction
	SavedDiffPath string // where the diff was saved on apply failure
	LandedCommits int    // branch mode: commits cherry-picked successfully
	TotalCommits  int    // branch mode: total commits in worktree branch
}

// repoLocksMu guards the repoLocks map.
var repoLocksMu sync.Mutex

// repoLocks holds a per-repo mutex keyed by resolved git root path. This
// serializes merge-back operations so that multiple worktree-isolated
// subagents finishing near-simultaneously in the same batch don't corrupt
// the parent's working tree or each other's merges.
var repoLocks = make(map[string]*sync.Mutex)

// repoLockFor returns the mutex for the given git root, allocating one on
// first use.
func repoLockFor(gitRoot string) *sync.Mutex {
	cleaned := filepath.Clean(gitRoot)
	repoLocksMu.Lock()
	defer repoLocksMu.Unlock()
	mu, ok := repoLocks[cleaned]
	if !ok {
		mu = &sync.Mutex{}
		repoLocks[cleaned] = mu
	}
	return mu
}

// resolveGitRoot returns the git toplevel for the given directory.
func resolveGitRoot(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("resolve git root for %s: %w", dir, err)
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return "", fmt.Errorf("git root is empty for %s", dir)
	}
	return root, nil
}

// worktreeBranchName returns the git branch name for a worktree directory.
// The worktree directory is named after the branch (crush-agent-<slug>).
func worktreeBranchName(worktreeDir string) string {
	return filepath.Base(worktreeDir)
}

// mergeBackWorktree merges changes from a subagent's worktree back into the
// parent's working tree. The merge is serialized by a per-repo lock so
// concurrent subagent completions don't corrupt each other's merges.
//
// Pre-existing dirty state in the parent's working tree is stashed before
// applying and restored after, so merged changes never silently fold into
// or overwrite in-flight user edits.
func (c *coordinator) mergeBackWorktree(ctx context.Context, parentWorkDir, worktreeDir string, mode MergeBackMode) MergeBackResult {
	result := MergeBackResult{Mode: mode}

	gitRoot, err := resolveGitRoot(parentWorkDir)
	if err != nil {
		result.Message = fmt.Sprintf("merge-back failed: %v", err)
		return result
	}

	mu := repoLockFor(gitRoot)
	mu.Lock()
	defer mu.Unlock()

	branch := worktreeBranchName(worktreeDir)

	// Find the branch point — the merge-base of the worktree branch and
	// the parent's current HEAD. This is the commit the worktree was
	// created from.
	branchPoint, err := gitMergeBase(gitRoot, branch, "HEAD")
	if err != nil {
		result.Message = fmt.Sprintf("merge-back failed to find branch point: %v", err)
		return result
	}
	if branchPoint == "" {
		result.Message = "merge-back failed: branch point is empty"
		return result
	}

	// Stash any pre-existing dirty state in the parent's working tree so
	// the apply/cherry-pick operates on a clean tree and merged changes
	// never silently fold into or overwrite in-flight user edits.
	stashed, stashErr := gitStashPush(gitRoot)
	if stashErr != nil {
		result.Message = fmt.Sprintf("merge-back failed to stash parent state: %v", stashErr)
		return result
	}
	defer func() {
		if !stashed {
			return
		}
		if popErr := gitStashPop(gitRoot); popErr != nil {
			// Don't override a more specific failure message.
			if result.Success {
				result.Success = false
				result.Message = fmt.Sprintf("merge-back applied but failed to restore stashed parent state: %v", popErr)
			} else {
				slog.Error("merge-back: failed to restore stashed parent state", "error", popErr)
			}
		}
	}()

	switch mode {
	case MergeBackModeBranch:
		result = c.mergeBackBranch(ctx, gitRoot, worktreeDir, branch, branchPoint)
	default:
		result = c.mergeBackPatch(ctx, gitRoot, worktreeDir, branchPoint)
	}
	return result
}

// mergeBackPatch computes the subagent's diff relative to the branch point
// and applies it onto the parent's working tree.
func (c *coordinator) mergeBackPatch(_ context.Context, gitRoot, worktreeDir, branchPoint string) MergeBackResult {
	result := MergeBackResult{Mode: MergeBackModePatch}

	diff, err := gitDiffInWorktree(worktreeDir, branchPoint)
	if err != nil {
		result.Message = fmt.Sprintf("patch merge-back: failed to compute diff: %v", err)
		return result
	}
	if strings.TrimSpace(diff) == "" {
		result.Success = true
		result.Message = "no changes to merge"
		return result
	}

	if applyErr := gitApply(gitRoot, diff); applyErr != nil {
		// Apply failed — save the diff to artifacts and report the
		// conflict so the parent can resolve manually.
		conflictFile := extractConflictFile(applyErr)
		savedPath, saveErr := c.saveFailedDiff(worktreeDir, diff, "patch-merge-failed.diff")
		if saveErr != nil {
			result.Message = fmt.Sprintf("patch merge-back: apply failed (%v) and could not save diff: %v", applyErr, saveErr)
		} else {
			result.SavedDiffPath = savedPath
			result.ConflictFile = conflictFile
			result.Message = fmt.Sprintf("subagent changes could not be auto-applied due to a conflict on %q; diff saved at %s for manual resolution", conflictFile, savedPath)
		}
		return result
	}

	result.Success = true
	result.Message = "patch merge-back applied successfully"
	return result
}

// mergeBackBranch cherry-picks the subagent's commits onto the parent's
// current branch, preserving commit history. Falls back to patch mode when
// the subagent made no commits.
func (c *coordinator) mergeBackBranch(ctx context.Context, gitRoot, worktreeDir, branch, branchPoint string) MergeBackResult {
	result := MergeBackResult{Mode: MergeBackModeBranch}

	commits, err := gitLogRange(gitRoot, branchPoint, branch)
	if err != nil {
		result.Message = fmt.Sprintf("branch merge-back: failed to list commits: %v", err)
		return result
	}
	result.TotalCommits = len(commits)

	if len(commits) == 0 {
		// No commits — fall back to patch mode for working-tree changes.
		patchResult := c.mergeBackPatch(ctx, gitRoot, worktreeDir, branchPoint)
		patchResult.Mode = MergeBackModeBranch
		return patchResult
	}

	// Cherry-pick commits in oldest-first order. Abort on first conflict.
	landed := 0
	for _, commit := range commits {
		if cpErr := gitCherryPick(gitRoot, commit); cpErr != nil {
			conflictFile := extractConflictFile(cpErr)
			_ = gitCherryPickAbort(gitRoot)
			result.LandedCommits = landed
			result.ConflictFile = conflictFile
			result.Message = fmt.Sprintf("branch merge-back: landed %d/%d commits, aborted on %s due to conflict on %q; branch %s preserved for manual resolution", landed, len(commits), shortHash(commit), conflictFile, branch)
			return result
		}
		landed++
	}
	result.Success = true
	result.LandedCommits = landed
	result.Message = fmt.Sprintf("branch merge-back: cherry-picked %d commits successfully", landed)
	return result
}

// gitMergeBase returns the merge-base of two commits in the given repo.
func gitMergeBase(repo, a, b string) (string, error) {
	out, err := exec.Command("git", "-C", repo, "merge-base", a, b).Output()
	if err != nil {
		return "", fmt.Errorf("git merge-base %s %s: %w", a, b, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitStashPush stashes any uncommitted changes in the repo. Returns true if
// a stash entry was created, false if there was nothing to stash.
func gitStashPush(repo string) (bool, error) {
	statusOut, err := exec.Command("git", "-C", repo, "status", "--porcelain").Output()
	if err != nil {
		return false, fmt.Errorf("check stash status: %w", err)
	}
	if len(strings.TrimSpace(string(statusOut))) == 0 {
		return false, nil
	}
	out, err := exec.Command("git", "-C", repo, "stash", "push", "--include-untracked", "-m", "crush-merge-back-stash").CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("git stash push: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return true, nil
}

// gitStashPop restores the most recent stash entry.
func gitStashPop(repo string) error {
	out, err := exec.Command("git", "-C", repo, "stash", "pop").CombinedOutput()
	if err != nil {
		return fmt.Errorf("git stash pop: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// gitDiffInWorktree returns the full diff of the worktree (committed +
// staged + untracked changes) relative to the given branch-point commit.
// It stages all changes first so that newly-created files appear in the
// diff — `git diff` alone skips untracked files. The worktree's index
// state is irrelevant post-merge (the worktree is removed on success or
// preserved for manual resolution on failure).
func gitDiffInWorktree(worktreeDir, branchPoint string) (string, error) {
	addOut, addErr := exec.Command("git", "-C", worktreeDir, "add", "-A").CombinedOutput()
	if addErr != nil {
		return "", fmt.Errorf("git add -A: %w: %s", addErr, strings.TrimSpace(string(addOut)))
	}
	out, err := exec.Command("git", "-C", worktreeDir, "diff", "--cached", branchPoint).Output()
	if err != nil {
		return "", fmt.Errorf("git diff --cached %s: %w", branchPoint, err)
	}
	return string(out), nil
}

// gitApply applies a diff onto the repo's working tree.
func gitApply(repo, diff string) error {
	cmd := exec.Command("git", "-C", repo, "apply")
	cmd.Stdin = strings.NewReader(diff)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git apply: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// gitLogRange returns the list of commit hashes in the range from..to,
// oldest-first (for cherry-pick ordering).
func gitLogRange(repo, from, to string) ([]string, error) {
	out, err := exec.Command("git", "-C", repo, "log", "--reverse", "--format=%H", fmt.Sprintf("%s..%s", from, to)).Output()
	if err != nil {
		return nil, fmt.Errorf("git log %s..%s: %w", from, to, err)
	}
	var commits []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if h := strings.TrimSpace(line); h != "" {
			commits = append(commits, h)
		}
	}
	return commits, nil
}

// gitCherryPick cherry-picks a single commit onto the current branch.
func gitCherryPick(repo, commit string) error {
	out, err := exec.Command("git", "-C", repo, "cherry-pick", commit).CombinedOutput()
	if err != nil {
		return fmt.Errorf("cherry-pick %s: %w: %s", shortHash(commit), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// gitCherryPickAbort aborts an in-progress cherry-pick.
func gitCherryPickAbort(repo string) error {
	out, err := exec.Command("git", "-C", repo, "cherry-pick", "--abort").CombinedOutput()
	if err != nil {
		return fmt.Errorf("cherry-pick --abort: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// extractConflictFile best-effort parses the conflicting file path from a
// git apply or cherry-pick error message. Returns empty string if not found.
func extractConflictFile(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	// git apply errors often contain: "patch failed: <file>" or
	// "error: <file>: does not exist in index".
	for _, marker := range []string{"patch failed: ", "does not match index: ", "conflict in "} {
		if idx := strings.Index(msg, marker); idx >= 0 {
			rest := msg[idx+len(marker):]
			// Take up to first newline/space.
			for i, r := range rest {
				if r == '\n' || r == ' ' {
					return rest[:i]
				}
			}
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// shortHash returns the first 8 chars of a git hash for logging.
func shortHash(hash string) string {
	if len(hash) >= 8 {
		return hash[:8]
	}
	return hash
}

// saveFailedDiff writes a failed diff to the project's artifacts directory
// under <project-data-dir>/worktrees/merge-failures/ and returns the path.
func (c *coordinator) saveFailedDiff(worktreeDir, diff, filename string) (string, error) {
	gitRoot, err := resolveGitRoot(worktreeDir)
	if err != nil {
		return "", err
	}
	artifactsDir := filepath.Join(c.subagentWorktreeRoot(gitRoot), "merge-failures")
	if err := os.MkdirAll(artifactsDir, 0o755); err != nil {
		return "", fmt.Errorf("create artifacts dir: %w", err)
	}
	ts := time.Now().Format("20060102-150405")
	path := filepath.Join(artifactsDir, fmt.Sprintf("%s-%s", ts, filename))
	if err := os.WriteFile(path, []byte(diff), 0o644); err != nil {
		return "", fmt.Errorf("write diff: %w", err)
	}
	return path, nil
}
