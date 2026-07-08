package tools

import (
	"path/filepath"
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

// filePathLockFor returns the mutex for the given absolute file path,
// allocating one on first use. The path is cleaned so that "foo.txt" and
// "./foo.txt" resolve to the same lock.
func filePathLockFor(absPath string) *sync.Mutex {
	cleaned := filepath.Clean(absPath)
	filePathLocksMu.Lock()
	defer filePathLocksMu.Unlock()
	mu, ok := filePathLocks[cleaned]
	if !ok {
		mu = &sync.Mutex{}
		filePathLocks[cleaned] = mu
	}
	return mu
}
