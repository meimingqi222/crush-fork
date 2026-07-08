package tools

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
)

// filePathLocksMu guards the filePathLocks map.
var filePathLocksMu sync.Mutex

// filePathLocks holds a per-path mutex for every file path that has been
// written or edited. Serializing concurrent writers to the same path
// removes the undefined-interleaving hazard between concurrently running
// "session"-isolated subagents (or a subagent racing the interactive main
// session) that happen to touch the same file.
//
// This is a stopgap (P1.a): it does not solve the semantic merge problem
// — one writer's edit can still be clobbered by another's based on stale
// old-content — but it gives concurrent writers a well-defined sequential
// order instead of a data race. Worktree isolation (P1.b) is the real
// fix; this lock remains useful afterward for "session"-isolated writers,
// which stay the default for single-task calls.
var filePathLocks = make(map[string]*sync.Mutex)

const lockPathMaxDepth = 64

// resolveLockPath resolves symlinks in an absolute path without requiring the
// final component to exist. Non-existent components are appended to the
// resolved parent directory. A recursion limit guards against symlink loops.
func resolveLockPath(path string, visited map[string]int, depth int) string {
	if depth > lockPathMaxDepth {
		return path
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	abs = filepath.Clean(abs)
	if visited == nil {
		visited = make(map[string]int)
	}
	if visited[abs] > 0 {
		return path
	}
	visited[abs]++

	info, err := os.Lstat(abs)
	if err != nil {
		if !os.IsNotExist(err) {
			return abs
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return abs
		}
		resolvedParent := resolveLockPath(parent, visited, depth+1)
		return filepath.Join(resolvedParent, filepath.Base(abs))
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return abs
	}
	target, err := os.Readlink(abs)
	if err != nil {
		return abs
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(abs), target)
	}
	return resolveLockPath(target, visited, depth+1)
}

// CanonicalLockKey returns the canonical key used for a file or repository
// path lock. It resolves symlinks, cleans the path, and lowercases the result
// on filesystems that are case-insensitive by default (Windows and macOS) so
// that paths differing only in case or by symlink resolve to the same mutex.
func CanonicalLockKey(absPath string) string {
	key := resolveLockPath(absPath, nil, 0)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		key = strings.ToLower(key)
	}
	return key
}

// FilePathLockFor returns the mutex for the given absolute file path,
// allocating one on first use. The path is cleaned so that "foo.txt" and
// "./foo.txt" resolve to the same lock.
func FilePathLockFor(absPath string) *sync.Mutex {
	key := CanonicalLockKey(absPath)
	filePathLocksMu.Lock()
	defer filePathLocksMu.Unlock()
	mu, ok := filePathLocks[key]
	if !ok {
		mu = &sync.Mutex{}
		filePathLocks[key] = mu
	}
	return mu
}

// LockFilePaths acquires the file-path mutex for every path in absPaths,
// avoiding deadlocks by locking in a consistent sorted order. It returns a
// function that unlocks the acquired mutexes in reverse order.
func LockFilePaths(absPaths []string) func() {
	keys := make([]string, 0, len(absPaths))
	muByKey := make(map[string]*sync.Mutex, len(absPaths))
	seen := make(map[string]struct{}, len(absPaths))
	for _, p := range absPaths {
		key := CanonicalLockKey(p)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		muByKey[key] = FilePathLockFor(p)
		keys = append(keys, key)
	}

	sort.Strings(keys)
	mus := make([]*sync.Mutex, 0, len(keys))
	for _, key := range keys {
		mu := muByKey[key]
		mu.Lock()
		mus = append(mus, mu)
	}

	return func() {
		for i := len(mus) - 1; i >= 0; i-- {
			mus[i].Unlock()
		}
	}
}
