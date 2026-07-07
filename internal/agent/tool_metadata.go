package agent

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/plugin"
)

func builtinToolMetadata(name string) tools.ToolMetadata {
	switch name {
	case AgentToolName:
		return tools.ToolMetadata{RiskHint: "delegation", SearchHint: "delegate independent work to subagents", SearchTags: []string{"subagent", "delegate", "parallel", "worker", "task"}}
	case tools.ToolSearchToolName:
		return tools.ToolMetadata{ReadOnly: true, ConcurrencySafe: true, RiskHint: "read", SearchHint: "load deferred tool definitions and activate them for this run", SearchTags: []string{"discover-tools", "activate-tools", "tool-registry", "deferred-tools", "select-tool"}, Direct: true}
	case tools.AgenticFetchToolName:
		return tools.ToolMetadata{ConcurrencySafe: true, RiskHint: "network", SearchHint: "research web content", SearchTags: []string{"web-research", "web-search", "url", "fetch", "internet"}}
	case tools.BashToolName:
		return tools.ToolMetadata{RiskHint: "execute", SearchHint: "execute shell commands", SearchTags: []string{"terminal", "shell-command", "command-line", "script", "process"}}
	case tools.JobToolName:
		return tools.ToolMetadata{RiskHint: "execute", SearchHint: "inspect or control background shell jobs", SearchTags: []string{"background-job", "process", "terminal", "shell"}}
	case tools.DownloadToolName:
		return tools.ToolMetadata{ConcurrencySafe: true, RiskHint: "network", SearchHint: "download URL to local file", SearchTags: []string{"url-download", "http", "fetch", "file"}}
	case tools.EditToolName, tools.WriteToolName:
		return tools.ToolMetadata{RiskHint: "write", SearchHint: "modify local files", SearchTags: []string{"file-edit", "file-write", "patch", "modify"}, Direct: true}
	case tools.ReadToolName:
		return tools.ToolMetadata{ReadOnly: true, ConcurrencySafe: true, RiskHint: "read", SearchHint: "read local files or URL content", SearchTags: []string{"file-read", "url-read", "filesystem", "http", "content"}, Direct: true}
	case tools.GlobToolName, tools.GrepToolName:
		return tools.ToolMetadata{ReadOnly: true, ConcurrencySafe: true, RiskHint: "read", SearchHint: "inspect local files", SearchTags: []string{"file-search", "pattern", "filesystem", "code-search"}, Direct: true}
	case tools.SourcegraphToolName:
		return tools.ToolMetadata{ReadOnly: true, ConcurrencySafe: true, RiskHint: "network", SearchHint: "search public repositories", SearchTags: []string{"code-search", "repository-search", "cross-repo", "sourcegraph", "symbols"}, Exposure: tools.ToolExposureDeferred, Direct: true}
	case tools.RetainToolName:
		return tools.ToolMetadata{RiskHint: "write", SearchHint: "retain a memory event in the event log", SearchTags: []string{"memory", "event-log", "state", "remember"}}
	case tools.RecallToolName:
		return tools.ToolMetadata{ReadOnly: true, ConcurrencySafe: true, RiskHint: "read", SearchHint: "recall materialized memory summaries and events", SearchTags: []string{"memory", "recall", "history", "summary"}}
	case tools.ReflectToolName:
		return tools.ToolMetadata{ReadOnly: true, ConcurrencySafe: true, RiskHint: "read", SearchHint: "reflect on memories across sessions", SearchTags: []string{"memory", "reflection", "synthesis", "cross-session"}}
	case tools.MemoryStatusToolName:
		return tools.ToolMetadata{ReadOnly: true, ConcurrencySafe: true, RiskHint: "read", SearchHint: "view memory engine pipeline status", SearchTags: []string{"memory", "status", "observability"}}
	case tools.TodosToolName:
		return tools.ToolMetadata{RiskHint: "write", SearchHint: "track structured task progress", SearchTags: []string{"todo", "task-list", "planning", "progress"}, Direct: true}
	case tools.SendMessageToolName:
		return tools.ToolMetadata{RiskHint: "write", SearchHint: "send mailbox messages to running task graph tasks", SearchTags: []string{"mailbox", "taskgraph", "message", "subagent"}, Direct: true}
	case tools.TaskStopToolName:
		return tools.ToolMetadata{RiskHint: "write", SearchHint: "request task cancellation through mailbox protocol", SearchTags: []string{"mailbox", "taskgraph", "cancel", "stop-task"}, Direct: true}
	case tools.LSPToolName:
		return tools.ToolMetadata{ReadOnly: true, ConcurrencySafe: true, RiskHint: "read", SearchHint: "query language-server code intelligence", SearchTags: []string{"lsp", "code-intelligence", "symbols", "definitions", "references", "diagnostics"}, Direct: true}
	case tools.RequestUserInputToolName, tools.PlanExitToolName, tools.ResolveToolName:
		return tools.ToolMetadata{ReadOnly: true, ConcurrencySafe: true, RiskHint: "read", SearchHint: "plan mode interaction control", SearchTags: []string{"plan-mode", "user-input", "interaction", "approval"}, Direct: true}
	case tools.YieldToolName:
		return tools.ToolMetadata{ReadOnly: true, ConcurrencySafe: true, RiskHint: "read", SearchHint: "submit subagent result or structured completion metadata", SearchTags: []string{"subagent", "completion", "result", "metadata"}}
	case tools.CrushToolName:
		return tools.ToolMetadata{ReadOnly: true, ConcurrencySafe: true, RiskHint: "read", SearchHint: "inspect Crush instance status and logs", SearchTags: []string{"crush", "status", "logs", "debug", "diagnostics"}}
	default:
		return tools.ToolMetadata{RiskHint: "execute"}
	}
}

func metadataFromPluginToolDefinition(def plugin.ToolDefinition) tools.ToolMetadata {
	metadata := builtinToolMetadata(def.Name)
	if len(def.Metadata) == 0 {
		if metadata.RiskHint == "" {
			metadata.RiskHint = "execute"
		}
		if metadata.SearchHint == "" {
			metadata.SearchHint = strings.TrimSpace(def.Description)
		}
		return metadata
	}

	if value, ok := boolMetadataValue(def.Metadata, "read_only"); ok {
		metadata.ReadOnly = value
	}
	if value, ok := boolMetadataValue(def.Metadata, "concurrency_safe"); ok {
		metadata.ConcurrencySafe = value
	}
	if value, ok := boolMetadataValue(def.Metadata, "direct"); ok {
		metadata.Direct = value
	}
	if value, ok := stringMetadataValue(def.Metadata, "risk_hint"); ok {
		metadata.RiskHint = value
	}
	if value, ok := stringMetadataValue(def.Metadata, "search_hint"); ok {
		metadata.SearchHint = value
	}
	if value, ok := stringMetadataValue(def.Metadata, "exposure"); ok {
		metadata.Exposure = tools.ToolExposure(value)
	}
	if values := stringSliceMetadataValue(def.Metadata, "search_tags"); len(values) > 0 {
		metadata.SearchTags = values
	}
	if metadata.SearchHint == "" {
		metadata.SearchHint = strings.TrimSpace(def.Description)
	}
	if metadata.RiskHint == "" {
		metadata.RiskHint = "execute"
	}
	return metadata
}

func boolMetadataValue(metadata map[string]any, key string) (bool, bool) {
	raw, ok := metadata[key]
	if !ok {
		return false, false
	}
	switch value := raw.(type) {
	case bool:
		return value, true
	case string:
		normalized := strings.TrimSpace(strings.ToLower(value))
		switch normalized {
		case "true", "1", "yes", "on":
			return true, true
		case "false", "0", "no", "off":
			return false, true
		}
	}
	return false, false
}

func stringMetadataValue(metadata map[string]any, key string) (string, bool) {
	raw, ok := metadata[key]
	if !ok {
		return "", false
	}
	switch value := raw.(type) {
	case string:
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return "", false
		}
		return trimmed, true
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", value)), true
	}
}

func stringSliceMetadataValue(metadata map[string]any, key string) []string {
	raw, ok := metadata[key]
	if !ok {
		return nil
	}
	switch value := raw.(type) {
	case []string:
		return normalizeSearchTags(value)
	case []any:
		items := make([]string, 0, len(value))
		for _, item := range value {
			items = append(items, fmt.Sprintf("%v", item))
		}
		return normalizeSearchTags(items)
	case string:
		if strings.TrimSpace(value) == "" {
			return nil
		}
		parts := strings.Split(value, ",")
		return normalizeSearchTags(parts)
	default:
		return nil
	}
}

func normalizeSearchTags(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(tags))
	normalized := make([]string, 0, len(tags))
	for _, tag := range tags {
		trimmed := strings.TrimSpace(tag)
		if trimmed == "" {
			continue
		}
		key := strings.ToLower(trimmed)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		normalized = append(normalized, trimmed)
	}
	return normalized
}
