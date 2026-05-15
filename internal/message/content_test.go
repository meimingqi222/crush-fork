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
		Name: "read",
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

	cleaned, changed := StripTextualToolCallProtocol("before\n<|tool_calls_section_begin|><|tool_call_begin|>functions.read:25<|tool_call_argument_begin|>{\"path\":\"main.go\"}<|tool_call_end|><|tool_calls_section_end|>\nafter")

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

func TestFilterNonTextContent(t *testing.T) {
	t.Parallel()

	msgs := []Message{
		{
			Role: User,
			Parts: []ContentPart{
				TextContent{Text: "Hello"},
				ImageURLContent{URL: "http://example.com/image.png"},
			},
		},
		{
			Role: Assistant,
			Parts: []ContentPart{
				TextContent{Text: "Hi there"},
				BinaryContent{
					Path:     "/path/to/image.jpg",
					MIMEType: "image/jpeg",
					Data:     []byte("fake-image-data"),
				},
			},
		},
		{
			Role: User,
			Parts: []ContentPart{
				TextContent{Text: "How are you?"},
			},
		},
	}

	filtered := FilterNonTextContent(msgs)

	require.Equal(t, len(msgs), len(filtered), "Should preserve message count")

	// First message should have text but not image
	require.Equal(t, 1, len(filtered[0].Parts), "First message should have 1 part after filtering")
	_, hasImage := filtered[0].Parts[0].(ImageURLContent)
	require.False(t, hasImage, "First message should not have ImageURLContent")
	_, hasText := filtered[0].Parts[0].(TextContent)
	require.True(t, hasText, "First message should have TextContent")

	// Second message should have text but not binary content
	require.Equal(t, 1, len(filtered[1].Parts), "Second message should have 1 part after filtering")
	_, hasBinary := filtered[1].Parts[0].(BinaryContent)
	require.False(t, hasBinary, "Second message should not have BinaryContent")
	_, hasText2 := filtered[1].Parts[0].(TextContent)
	require.True(t, hasText2, "Second message should have TextContent")

	// Third message should be unchanged (no non-text content)
	require.Equal(t, 1, len(filtered[2].Parts), "Third message should have 1 part")
	_, hasText3 := filtered[2].Parts[0].(TextContent)
	require.True(t, hasText3, "Third message should have TextContent")
}

func TestFilterNonTextContentWithPDF(t *testing.T) {
	t.Parallel()

	msgs := []Message{
		{
			Role: User,
			Parts: []ContentPart{
				TextContent{Text: "Here is a PDF"},
				BinaryContent{
					Path:     "/path/to/document.pdf",
					MIMEType: "application/pdf",
					Data:     []byte("fake-pdf-data"),
				},
			},
		},
	}

	filtered := FilterNonTextContent(msgs)

	require.Equal(t, len(msgs), len(filtered), "Should preserve message count")
	// All binary content should be filtered for non-multimodal models
	require.Equal(t, 1, len(filtered[0].Parts), "Binary content should be filtered")
	_, hasPDF := filtered[0].Parts[0].(BinaryContent)
	require.False(t, hasPDF, "PDF binary content should be filtered")
	_, hasText := filtered[0].Parts[0].(TextContent)
	require.True(t, hasText, "Text content should be preserved")
}

func TestFilterNonTextContentOnlyNonText(t *testing.T) {
	t.Parallel()

	msgs := []Message{
		{
			Role: User,
			Parts: []ContentPart{
				ImageURLContent{URL: "http://example.com/image.png"},
			},
		},
		{
			Role: User,
			Parts: []ContentPart{
				BinaryContent{
					Path:     "/path/to/image.jpg",
					MIMEType: "image/jpeg",
					Data:     []byte("fake-image-data"),
				},
			},
		},
	}

	filtered := FilterNonTextContent(msgs)

	require.Equal(t, len(msgs), len(filtered), "Should preserve message count")
	// Messages with only non-text content should become empty
	require.Equal(t, 0, len(filtered[0].Parts), "Message with only image URL should be empty")
	require.Equal(t, 0, len(filtered[1].Parts), "Message with only binary content should be empty")
}

func TestCountNonTextContent(t *testing.T) {
	t.Parallel()

	msgs := []Message{
		{
			Role: User,
			Parts: []ContentPart{
				TextContent{Text: "Hello"},
				ImageURLContent{URL: "http://example.com/image1.png"},
				ImageURLContent{URL: "http://example.com/image2.png"},
			},
		},
		{
			Role: Assistant,
			Parts: []ContentPart{
				TextContent{Text: "Hi"},
				BinaryContent{
					Path:     "/path/to/image.jpg",
					MIMEType: "image/jpeg",
					Data:     []byte("fake-image-data"),
				},
				BinaryContent{
					Path:     "/path/to/document.pdf",
					MIMEType: "application/pdf",
					Data:     []byte("fake-pdf-data"),
				},
			},
		},
	}

	count := CountNonTextContent(msgs)
	require.Equal(t, 4, count, "Should count 4 non-text contents (2 URLs + 2 binary)")
}
