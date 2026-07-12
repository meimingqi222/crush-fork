package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/agent/tools"
)

// repairTestTool is a minimal fantasy.AgentTool implementation used to build
// AvailableTools lists for repairMisnamedToolCall tests.
type repairTestTool struct {
	name   string
	params map[string]any
}

func (t *repairTestTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{Name: t.name, Parameters: t.params}
}

func (t *repairTestTool) Run(_ context.Context, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return fantasy.ToolResponse{}, nil
}

func (t *repairTestTool) ProviderOptions() fantasy.ProviderOptions     { return nil }
func (t *repairTestTool) SetProviderOptions(_ fantasy.ProviderOptions) {}

func newRepairTestTool(name string, paramNames ...string) *repairTestTool {
	params := make(map[string]any, len(paramNames))
	for _, p := range paramNames {
		params[p] = map[string]any{"type": "string"}
	}
	return &repairTestTool{name: name, params: params}
}

func toolNotFoundOptions(originalName, input string, availableTools ...fantasy.AgentTool) fantasy.ToolCallRepairOptions {
	return fantasy.ToolCallRepairOptions{
		OriginalToolCall: fantasy.ToolCallContent{
			ToolCallID: "call-1",
			ToolName:   originalName,
			Input:      input,
		},
		ValidationError: errors.New("tool not found: " + originalName),
		AvailableTools:  availableTools,
	}
}

func TestRepairMisnamedToolCall_CaseInsensitiveMatch(t *testing.T) {
	t.Parallel()

	grep := newRepairTestTool(tools.GrepToolName, "pattern", "path")
	repaired, err := repairMisnamedToolCall(t.Context(), toolNotFoundOptions("Grep", `{"pattern":"foo"}`, grep))
	require.NoError(t, err)
	require.NotNil(t, repaired)
	require.Equal(t, tools.GrepToolName, repaired.ToolName)
	require.False(t, repaired.Invalid)
	require.Nil(t, repaired.ValidationError)
}

func TestRepairMisnamedToolCall_AliasMatch(t *testing.T) {
	t.Parallel()

	agentTool := newRepairTestTool(AgentToolName, "prompt")
	repaired, err := repairMisnamedToolCall(t.Context(), toolNotFoundOptions("Task", `{"prompt":"do it"}`, agentTool))
	require.NoError(t, err)
	require.NotNil(t, repaired)
	require.Equal(t, AgentToolName, repaired.ToolName)
}

func TestRepairMisnamedToolCall_AliasTargetMissing(t *testing.T) {
	t.Parallel()

	// "Task" aliases to the "agent" tool, but it's not in AvailableTools here
	// (e.g. disabled for this session), so no repair should be attempted.
	other := newRepairTestTool(tools.GrepToolName, "pattern")
	repaired, err := repairMisnamedToolCall(t.Context(), toolNotFoundOptions("Task", `{"prompt":"do it"}`, other))
	require.NoError(t, err)
	require.Nil(t, repaired)
}

func TestRepairMisnamedToolCall_ParamRename(t *testing.T) {
	t.Parallel()

	glob := newRepairTestTool(tools.GlobToolName, "path", "limit")
	repaired, err := repairMisnamedToolCall(t.Context(), toolNotFoundOptions("Glob", `{"glob_pattern":"**/*.go"}`, glob))
	require.NoError(t, err)
	require.NotNil(t, repaired)
	require.Equal(t, tools.GlobToolName, repaired.ToolName)

	var args map[string]any
	require.NoError(t, json.Unmarshal([]byte(repaired.Input), &args))
	require.Equal(t, "**/*.go", args["path"])
	require.NotContains(t, args, "glob_pattern")
}

func TestRepairMisnamedToolCall_ParamRenameDoesNotClobberExisting(t *testing.T) {
	t.Parallel()

	glob := newRepairTestTool(tools.GlobToolName, "path", "limit")
	repaired, err := repairMisnamedToolCall(t.Context(), toolNotFoundOptions("Glob", `{"glob_pattern":"**/*.go","path":"src"}`, glob))
	require.NoError(t, err)
	require.NotNil(t, repaired)

	var args map[string]any
	require.NoError(t, json.Unmarshal([]byte(repaired.Input), &args))
	// The legitimate "path" value must survive untouched; the hallucinated
	// "glob_pattern" key is left in place since we couldn't safely rename it.
	require.Equal(t, "src", args["path"])
	require.Equal(t, "**/*.go", args["glob_pattern"])
}

func TestRepairMisnamedToolCall_NoMatch(t *testing.T) {
	t.Parallel()

	grep := newRepairTestTool(tools.GrepToolName, "pattern")
	repaired, err := repairMisnamedToolCall(t.Context(), toolNotFoundOptions("TotallyUnknownTool", `{}`, grep))
	require.NoError(t, err)
	require.Nil(t, repaired)
}

func TestRepairMisnamedToolCall_AmbiguousCaseInsensitiveMatch(t *testing.T) {
	t.Parallel()

	// Two tools that only differ by case (e.g. one built-in, one from an MCP
	// server) - repair must not guess between them.
	a := newRepairTestTool("Grep")
	b := newRepairTestTool("grep")
	repaired, err := repairMisnamedToolCall(t.Context(), toolNotFoundOptions("GREP", `{}`, a, b))
	require.NoError(t, err)
	require.Nil(t, repaired)
}

func TestRepairMisnamedToolCall_McpToolNamesNotTouched(t *testing.T) {
	t.Parallel()

	// An MCP tool that happens to be named "task" (case-insensitive) is not
	// what Claude Code's "Task" alias should ever resolve to via the alias
	// table, but a case-insensitive match against it is still legitimate
	// disambiguation - it must resolve to the one available match rather
	// than being skipped.
	mcpTool := newRepairTestTool("mcp_myserver_search")
	repaired, err := repairMisnamedToolCall(t.Context(), toolNotFoundOptions("Grep", `{}`, mcpTool))
	require.NoError(t, err)
	require.Nil(t, repaired, "must not repair to an unrelated MCP tool")
}

func TestRepairMisnamedToolCall_IgnoresNonToolNotFoundErrors(t *testing.T) {
	t.Parallel()

	grep := newRepairTestTool(tools.GrepToolName, "pattern")
	opts := toolNotFoundOptions("grep", `{"pattern":123}`, grep)
	opts.ValidationError = errors.New("invalid JSON input: unexpected type")
	repaired, err := repairMisnamedToolCall(t.Context(), opts)
	require.NoError(t, err)
	require.Nil(t, repaired)
}

func TestRepairMisnamedToolCall_NilValidationError(t *testing.T) {
	t.Parallel()

	grep := newRepairTestTool(tools.GrepToolName, "pattern")
	opts := toolNotFoundOptions("Grep", `{}`, grep)
	opts.ValidationError = nil
	repaired, err := repairMisnamedToolCall(t.Context(), opts)
	require.NoError(t, err)
	require.Nil(t, repaired)
}

func deferredNameSet(names ...string) func(string) bool {
	set := make(map[string]bool, len(names))
	for _, name := range names {
		set[name] = true
	}
	return func(name string) bool { return set[name] }
}

func TestRepairDeferredToolCall_RewritesToToolSearch(t *testing.T) {
	t.Parallel()

	toolSearch := newRepairTestTool(tools.ToolSearchToolName, "query", "limit")
	opts := toolNotFoundOptions("mcp_acemcp_search_context", `{"query":"how does it work"}`, toolSearch)
	repaired := repairDeferredToolCall(opts, deferredNameSet("mcp_acemcp_search_context"))
	require.NotNil(t, repaired)
	require.Equal(t, tools.ToolSearchToolName, repaired.ToolName)
	require.False(t, repaired.Invalid)
	require.Nil(t, repaired.ValidationError)
	require.Equal(t, "call-1", repaired.ToolCallID)

	var params tools.ToolSearchParams
	require.NoError(t, json.Unmarshal([]byte(repaired.Input), &params))
	require.Equal(t, "select:mcp_acemcp_search_context", params.Query)
}

func TestRepairDeferredToolCall_CaseInsensitiveName(t *testing.T) {
	t.Parallel()

	toolSearch := newRepairTestTool(tools.ToolSearchToolName, "query")
	opts := toolNotFoundOptions("MCP_ACEMCP_SEARCH_CONTEXT", `{}`, toolSearch)
	repaired := repairDeferredToolCall(opts, deferredNameSet("mcp_acemcp_search_context"))
	require.NotNil(t, repaired)

	var params tools.ToolSearchParams
	require.NoError(t, json.Unmarshal([]byte(repaired.Input), &params))
	require.Equal(t, "select:mcp_acemcp_search_context", params.Query)
}

func TestRepairDeferredToolCall_NotDeferred(t *testing.T) {
	t.Parallel()

	toolSearch := newRepairTestTool(tools.ToolSearchToolName, "query")
	opts := toolNotFoundOptions("mcp_other_tool", `{}`, toolSearch)
	repaired := repairDeferredToolCall(opts, deferredNameSet("mcp_acemcp_search_context"))
	require.Nil(t, repaired)
}

func TestRepairDeferredToolCall_ToolSearchUnavailable(t *testing.T) {
	t.Parallel()

	grep := newRepairTestTool(tools.GrepToolName, "pattern")
	opts := toolNotFoundOptions("mcp_acemcp_search_context", `{}`, grep)
	repaired := repairDeferredToolCall(opts, deferredNameSet("mcp_acemcp_search_context"))
	require.Nil(t, repaired)
}

func TestSessionAgentRepairToolCall_DeferredTakesPrecedence(t *testing.T) {
	t.Parallel()

	runtime := &deferredToolRuntimeStub{knownDeferred: map[string]bool{"mcp_acemcp_search_context": true}}
	a := &sessionAgent{deferredToolRuntime: runtime}

	toolSearch := newRepairTestTool(tools.ToolSearchToolName, "query")
	opts := toolNotFoundOptions("mcp_acemcp_search_context", `{"query":"anything"}`, toolSearch)
	repaired, err := a.repairToolCall(t.Context(), opts)
	require.NoError(t, err)
	require.NotNil(t, repaired)
	require.Equal(t, tools.ToolSearchToolName, repaired.ToolName)
}

func TestSessionAgentRepairToolCall_FallsBackToMisnamedRepair(t *testing.T) {
	t.Parallel()

	runtime := &deferredToolRuntimeStub{knownDeferred: map[string]bool{"mcp_acemcp_search_context": true}}
	a := &sessionAgent{deferredToolRuntime: runtime}

	grep := newRepairTestTool(tools.GrepToolName, "pattern", "path")
	repaired, err := a.repairToolCall(t.Context(), toolNotFoundOptions("Grep", `{"pattern":"foo"}`, grep))
	require.NoError(t, err)
	require.NotNil(t, repaired)
	require.Equal(t, tools.GrepToolName, repaired.ToolName)
}

func TestSessionAgentRepairToolCall_NilDeferredRuntime(t *testing.T) {
	t.Parallel()

	a := &sessionAgent{}
	toolSearch := newRepairTestTool(tools.ToolSearchToolName, "query")
	opts := toolNotFoundOptions("mcp_acemcp_search_context", `{}`, toolSearch)
	repaired, err := a.repairToolCall(t.Context(), opts)
	require.NoError(t, err)
	require.Nil(t, repaired)
}
