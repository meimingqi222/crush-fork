package tools

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSubagentInheritsParentSessionCwd verifies that the session working
// directory set in the parent context is inherited by tools when called
// from a subagent context (i.e., the context is derived from the parent).
func TestSubagentInheritsParentSessionCwd(t *testing.T) {
	t.Parallel()

	parentWorkingDir := filepath.Join(string(filepath.Separator), "workspace", "parent")
	subagentWorkingDir := filepath.Join(string(filepath.Separator), "workspace", "subagent")

	// Parent context has its session cwd.
	parentCtx := context.WithValue(context.Background(), WorkingDirContextKey, parentWorkingDir)

	// Subagent derives a new context from parent (as happens in real subagent runs).
	subagentCtx := context.WithValue(parentCtx, SessionIDContextKey, "subagent-session")

	// Tools should use the parent's session cwd, not the subagent's tool-level fallback.
	resolved := ResolveToolPath(subagentCtx, subagentWorkingDir, "file.go")
	require.Equal(t, parentWorkingDir, resolved.WorkingDir,
		"subagent should inherit parent session cwd")
	require.Equal(t, filepath.Join(parentWorkingDir, "file.go"), resolved.AbsolutePath)
	require.Equal(t, "file.go", resolved.DisplayPath,
		"subagent should display path relative to parent session cwd")
}

// TestSubagentSessionCwdDoesNotLeakToParent verifies that when a subagent
// sets a different working directory in its own derived context, it does not
// affect the parent context.
func TestSubagentSessionCwdDoesNotLeakToParent(t *testing.T) {
	t.Parallel()

	parentWorkingDir := filepath.Join(string(filepath.Separator), "workspace", "parent")
	subagentWorkingDir := filepath.Join(string(filepath.Separator), "workspace", "subagent")

	// Parent context.
	parentCtx := context.WithValue(context.Background(), WorkingDirContextKey, parentWorkingDir)

	// Subagent overrides the working directory in its derived context.
	subagentCtx := context.WithValue(parentCtx, WorkingDirContextKey, subagentWorkingDir)

	// Subagent's tools should use the subagent's cwd.
	resolvedSub := ResolveToolPath(subagentCtx, parentWorkingDir, "file.go")
	require.Equal(t, subagentWorkingDir, resolvedSub.WorkingDir,
		"subagent should use its own cwd override")

	// Parent's tools should still use the parent's cwd.
	resolvedParent := ResolveToolPath(parentCtx, subagentWorkingDir, "file.go")
	require.Equal(t, parentWorkingDir, resolvedParent.WorkingDir,
		"parent should still use its own cwd after subagent override")
}

// TestToolPathMetadataIsOutsideSessionForSubagent verifies that
// IsOutsideSession is correctly computed relative to the session cwd
// inherited from the parent context, not the tool's fallback dir.
func TestToolPathMetadataIsOutsideSessionForSubagent(t *testing.T) {
	t.Parallel()

	parentWorkingDir := filepath.Join(string(filepath.Separator), "workspace", "parent")
	toolFallbackDir := filepath.Join(string(filepath.Separator), "workspace", "fallback")

	// Parent context has its session cwd.
	parentCtx := context.WithValue(context.Background(), WorkingDirContextKey, parentWorkingDir)
	// Subagent derives context from parent.
	subagentCtx := context.WithValue(parentCtx, SessionIDContextKey, "subagent-1")

	// A path inside the parent session cwd should not be outside.
	resolved := ResolveToolPath(subagentCtx, toolFallbackDir, "src/main.go")
	require.False(t, resolved.IsOutsideSession,
		"path inside parent session cwd should not be outside for subagent")

	// A path outside the parent session cwd should be outside.
	resolved = ResolveToolPath(subagentCtx, toolFallbackDir, "/tmp/external.go")
	require.True(t, resolved.IsOutsideSession,
		"absolute path outside parent session cwd should be outside for subagent")
}
