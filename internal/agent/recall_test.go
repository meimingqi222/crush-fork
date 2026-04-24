package agent

import (
	"context"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/memory"
	"github.com/stretchr/testify/require"
)

func TestBuildAutoRecallBlockIncludesMemory(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "recall")
	require.NoError(t, err)

	require.NoError(t, env.memory.Store(t.Context(), memory.StoreParams{Key: "project/goal", Value: "Ship MVP search flow", Scope: "project", Category: "product", Type: "goal", Tags: []string{"search", "launch"}}))
	require.NoError(t, env.memory.Store(t.Context(), memory.StoreParams{Key: sessionMemoryKey(sess.ID), Value: "Continue current search migration", Scope: "session", Type: "project"}))

	model := &memoryRelevanceLanguageModel{
		response: `["project/goal"]`,
	}
	bgModel := &backgroundModel{
		model:    Model{Model: model},
		provider: config.ProviderConfig{ID: "test"},
	}

	block := buildAutoRecallBlock(context.Background(), env.memory, bgModel, sess.ID, "search implementation", nil, nil)
	require.Contains(t, block, "Relevant long-term memory:")
	require.Contains(t, block, sessionMemoryKey(sess.ID))
	require.Contains(t, block, "project/goal")
	require.Contains(t, block, "product/goal")
	require.Contains(t, block, "#launch #search")
}

func TestBuildAutoRecallBlockSkipsEmptyResults(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "recall-empty")
	require.NoError(t, err)

	block := buildAutoRecallBlock(context.Background(), env.memory, nil, sess.ID, "unmatched query test", nil, nil)
	require.Empty(t, block)
}

func TestBuildAutoRecallBlockFiltersMemoryByAgentPolicy(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "recall-policy")
	require.NoError(t, err)

	require.NoError(t, env.memory.Store(t.Context(), memory.StoreParams{Key: "scope/project", Value: "Project memory", Scope: "project"}))
	require.NoError(t, env.memory.Store(t.Context(), memory.StoreParams{Key: sessionMemoryKey(sess.ID), Value: "Session memory", Scope: "session"}))

	ctx := context.WithValue(context.Background(), tools.AgentMemoryContextKey, "isolated")
	block := buildAutoRecallBlock(ctx, env.memory, nil, sess.ID, "check memory", nil, nil)
	require.Contains(t, block, sessionMemoryKey(sess.ID))
	require.NotContains(t, block, "scope/project")

	ephemeralCtx := context.WithValue(context.Background(), tools.AgentMemoryContextKey, "ephemeral")
	ephemeralBlock := buildAutoRecallBlock(ephemeralCtx, env.memory, nil, sess.ID, "check memory", nil, nil)
	require.NotContains(t, ephemeralBlock, "Relevant long-term memory:")
}

func TestBuildAutoRecallBlockWithBackgroundModelSkipsOtherSessionMemory(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "recall-bg-default")
	require.NoError(t, err)

	otherSession, err := env.sessions.Create(t.Context(), "other-session")
	require.NoError(t, err)

	require.NoError(t, env.memory.Store(t.Context(), memory.StoreParams{
		Key:   "project/goal",
		Value: "Ship search recall",
		Scope: "project",
	}))
	require.NoError(t, env.memory.Store(t.Context(), memory.StoreParams{
		Key:   sessionMemoryKey(otherSession.ID),
		Value: "Do not leak this other session state",
		Scope: "session",
	}))

	model := &memoryRelevanceLanguageModel{
		response: `["project/goal","` + sessionMemoryKey(otherSession.ID) + `"]`,
	}
	bgModel := &backgroundModel{
		model:    Model{Model: model},
		provider: config.ProviderConfig{ID: "test"},
	}

	block := buildAutoRecallBlock(context.Background(), env.memory, bgModel, sess.ID, "search query", nil, nil)
	require.Contains(t, model.prompt, "project/goal")
	require.Contains(t, block, "project/goal")
	require.NotContains(t, block, sessionMemoryKey(otherSession.ID))
}

func TestBuildAutoRecallBlockWithBackgroundModelHonorsProjectScope(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "recall-bg-project")
	require.NoError(t, err)

	require.NoError(t, env.memory.Store(t.Context(), memory.StoreParams{
		Key:   "project/goal",
		Value: "Project-only search memory",
		Scope: "project",
	}))
	require.NoError(t, env.memory.Store(t.Context(), memory.StoreParams{
		Key:   sessionMemoryKey(sess.ID),
		Value: "Current session state",
		Scope: "session",
	}))

	model := &memoryRelevanceLanguageModel{
		response: `["project/goal","` + sessionMemoryKey(sess.ID) + `"]`,
	}
	bgModel := &backgroundModel{
		model:    Model{Model: model},
		provider: config.ProviderConfig{ID: "test"},
	}

	ctx := context.WithValue(context.Background(), tools.AgentMemoryContextKey, "project")
	block := buildAutoRecallBlock(ctx, env.memory, bgModel, sess.ID, "search query", nil, nil)
	require.Contains(t, model.prompt, "project/goal")
	require.NotContains(t, model.prompt, sessionMemoryKey(sess.ID))
	require.Contains(t, block, "project/goal")
	require.NotContains(t, block, sessionMemoryKey(sess.ID))
}

func TestAutoRecallMemoryScopeRespectsIsolationHints(t *testing.T) {
	t.Parallel()

	scope, include := autoRecallMemoryScope(context.WithValue(context.Background(), tools.AgentIsolationContextKey, "workspace"))
	require.True(t, include)
	require.Equal(t, "project", scope)

	scope, include = autoRecallMemoryScope(context.WithValue(context.Background(), tools.AgentIsolationContextKey, "process"))
	require.True(t, include)
	require.Equal(t, "session", scope)
}

func TestBuildAutoRecallBlockFiltersAlreadySurfaced(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "recall-dedup")
	require.NoError(t, err)

	require.NoError(t, env.memory.Store(t.Context(), memory.StoreParams{
		Key:   "project/goal",
		Value: "Ship search recall",
		Scope: "project",
	}))
	require.NoError(t, env.memory.Store(t.Context(), memory.StoreParams{
		Key:   "project/other",
		Value: "Other project memory",
		Scope: "project",
	}))

	model := &memoryRelevanceLanguageModel{
		response: `["project/other"]`,
	}
	bgModel := &backgroundModel{
		model:    Model{Model: model},
		provider: config.ProviderConfig{ID: "test"},
	}

	// Simulate project/goal already surfaced in a prior turn.
	alreadySurfaced := map[string]bool{"project/goal": true}
	block := buildAutoRecallBlock(context.Background(), env.memory, bgModel, sess.ID, "search query", nil, alreadySurfaced)

	// The manifest sent to the LLM should not include the already-surfaced key.
	require.NotContains(t, model.prompt, "project/goal")
	require.Contains(t, model.prompt, "project/other")
	require.Contains(t, block, "project/other")
	require.NotContains(t, block, "project/goal")
}

func TestMemoryFreshnessText(t *testing.T) {
	t.Parallel()

	// Fresh memory (today) — no caveat.
	now := time.Now().UnixNano()
	require.Empty(t, memoryFreshnessText(now))

	// 1-day-old memory — no caveat (boundary is >1 day).
	oneDayAgo := time.Now().Add(-24 * time.Hour).UnixNano()
	require.Empty(t, memoryFreshnessText(oneDayAgo))

	// 2-day-old memory — caveat present.
	twoDaysAgo := time.Now().Add(-48 * time.Hour).UnixNano()
	caveat := memoryFreshnessText(twoDaysAgo)
	require.Contains(t, caveat, "2 days old")
	require.Contains(t, caveat, "point-in-time observations")

	// 30-day-old memory.
	thirtyDaysAgo := time.Now().Add(-30 * 24 * time.Hour).UnixNano()
	caveat = memoryFreshnessText(thirtyDaysAgo)
	require.Contains(t, caveat, "30 days old")

	// Zero / negative — no caveat.
	require.Empty(t, memoryFreshnessText(0))
}

func TestBuildAutoRecallBlockIncludesStalenessCaveat(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "recall-stale")
	require.NoError(t, err)

	// Store a memory that is "old" (UpdatedAt in the past).
	require.NoError(t, env.memory.Store(t.Context(), memory.StoreParams{
		Key:   "project/old-decision",
		Value: "Use library X for search",
		Scope: "project",
	}))

	model := &memoryRelevanceLanguageModel{
		response: `["project/old-decision"]`,
	}
	bgModel := &backgroundModel{
		model:    Model{Model: model},
		provider: config.ProviderConfig{ID: "test"},
	}

	block := buildAutoRecallBlock(context.Background(), env.memory, bgModel, sess.ID, "search query", nil, nil)
	require.Contains(t, block, "project/old-decision")
	// The memory was just created so it should be fresh — no staleness caveat.
	require.NotContains(t, block, "days old")
}

func TestMaxSessionRecallBytes(t *testing.T) {
	t.Parallel()

	require.Equal(t, 60*1024, maxSessionRecallBytes)
}
