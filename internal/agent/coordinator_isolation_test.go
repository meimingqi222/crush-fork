package agent

import (
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// builtinAgentsForIsolationTest returns the built-in subagent catalog
// (read-only + writers) keyed by canonical ID, so tests don't depend on
// the full config loader.
func builtinAgentsForIsolationTest() map[string]config.Agent {
	ids := []string{
		config.AgentGeneral,
		config.AgentDesigner,
		config.AgentQuickTask,
		config.AgentExplore,
		config.AgentPlan,
		config.AgentReview,
		config.AgentLibrarian,
	}
	out := make(map[string]config.Agent, len(ids))
	for _, id := range ids {
		out[id] = config.Agent{ID: id, Mode: config.AgentModeSubagent}
	}
	return out
}

func TestComputeBatchIsolationDefault(t *testing.T) {
	t.Parallel()
	agents := builtinAgentsForIsolationTest()

	tests := []struct {
		name  string
		tasks []subagentTask
		want  string
	}{
		{
			name: "single writer task does not trigger batch default",
			tasks: []subagentTask{
				{SubagentType: config.AgentGeneral},
			},
			want: "",
		},
		{
			name: "two writer tasks default to worktree",
			tasks: []subagentTask{
				{SubagentType: config.AgentGeneral},
				{SubagentType: config.AgentDesigner},
			},
			want: "worktree",
		},
		{
			name: "writer and read-only task does not trigger (only one writer)",
			tasks: []subagentTask{
				{SubagentType: config.AgentGeneral},
				{SubagentType: config.AgentExplore},
			},
			want: "",
		},
		{
			name: "two read-only tasks do not trigger",
			tasks: []subagentTask{
				{SubagentType: config.AgentExplore},
				{SubagentType: config.AgentReview},
			},
			want: "",
		},
		{
			name: "three tasks with two writers triggers",
			tasks: []subagentTask{
				{SubagentType: config.AgentExplore},
				{SubagentType: config.AgentGeneral},
				{SubagentType: config.AgentQuickTask},
			},
			want: "worktree",
		},
		{
			name: "unknown agent type treated as writer",
			tasks: []subagentTask{
				{SubagentType: "custom-writer"},
				{SubagentType: config.AgentGeneral},
			},
			want: "worktree",
		},
		{
			name:  "empty task list does not trigger",
			tasks: []subagentTask{},
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := computeBatchIsolationDefault(tt.tasks, agents)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestResolveTaskIsolation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		taskIsolation     string
		batchDefault      string
		agentCfgIsolation string
		want              string
	}{
		{
			name:              "task override wins over batch default and agent config",
			taskIsolation:     "worktree",
			batchDefault:      "worktree",
			agentCfgIsolation: "session",
			want:              "worktree",
		},
		{
			name:              "task session override wins over worktree batch default",
			taskIsolation:     "session",
			batchDefault:      "worktree",
			agentCfgIsolation: "session",
			want:              "session",
		},
		{
			name:              "task none explicitly opts out of batch default",
			taskIsolation:     "none",
			batchDefault:      "worktree",
			agentCfgIsolation: "session",
			want:              "",
		},
		{
			name:              "none opt-out with empty agent config falls through to global default",
			taskIsolation:     "none",
			batchDefault:      "worktree",
			agentCfgIsolation: "",
			want:              "",
		},
		{
			name:              "empty task uses batch default",
			taskIsolation:     "",
			batchDefault:      "worktree",
			agentCfgIsolation: "session",
			want:              "worktree",
		},
		{
			name:              "empty task and empty batch uses agent config",
			taskIsolation:     "",
			batchDefault:      "",
			agentCfgIsolation: "session",
			want:              "session",
		},
		{
			name:              "all empty falls through to global default",
			taskIsolation:     "",
			batchDefault:      "",
			agentCfgIsolation: "",
			want:              "",
		},
		{
			name:              "whitespace and case normalized",
			taskIsolation:     "  WorkTree  ",
			batchDefault:      "session",
			agentCfgIsolation: "session",
			want:              "worktree",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := resolveTaskIsolation(tt.taskIsolation, tt.batchDefault, tt.agentCfgIsolation)
			require.Equal(t, tt.want, got)
		})
	}
}
