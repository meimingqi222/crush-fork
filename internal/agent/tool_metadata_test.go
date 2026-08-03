package agent

import (
	"testing"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

const (
	primaryAgent  = false
	subagentAgent = true
)

// TestSubagentChannelToolsStayExposed pins the exposure split for the tools a
// subagent uses as its working channel: deferred for primary agents, exposed
// for subagents. templates/agent_tool.md tells the model these are available
// to subagents, so a regression here would make the system prompt assert
// something false and cost a wasted tool_search round trip.
func TestSubagentChannelToolsStayExposed(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		tools.IrcToolName,
		tools.SendMessageToolName,
		tools.TaskStopToolName,
	} {
		primary := builtinToolMetadataForAgent(name, primaryAgent, session.CollaborationModeDefault)
		require.True(t, primary.IsDeferred(), "%s must be deferred for primary agents", name)

		sub := builtinToolMetadataForAgent(name, subagentAgent, session.CollaborationModeDefault)
		require.False(t, sub.IsDeferred(), "%s must be exposed to subagents", name)
		require.Equal(t, tools.ToolExposureDefault, sub.Exposure)
	}
}

// TestAgentToolDeferredExceptOrchestrate pins the agent-tool exposure rule:
// deferred in ordinary sessions (it carries the largest description in the
// toolset), always exposed in Orchestrate mode where delegation is the whole
// point. coder.md.tpl instructs the model to activate it via tool_search, so
// the two must not drift apart.
func TestAgentToolDeferredExceptOrchestrate(t *testing.T) {
	t.Parallel()

	for _, mode := range []session.CollaborationMode{
		session.CollaborationModeDefault,
		session.CollaborationModePlan,
	} {
		require.True(t,
			builtinToolMetadataForAgent(AgentToolName, primaryAgent, mode).IsDeferred(),
			"agent must be deferred in %s mode", mode)
	}

	orchestrate := builtinToolMetadataForAgent(AgentToolName, primaryAgent, session.CollaborationModeOrchestrate)
	require.False(t, orchestrate.IsDeferred(), "agent must stay exposed in Orchestrate mode")

	// Orchestrate mode allows the agent tool by name; a deferred agent tool
	// there would be allowed but invisible.
	_, allowed := orchestrateModeAllowedToolNames[AgentToolName]
	require.True(t, allowed, "orchestrate allowlist and exposure rule must agree")
}

// TestDeferredSetMatchesExpectation locks the deferred built-in roster so a
// tool is not silently added to or dropped from the lazy-loaded set. Each
// entry costs a tool_search round trip when the model needs it, so growth
// here should be a deliberate decision.
func TestDeferredSetMatchesExpectation(t *testing.T) {
	t.Parallel()

	deferred := []string{
		AgentToolName,
		tools.CrushToolName,
		tools.DownloadToolName,
		tools.GraphToolName,
		tools.IrcToolName,
		tools.JobToolName,
		tools.MemoryStatusToolName,
		tools.ReflectToolName,
		tools.SendMessageToolName,
		tools.SourcegraphToolName,
		tools.TaskStopToolName,
	}
	for _, name := range deferred {
		require.True(t,
			builtinToolMetadataForAgent(name, primaryAgent, session.CollaborationModeDefault).IsDeferred(),
			"%s should be deferred for a primary agent", name)
	}

	alwaysOn := []string{
		tools.ReadToolName,
		tools.EditToolName,
		tools.WriteToolName,
		tools.GrepToolName,
		tools.GlobToolName,
		tools.BashToolName,
		tools.TodosToolName,
		tools.RecallToolName,
		tools.RetainToolName,
		tools.LSPToolName,
	}
	for _, name := range alwaysOn {
		require.False(t,
			builtinToolMetadataForAgent(name, primaryAgent, session.CollaborationModeDefault).IsDeferred(),
			"%s must stay in the default toolset", name)
	}
}

// TestBuiltinToolMetadataForAgentPreservesOtherFields guards against the
// exposure override rebuilding metadata and dropping search hints/tags, which
// would silently degrade tool_search ranking for exactly the tools that can
// only be reached through tool_search.
func TestBuiltinToolMetadataForAgentPreservesOtherFields(t *testing.T) {
	t.Parallel()

	for _, name := range []string{tools.IrcToolName, AgentToolName} {
		base := builtinToolMetadata(name)
		got := builtinToolMetadataForAgent(name, subagentAgent, session.CollaborationModeOrchestrate)

		require.Equal(t, base.SearchHint, got.SearchHint, name)
		require.Equal(t, base.SearchTags, got.SearchTags, name)
		require.Equal(t, base.RiskHint, got.RiskHint, name)
		require.Equal(t, base.ConcurrencySafe, got.ConcurrencySafe, name)
	}
}
