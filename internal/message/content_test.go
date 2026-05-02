package message

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func makeTestAttachments(n int, contentSize int) []Attachment {
	attachments := make([]Attachment, n)
	content := []byte(strings.Repeat("x", contentSize))
	for i := range n {
		attachments[i] = Attachment{
			FilePath: fmt.Sprintf("/path/to/file%d.txt", i),
			MimeType: "text/plain",
			Content:  content,
		}
	}
	return attachments
}

func TestMessageIsThinkingEndsWhenToolCallStarts(t *testing.T) {
	t.Parallel()

	msg := Message{
		Role: Assistant,
		Parts: []ContentPart{
			ReasoningContent{Thinking: "I should inspect the file."},
		},
	}
	require.True(t, msg.IsThinking())

	msg.AddToolCall(ToolCall{
		ID:   "call-1",
		Name: "view",
	})

	require.False(t, msg.IsThinking())
}

func TestFinishThinkingPreservesReasoningMetadata(t *testing.T) {
	t.Parallel()

	msg := Message{
		Role: Assistant,
		Parts: []ContentPart{
			ReasoningContent{
				Thinking:         "thinking",
				Signature:        "sig",
				ThoughtSignature: "thought-sig",
				ToolID:           "tool-1",
				StartedAt:        10,
			},
		},
	}

	msg.FinishThinking()
	reasoning := msg.ReasoningContent()
	require.Equal(t, "thinking", reasoning.Thinking)
	require.Equal(t, "sig", reasoning.Signature)
	require.Equal(t, "thought-sig", reasoning.ThoughtSignature)
	require.Equal(t, "tool-1", reasoning.ToolID)
	require.Equal(t, int64(10), reasoning.StartedAt)
	require.NotZero(t, reasoning.FinishedAt)
}

func TestSetReasoningThinkingPreservesMetadata(t *testing.T) {
	t.Parallel()

	msg := Message{
		Role: Assistant,
		Parts: []ContentPart{
			ReasoningContent{
				Thinking:         "old",
				Signature:        "sig",
				ThoughtSignature: "thought-sig",
				ToolID:           "tool-1",
				StartedAt:        10,
				FinishedAt:       20,
			},
		},
	}

	msg.SetReasoningThinking("new")
	reasoning := msg.ReasoningContent()
	require.Equal(t, "new", reasoning.Thinking)
	require.Equal(t, "sig", reasoning.Signature)
	require.Equal(t, "thought-sig", reasoning.ThoughtSignature)
	require.Equal(t, "tool-1", reasoning.ToolID)
	require.Equal(t, int64(10), reasoning.StartedAt)
	require.Equal(t, int64(20), reasoning.FinishedAt)
}

func TestStripTextualToolCallProtocol(t *testing.T) {
	t.Parallel()

	cleaned, changed := StripTextualToolCallProtocol("before\n<|tool_calls_section_begin|><|tool_call_begin|>functions.view:25<|tool_call_argument_begin|>{\"file_path\":\"main.go\"}<|tool_call_end|><|tool_calls_section_end|>\nafter")

	require.True(t, changed)
	require.Equal(t, "before\n\nafter", cleaned)
}

func BenchmarkPromptWithTextAttachments(b *testing.B) {
	cases := []struct {
		name        string
		numFiles    int
		contentSize int
	}{
		{"1file_100bytes", 1, 100},
		{"5files_1KB", 5, 1024},
		{"10files_10KB", 10, 10 * 1024},
		{"20files_50KB", 20, 50 * 1024},
	}

	for _, tc := range cases {
		attachments := makeTestAttachments(tc.numFiles, tc.contentSize)
		prompt := "Process these files"

		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				_ = PromptWithTextAttachments(prompt, attachments)
			}
		})
	}
}
