package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// TestCrossToolGlobToReadToEdit verifies that a path returned by glob can be
// directly passed to read, and a path from read can be directly passed to edit.
// This is the core cross-tool path composability contract.
func TestCrossToolGlobToReadToEdit(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	// Create a nested file structure that mirrors a real repo.
	filePath := filepath.Join(root, "src", "main.go")
	require.NoError(t, os.MkdirAll(filepath.Dir(filePath), 0o755))
	require.NoError(t, os.WriteFile(filePath, []byte("package main\n\nfunc main() {}\n"), 0o644))

	// --- Step 1: glob returns a path ---
	globTool := NewGlobTool(root)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	globInput, err := json.Marshal(GlobParams{Path: "src/**/*.go"})
	require.NoError(t, err)
	globResp, err := globTool.Run(ctx, fantasy.ToolCall{
		ID:    "glob-1",
		Name:  GlobToolName,
		Input: string(globInput),
	})
	require.NoError(t, err)
	require.False(t, globResp.IsError)

	// The glob output should contain a path relative to the working directory.
	globOutput := globResp.Content
	require.Contains(t, globOutput, "src/main.go")

	// Extract the path from glob output (it's one path per line).
	globPath := "src/main.go" // glob returns paths relative to session cwd

	// --- Step 2: read the file using the glob path directly ---
	readTool := NewReadTool(nil, nil, &readToolFileTracker{}, root, config.ToolLs{}, nil, nil)
	readInput, err := json.Marshal(ReadParams{Path: globPath})
	require.NoError(t, err)
	readResp, err := readTool.Run(ctx, fantasy.ToolCall{
		ID:    "read-1",
		Name:  ReadToolName,
		Input: string(readInput),
	})
	require.NoError(t, err)
	require.False(t, readResp.IsError)
	require.Contains(t, readResp.Content, "package main")

	// Verify read metadata has the expected display path.
	var readMeta ReadResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(readResp.Metadata), &readMeta))
	require.Equal(t, globPath, readMeta.DisplayPath)

	// --- Step 3: edit the file using the same path ---
	permissions := &mockWritePermissionService{
		Broker: pubsub.NewBroker[permission.PermissionRequest](), granted: true,
	}
	historyService := &mockHistoryService{Broker: pubsub.NewBroker[history.File]()}
	editTool := NewEditTool(nil, permissions, historyService, &mockFileTracker{}, root)
	editCtx := newNonPlanModeContext("test-session")
	editInput, err := json.Marshal(EditParams{
		FilePath:  globPath,
		OldString: "func main() {}",
		NewString: "func main() { fmt.Println(\"hello\") }",
	})
	require.NoError(t, err)
	editResp, err := editTool.Run(editCtx, fantasy.ToolCall{
		ID:    "edit-1",
		Name:  EditToolName,
		Input: string(editInput),
	})
	require.NoError(t, err)
	require.False(t, editResp.IsError, "edit should succeed, got: %s", editResp.Content)

	// Verify the file was actually edited.
	content, err := os.ReadFile(filePath)
	require.NoError(t, err)
	require.Contains(t, string(content), "fmt.Println")
}

// TestCrossToolGrepToRead verifies that grep results can be directly used
// to read the matched files.
func TestCrossToolGrepToRead(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	file1 := filepath.Join(root, "handler.go")
	file2 := filepath.Join(root, "model.go")
	require.NoError(t, os.WriteFile(file1, []byte("package main\n\nfunc handleRequest() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(file2, []byte("package main\n\ntype Model struct{}\n"), 0o644))

	grepTool := NewGrepTool(root, config.ToolGrep{})
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	grepInput, err := json.Marshal(GrepParams{Pattern: "package main", Path: "."})
	require.NoError(t, err)
	grepResp, err := grepTool.Run(ctx, fantasy.ToolCall{
		ID:    "grep-1",
		Name:  GrepToolName,
		Input: string(grepInput),
	})
	require.NoError(t, err)
	require.False(t, grepResp.IsError)

	// Grep output should contain file paths relative to session cwd.
	require.Contains(t, grepResp.Content, "handler.go")
	require.Contains(t, grepResp.Content, "model.go")

	// Use one of the grep result paths to read the file.
	readTool := NewReadTool(nil, nil, &readToolFileTracker{}, root, config.ToolLs{}, nil, nil)
	readInput, err := json.Marshal(ReadParams{Path: "handler.go"})
	require.NoError(t, err)
	readResp, err := readTool.Run(ctx, fantasy.ToolCall{
		ID:    "read-1",
		Name:  ReadToolName,
		Input: string(readInput),
	})
	require.NoError(t, err)
	require.False(t, readResp.IsError)
	require.Contains(t, readResp.Content, "handleRequest")
}

// TestCrossToolBashSubdirDoesNotPolluteSessionCwd verifies that running bash
// with a different working directory does not change the session cwd for
// subsequent file tool calls.
func TestCrossToolBashSubdirDoesNotPolluteSessionCwd(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	subdir := filepath.Join(root, "subdir")
	require.NoError(t, os.MkdirAll(subdir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(subdir, "child_unique_name_xyz.go"), []byte("package subdir\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "parent_unique_name_abc.go"), []byte("package main\n"), 0o644))

	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	// Run bash with working_dir set to a subdirectory.
	bashTool := newBashToolForTestWithHooksAndOptions(root, nil)

	bashInput, err := json.Marshal(BashParams{
		Command:    "pwd",
		WorkingDir: "subdir",
	})
	require.NoError(t, err)
	bashResp, err := bashTool.Run(ctx, fantasy.ToolCall{
		ID:    "bash-1",
		Name:  BashToolName,
		Input: string(bashInput),
	})
	require.NoError(t, err)
	require.False(t, bashResp.IsError)
	// Bash should execute in the subdirectory.
	require.Contains(t, bashResp.Content, "subdir")

	// Now read a file using a relative path - it should resolve from the
	// session cwd (root), NOT from the bash command cwd (subdir).
	readTool := NewReadTool(nil, nil, &readToolFileTracker{}, root, config.ToolLs{}, nil, nil)
	readInput, err := json.Marshal(ReadParams{Path: "parent_unique_name_abc.go"})
	require.NoError(t, err)
	readResp, err := readTool.Run(ctx, fantasy.ToolCall{
		ID:    "read-2",
		Name:  ReadToolName,
		Input: string(readInput),
	})
	require.NoError(t, err)
	require.False(t, readResp.IsError, "reading parent.go from session cwd should succeed")
	require.Contains(t, readResp.Content, "package main")

	// Verify the bash response metadata shows a different command working directory
	// than the session cwd, confirming the session cwd was not changed.
	var bashMeta BashResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(bashResp.Metadata), &bashMeta))
	// BashResponseMetadata.WorkingDirectory is the command cwd (execWorkingDir).
	require.Contains(t, bashMeta.WorkingDirectory, "subdir", "bash WorkingDirectory should be the command cwd (subdir)")
	require.Contains(t, bashMeta.CommandWorkingDirectory, "subdir", "command cwd in metadata should be subdir")
}

// TestCrossToolSessionCwdOverrideFallback verifies that when a session cwd
// is set in context, all tools use it instead of the tool's fallback working dir.
func TestCrossToolSessionCwdOverrideFallback(t *testing.T) {
	t.Parallel()

	fallbackDir := t.TempDir()
	sessionDir := t.TempDir()

	// Create a file only in sessionDir.
	filePath := filepath.Join(sessionDir, "exclusive.go")
	require.NoError(t, os.WriteFile(filePath, []byte("package exclusive\n"), 0o644))

	// Create a different file in fallbackDir.
	require.NoError(t, os.WriteFile(filepath.Join(fallbackDir, "fallback.go"), []byte("package fallback\n"), 0o644))

	// Set session cwd in context.
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	ctx = context.WithValue(ctx, WorkingDirContextKey, sessionDir)

	// Glob should search in sessionDir, not fallbackDir.
	globTool := NewGlobTool(fallbackDir)
	globInput, err := json.Marshal(GlobParams{Path: "*.go"})
	require.NoError(t, err)
	globResp, err := globTool.Run(ctx, fantasy.ToolCall{
		ID:    "glob-1",
		Name:  GlobToolName,
		Input: string(globInput),
	})
	require.NoError(t, err)
	require.False(t, globResp.IsError)
	require.Contains(t, globResp.Content, "exclusive.go")
	require.NotContains(t, globResp.Content, "fallback.go")

	// Read should find the file in sessionDir.
	readTool := NewReadTool(nil, nil, &readToolFileTracker{}, fallbackDir, config.ToolLs{}, nil, nil)
	readInput, err := json.Marshal(ReadParams{Path: "exclusive.go"})
	require.NoError(t, err)
	readResp, err := readTool.Run(ctx, fantasy.ToolCall{
		ID:    "read-1",
		Name:  ReadToolName,
		Input: string(readInput),
	})
	require.NoError(t, err)
	require.False(t, readResp.IsError)
	require.Contains(t, readResp.Content, "package exclusive")
}

// TestCrossToolAbsolutePathStaysAbsolute verifies that absolute paths from
// read results can be passed to edit even when they're outside session cwd.
func TestCrossToolAbsolutePathStaysAbsolute(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	externalDir := t.TempDir()
	externalFile := filepath.Join(externalDir, "external.go")
	require.NoError(t, os.WriteFile(externalFile, []byte("package external\n\nvar X = 42\n"), 0o644))

	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	// Read using an absolute path outside the session cwd.
	permSvc := &mockWritePermissionService{
		Broker: pubsub.NewBroker[permission.PermissionRequest](), granted: true,
	}
	readTool := NewReadTool(nil, permSvc, &readToolFileTracker{}, root, config.ToolLs{}, nil, nil)
	readInput, err := json.Marshal(ReadParams{Path: externalFile})
	require.NoError(t, err)
	readResp, err := readTool.Run(ctx, fantasy.ToolCall{
		ID:    "read-1",
		Name:  ReadToolName,
		Input: string(readInput),
	})
	require.NoError(t, err)
	require.False(t, readResp.IsError)
	require.Contains(t, readResp.Content, "package external")

	// Verify metadata shows the path is outside session.
	var readMeta ReadResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(readResp.Metadata), &readMeta))
	require.True(t, readMeta.IsOutsideSession, "external file should be marked as outside session")

	// Edit using the same absolute path.
	permissions := &mockWritePermissionService{
		Broker: pubsub.NewBroker[permission.PermissionRequest](), granted: true,
	}
	historyService := &mockHistoryService{Broker: pubsub.NewBroker[history.File]()}
	editTool := NewEditTool(nil, permissions, historyService, &mockFileTracker{}, root)
	editCtx := newNonPlanModeContext("test-session")
	editInput, err := json.Marshal(EditParams{
		FilePath:  externalFile,
		OldString: "var X = 42",
		NewString: "var X = 100",
	})
	require.NoError(t, err)
	editResp, err := editTool.Run(editCtx, fantasy.ToolCall{
		ID:    "edit-1",
		Name:  EditToolName,
		Input: string(editInput),
	})
	require.NoError(t, err)
	require.False(t, editResp.IsError)

	// Verify the file was edited.
	content, err := os.ReadFile(externalFile)
	require.NoError(t, err)
	require.Contains(t, string(content), "var X = 100")
}
