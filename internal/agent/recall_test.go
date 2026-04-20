package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/memory"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestBuildAutoRecallBlockIncludesMemoryAndHistory(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "recall")
	require.NoError(t, err)

	require.NoError(t, env.memory.Store(t.Context(), memory.StoreParams{Key: "project/goal", Value: "Ship MVP search flow", Scope: "project", Category: "product", Type: "goal", Tags: []string{"search", "launch"}}))
	require.NoError(t, env.memory.Store(t.Context(), memory.StoreParams{Key: sessionMemoryKey(sess.ID), Value: "Continue current search migration", Scope: "session", Type: "project"}))
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "Remember the search implementation details"}},
	})
	require.NoError(t, err)

	block := buildAutoRecallBlock(context.Background(), env.history, env.memory, nil, sess.ID, "search")
	require.Contains(t, block, "Relevant long-term memory:")
	require.Contains(t, block, sessionMemoryKey(sess.ID))
	require.Contains(t, block, "project/goal")
	require.Contains(t, block, "product/goal")
	require.Contains(t, block, "#launch #search")
	require.Contains(t, block, "Relevant session history:")
	require.Contains(t, block, "search implementation details")
}

func TestBuildAutoRecallBlockMarksVerifiedAndMissingPaths(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "recall-paths")
	require.NoError(t, err)

	workingDir := t.TempDir()
	existingPath := filepath.Join(workingDir, "internal", "agent", "recall.go")
	require.NoError(t, os.MkdirAll(filepath.Dir(existingPath), 0o755))
	require.NoError(t, os.WriteFile(existingPath, []byte("package agent\n"), 0o644))

	memoryValue := "Recall D:/archive/notes.txt and internal/agent/recall.go plus docs/missing.md for follow-up"
	if runtime.GOOS != "windows" {
		memoryValue = "Recall /definitely/missing/notes.txt and internal/agent/recall.go plus docs/missing.md for follow-up"
	}

	require.NoError(t, env.memory.Store(t.Context(), memory.StoreParams{
		Key:   "project/paths",
		Scope: "project",
		Value: memoryValue,
	}))

	ctx := context.WithValue(context.Background(), tools.WorkingDirContextKey, workingDir)
	block := buildAutoRecallBlock(ctx, env.history, env.memory, nil, sess.ID, "paths")
	require.Contains(t, block, "Path checks:")
	require.Contains(t, block, "internal/agent/recall.go (verified)")
	require.Contains(t, block, "docs/missing.md (missing)")
	if runtime.GOOS == "windows" {
		require.Contains(t, block, "D:/archive/notes.txt (missing)")
	} else {
		require.Contains(t, block, "/definitely/missing/notes.txt (missing)")
	}
}

func TestBuildAutoRecallBlockMarksDirectoryLikePathsUnverified(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "recall-unverified")
	require.NoError(t, err)

	workingDir := t.TempDir()
	require.NoError(t, env.memory.Store(t.Context(), memory.StoreParams{
		Key:   "project/layout",
		Scope: "project",
		Value: "Investigate src/components and assets/icons before changing anything.",
	}))

	ctx := context.WithValue(context.Background(), tools.WorkingDirContextKey, workingDir)
	block := buildAutoRecallBlock(ctx, env.history, env.memory, nil, sess.ID, "layout")
	require.Contains(t, block, "src/components (unverified)")
	require.Contains(t, block, "assets/icons (unverified)")
}

func TestBuildAutoRecallBlockIgnoresNonPathLikeMemoryText(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "recall-no-paths")
	require.NoError(t, err)

	require.NoError(t, env.memory.Store(t.Context(), memory.StoreParams{
		Key:   "project/note",
		Scope: "project",
		Value: "Keep search launch status green and coordinate with LaunchPlan owners.",
	}))

	block := buildAutoRecallBlock(context.Background(), env.history, env.memory, nil, sess.ID, "launch")
	require.NotContains(t, block, "Path checks:")
}

func TestBuildAutoRecallBlockSkipsEmptyResults(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "recall-empty")
	require.NoError(t, err)

	block := buildAutoRecallBlock(context.Background(), env.history, env.memory, nil, sess.ID, "unmatched query")
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
	block := buildAutoRecallBlock(ctx, env.history, env.memory, nil, sess.ID, "memory")
	require.Contains(t, block, sessionMemoryKey(sess.ID))
	require.NotContains(t, block, "scope/project")

	ephemeralCtx := context.WithValue(context.Background(), tools.AgentMemoryContextKey, "ephemeral")
	ephemeralBlock := buildAutoRecallBlock(ephemeralCtx, env.history, env.memory, nil, sess.ID, "memory")
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

	block := buildAutoRecallBlock(context.Background(), env.history, env.memory, bgModel, sess.ID, "search")
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
	block := buildAutoRecallBlock(ctx, env.history, env.memory, bgModel, sess.ID, "search")
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

func TestFormatAutoRecallHistoryUsesSessionScopedResults(t *testing.T) {
	t.Parallel()

	results := []history.MessageSearchResult{{Role: message.User, Text: "alpha"}, {Role: message.Assistant, Text: "beta"}}
	formatted := formatAutoRecallHistory(results)
	require.Contains(t, formatted, "[user] alpha")
	require.Contains(t, formatted, "[assistant] beta")
}
