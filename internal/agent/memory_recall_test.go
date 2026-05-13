package agent

import (
	"context"
	"fmt"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/memory"
	"github.com/stretchr/testify/require"
)

type memoryRelevanceLanguageModel struct {
	prompt   string
	response string
}

func (m *memoryRelevanceLanguageModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	panic("unexpected Generate call")
}

func (m *memoryRelevanceLanguageModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	for _, msg := range call.Prompt {
		for _, part := range msg.Content {
			if textPart, ok := part.(fantasy.TextPart); ok {
				m.prompt = textPart.Text
			}
		}
	}
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "selection"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "selection", Delta: m.response}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "selection"}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}

func (m *memoryRelevanceLanguageModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	panic("unexpected GenerateObject call")
}

func (m *memoryRelevanceLanguageModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	panic("unexpected StreamObject call")
}

func (m *memoryRelevanceLanguageModel) Provider() string {
	return "test"
}

func (m *memoryRelevanceLanguageModel) Model() string {
	return "memory-relevance"
}

func TestParseMemorySelectionResponse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{
			name:     "valid JSON array",
			input:    `["project/goal", "user/preferred-language"]`,
			expected: []string{"project/goal", "user/preferred-language"},
		},
		{
			name:     "wrapped in markdown",
			input:    "Here are the relevant memories:\n```json\n[\"key1\", \"key2\"]\n```",
			expected: []string{"key1", "key2"},
		},
		{
			name:     "empty array",
			input:    "[]",
			expected: nil,
		},
		{
			name:     "no JSON found",
			input:    "No relevant memories found.",
			expected: nil,
		},
		{
			name:     "deduplicates keys",
			input:    `["key1", "key1", "key2"]`,
			expected: []string{"key1", "key2"},
		},
		{
			name:     "respects max selected",
			input:    `["k1", "k2", "k3", "k4", "k5", "k6", "k7"]`,
			expected: []string{"k1", "k2", "k3", "k4", "k5"},
		},
		{
			name:     "skips empty keys",
			input:    `["key1", "", "key2"]`,
			expected: []string{"key1", "key2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := parseMemorySelectionResponse(tt.input)
			require.Equal(t, tt.expected, result)
		})
	}
}

func TestBuildMemoryManifest(t *testing.T) {
	t.Parallel()

	infos := []memory.MemoryFileInfo{
		{Key: "project/goal", Description: "Ship MVP", Type: "project", Tags: []string{"roadmap"}},
		{Key: "user/lang", Description: "Prefers Go", Type: "user"},
	}

	manifest := buildMemoryManifest(infos)
	require.Contains(t, manifest, "project/goal")
	require.Contains(t, manifest, "Ship MVP")
	require.Contains(t, manifest, "#roadmap")
	require.Contains(t, manifest, "user/lang")
	require.Contains(t, manifest, "Prefers Go")
}

func TestSelectRelevantMemoriesSkipsSessionScopedEntriesByDefault(t *testing.T) {
	t.Parallel()

	memorySvc, err := memory.NewService(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, memorySvc.Store(t.Context(), memory.StoreParams{Key: "project/goal", Value: "# Goal\n\nShip search", Scope: "project"}))
	require.NoError(t, memorySvc.Store(t.Context(), memory.StoreParams{Key: "session/other/current", Value: "# Current session state\n\nDo not leak", Scope: "session"}))

	model := &memoryRelevanceLanguageModel{
		response: `["project/goal","session/other/current"]`,
	}

	entries := selectRelevantMemories(t.Context(), memorySvc, Model{Model: model}, config.ProviderConfig{ID: "test"}, "search", "", nil, nil)
	require.Len(t, entries, 1)
	require.Equal(t, "project/goal", entries[0].Key)
	require.NotContains(t, model.prompt, "session/other/current")
}

func TestSelectRelevantMemoriesUsesExpandedCandidatePool(t *testing.T) {
	t.Parallel()

	memorySvc, err := memory.NewService(t.TempDir())
	require.NoError(t, err)

	for i := range memoryRelevanceMaxFiles + 5 {
		require.NoError(t, memorySvc.Store(t.Context(), memory.StoreParams{
			Key:   "project/memory-" + string(rune('a'+(i%26))) + "-" + string(rune('0'+(i%10))),
			Value: fmt.Sprintf("uniquetoken%d", i),
			Scope: "project",
			Type:  "project",
		}))
	}

	lateKey := "project/target-memory"
	require.NoError(t, memorySvc.Store(t.Context(), memory.StoreParams{
		Key:   lateKey,
		Value: "Target memory value",
		Scope: "project",
		Type:  "project",
	}))

	model := &memoryRelevanceLanguageModel{response: `["project/target-memory"]`}
	entries := selectRelevantMemories(t.Context(), memorySvc, Model{Model: model}, config.ProviderConfig{ID: "test"}, "target", "", nil, nil)
	require.Len(t, entries, 1)
	require.Equal(t, lateKey, entries[0].Key)
	require.Contains(t, model.prompt, lateKey)
}

func TestParseExtractedMemories(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected []extractedMemory
	}{
		{
			name:     "valid memories",
			input:    `[{"key": "user/style", "description": "Concise code", "content": "User prefers concise code"}]`,
			expected: []extractedMemory{{Key: "user/style", Description: "Concise code", Content: "User prefers concise code", Action: "store"}},
		},
		{
			name:     "multiple memories",
			input:    `[{"key": "k1", "description": "d1", "content": "c1"}, {"key": "k2", "description": "d2", "content": "c2"}]`,
			expected: []extractedMemory{{Key: "k1", Description: "d1", Content: "c1", Action: "store"}, {Key: "k2", Description: "d2", Content: "c2", Action: "store"}},
		},
		{
			name:     "skips invalid entries",
			input:    `[{"key": "", "description": "d", "content": "c"}, {"key": "k", "description": "d", "content": ""}, {"key": "valid", "description": "desc", "content": "content"}]`,
			expected: []extractedMemory{{Key: "valid", Description: "desc", Content: "content", Action: "store"}},
		},
		{
			name:     "empty array",
			input:    `[]`,
			expected: []extractedMemory{},
		},
		{
			name:     "no JSON array",
			input:    "No memories to extract",
			expected: nil,
		},
		{
			name:     "default description",
			input:    `[{"key": "test", "content": "content"}]`,
			expected: []extractedMemory{{Key: "test", Description: "Extracted from conversation", Content: "content", Action: "store"}},
		},
		{
			name:     "preserves delete actions",
			input:    `[{"action": "delete", "key": "project/obsolete"}]`,
			expected: []extractedMemory{{Key: "project/obsolete", Action: "delete"}},
		},
		{
			name:     "normalizes update actions",
			input:    `[{"action": "update", "key": "user/style", "description": "d", "content": "c"}]`,
			expected: []extractedMemory{{Key: "user/style", Description: "d", Content: "c", Action: "update"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := parseExtractedMemories(tt.input)
			require.Equal(t, tt.expected, result)
		})
	}
}
