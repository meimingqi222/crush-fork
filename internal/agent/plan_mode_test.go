package agent

import (
	"testing"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestCollaborationModePrompt(t *testing.T) {
	t.Parallel()

	autoPrompt := permissionModePrompt(session.PermissionModeAuto)
	require.Contains(t, autoPrompt, "Auto Mode")
	require.Contains(t, autoPrompt, "Minimize interruptions")

	prompt := collaborationModePrompt(session.CollaborationModePlan)
	require.Contains(t, prompt, "Plan Mode")
	require.Contains(t, prompt, "request_user_input")
	require.Contains(t, prompt, "resolve")
	require.Contains(t, prompt, "active plan file")
	require.Contains(t, prompt, "READ-ONLY work only")

	// Mandatory plan sections.
	require.Contains(t, prompt, "**Context**")
	require.Contains(t, prompt, "**Approach**")
	require.Contains(t, prompt, "**Critical files & anchors**")
	require.Contains(t, prompt, "**Verification**")
	require.Contains(t, prompt, "**Assumptions & contingencies**")

	// Prohibited sections and references.
	require.Contains(t, prompt, "Non-Goals")
	require.Contains(t, prompt, "Alternatives")
	require.Contains(t, prompt, "Risks")
	require.Contains(t, prompt, "Future Work")
	require.Contains(t, prompt, "as discussed")
	require.Contains(t, prompt, "unverified")

	// Re-entry procedure.
	require.Contains(t, prompt, "## Re-entry")
	require.Contains(t, prompt, "Same task continuing → update")
	require.Contains(t, prompt, "Different task → overwrite")

	// Strict alignment with oh-my-pi.
	require.Contains(t, prompt, "execution spec")
	require.Contains(t, prompt, "ZERO design decisions")
	require.Contains(t, prompt, "Ground every claim")
	require.Contains(t, prompt, "Your turn ends ONLY by")

	defaultPrompt := collaborationModePrompt(session.CollaborationModeDefault)
	require.Empty(t, defaultPrompt)
}

func TestBuildSystemPromptForCollaborationMode(t *testing.T) {
	t.Parallel()

	base := "Base system prompt."

	defaultPrompt := buildSystemPromptForModes(base, session.CollaborationModeDefault, session.PermissionModeDefault)
	require.Contains(t, defaultPrompt, base)
	require.Contains(t, defaultPrompt, "Auto Mode is not active")

	autoPrompt := buildSystemPromptForModes(base, session.CollaborationModeDefault, session.PermissionModeAuto)
	require.Contains(t, autoPrompt, base)
	require.Contains(t, autoPrompt, "You are in Auto Mode.")

	planPrompt := buildSystemPromptForModes(base, session.CollaborationModePlan, session.PermissionModeDefault)
	require.Contains(t, planPrompt, base)
	require.Contains(t, planPrompt, "You are in Plan Mode.")
}

func TestRiskLevelForTool(t *testing.T) {
	t.Parallel()

	require.Equal(t, toolRiskDelegation, riskLevelForTool(AgentToolName))
	require.Equal(t, toolRiskNetwork, riskLevelForTool(tools.AgenticFetchToolName))
	require.Equal(t, toolRiskRead, riskLevelForTool(tools.ReadToolName))
	require.Equal(t, toolRiskRead, riskLevelForTool(tools.ResolveToolName))
	require.Equal(t, toolRiskWrite, riskLevelForTool(tools.WriteToolName))
	require.Equal(t, toolRiskWrite, riskLevelForTool(tools.EditToolName))
	require.Equal(t, toolRiskWrite, riskLevelForTool(tools.RetainToolName))
	require.Equal(t, toolRiskExecute, riskLevelForTool(tools.BashToolName))
	require.Equal(t, toolRiskExecute, riskLevelForTool("unknown_tool"))
}

func TestFilterToolsForRiskPolicy(t *testing.T) {
	t.Parallel()

	baseTools := []string{
		tools.ReadToolName,
		tools.BashToolName,
		tools.RetainToolName,
		tools.RequestUserInputToolName,
		tools.ResolveToolName,
	}

	require.Equal(t, []string{
		tools.ReadToolName,
		tools.BashToolName,
		tools.RetainToolName,
		tools.RequestUserInputToolName,
		tools.ResolveToolName,
	}, filterToolsForRiskPolicy(baseTools, session.CollaborationModeDefault, nil))

	require.Equal(t, []string{
		tools.ReadToolName,
		tools.RetainToolName,
		tools.RequestUserInputToolName,
		tools.ResolveToolName,
	}, filterToolsForRiskPolicy(baseTools, session.CollaborationModeDefault, []string{tools.BashToolName}))

	require.Equal(t, []string{
		tools.ReadToolName,
		tools.BashToolName,
		tools.RequestUserInputToolName,
		tools.ResolveToolName,
	}, filterToolsForRiskPolicy(baseTools, session.CollaborationModePlan, []string{tools.ReadToolName}))

	require.Equal(t, []string{
		AgentToolName,
		tools.AgenticFetchToolName,
		tools.ReadToolName,
		tools.RequestUserInputToolName,
		tools.ResolveToolName,
	}, filterToolsForRiskPolicy([]string{AgentToolName, tools.AgenticFetchToolName, tools.ReadToolName}, session.CollaborationModePlan, nil))

	require.Equal(t, []string{
		tools.LSPToolName,
		tools.RequestUserInputToolName,
		tools.ResolveToolName,
	}, filterToolsForRiskPolicy([]string{
		tools.LSPToolName,
	}, session.CollaborationModePlan, nil))
}

func TestFilterToolsForCollaborationMode(t *testing.T) {
	t.Parallel()

	baseTools := []string{
		AgentToolName,
		"bash",
		"grep",
		tools.ReadToolName,
		tools.GlobToolName,
		tools.AgenticFetchToolName,
		tools.EditToolName,
		tools.WriteToolName,
		tools.RetainToolName,
		tools.RequestUserInputToolName,
		tools.ResolveToolName,
		tools.LSPToolName,
		tools.SourcegraphToolName,
	}

	require.Equal(t, []string{
		AgentToolName,
		"bash",
		"grep",
		tools.ReadToolName,
		tools.GlobToolName,
		tools.AgenticFetchToolName,
		tools.EditToolName,
		tools.WriteToolName,
		tools.RetainToolName,
		tools.RequestUserInputToolName,
		tools.ResolveToolName,
		tools.LSPToolName,
		tools.SourcegraphToolName,
	}, filterToolsForCollaborationMode(baseTools, session.CollaborationModeDefault))

	require.Equal(t, []string{
		AgentToolName,
		"bash",
		"grep",
		tools.ReadToolName,
		tools.GlobToolName,
		tools.AgenticFetchToolName,
		tools.EditToolName,
		tools.WriteToolName,
		tools.RequestUserInputToolName,
		tools.ResolveToolName,
		tools.LSPToolName,
		tools.SourcegraphToolName,
	}, filterToolsForCollaborationMode(baseTools, session.CollaborationModePlan))
}
