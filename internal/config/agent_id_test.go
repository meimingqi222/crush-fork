package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfig_AgentIDs(t *testing.T) {
	cfg := &Config{
		Options: &Options{
			DisabledTools: []string{},
		},
	}
	cfg.SetupAgents()

	t.Run("Coder agent should have correct ID", func(t *testing.T) {
		coderAgent, ok := cfg.Agents[AgentCoder]
		require.True(t, ok)
		assert.Equal(t, AgentCoder, coderAgent.ID, "Coder agent ID should be '%s'", AgentCoder)
		assert.Equal(t, "orchestrator", coderAgent.Role)
	})

	t.Run("Task alias should resolve to explore", func(t *testing.T) {
		_, ok := cfg.Agents[AgentTask]
		require.False(t, ok)
		assert.Equal(t, AgentExplore, CanonicalSubagentID(AgentTask))
		assert.Equal(t, "reviewer", CanonicalSubagentID("reviewer"))
		assert.Equal(t, AgentReview, RequestedSubagentID("reviewer"))
		assert.Equal(t, AgentPlan, RequestedSubagentID("planner"))
		assert.Equal(t, AgentQuickTask, RequestedSubagentID("quick"))
	})

	t.Run("General agent should have correct ID", func(t *testing.T) {
		generalAgent, ok := cfg.Agents[AgentGeneral]
		require.True(t, ok)
		assert.Equal(t, AgentGeneral, generalAgent.ID, "General agent ID should be '%s'", AgentGeneral)
		assert.Equal(t, "executor", generalAgent.Role)
	})

	t.Run("Explore agent should have correct ID", func(t *testing.T) {
		exploreAgent, ok := cfg.Agents[AgentExplore]
		require.True(t, ok)
		assert.Equal(t, AgentExplore, exploreAgent.ID, "Explore agent ID should be '%s'", AgentExplore)
		assert.Equal(t, "researcher", exploreAgent.Role)
		assert.Equal(t, SelectedModelTypeSmall, exploreAgent.Model)
	})

	t.Run("Specialized subagents should have correct IDs", func(t *testing.T) {
		planAgent, ok := cfg.Agents[AgentPlan]
		require.True(t, ok)
		assert.Equal(t, AgentPlan, planAgent.ID)
		assert.Equal(t, "planner", planAgent.Role)
		assert.Equal(t, SelectedModelTypePlan, planAgent.Model)

		reviewAgent, ok := cfg.Agents[AgentReview]
		require.True(t, ok)
		assert.Equal(t, AgentReview, reviewAgent.ID)
		assert.Equal(t, "reviewer", reviewAgent.Role)
		assert.Equal(t, SelectedModelTypeReview, reviewAgent.Model)

		designerAgent, ok := cfg.Agents[AgentDesigner]
		require.True(t, ok)
		assert.Equal(t, AgentDesigner, designerAgent.ID)
		assert.Equal(t, "designer", designerAgent.Role)
		assert.Equal(t, SelectedModelTypeDesigner, designerAgent.Model)

		librarianAgent, ok := cfg.Agents[AgentLibrarian]
		require.True(t, ok)
		assert.Equal(t, AgentLibrarian, librarianAgent.ID)
		assert.Equal(t, "researcher", librarianAgent.Role)
		assert.Equal(t, SelectedModelTypeLibrarian, librarianAgent.Model)

		quickTaskAgent, ok := cfg.Agents[AgentQuickTask]
		require.True(t, ok)
		assert.Equal(t, AgentQuickTask, quickTaskAgent.ID)
		assert.Equal(t, "executor", quickTaskAgent.Role)
		assert.Equal(t, SelectedModelTypeQuickTask, quickTaskAgent.Model)
	})
}

func TestSelectedModelForTypeFallsBackForSpecializedAgents(t *testing.T) {
	cfg := &Config{
		Models: map[SelectedModelType]SelectedModel{
			SelectedModelTypeLarge: {Provider: "test", Model: "large"},
			SelectedModelTypeSmall: {Provider: "test", Model: "small"},
		},
	}

	model, ok := cfg.SelectedModelForType(SelectedModelTypeReview)
	require.True(t, ok)
	assert.Equal(t, "large", model.Model)

	cfg.Models[SelectedModelTypeReview] = SelectedModel{Provider: "test", Model: "review"}
	model, ok = cfg.SelectedModelForType(SelectedModelTypeReview)
	require.True(t, ok)
	assert.Equal(t, "review", model.Model)

	model, ok = cfg.SelectedModelForType(SelectedModelTypeQuickTask)
	require.True(t, ok)
	assert.Equal(t, "small", model.Model)

	model, ok = cfg.SelectedModelForType(SelectedModelTypeLibrarian)
	require.True(t, ok)
	assert.Equal(t, "small", model.Model)
}
