package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/clientfs"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

func TestFileToolsUseUnsavedClientFSAndPreserveRevisionMetadata(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	require.NoError(t, os.WriteFile(path, []byte("durable\n"), 0o600))
	remote := &toolClientFS{
		path: path, content: "unsaved\n", revision: "buffer:4",
		sourceURI: "vscode-notebook-cell:///workspace/main.go",
	}
	scope, err := clientfs.New(clientfs.Config{SessionID: "session-1", Workspace: root, Caller: remote})
	require.NoError(t, err)
	ctx := clientfs.WithScope(newNonPlanModeContext("session-1"), scope)

	readTool := NewReadTool(nil, nil, &readToolFileTracker{}, root, config.ToolLs{}, nil, nil)
	read := runReadToolForTest(t, readTool, ctx, ReadParams{Path: path + ":raw"})
	require.Contains(t, read.Content, "unsaved")
	readMetadata := parseReadMetadata(t, read)
	require.Equal(t, remote.sourceURI, readMetadata.SourceURI)
	require.Equal(t, "buffer:4", readMetadata.Revision)

	permissions := &mockWritePermissionService{
		Broker: pubsub.NewBroker[permission.PermissionRequest](), granted: true,
	}
	historyService := &mockHistoryService{Broker: pubsub.NewBroker[history.File]()}
	writeTool := NewWriteTool(nil, permissions, historyService, &mockFileTracker{}, root)
	written, err := runWriteTool(t, writeTool, ctx, WriteParams{FilePath: path, Content: "agent\n"})
	require.NoError(t, err)
	var writeMetadata WriteResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(written.Metadata), &writeMetadata))
	require.Equal(t, remote.sourceURI, writeMetadata.SourceURI)
	require.Equal(t, "client:5", writeMetadata.Revision)
	require.Equal(t, "agent\n", remote.currentContent())
	local, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "durable\n", string(local), "client FS must not bypass the client by writing disk directly")

	editTool := NewEditTool(nil, permissions, historyService, &mockFileTracker{}, root, 0.92)
	input, err := json.Marshal(EditParams{FilePath: path, OldString: "agent", NewString: "edited"})
	require.NoError(t, err)
	edited, err := editTool.Run(ctx, fantasy.ToolCall{ID: "edit-1", Name: EditToolName, Input: string(input)})
	require.NoError(t, err)
	var editMetadata EditResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(edited.Metadata), &editMetadata))
	require.Equal(t, remote.sourceURI, editMetadata.SourceURI)
	require.Equal(t, "client:6", editMetadata.Revision)
	require.Equal(t, "edited\n", remote.currentContent())
}

func TestEditToolRejectsClientFSRevisionConflict(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	require.NoError(t, os.WriteFile(path, []byte("durable"), 0o600))
	remote := &toolClientFS{
		path: path, content: "unsaved", revision: "buffer:1",
		sourceURI: "file:///workspace/main.go", conflictWrite: true,
	}
	scope, err := clientfs.New(clientfs.Config{SessionID: "s", Workspace: root, Caller: remote})
	require.NoError(t, err)
	ctx := clientfs.WithScope(newNonPlanModeContext("s"), scope)
	permissions := &mockWritePermissionService{
		Broker: pubsub.NewBroker[permission.PermissionRequest](), granted: true,
	}
	tool := NewEditTool(
		nil, permissions, &mockHistoryService{Broker: pubsub.NewBroker[history.File]()},
		&mockFileTracker{}, root, 0.92,
	)
	input, err := json.Marshal(EditParams{FilePath: path, OldString: "unsaved", NewString: "agent"})
	require.NoError(t, err)
	_, err = tool.Run(ctx, fantasy.ToolCall{ID: "edit-1", Name: EditToolName, Input: string(input)})
	require.ErrorIs(t, err, clientfs.ErrRevisionConflict)
	require.Equal(t, "unsaved", remote.currentContent())
}

type toolClientFSError string

func (e toolClientFSError) Error() string        { return string(e) }
func (e toolClientFSError) ClientFSCode() string { return string(e) }

type toolClientFS struct {
	mu            sync.Mutex
	path          string
	content       string
	revision      string
	sourceURI     string
	conflictWrite bool
	writes        int
}

func (c *toolClientFS) Call(_ context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	base := map[string]any{
		"path": c.path, "sourceUri": c.sourceURI, "revision": c.revision,
		"size": len(c.content), "exists": true,
	}
	switch method {
	case "crush/fs/read":
		base["content"] = c.content
		return json.Marshal(base)
	case "crush/fs/stat":
		return json.Marshal(base)
	case "crush/fs/write":
		payload, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		var input struct {
			ExpectedRevision string `json:"expectedRevision"`
			Content          string `json:"content"`
		}
		if json.Unmarshal(payload, &input) != nil {
			return nil, errors.New("invalid write")
		}
		if c.conflictWrite || input.ExpectedRevision != c.revision {
			return nil, toolClientFSError("CRUSH_REVISION_CONFLICT")
		}
		c.content = input.Content
		c.writes++
		c.revision = "client:" + string(rune('5'+c.writes-1))
		base["revision"] = c.revision
		base["size"] = len(c.content)
		return json.Marshal(base)
	default:
		return nil, errors.New("unexpected method")
	}
}

func (c *toolClientFS) currentContent() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.content
}
