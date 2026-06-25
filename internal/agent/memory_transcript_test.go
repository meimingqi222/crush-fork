package agent

import (
	"fmt"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestBuildTranscriptWindow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		msgs      []message.Message
		wantEmpty bool
	}{
		{
			name:      "empty messages",
			msgs:      nil,
			wantEmpty: true,
		},
		{
			name: "user message only",
			msgs: []message.Message{
				newTestMessage("test-session", "msg-1", message.User, "Hello world"),
			},
			wantEmpty: false,
		},
		{
			name: "assistant message only",
			msgs: []message.Message{
				newTestMessage("test-session", "msg-1", message.Assistant, "Hi there!"),
			},
			wantEmpty: false,
		},
		{
			name: "mixed messages",
			msgs: []message.Message{
				newTestMessage("test-session", "msg-1", message.User, "Hello"),
				newTestMessage("test-session", "msg-2", message.Assistant, "Hi!"),
				newTestMessage("test-session", "msg-3", message.User, "How are you?"),
			},
			wantEmpty: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := buildTranscriptWindow(tt.msgs)
			if tt.wantEmpty {
				require.Empty(t, result)
			} else {
				require.NotEmpty(t, result)
			}
		})
	}
}

func TestBuildTranscriptWindowTruncation(t *testing.T) {
	t.Parallel()

	// Create messages that would exceed the limit.
	var msgs []message.Message
	for i := 0; i < 1000; i++ {
		msgs = append(msgs, newTestMessage(
			"test-session",
			fmt.Sprintf("msg-%d", i),
			message.User,
			"This is a test message that is reasonably long to test truncation. ",
		))
	}

	result := buildTranscriptWindow(msgs)
	require.NotEmpty(t, result)
	require.LessOrEqual(t, len([]rune(result)), transcriptWindowMaxRunes)
}

func TestStripMemoryTags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "no tags",
			input: "Hello world",
			want:  "Hello world",
		},
		{
			name:  "hindsight_memories block",
			input: "USER: hi\n<system-reminder>\n<hindsight_memories>\n- memory 1\n- memory 2\n</hindsight_memories>\n</system-reminder>",
			want:  "USER: hi\n",
		},
		{
			name:  "mental_models block",
			input: "<mental_models>\nCurated summaries.\n# Model\nContent.\n</mental_models>\nReal content here.",
			want:  "\nReal content here.",
		},
		{
			name:  "system-reminder wrapper only",
			input: "<system-reminder>\nSome reminder.\n</system-reminder>\nActual message.",
			want:  "\nActual message.",
		},
		{
			name:  "legacy relevant_memories block",
			input: "<relevant_memories>\nold memory\n</relevant_memories>\nKeep this.",
			want:  "\nKeep this.",
		},
		{
			name:  "memories block",
			input: "<memories>\nnew memory\n</memories>\nKeep this.",
			want:  "\nKeep this.",
		},
		{
			name:  "multiple blocks in one message",
			input: "<system-reminder>\n<hindsight_memories>\nmem\n</hindsight_memories>\n</system-reminder>\n<mental_models>\nmodel\n</mental_models>\nClean text.",
			want:  "\n\nClean text.",
		},
		{
			name:  "tags with multiline content (dotall)",
			input: "<hindsight_memories>\nline1\nline2\nline3\n</hindsight_memories>\nAfter.",
			want:  "\nAfter.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := stripMemoryTags(tt.input)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestBuildTranscriptWindowStripsMemoryTags(t *testing.T) {
	t.Parallel()

	// Simulate a user message that has memory content injected (as done by
	// FormatAutoRecallMessage wrapping formatEventsAsRecall output).
	userMsg := newTestMessage("sess", "msg-1", message.User,
		"<system-reminder>\n<hindsight_memories>\n- Use SQLite for storage.\n</hindsight_memories>\n</system-reminder>\nHow do I set up the database?")
	assistantMsg := newTestMessage("sess", "msg-2", message.Assistant,
		"<mental_models>\n# Project Conventions\nUse gofumpt.\n</mental_models>\nYou should use SQLite.")

	result := buildTranscriptWindow([]message.Message{userMsg, assistantMsg})

	// Memory tags must not appear in the retained transcript.
	require.NotContains(t, result, "<hindsight_memories>")
	require.NotContains(t, result, "</hindsight_memories>")
	require.NotContains(t, result, "<mental_models>")
	require.NotContains(t, result, "</mental_models>")
	require.NotContains(t, result, "<system-reminder>")
	require.NotContains(t, result, "</system-reminder>")

	// Actual conversation content must be preserved.
	require.Contains(t, result, "How do I set up the database?")
	require.Contains(t, result, "You should use SQLite.")
}

func TestBuildTranscriptWindowEmptyAfterStripping(t *testing.T) {
	t.Parallel()

	// A message that is ONLY memory tags should produce no line (empty after
	// stripping + trimming).
	msg := newTestMessage("sess", "msg-1", message.User,
		"<system-reminder>\n<hindsight_memories>\n- memory\n</hindsight_memories>\n</system-reminder>")
	result := buildTranscriptWindow([]message.Message{msg})
	require.Empty(t, result)
}

// newTestMessage creates a test message.
func newTestMessage(sessionID, id string, role message.MessageRole, text string) message.Message {
	now := time.Now().Unix()
	return message.Message{
		ID:        id,
		SessionID: sessionID,
		Role:      role,
		CreatedAt: now,
		UpdatedAt: now,
		Parts:     []message.ContentPart{message.TextContent{Text: text}},
	}
}
