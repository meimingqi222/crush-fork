package agent

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/memory"
	"github.com/stretchr/testify/require"
)

type sessionMemoryLanguageModel struct {
	response string
	prompt   string
}

func (m *sessionMemoryLanguageModel) Generate(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
	for _, msg := range call.Prompt {
		for _, part := range msg.Content {
			if textPart, ok := part.(fantasy.TextPart); ok {
				m.prompt = textPart.Text
			}
		}
	}
	return &fantasy.Response{
		Content: fantasy.ResponseContent{fantasy.TextContent{Text: m.response}},
	}, nil
}

func (m *sessionMemoryLanguageModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	panic("unexpected Stream call")
}

func (m *sessionMemoryLanguageModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	panic("unexpected GenerateObject call")
}

func (m *sessionMemoryLanguageModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	panic("unexpected StreamObject call")
}

func (m *sessionMemoryLanguageModel) Provider() string {
	return "test"
}

func (m *sessionMemoryLanguageModel) Model() string {
	return "session-memory"
}

func TestUpdateSessionMemoryStoresSessionScopedEntry(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	model := &sessionMemoryLanguageModel{
		response: `[{"action":"update","key":"ignored","description":"Current session state","content":"Current goal: finish memory cleanup\nNext: verify tests.","type":"project","scope":"session"}]`,
	}
	bgModel := &backgroundModel{model: Model{Model: model}, provider: config.ProviderConfig{ID: "test"}}

	sess, err := env.sessions.Create(t.Context(), "session-memory")
	require.NoError(t, err)
	(*env.filetracker).RecordRead(t.Context(), sess.ID, env.workingDir+"/internal/agent/recall.go")

	updateSessionMemory(t.Context(), env.memory, bgModel, *env.filetracker, sess.ID, "continue memory work", []string{
		"USER: continue memory work",
		"ASSISTANT: updated dream cleanup",
	})

	entry, err := env.memory.Get(t.Context(), sessionMemoryKey(sess.ID))
	require.NoError(t, err)
	require.Equal(t, "session", entry.Scope)
	require.Equal(t, "project", entry.Type)
	require.Contains(t, entry.Value, "Current goal: finish memory cleanup")
	require.Contains(t, model.prompt, "Recent conversation:")
	require.Contains(t, model.prompt, "Recently read files:")
	require.Contains(t, model.prompt, "internal/agent/recall.go")
}

func TestRecallEntriesForSessionPrependsSessionMemory(t *testing.T) {
	t.Parallel()

	memorySvc, err := memory.NewService(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, memorySvc.Store(t.Context(), memory.StoreParams{Key: "project/general", Value: "# General\n\nsearch memory", Scope: "project"}))
	require.NoError(t, memorySvc.Store(t.Context(), memory.StoreParams{Key: sessionMemoryKey("sess-1"), Value: "# Current session state\n\ncontinue search migration", Scope: "session"}))

	model := &memoryRelevanceLanguageModel{
		response: `["project/general"]`,
	}
	bgModel := &backgroundModel{
		model:    Model{Model: model},
		provider: config.ProviderConfig{ID: "test"},
	}

	entries := recallEntriesForSession(t.Context(), memorySvc, bgModel, "sess-1", "search", "", nil, nil)
	require.NotEmpty(t, entries)
	require.Equal(t, sessionMemoryKey("sess-1"), entries[0].Key)
	found := false
	for _, entry := range entries {
		if entry.Key == "project/general" {
			found = true
			break
		}
	}
	require.True(t, found)
}

func TestRecallEntriesForSessionSkipsSessionMemoryForProjectScope(t *testing.T) {
	t.Parallel()

	memorySvc, err := memory.NewService(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, memorySvc.Store(t.Context(), memory.StoreParams{Key: "project/general", Value: "# General\n\nsearch memory", Scope: "project"}))
	require.NoError(t, memorySvc.Store(t.Context(), memory.StoreParams{Key: sessionMemoryKey("sess-1"), Value: "# Current session state\n\ncontinue search migration", Scope: "session"}))

	model := &memoryRelevanceLanguageModel{
		response: `["project/general"]`,
	}
	bgModel := &backgroundModel{
		model:    Model{Model: model},
		provider: config.ProviderConfig{ID: "test"},
	}

	entries := recallEntriesForSession(t.Context(), memorySvc, bgModel, "sess-1", "search", "project", nil, nil)
	require.NotEmpty(t, entries)
	for _, entry := range entries {
		require.NotEqual(t, sessionMemoryKey("sess-1"), entry.Key)
	}
}

func TestFinishPendingExtractionRemovesOnlyMatchingEntry(t *testing.T) {
	t.Parallel()

	agent := &sessionAgent{
		pendingExtractions: make(map[string][]pendingExtraction),
	}

	_, cancel1 := context.WithCancel(t.Context())
	_, cancel2 := context.WithCancel(t.Context())

	agent.extractionMu.Lock()
	firstID := agent.trackPendingExtractionLocked("session-1", cancel1)
	secondID := agent.trackPendingExtractionLocked("session-1", cancel2)
	agent.extractionMu.Unlock()

	agent.finishPendingExtraction("session-1", firstID)

	require.Contains(t, agent.pendingExtractions, "session-1")
	require.Len(t, agent.pendingExtractions["session-1"], 1)
	require.Equal(t, secondID, agent.pendingExtractions["session-1"][0].id)
}

func TestBuildSessionMemoryHistoryTrimsToRecentTurns(t *testing.T) {
	t.Parallel()

	history := make([]string, 0, sessionMemoryMaxHistory+2)
	for i := range sessionMemoryMaxHistory + 2 {
		history = append(history, "turn "+strings.Repeat("x", i+1))
	}
	block := buildSessionMemoryHistory(history)
	require.NotContains(t, block, history[0]+"\n\n")
	require.Contains(t, block, history[len(history)-1])
}

func TestShouldUpdateSessionMemory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                    string
		initialized             bool
		currentPromptTokens     int64
		tokensAtLastExtraction  int64
		toolCallsSinceLast      int
		currentRunToolUses      int
		wantShouldUpdate        bool
		wantInitializedAfterRun bool
	}{
		{
			name:                    "does not initialize below token threshold",
			currentPromptTokens:     sessionMemoryInitializationTokens - 1,
			wantShouldUpdate:        false,
			wantInitializedAfterRun: false,
		},
		{
			name:                    "initializes and updates at first natural break",
			currentPromptTokens:     sessionMemoryInitializationTokens,
			wantShouldUpdate:        true,
			wantInitializedAfterRun: true,
		},
		{
			name:                    "updates at natural break after enough growth",
			initialized:             true,
			currentPromptTokens:     sessionMemoryInitializationTokens + sessionMemoryMinimumTokensBetweenTurns,
			currentRunToolUses:      0,
			wantShouldUpdate:        true,
			wantInitializedAfterRun: true,
		},
		{
			name:                    "waits during tool-heavy turn until tool threshold met",
			initialized:             true,
			currentPromptTokens:     sessionMemoryInitializationTokens + sessionMemoryMinimumTokensBetweenTurns,
			currentRunToolUses:      2,
			toolCallsSinceLast:      sessionMemoryToolCallsBetweenUpdates - 1,
			wantShouldUpdate:        false,
			wantInitializedAfterRun: true,
		},
		{
			name:                    "updates during tool-heavy turn after tool threshold",
			initialized:             true,
			currentPromptTokens:     sessionMemoryInitializationTokens + sessionMemoryMinimumTokensBetweenTurns,
			currentRunToolUses:      2,
			toolCallsSinceLast:      sessionMemoryToolCallsBetweenUpdates,
			wantShouldUpdate:        true,
			wantInitializedAfterRun: true,
		},
		{
			name:                    "requires token growth even if tool threshold met",
			initialized:             true,
			currentPromptTokens:     sessionMemoryInitializationTokens + sessionMemoryMinimumTokensBetweenTurns - 1,
			tokensAtLastExtraction:  sessionMemoryInitializationTokens,
			toolCallsSinceLast:      sessionMemoryToolCallsBetweenUpdates,
			currentRunToolUses:      1,
			wantShouldUpdate:        false,
			wantInitializedAfterRun: true,
		},
		{
			name:                    "handles token underflow after compaction by requiring growth from new baseline",
			initialized:             true,
			currentPromptTokens:     sessionMemoryInitializationTokens - 1000,
			tokensAtLastExtraction:  sessionMemoryInitializationTokens + 1000,
			toolCallsSinceLast:      sessionMemoryToolCallsBetweenUpdates,
			currentRunToolUses:      0,
			wantShouldUpdate:        false,
			wantInitializedAfterRun: true,
		},
		{
			name:                    "updates after compaction once enough tokens accumulate from new baseline",
			initialized:             true,
			currentPromptTokens:     sessionMemoryInitializationTokens + 1000 + sessionMemoryMinimumTokensBetweenTurns,
			tokensAtLastExtraction:  sessionMemoryInitializationTokens + 1000,
			toolCallsSinceLast:      0,
			currentRunToolUses:      0,
			wantShouldUpdate:        true,
			wantInitializedAfterRun: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotShouldUpdate, gotInitialized := shouldUpdateSessionMemory(
				tt.initialized,
				tt.currentPromptTokens,
				tt.tokensAtLastExtraction,
				tt.toolCallsSinceLast,
				tt.currentRunToolUses,
			)
			require.Equal(t, tt.wantShouldUpdate, gotShouldUpdate)
			require.Equal(t, tt.wantInitializedAfterRun, gotInitialized)
		})
	}
}
