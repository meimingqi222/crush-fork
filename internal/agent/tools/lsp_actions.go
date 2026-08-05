package tools

import (
	"context"
	_ "embed"
	"fmt"
	"sort"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/fsext"
	"github.com/charmbracelet/crush/internal/lsp"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
)

func lspClientForFile(ctx context.Context, lspManager *lsp.Manager, filePath string) (*lsp.Client, string, fantasy.ToolResponse, bool) {
	if strings.TrimSpace(filePath) == "" {
		return nil, "", fantasy.NewTextErrorResponse("file_path is required"), false
	}
	if lspManager == nil || lspManager.Clients().Len() == 0 {
		return nil, "", fantasy.NewTextErrorResponse("no LSP clients available"), false
	}

	absPath := ResolveToolPath(ctx, "", filePath).AbsolutePath
	openInLSPs(ctx, lspManager, absPath)
	client := firstHandlingClient(lspManager, absPath)
	if client == nil {
		return nil, absPath, fantasy.NewTextResponse(fmt.Sprintf("No LSP client handles %s", absPath)), false
	}
	return client, absPath, fantasy.ToolResponse{}, true
}

func requestLSPWritePermission(ctx context.Context, permissions permission.Service, workingDir string, call fantasy.ToolCall, filePath, toolName, description string, params any) (*fantasy.ToolResponse, error) {
	if permissions == nil {
		return nil, nil
	}
	sessionID := GetSessionFromContext(ctx)
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}
	effectiveWorkingDir := EffectiveWorkingDir(ctx, workingDir)
	permissionResponse, err := RequestPermission(ctx, permissions,
		permission.CreatePermissionRequest{
			SessionID:   sessionID,
			Path:        fsext.PathOrPrefix(filePath, effectiveWorkingDir),
			ToolCallID:  call.ID,
			ToolName:    toolName,
			Action:      "write",
			Description: description,
			Params:      params,
		},
	)
	if err != nil {
		return nil, err
	}
	return permissionResponse, nil
}

func formatCodeActions(actions []protocol.CodeAction) string {
	var output strings.Builder
	fmt.Fprintf(&output, "Found %d code action(s):\n\n", len(actions))
	for index, action := range actions {
		parts := make([]string, 0, 4)
		parts = append(parts, fmt.Sprintf("%d.", index+1), strings.TrimSpace(action.Title))
		if strings.TrimSpace(string(action.Kind)) != "" {
			parts = append(parts, fmt.Sprintf("[%s]", action.Kind))
		}
		if action.Edit != nil {
			parts = append(parts, "(edit)")
		}
		if action.Command != nil {
			parts = append(parts, "(command)")
		}
		if action.Disabled != nil && strings.TrimSpace(action.Disabled.Reason) != "" {
			parts = append(parts, fmt.Sprintf("(disabled: %s)", strings.TrimSpace(action.Disabled.Reason)))
		}
		output.WriteString(strings.Join(parts, " "))
		output.WriteByte('\n')
	}
	return strings.TrimSpace(output.String())
}

func workspaceEditEmpty(edit protocol.WorkspaceEdit) bool {
	return len(edit.Changes) == 0 && len(edit.DocumentChanges) == 0
}

func notifyWorkspaceEditPaths(ctx context.Context, lspManager *lsp.Manager, edit protocol.WorkspaceEdit) {
	paths := workspaceEditPaths(edit)
	for _, path := range paths {
		notifyLSPs(ctx, lspManager, path)
	}
}

func workspaceEditPaths(edit protocol.WorkspaceEdit) []string {
	seen := make(map[string]struct{})
	paths := make([]string, 0, len(edit.Changes)+len(edit.DocumentChanges))
	appendPath := func(path string) {
		path = normalizeWorkspaceEditPath(path)
		if path == "" {
			return
		}
		if _, exists := seen[path]; exists {
			return
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}

	for uri := range edit.Changes {
		path, err := uri.Path()
		if err == nil {
			appendPath(path)
		}
	}

	for _, change := range edit.DocumentChanges {
		if change.TextDocumentEdit != nil {
			path, err := change.TextDocumentEdit.TextDocument.URI.Path()
			if err == nil {
				appendPath(path)
			}
		}
		if change.CreateFile != nil {
			path, err := change.CreateFile.URI.Path()
			if err == nil {
				appendPath(path)
			}
		}
		if change.DeleteFile != nil {
			path, err := change.DeleteFile.URI.Path()
			if err == nil {
				appendPath(path)
			}
		}
		if change.RenameFile != nil {
			oldPath, oldErr := change.RenameFile.OldURI.Path()
			if oldErr == nil {
				appendPath(oldPath)
			}
			newPath, newErr := change.RenameFile.NewURI.Path()
			if newErr == nil {
				appendPath(newPath)
			}
		}
	}

	sort.Strings(paths)
	return paths
}

func normalizeWorkspaceEditPath(path string) string {
	path = strings.TrimSpace(path)
	if len(path) >= 3 && path[0] == '\\' && path[2] == ':' {
		return path[1:]
	}
	return path
}
