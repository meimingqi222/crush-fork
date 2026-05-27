package agent

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildDelegationPromptPrefixSkipsSubagentsAndMissingAgentTool(t *testing.T) {
	t.Parallel()

	base := "provider-prefix"

	noAgentTool := buildDelegationPromptPrefix(base, nil, false)
	assert.Equal(t, base, noAgentTool)

	withSubagent := buildDelegationPromptPrefix(base, []fantasy.AgentTool{testAgentTool()}, true)
	assert.Equal(t, base, withSubagent)
}

func TestBuildDelegationPromptPrefixAddsCostAwareDelegationPolicyForPrimaryAgent(t *testing.T) {
	t.Parallel()

	prefix := buildDelegationPromptPrefix("provider-prefix", []fantasy.AgentTool{testAgentTool()}, false)

	assert.Contains(t, prefix, "provider-prefix")
	assert.Contains(t, prefix, "coordinator agent")
	assert.Contains(t, prefix, "explore subagent")
	assert.Contains(t, prefix, "Phase 1")
	assert.Contains(t, prefix, "Phase 2")
	assert.Contains(t, prefix, "Phase 2 — Design")
	assert.NotContains(t, prefix, "Phase 2 — Plan")
	assert.Contains(t, prefix, "Phase 3")
	assert.Contains(t, prefix, "Phase 4")
	assert.Contains(t, prefix, "Cost comparison")
	assert.Contains(t, prefix, "read/grep/glob")
	assert.Contains(t, prefix, "read/list files and return raw contents")
	assert.Contains(t, prefix, "not to\nread and relay file contents")
	assert.Contains(t, prefix, "do final review in primary")
	assert.Contains(t, prefix, "Prefer combined investigation + action")
	assert.Contains(t, prefix, "do not ask 'explore' to run build, test, lint, package-manager, or")
	assert.Contains(t, prefix, "do not ask it for final code-review approval")
}

func TestPromptForAgentUsesWorkerPromptForWritableSubagents(t *testing.T) {
	t.Parallel()

	promptBuilder, err := promptForAgent(config.Agent{ID: config.AgentCoder}, false)
	require.NoError(t, err)
	assert.Equal(t, "coder", promptBuilder.Name())

	promptBuilder, err = promptForAgent(config.Agent{ID: config.AgentGeneral, Role: "executor"}, true)
	require.NoError(t, err)
	assert.Equal(t, "general", promptBuilder.Name())

	promptBuilder, err = promptForAgent(config.Agent{ID: config.AgentExplore, Role: "researcher"}, true)
	require.NoError(t, err)
	assert.Equal(t, "explore", promptBuilder.Name())

	promptBuilder, err = promptForAgent(config.Agent{
		ID:           "reviewer",
		Role:         "planner",
		Mode:         config.AgentModeSubagent,
		AllowedTools: []string{"bash", "read"},
	}, true)
	require.NoError(t, err)
	assert.Equal(t, "general", promptBuilder.Name())
}

func testAgentTool() fantasy.AgentTool {
	return fantasy.NewAgentTool(
		AgentToolName,
		"delegates work",
		func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("ok"), nil
		},
	)
}
