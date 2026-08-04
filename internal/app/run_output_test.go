package app

import (
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestShouldStreamMessageToOutput(t *testing.T) {
	t.Parallel()

	const sessionID = "sess-1"
	parts := []message.ContentPart{message.TextContent{Text: "hello"}}

	tests := []struct {
		name string
		msg  message.Message
		want bool
	}{
		{
			name: "assistant reply in session is streamed",
			msg:  message.Message{SessionID: sessionID, Role: message.Assistant, Parts: parts},
			want: true,
		},
		{
			// Compaction summaries are assistant messages in the same
			// session; streaming them would dump the structured summary
			// into `crush run`'s stdout alongside the real answer.
			name: "compaction summary is suppressed",
			msg: message.Message{
				SessionID:        sessionID,
				Role:             message.Assistant,
				Parts:            parts,
				IsSummaryMessage: true,
			},
			want: false,
		},
		{
			name: "other session is ignored",
			msg:  message.Message{SessionID: "other", Role: message.Assistant, Parts: parts},
			want: false,
		},
		{
			name: "user message is ignored",
			msg:  message.Message{SessionID: sessionID, Role: message.User, Parts: parts},
			want: false,
		},
		{
			name: "tool message is ignored",
			msg:  message.Message{SessionID: sessionID, Role: message.Tool, Parts: parts},
			want: false,
		},
		{
			name: "assistant message without parts is ignored",
			msg:  message.Message{SessionID: sessionID, Role: message.Assistant},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, shouldStreamMessageToOutput(tt.msg, sessionID))
		})
	}
}
