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
