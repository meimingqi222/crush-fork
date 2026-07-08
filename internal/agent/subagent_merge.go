package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/crush/internal/agent/tools"
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
)

// MergeBackResult describes the outcome of a merge-back operation.
type MergeBackResult struct {
	Mode          MergeBackMode
	Success       bool
	Message       string
	ConflictFile  string // first conflicting file, best-effort extraction
	SavedDiffPath string // where the diff was saved on apply failure
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
	cleaned := tools.CanonicalLockKey(gitRoot)
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
// Before merging, the parent working tree files that the patch will touch
// are stashed (and restored afterward) so that merged changes never silently
// fold into, or overwrite, in-flight user edits. File-path locks are held for
// every file in the patch so that merge-back does not race concurrent
// Write/Edit/Download operations on the same paths.
func (c *coordinator) mergeBackWorktree(ctx context.Context, parentWorkDir, worktreeDir string) (result MergeBackResult) {
	result = MergeBackResult{Mode: MergeBackModePatch}

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

	// Stage all changes in the worktree so that untracked files appear in
	// the diff and in the file list used for locking.
	if _, addErr := exec.Command("git", "-C", worktreeDir, "add", "-A").CombinedOutput(); addErr != nil {
		result.Message = fmt.Sprintf("merge-back failed to stage worktree changes: %v", addErr)
		return result
	}

	names, err := gitDiffNamesInWorktree(worktreeDir, branchPoint)
	if err != nil {
		result.Message = fmt.Sprintf("merge-back failed to list changed files: %v", err)
		return result
	}

	absPaths := make([]string, 0, len(names))
	for _, name := range names {
		if name == "" {
			continue
		}
		absPaths = append(absPaths, filepath.Join(gitRoot, name))
	}
	if len(absPaths) > 0 {
		unlock := tools.LockFilePaths(absPaths)
		defer unlock()
	}

	// Stash any pre-existing dirty state in the touched parent files so
	// the apply operates on a clean tree and merged changes never silently
	// fold into or overwrite in-flight user edits.
	stashed, stashErr := gitStashPush(gitRoot, names)
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

	result = c.mergeBackPatch(ctx, gitRoot, worktreeDir, branchPoint)
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

// gitMergeBase returns the merge-base of two commits in the given repo.
func gitMergeBase(repo, a, b string) (string, error) {
	out, err := exec.Command("git", "-C", repo, "merge-base", a, b).Output()
	if err != nil {
		return "", fmt.Errorf("git merge-base %s %s: %w", a, b, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitRevParseHead returns the current HEAD commit for the given repo.
func gitRevParseHead(repo string) (string, error) {
	out, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitDiffStat returns a --stat summary of the diff from <from> to <to>.
func gitDiffStat(repo, from, to string) (string, error) {
	out, err := exec.Command("git", "-C", repo, "diff", "--stat", fmt.Sprintf("%s..%s", from, to)).Output()
	if err != nil {
		return "", fmt.Errorf("git diff --stat %s..%s: %w", from, to, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitStashPush stashes changes to the given paths in the repo. Returns true
// if a stash entry was created, false if there was nothing to stash.
func gitStashPush(repo string, paths []string) (bool, error) {
	if len(paths) == 0 {
		return false, nil
	}

	statusArgs := []string{"-C", repo, "status", "--porcelain", "--"}
	statusArgs = append(statusArgs, paths...)
	statusOut, err := exec.Command("git", statusArgs...).Output()
	if err != nil {
		return false, fmt.Errorf("check stash status: %w", err)
	}
	if len(strings.TrimSpace(string(statusOut))) == 0 {
		return false, nil
	}

	stashArgs := []string{"-C", repo, "stash", "push", "--include-untracked", "-m", "crush-merge-back-stash", "--"}
	stashArgs = append(stashArgs, paths...)
	out, err := exec.Command("git", stashArgs...).CombinedOutput()
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

// gitDiffNamesInWorktree returns the names of files changed in the worktree
// relative to the branch-point commit.
func gitDiffNamesInWorktree(worktreeDir, branchPoint string) ([]string, error) {
	out, err := exec.Command("git", "-C", worktreeDir, "diff", "--cached", "--name-only", branchPoint).Output()
	if err != nil {
		return nil, fmt.Errorf("git diff --cached --name-only %s: %w", branchPoint, err)
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		if n := strings.TrimSpace(line); n != "" {
			names = append(names, n)
		}
	}
	return names, nil
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

var conflictFileRegexes = []*regexp.Regexp{
	regexp.MustCompile(`patch failed:\s*([^\s:]+)`),
	regexp.MustCompile(`error:\s*([^\s:]+):\s*(?:does not match index|does not exist in index|patch does not apply|already exists in index|already exists in working directory)`),
	regexp.MustCompile(`(?:Merge conflict|CONFLICT \(content\): Merge conflict) in\s+([^\s]+)`),
	regexp.MustCompile(`conflict in\s+([^\s]+)`),
}

// extractConflictFile best-effort parses the conflicting file path from a
// git apply or cherry-pick error message. Returns empty string if not found.
func extractConflictFile(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	for _, re := range conflictFileRegexes {
		if matches := re.FindStringSubmatch(msg); matches != nil {
			return matches[1]
		}
	}
	return ""
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
