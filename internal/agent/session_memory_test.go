package agent

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/memory/engine"
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

func TestUpdateSessionMemoryStoresWorkingMemoryEvent(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	model := &sessionMemoryLanguageModel{
		response: `[{"action":"update","key":"ignored","description":"Current session state","content":"Current goal: finish memory cleanup\nNext: verify tests.","type":"project","scope":"session"}]`,
	}
	bgModel := &backgroundModel{model: Model{Model: model}, provider: config.ProviderConfig{ID: "test"}}

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	eng := engine.New(conn, engine.Config{Enabled: true})

	sess, err := env.sessions.Create(t.Context(), "session-memory")
	require.NoError(t, err)
	(*env.filetracker).RecordRead(t.Context(), sess.ID, env.workingDir+"/internal/agent/recall.go")

	updateSessionMemoryEventStore(t.Context(), eng.EventStore(), bgModel, *env.filetracker, sess.ID, "continue memory work", []string{
		"USER: continue memory work",
		"ASSISTANT: updated memory cleanup",
	})

	content := readWorkingMemoryContent(t.Context(), eng.EventStore(), sess.ID)
	require.Contains(t, content, "Current goal: finish memory cleanup")
	require.Contains(t, model.prompt, "Recent conversation:")
	require.Contains(t, model.prompt, "Recently read files:")
	require.Contains(t, model.prompt, "internal/agent/recall.go")
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

	agent.finishPendingExtraction("session-1", secondID)
	require.NotContains(t, agent.pendingExtractions, "session-1")
}

func TestBuildSessionMemoryHistoryCapsRecentMessages(t *testing.T) {
	t.Parallel()

	history := make([]string, 0, sessionMemoryMaxHistory+3)
	for i := 0; i < sessionMemoryMaxHistory+3; i++ {
		history = append(history, "msg")
	}

	block := buildSessionMemoryHistory(history)
	require.Len(t, strings.Split(block, "\n\n"), sessionMemoryMaxHistory)
}

func TestUpdateSessionMemorySupersedesOldEntry(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	model := &sessionMemoryLanguageModel{
		response: `[{"content":"first memory"}]`,
	}
	bgModel := &backgroundModel{model: Model{Model: model}, provider: config.ProviderConfig{ID: "test"}}

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	eng := engine.New(conn, engine.Config{Enabled: true})

	sess, err := env.sessions.Create(t.Context(), "session-supersede")
	require.NoError(t, err)

	// 第一条 memory
	updateSessionMemoryEventStore(t.Context(), eng.EventStore(), bgModel, *env.filetracker, sess.ID, "prompt-1", []string{
		"USER: prompt-1",
	})
	first := readWorkingMemoryContent(t.Context(), eng.EventStore(), sess.ID)
	require.Equal(t, "first memory", first)

	// 第二条 memory 应该 supersede 第一条
	model.response = `[{"content":"second memory"}]`
	updateSessionMemoryEventStore(t.Context(), eng.EventStore(), bgModel, *env.filetracker, sess.ID, "prompt-2", []string{
		"USER: prompt-2",
	})
	second := readWorkingMemoryContent(t.Context(), eng.EventStore(), sess.ID)
	require.Equal(t, "second memory", second)
}
