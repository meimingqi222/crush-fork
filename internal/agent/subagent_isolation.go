package agent

import (
	"context"
	"strings"
)

// SubagentIsolationProvider is a reserved abstraction for future sandbox
// providers (e.g. external_sandbox, managed_sandbox). Current worktree
// isolation is implemented directly in coordinator.go via private workspace
// preparation methods. This interface is not yet wired into the coordinator
// execution path.
type SubagentIsolationProvider interface {
	Kind() SubagentIsolationKind
	Available() bool
	Prepare(ctx context.Context, runtime SubagentRuntimeContext) (SubagentIsolation, error)
	Cleanup(ctx context.Context, isolation SubagentIsolation) error
}

type noneIsolationProvider struct{}

func (noneIsolationProvider) Kind() SubagentIsolationKind { return SubagentIsolationNone }
func (noneIsolationProvider) Available() bool             { return true }
func (noneIsolationProvider) Prepare(_ context.Context, runtime SubagentRuntimeContext) (SubagentIsolation, error) {
	return SubagentIsolation{
		Kind:      SubagentIsolationNone,
		Available: true,
		Path:      strings.TrimSpace(runtime.Workspace.Root),
	}, nil
}
func (noneIsolationProvider) Cleanup(context.Context, SubagentIsolation) error { return nil }

type worktreeIsolationProvider struct{}

func (worktreeIsolationProvider) Kind() SubagentIsolationKind { return SubagentIsolationWorktree }
func (worktreeIsolationProvider) Available() bool             { return true }
func (worktreeIsolationProvider) Prepare(_ context.Context, runtime SubagentRuntimeContext) (SubagentIsolation, error) {
	return SubagentIsolation{
		Kind:      SubagentIsolationWorktree,
		Available: true,
		Path:      strings.TrimSpace(runtime.Workspace.Root),
	}, nil
}
func (worktreeIsolationProvider) Cleanup(context.Context, SubagentIsolation) error { return nil }
