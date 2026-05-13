package agent

import (
	"testing"

	"github.com/charmbracelet/crush/internal/memory/engine"
	"github.com/stretchr/testify/require"
)

func TestExtractEvents_ValidJSON(t *testing.T) {
	t.Parallel()

	input := `[{"kind":"decision","scope":"project","content":"use sqlite","summary":"Use SQLite","confidence":0.9,"importance":0.7,"tags":["db"]}]`

	events, err := parseExtractedEvents(input)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, engine.MemoryKindDecision, events[0].Kind)
	require.Equal(t, engine.MemoryScopeProject, events[0].Scope)
	require.Equal(t, "use sqlite", events[0].Content)
	require.Equal(t, "Use SQLite", events[0].Summary)
	require.Equal(t, 0.9, events[0].Confidence)
	require.Equal(t, 0.7, events[0].Importance)
	require.Equal(t, []string{"db"}, events[0].Tags)
}

func TestExtractEvents_EmptyArray(t *testing.T) {
	t.Parallel()

	events, err := parseExtractedEvents("[]")
	require.NoError(t, err)
	require.Empty(t, events)
}

func TestExtractEvents_NotJSON(t *testing.T) {
	t.Parallel()

	events, err := parseExtractedEvents("the model said nothing worth extracting")
	require.NoError(t, err)
	require.Empty(t, events)
}

func TestExtractEvents_MultipleEvents(t *testing.T) {
	t.Parallel()

	input := `[
		{"kind":"decision","scope":"project","content":"Use Postgres","summary":"DB choice","confidence":0.9,"importance":0.8},
		{"kind":"preference","scope":"user","content":"User likes tabs","summary":"Indent preference","confidence":0.7,"importance":0.4,"tags":["style"]},
		{"kind":"pitfall","scope":"project","content":"Avoid N+1 queries","summary":"Performance","confidence":0.8,"importance":0.9,"tags":["sql","performance"]}
	]`

	events, err := parseExtractedEvents(input)
	require.NoError(t, err)
	require.Len(t, events, 3)
	require.Equal(t, engine.MemoryKindDecision, events[0].Kind)
	require.Equal(t, engine.MemoryKindPreference, events[1].Kind)
	require.Equal(t, engine.MemoryKindPitfall, events[2].Kind)
}

func TestExtractEvents_SanitizesEmptyContent(t *testing.T) {
	t.Parallel()

	input := `[
		{"kind":"decision","scope":"project","content":"valid","summary":"ok","confidence":0.9,"importance":0.5},
		{"kind":"decision","content":"","summary":"empty kind defaults"},
		{"kind":"","content":"missing kind"}
	]`

	events, err := parseExtractedEvents(input)
	require.NoError(t, err)
	require.Len(t, events, 1, "should only keep the valid event")
	require.Equal(t, "valid", events[0].Content)
}

func TestExtractEvents_DefaultsScope(t *testing.T) {
	t.Parallel()

	input := `[{"kind":"decision","content":"some decision","confidence":0.8,"importance":0.6}]`

	events, err := parseExtractedEvents(input)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, engine.MemoryScopeSession, events[0].Scope, "should default to session scope")
}

func TestExtractEvents_DefaultsConfidenceImportance(t *testing.T) {
	t.Parallel()

	input := `[{"kind":"reference","content":"a reference","scope":"project"}]`

	events, err := parseExtractedEvents(input)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, 0.5, events[0].Confidence, "should default to 0.5 confidence")
	require.Equal(t, 0.5, events[0].Importance, "should default to 0.5 importance")
}

func TestExtractEvents_JSONInMarkdown(t *testing.T) {
	t.Parallel()

	input := "Here are the memories I found:\n\n```json\n[{\"kind\":\"decision\",\"scope\":\"project\",\"content\":\"test\",\"summary\":\"test\",\"confidence\":0.9,\"importance\":0.7}]\n```"

	events, err := parseExtractedEvents(input)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, "test", events[0].Content)
}

func TestExtractEvents_JSONWithSurroundingText(t *testing.T) {
	t.Parallel()

	input := `I analyzed the conversation. [{"kind":"decision","content":"test decision","scope":"project","confidence":0.8,"importance":0.6}] That's all I found.`

	events, err := parseExtractedEvents(input)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, "test decision", events[0].Content)
}

func TestExtractEvents_AllEventKinds(t *testing.T) {
	t.Parallel()

	kinds := []engine.MemoryKind{
		engine.MemoryKindDecision,
		engine.MemoryKindPreference,
		engine.MemoryKindProcedure,
		engine.MemoryKindPitfall,
		engine.MemoryKindReference,
		engine.MemoryKindTaskState,
	}

	for _, kind := range kinds {
		input := `[{"kind":"` + string(kind) + `","content":"test ` + string(kind) + `","scope":"project","confidence":0.8,"importance":0.5}]`
		events, err := parseExtractedEvents(input)
		require.NoError(t, err)
		require.Len(t, events, 1, "should parse kind: %s", kind)
		require.Equal(t, kind, events[0].Kind)
	}
}
