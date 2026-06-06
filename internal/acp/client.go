package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/crush/internal/permission"
	"github.com/google/uuid"
)

// RunPermissionBridge subscribes to the permission service and forwards each
// request to the connected ACP client via session/request_permission.
//
// This is the ACP-mode equivalent of the TUI's permission dialog: the TUI
// subscribes to the same pubsub channel and calls Grant/Deny from UI code;
// here we do the same over the JSON-RPC wire.
//
// Must be called in a goroutine; it blocks until ctx is cancelled.
func RunPermissionBridge(ctx context.Context, perms permission.Service, server *Server) {
	ch := perms.Subscribe(ctx)
	for {
		select {
		case event, ok := <-ch:
			if !ok {
				return
			}
			req := event.Payload
			go handlePermissionRequest(ctx, req, perms, server)
		case <-ctx.Done():
			return
		}
	}
}

// GetToolKind maps a generic tool name to the standardized ACP ToolKind.
func GetToolKind(toolName string) ToolKind {
	switch toolName {
	case "read", "view", "fs/read_text_file", "view_file":
		return ToolKindRead
	case "write", "edit", "patch", "fs/write_text_file", "replace_file_content", "multi_replace_file_content", "write_to_file":
		return ToolKindEdit
	case "delete", "fs/delete_file":
		return ToolKindDelete
	case "move", "rename":
		return ToolKindMove
	case "search", "grep", "grep_search", "glob", "locate", "search_context", "codebase_search", "github_codebase_search", "warp_grep", "tool_search":
		return ToolKindSearch
	case "execute", "bash", "run_command", "shell", "execute_command", "run_terminal_command", "bash_exec", "terminal":
		return ToolKindExecute
	case "think", "agent":
		return ToolKindThink
	case "fetch", "web_fetch", "search_web", "download":
		return ToolKindFetch
	default:
		return ToolKindOther
	}
}

// extractParam pulls a string parameter value from any interface (typically struct or map).
func extractParam(params any, keys ...string) string {
	if params == nil {
		return ""
	}

	// Direct map assertion.
	if m, ok := params.(map[string]any); ok {
		for _, key := range keys {
			if val, ok := m[key]; ok {
				if str, ok := val.(string); ok {
					return str
				}
				return fmt.Sprintf("%v", val)
			}
		}
	}

	// fallback via json marshal/unmarshal.
	data, err := json.Marshal(params)
	if err == nil {
		var m map[string]any
		if err := json.Unmarshal(data, &m); err == nil {
			for _, key := range keys {
				if val, ok := m[key]; ok {
					if str, ok := val.(string); ok {
						return str
					}
					return fmt.Sprintf("%v", val)
				}
			}
		}
	}
	return ""
}

// GetBeautifulTitle creates a human-readable title for tool calls to prevent raw JSON displays in IDEs.
func GetBeautifulTitle(toolName, action string, params any) string {
	switch toolName {
	case "read", "view", "fs/read_text_file", "view_file":
		filePath := extractParam(params, "file_path", "filePath", "filepath", "path", "TargetFile", "target_file")
		if filePath != "" {
			base := filepath.Base(filePath)
			if base == "." || base == "/" || base == "\\" || filePath == "" {
				return "List project root"
			}
			hasExt := strings.Contains(base, ".") && !strings.HasPrefix(base, ".")
			if hasExt {
				return fmt.Sprintf("Read file: %s", base)
			}
			return fmt.Sprintf("Read directory: %s", base)
		}
		return "Read file"
	case "write", "edit", "patch", "fs/write_text_file", "replace_file_content", "multi_replace_file_content", "write_to_file":
		filePath := extractParam(params, "file_path", "filePath", "filepath", "path", "TargetFile", "target_file")
		if filePath != "" {
			return fmt.Sprintf("Edit file: %s", filepath.Base(filePath))
		}
		return "Edit file"
	case "delete", "fs/delete_file":
		filePath := extractParam(params, "file_path", "filePath", "filepath", "path", "TargetFile", "target_file")
		if filePath != "" {
			return fmt.Sprintf("Delete file: %s", filepath.Base(filePath))
		}
		return "Delete file"
	case "move", "rename":
		src := extractParam(params, "src", "source", "old_path")
		dest := extractParam(params, "dest", "destination", "new_path")
		if src != "" && dest != "" {
			return fmt.Sprintf("Move file: %s -> %s", filepath.Base(src), filepath.Base(dest))
		}
		return "Move file"
	case "search", "grep", "grep_search", "glob", "locate", "search_context", "codebase_search", "github_codebase_search", "warp_grep", "tool_search":
		query := extractParam(params, "query", "Query", "pattern", "Pattern", "q")
		path := extractParam(params, "path", "Path", "SearchPath", "search_path", "project_root_path", "projectRootPath")

		if query != "" && path != "" {
			if len(query) > 20 {
				query = query[:17] + "..."
			}
			return fmt.Sprintf("Search: '%s' (%s)", query, filepath.Base(path))
		}
		if query != "" {
			if len(query) > 30 {
				query = query[:27] + "..."
			}
			return fmt.Sprintf("Search: '%s'", query)
		}
		if path != "" {
			return fmt.Sprintf("List project: %s", filepath.Base(path))
		}
		return "List project"
	case "execute", "bash", "run_command", "shell", "execute_command", "run_terminal_command", "bash_exec", "terminal":
		command := extractParam(params, "command", "CommandLine", "command_line", "cmd", "script", "args", "arguments", "code")
		if command != "" {
			if len(command) > 40 {
				command = command[:37] + "..."
			}
			return fmt.Sprintf("Run command: '%s'", command)
		}
		return "Run terminal command"
	case "fetch", "web_fetch", "search_web", "download":
		url := extractParam(params, "url", "Url", "URL", "query", "Query")
		if url != "" {
			if len(url) > 45 {
				url = url[:42] + "..."
			}
			return fmt.Sprintf("Fetch: %s", url)
		}
		return "Access webpage/fetch data"
	case "agent":
		type localTask struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		type localAgentParams struct {
			Description  string      `json:"description"`
			SubagentType string      `json:"subagent_type"`
			Prompt       string      `json:"prompt"`
			Tasks        []localTask `json:"tasks"`
		}

		var ap localAgentParams
		if data, err := json.Marshal(params); err == nil {
			_ = json.Unmarshal(data, &ap)
		}

		subagentType := "general"
		if ap.SubagentType != "" {
			subagentType = ap.SubagentType
		}

		// Parallel tasks.
		if len(ap.Tasks) > 1 {
			names := []string{}
			for _, t := range ap.Tasks {
				name := t.Description
				if name == "" {
					name = t.Name
				}
				if name != "" {
					names = append(names, name)
				}
			}
			if len(names) > 0 {
				joined := strings.Join(names, ", ")
				if len(joined) > 40 {
					joined = joined[:37] + "..."
				}
				return fmt.Sprintf("Parallel tasks: %s", joined)
			}
			return fmt.Sprintf("Delegate %d parallel sub-agents", len(ap.Tasks))
		}

		// Single task.
		taskDesc := ap.Description
		if taskDesc == "" && len(ap.Tasks) == 1 {
			taskDesc = ap.Tasks[0].Description
			if taskDesc == "" {
				taskDesc = ap.Tasks[0].Name
			}
		}
		if taskDesc == "" && ap.Prompt != "" {
			words := strings.Fields(ap.Prompt)
			if len(words) > 5 {
				taskDesc = strings.Join(words[:5], " ") + "..."
			} else {
				taskDesc = ap.Prompt
			}
		}

		if taskDesc != "" {
			if len(taskDesc) > 40 {
				taskDesc = taskDesc[:37] + "..."
			}
			return fmt.Sprintf("Delegate sub-agent (@%s): %s", subagentType, taskDesc)
		}
		return fmt.Sprintf("Delegate sub-agent (@%s)", subagentType)
	default:
		if action != "" {
			return fmt.Sprintf("%s: %s", toolName, action)
		}
		return toolName
	}
}

// handlePermissionRequest forwards a single permission request to the client
// and applies the result.
func handlePermissionRequest(ctx context.Context, req permission.PermissionRequest, perms permission.Service, server *Server) {
	allowOnceID := uuid.New().String()
	allowAlwaysID := uuid.New().String()
	rejectOnceID := uuid.New().String()
	rejectAlwaysID := uuid.New().String()

	toolCall := ACPToolCall{
		ToolCallID: req.ToolCallID,
		Title:      GetBeautifulTitle(req.ToolName, req.Action, req.Params),
		Kind:       GetToolKind(req.ToolName),
		Status:     ToolCallStatusInProgress,
		RawInput:   req.Params,
	}

	params := RequestPermissionParams{
		SessionID:          req.SessionID,
		AuthoritySessionID: req.AuthoritySessionID,
		ToolCall:           toolCall,
		Options: []PermissionOption{
			{OptionID: allowOnceID, Name: "Allow once", Kind: PermissionOptionAllowOnce},
			{OptionID: rejectOnceID, Name: "Reject", Kind: PermissionOptionRejectOnce},
			{OptionID: rejectAlwaysID, Name: "Reject always", Kind: PermissionOptionRejectAlways},
		},
	}
	if req.AutoReview == nil {
		params.Options = append(params.Options[:1], append([]PermissionOption{
			{OptionID: allowAlwaysID, Name: "Allow always", Kind: PermissionOptionAllowAlways},
		}, params.Options[1:]...)...)
	}

	raw, err := server.Call(ctx, "session/request_permission", params)
	if err != nil {
		slog.Warn("ACP: request_permission failed, denying", "err", err, "tool", req.ToolName)
		perms.Deny(req)
		return
	}

	var result RequestPermissionResult
	if err := json.Unmarshal(raw, &result); err != nil {
		slog.Warn("ACP: failed to parse permission result, denying", "err", err)
		perms.Deny(req)
		return
	}

	switch result.Outcome.Outcome {
	case "selected":
		switch result.Outcome.OptionID {
		case allowAlwaysID:
			if req.AutoReview != nil {
				perms.Grant(req)
			} else {
				perms.GrantPersistent(req)
			}
		case allowOnceID:
			perms.Grant(req)
		default:
			// Reject selection or unknown option.
			perms.Deny(req)
		}
	case "cancelled":
		perms.Deny(req)
	default:
		perms.Deny(req)
	}
}
