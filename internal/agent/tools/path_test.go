package tools

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFormatToolPath(t *testing.T) {
	t.Parallel()

	workingDir := filepath.Join(string(filepath.Separator), "workspace", "crush")
	require.Equal(t, "fantasy/agent.go", FormatToolPath(filepath.Join(workingDir, "fantasy", "agent.go"), workingDir))
	expectedOutside, err := filepath.Abs(filepath.Join(string(filepath.Separator), "tmp", "agent.go"))
	require.NoError(t, err)
	require.Equal(t, filepath.ToSlash(expectedOutside), FormatToolPath(filepath.Join(string(filepath.Separator), "tmp", "agent.go"), workingDir))
}

func TestResolveToolPathUsesSessionWorkingDir(t *testing.T) {
	t.Parallel()

	globalDir := filepath.Join(string(filepath.Separator), "workspace", "global")
	sessionDir := filepath.Join(string(filepath.Separator), "workspace", "session")
	ctx := context.WithValue(context.Background(), WorkingDirContextKey, sessionDir)

	resolved := ResolveToolPath(ctx, globalDir, "fantasy/agent.go")
	require.Equal(t, sessionDir, resolved.WorkingDir)
	require.Equal(t, filepath.Join(sessionDir, "fantasy", "agent.go"), resolved.AbsolutePath)
	require.Equal(t, "fantasy/agent.go", resolved.DisplayPath)
}

func TestNewCommandToolPathMetadataSeparatesWorkingDirectories(t *testing.T) {
	t.Parallel()

	sessionDir := filepath.Join(string(filepath.Separator), "workspace", "crush")
	commandDir := filepath.Join(sessionDir, "fantasy")
	metadata := NewCommandToolPathMetadata(sessionDir, commandDir, "fantasy")

	require.Equal(t, sessionDir, metadata.WorkingDirectory)
	require.Equal(t, commandDir, metadata.CommandWorkingDirectory)
	require.Equal(t, commandDir, metadata.ResolvedPath)
	require.Equal(t, "fantasy", metadata.DisplayPath)
}

func TestDuplicateWorkingDirPrefixHintDoesNotRewritePath(t *testing.T) {
	t.Parallel()

	workingDir := filepath.Join(string(filepath.Separator), "workspace", "crush")
	input := "crush/fantasy/agent.go"
	resolved := ResolveToolPath(context.Background(), workingDir, input)

	require.Equal(t, filepath.Join(workingDir, input), resolved.AbsolutePath)
	require.Contains(t, DuplicateWorkingDirPrefixHint(input, workingDir), "try \"fantasy/agent.go\"")
	require.Equal(t, "", DuplicateWorkingDirPrefixHint("fantasy/agent.go", workingDir))
}

func TestIsOutsideSession(t *testing.T) {
	t.Parallel()

	workingDir := filepath.Join(string(filepath.Separator), "workspace", "crush")

	// Path inside session cwd.
	resolved := ResolveToolPath(context.Background(), workingDir, "fantasy/agent.go")
	require.False(t, resolved.IsOutsideSession, "path inside session cwd should not be outside")

	// Path outside session cwd (absolute path to /tmp).
	resolved = ResolveToolPath(context.Background(), workingDir, "/tmp/agent.go")
	require.True(t, resolved.IsOutsideSession, "absolute path outside session cwd should be outside")

	// Path to the session cwd itself.
	resolved = ResolveToolPath(context.Background(), workingDir, ".")
	require.False(t, resolved.IsOutsideSession, "session cwd itself is not outside")
}

func TestResolveToolPathPreservesSessionCwd(t *testing.T) {
	t.Parallel()

	globalDir := filepath.Join(string(filepath.Separator), "workspace", "global")
	sessionDir := filepath.Join(string(filepath.Separator), "workspace", "session", "sub")
	ctx := context.WithValue(context.Background(), WorkingDirContextKey, sessionDir)

	// Even when the fallback is a different directory, the session cwd wins.
	resolved := ResolveToolPath(ctx, globalDir, "agent.go")
	require.Equal(t, sessionDir, resolved.WorkingDir)
	require.Equal(t, filepath.Join(sessionDir, "agent.go"), resolved.AbsolutePath)
	require.False(t, resolved.IsOutsideSession)
}
