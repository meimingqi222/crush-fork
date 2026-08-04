package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"charm.land/fantasy"

	"github.com/charmbracelet/crush/internal/agent/tools"
)

// claudeCodeToolNameAliases maps tool names from Claude Code's built-in tool
// set (lowercased) to the equivalent crush tool name. Some models carry a
// strong training prior toward Claude Code's tool names (Grep, Bash, Glob,
// Task, TodoWrite, ...) and call them verbatim even though crush registers
// its tools under different names. Case-only differences (e.g. "Grep" vs
// "grep") are handled separately via case-insensitive matching in
// findRepairedToolName, so this table only needs entries whose crush name
// differs by more than casing.
var claudeCodeToolNameAliases = map[string]string{
	"task":       AgentToolName,
	"webfetch":   tools.WebFetchToolName,
	"websearch":  tools.WebSearchToolName,
	"multiedit":  tools.EditToolName,
	"bashoutput": tools.JobToolName,
	"killshell":  tools.JobToolName,
}

// toolCallParamAliases maps, per crush tool name, hallucinated parameter
// keys (as a weak model carrying Claude Code's tool schemas might produce)
// to the crush tool's actual parameter name. Entries are only added when the
// rename is unambiguous; anything uncertain is left for fantasy's built-in
// schema-driven repair pipeline (which drops unrecognized keys) to handle.
var toolCallParamAliases = map[string]map[string]string{
	tools.GlobToolName: {
		"pattern":      "path",
		"glob_pattern": "path",
	},
	tools.GrepToolName: {
		"glob":         "include",
		"file_pattern": "include",
		"-A":           "context_after",
		"-B":           "context_before",
	},
	tools.ReadToolName: {
		"file_path":     "path",
		"notebook_path": "path",
	},
}

// repairToolCall is the fantasy.RepairToolCallFunction wired into session
// runs. It first rewrites direct calls to known-but-unactivated deferred
// tools (all MCP tools are deferred) into a tool_search activation call,
// then falls back to the Claude Code name-alias repair below.
func (a *sessionAgent) repairToolCall(ctx context.Context, options fantasy.ToolCallRepairOptions) (*fantasy.ToolCallContent, error) {
	if options.ValidationError != nil &&
		strings.Contains(options.ValidationError.Error(), "tool not found") &&
		a.deferredToolRuntime != nil {
		if repaired := repairDeferredToolCall(options, a.deferredToolRuntime.isDeferredTool); repaired != nil {
			slog.Info("Repaired direct deferred tool call into tool_search activation",
				"original_tool_name", options.OriginalToolCall.ToolName,
				"repaired_input", repaired.Input,
			)
			return repaired, nil
		}
	}
	return repairMisnamedToolCall(ctx, options)
}

// repairDeferredToolCall rewrites a direct call to a known deferred tool
// that has not been activated for the session into a tool_search
// "select:<name>" call. Models regularly skip the tool_search step and call
// deferred MCP tools directly; letting that fail would surface a misleading
// "tool not found" error for a tool that does exist. The rewritten call
// activates the tool (so the next step's tool set includes it) and returns
// its schema, letting the model reissue the real call with correct
// parameters. Returns nil when the name is not a known deferred tool or
// tool_search is not part of the step's tool set.
func repairDeferredToolCall(options fantasy.ToolCallRepairOptions, isDeferred func(string) bool) *fantasy.ToolCallContent {
	if isDeferred == nil {
		return nil
	}
	name := strings.TrimSpace(options.OriginalToolCall.ToolName)
	if name == "" {
		return nil
	}
	if !isDeferred(name) {
		lowered := strings.ToLower(name)
		if lowered == name || !isDeferred(lowered) {
			return nil
		}
		name = lowered
	}

	toolSearchAvailable := false
	for _, t := range options.AvailableTools {
		if t.Info().Name == tools.ToolSearchToolName {
			toolSearchAvailable = true
			break
		}
	}
	if !toolSearchAvailable {
		return nil
	}

	input, err := json.Marshal(tools.ToolSearchParams{Query: "select:" + name})
	if err != nil {
		return nil
	}

	repaired := options.OriginalToolCall
	repaired.ToolName = tools.ToolSearchToolName
	repaired.Input = string(input)
	repaired.Invalid = false
	repaired.ValidationError = nil
	return &repaired
}

// repairMisnamedToolCall is a fantasy.RepairToolCallFunction that recovers
// from tool calls using Claude Code's built-in tool names/casing instead of
// crush's own tool names (e.g. "Grep" instead of "grep", "Task" instead of
// "agent"). It only handles tool-not-found validation errors; argument-shape
// problems are left to fantasy's built-in repair pipeline.
func repairMisnamedToolCall(_ context.Context, options fantasy.ToolCallRepairOptions) (*fantasy.ToolCallContent, error) {
	if options.ValidationError == nil || !strings.Contains(options.ValidationError.Error(), "tool not found") {
		return nil, nil
	}

	original := options.OriginalToolCall

	targetTool, ok := findRepairedTool(original.ToolName, options.AvailableTools)
	if !ok {
		return nil, nil
	}

	repairedName := targetTool.Info().Name
	repairedInput := original.Input
	renamedParams := renameToolCallParams(&repairedInput, targetTool.Info())

	repaired := original
	repaired.ToolName = repairedName
	repaired.Input = repairedInput
	repaired.Invalid = false
	repaired.ValidationError = nil

	slog.Info("Repaired misnamed tool call",
		"original_tool_name", original.ToolName,
		"repaired_tool_name", repairedName,
		"renamed_params", renamedParams,
	)

	return &repaired, nil
}

// findRepairedTool looks for a single unambiguous replacement for name among
// availableTools: first a case-insensitive name match, then a lookup in
// claudeCodeToolNameAliases (only applied when the alias target is itself
// present in availableTools, so MCP tools and disabled/unregistered tools
// are never guessed at).
func findRepairedTool(name string, availableTools []fantasy.AgentTool) (fantasy.AgentTool, bool) {
	var caseInsensitiveMatch fantasy.AgentTool
	ambiguous := false
	for _, t := range availableTools {
		if strings.EqualFold(t.Info().Name, name) {
			if caseInsensitiveMatch != nil {
				ambiguous = true
				continue
			}
			caseInsensitiveMatch = t
		}
	}
	if ambiguous {
		return nil, false
	}
	if caseInsensitiveMatch != nil {
		return caseInsensitiveMatch, true
	}

	aliasTarget, ok := claudeCodeToolNameAliases[strings.ToLower(name)]
	if !ok {
		return nil, false
	}
	for _, t := range availableTools {
		if t.Info().Name == aliasTarget {
			return t, true
		}
	}
	return nil, false
}

// renameToolCallParams rewrites *input in place, renaming any top-level keys
// that match a known hallucinated-parameter alias for info's tool to the
// tool's real parameter name. A rename only happens when the source key is
// not already a valid parameter of the tool, the destination key is a valid
// parameter of the tool, and the destination key isn't already present
// (so a legitimate value is never clobbered). It returns the list of keys
// that were renamed (old -> new), for logging.
func renameToolCallParams(input *string, info fantasy.ToolInfo) []string {
	aliases := toolCallParamAliases[info.Name]
	if len(aliases) == 0 {
		return nil
	}

	var args map[string]any
	if err := json.Unmarshal([]byte(*input), &args); err != nil {
		return nil
	}

	var renamed []string
	for badKey, goodKey := range aliases {
		val, exists := args[badKey]
		if !exists {
			continue
		}
		if _, badKeyIsValid := info.Parameters[badKey]; badKeyIsValid {
			// badKey is actually a real parameter for this tool; leave it alone.
			continue
		}
		if _, goodKeyValid := info.Parameters[goodKey]; !goodKeyValid {
			continue
		}
		if _, alreadySet := args[goodKey]; alreadySet {
			continue
		}
		args[goodKey] = val
		delete(args, badKey)
		renamed = append(renamed, badKey+"->"+goodKey)
	}

	if len(renamed) == 0 {
		return nil
	}

	out, err := json.Marshal(args)
	if err != nil {
		return nil
	}
	*input = string(out)
	return renamed
}
