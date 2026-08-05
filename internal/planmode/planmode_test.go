package planmode

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildExecutionPrompt(t *testing.T) {
	t.Parallel()

	prompt := BuildExecutionPrompt("- Ship it", ExecuteDirect)
	require.Contains(t, prompt, "Execute the approved plan below")
	require.Contains(t, prompt, "Approved plan:")
	require.Contains(t, prompt, "- Ship it")
	require.NotContains(t, prompt, "<proposed_plan>")

	// Approved-plan execution instructions.
	require.Contains(t, prompt, "read the active plan file")
	require.Contains(t, prompt, "`todo` tool")
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
