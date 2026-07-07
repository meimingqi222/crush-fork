package planmode

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExtractProposedPlan(t *testing.T) {
	t.Parallel()

	plan, ok := ExtractProposedPlan("before\n<proposed_plan>\n- Step 1\n- Step 2\n</proposed_plan>\nafter")
	require.True(t, ok)
	require.Equal(t, "- Step 1\n- Step 2", plan)
}

func TestExtractProposedPlanMissingTags(t *testing.T) {
	t.Parallel()

	_, ok := ExtractProposedPlan("no plan here")
	require.False(t, ok)
}

func TestBuildExecutionPrompt(t *testing.T) {
	t.Parallel()

	prompt := BuildExecutionPrompt("- Ship it", ExecuteDirect)
	require.Contains(t, prompt, "Execute the approved plan below")
	require.Contains(t, prompt, "Approved plan:")
	require.Contains(t, prompt, "- Ship it")
	require.NotContains(t, prompt, "<proposed_plan>")

	// Approved-plan execution instructions.
	require.Contains(t, prompt, "read the active plan file")
	require.Contains(t, prompt, "`todos` tool")
	require.Contains(t, prompt, "Verify each step")
	require.Contains(t, prompt, "plan file is authoritative")
}

func TestBuildExecutionPromptWithCompact(t *testing.T) {
	t.Parallel()

	prompt := BuildExecutionPrompt("- Ship it", ExecuteWithCompact)
	require.Contains(t, prompt, "Context has been compacted")
}

func TestBuildExecutionPromptKeepContext(t *testing.T) {
	t.Parallel()

	prompt := BuildExecutionPrompt("- Ship it", ExecuteKeepContext)
	require.Contains(t, prompt, "Full exploration context is preserved")
}
