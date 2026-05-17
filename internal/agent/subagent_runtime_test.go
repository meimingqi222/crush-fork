package agent

import (
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestSubagentResultContract_ProfileDefaults(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		profile     SubagentProfile
		wantPolicy  MissingFinishPolicy
		wantRetry   SubagentRetryPolicyKind
		wantRequire bool
	}{
		{
			name:        "explore",
			profile:     SubagentProfile{Kind: SubagentProfileExplore, ReadOnly: true},
			wantPolicy:  MissingFinishRetryThenWarn,
			wantRetry:   RetryReadOnlyOnly,
			wantRequire: true,
		},
		{
			name:        "general",
			profile:     SubagentProfile{Kind: SubagentProfileGeneral},
			wantPolicy:  MissingFinishRetryThenFail,
			wantRetry:   RetryIdempotent,
			wantRequire: true,
		},
		{
			name:        "review",
			profile:     SubagentProfile{Kind: SubagentProfileReview},
			wantPolicy:  MissingFinishRetryThenWarn,
			wantRetry:   RetryIdempotent,
			wantRequire: true,
		},
		{
			name:        "guardian",
			profile:     SubagentProfile{Kind: SubagentProfileGuardian},
			wantPolicy:  MissingFinishFail,
			wantRetry:   RetryIdempotent,
			wantRequire: true,
		},
	}

	for _, tt := range cases {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			contract := subagentResultContract(tt.profile)
			require.Equal(t, tt.wantPolicy, contract.MissingFinishPolicy)
			require.Equal(t, tt.wantRequire, contract.Required)

			retry := subagentRetryPolicy(tt.profile)
			require.Equal(t, tt.wantRetry, retry.Kind)
		})
	}
}

func TestApplySubagentRuntimeConfig_DoesNotOverrideProfileDefaultsWhenEmpty(t *testing.T) {
	t.Parallel()

	ctx := buildSubagentRuntimeContext(
		"parent-1", "child-1", "msg-1", "tc-1",
		subagentTask{Name: "t1", Description: "test"},
		config.Agent{ID: config.AgentGeneral, Mode: config.AgentModeSubagent},
		ParentPermissionContext{},
		nil,
		"session",
		"/tmp",
		nil,
	)

	// EffectiveSubagentRuntime returns empty strings for MissingFinishPolicy
	// and DefaultRetryPolicy when the user has not configured them.
	emptyCfg := config.SubagentRuntimeConfig{
		StructuredCompletionRequired: true,
	}
	applySubagentRuntimeConfig(&ctx, emptyCfg)

	// Profile default for general should remain intact.
	require.Equal(t, MissingFinishRetryThenFail, ctx.Result.MissingFinishPolicy)
	require.Equal(t, RetryIdempotent, ctx.Retry.Kind)
	require.True(t, ctx.Result.Required)
}

func TestApplySubagentRuntimeConfig_OverridesProfileDefaultsWhenExplicit(t *testing.T) {
	t.Parallel()

	ctx := buildSubagentRuntimeContext(
		"parent-1", "child-1", "msg-1", "tc-1",
		subagentTask{Name: "t1", Description: "test"},
		config.Agent{ID: config.AgentGeneral, Mode: config.AgentModeSubagent},
		ParentPermissionContext{},
		nil,
		"session",
		"/tmp",
		nil,
	)

	// User explicitly configured missing_finish_policy to "fail".
	overrideCfg := config.SubagentRuntimeConfig{
		StructuredCompletionRequired: true,
		MissingFinishPolicy:          string(MissingFinishFail),
	}
	applySubagentRuntimeConfig(&ctx, overrideCfg)

	// Profile default should be overridden.
	require.Equal(t, MissingFinishFail, ctx.Result.MissingFinishPolicy)
}

func TestApplySubagentRuntimeConfig_AllowsRecursiveAgentsWhenConfigured(t *testing.T) {
	t.Parallel()

	ctx := buildSubagentRuntimeContext(
		"parent-1", "child-1", "msg-1", "tc-1",
		subagentTask{Name: "t1", Description: "test"},
		config.Agent{ID: config.AgentGeneral, Mode: config.AgentModeSubagent, AllowedTools: []string{AgentToolName, "read"}},
		ParentPermissionContext{AllowedTools: []string{AgentToolName, "read"}},
		[]string{AgentToolName, "read"},
		"session",
		"/tmp",
		nil,
	)

	require.False(t, ctx.AgentProfile.CanSpawn)
	require.Contains(t, ctx.ToolProfile.Denied, AgentToolName)

	applySubagentRuntimeConfig(&ctx, config.SubagentRuntimeConfig{
		StructuredCompletionRequired: true,
		AllowRecursiveAgents:         true,
	})

	require.True(t, ctx.AgentProfile.CanSpawn)
	require.True(t, ctx.Permissions.CanSpawn)
	require.Contains(t, ctx.ToolProfile.Allowed, AgentToolName)
	require.NotContains(t, ctx.ToolProfile.Denied, AgentToolName)
}

func TestApplySubagentRuntimeConfig_DoesNotAllowReadOnlyAgentsToSpawn(t *testing.T) {
	t.Parallel()

	ctx := buildSubagentRuntimeContext(
		"parent-1", "child-1", "msg-1", "tc-1",
		subagentTask{Name: "t1", Description: "test"},
		config.Agent{ID: config.AgentReview, Mode: config.AgentModeSubagent, AllowedTools: []string{AgentToolName, "read"}},
		ParentPermissionContext{AllowedTools: []string{AgentToolName, "read"}},
		[]string{AgentToolName, "read"},
		"session",
		"/tmp",
		nil,
	)

	applySubagentRuntimeConfig(&ctx, config.SubagentRuntimeConfig{
		StructuredCompletionRequired: true,
		AllowRecursiveAgents:         true,
	})

	require.True(t, ctx.AgentProfile.ReadOnly)
	require.False(t, ctx.AgentProfile.CanSpawn)
	require.False(t, ctx.Permissions.CanSpawn)
	require.NotContains(t, ctx.ToolProfile.Allowed, AgentToolName)
	require.Contains(t, ctx.ToolProfile.Denied, AgentToolName)
}

func TestApplySubagentRuntimeConfig_DoesNotBypassParentDeniedAgentTool(t *testing.T) {
	t.Parallel()

	ctx := buildSubagentRuntimeContext(
		"parent-1", "child-1", "msg-1", "tc-1",
		subagentTask{Name: "t1", Description: "test"},
		config.Agent{ID: config.AgentGeneral, Mode: config.AgentModeSubagent, AllowedTools: []string{AgentToolName, "read"}},
		ParentPermissionContext{AllowedTools: []string{AgentToolName, "read"}, ExternalDeny: []string{AgentToolName}},
		[]string{AgentToolName, "read"},
		"session",
		"/tmp",
		nil,
	)

	applySubagentRuntimeConfig(&ctx, config.SubagentRuntimeConfig{
		StructuredCompletionRequired: true,
		AllowRecursiveAgents:         true,
	})

	require.False(t, ctx.AgentProfile.CanSpawn)
	require.False(t, ctx.Permissions.CanSpawn)
	require.NotContains(t, ctx.ToolProfile.Allowed, AgentToolName)
	require.Contains(t, ctx.ToolProfile.Denied, AgentToolName)
}
