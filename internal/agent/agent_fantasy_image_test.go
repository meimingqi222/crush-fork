package agent

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

type fakeVisionDescriber struct{}

func (fakeVisionDescriber) DescribeImage(context.Context, []byte, string, string) (string, error) {
	return "description", nil
}

func (fakeVisionDescriber) IsAvailable() bool { return true }

func TestPromptWithImageAttachmentPlaceholders(t *testing.T) {
	t.Parallel()

	attachments := []message.Attachment{
		{
			FileName: "paste_1.png",
			FilePath: "paste_1.png",
			MimeType: "image/png",
			Content:  []byte("fake-image-data"),
		},
		{
			FileName: "notes.txt",
			FilePath: "notes.txt",
			MimeType: "text/plain",
			Content:  []byte("some context"),
		},
	}

	prompt := promptWithImageAttachmentPlaceholders("describe this", attachments, true)
	require.Contains(t, prompt, "describe this")
	require.Contains(t, prompt, "some context")
	require.Contains(t, prompt, "paste_1.png")
	require.Contains(t, prompt, "describe_image")

	noVision := promptWithImageAttachmentPlaceholders("describe this", attachments, false)
	require.Contains(t, noVision, "paste_1.png")
	require.Contains(t, noVision, "no vision helper is configured")
}

func TestPromptWithImageAttachmentPlaceholdersForMessage(t *testing.T) {
	t.Parallel()

	attachments := []message.Attachment{
		{
			FileName: "paste_1.png",
			FilePath: "paste_1.png",
			MimeType: "image/png",
			Content:  []byte("first-image"),
		},
		{
			FileName: "paste_2.png",
			FilePath: "paste_2.png",
			MimeType: "image/png",
			Content:  []byte("second-image"),
		},
	}

	prompt := promptWithImageAttachmentPlaceholdersForMessage("describe these", attachments, "message-123", true)
	require.Contains(t, prompt, `message_id="message-123"`)
	require.Contains(t, prompt, "image_index=1")
	require.Contains(t, prompt, "image_index=2")
}

func TestStripImagePartsFromFantasyMessagesWithVision_PastedImage(t *testing.T) {
	t.Parallel()

	msg := message.Message{
		Role: message.User,
		Parts: []message.ContentPart{
			message.TextContent{Text: "这个图片里描述了什么？"},
			message.BinaryContent{
				Path:     "paste_1.png",
				MIMEType: "image/png",
				Data:     []byte("fake-image-data"),
			},
		},
	}

	fantasyMsgs := msg.ToAIMessage()
	require.Len(t, fantasyMsgs, 1)

	stripped := stripImagePartsFromFantasyMessagesWithVision(fantasyMsgs, fakeVisionDescriber{})
	require.Len(t, stripped, 1)

	textParts := make([]string, 0)
	for _, part := range stripped[0].Content {
		if tp, ok := part.(fantasy.TextPart); ok {
			textParts = append(textParts, tp.Text)
		}
	}

	joined := strings.Join(textParts, " ")
	require.Contains(t, joined, "这个图片里描述了什么？")
	require.Contains(t, joined, "paste_1.png")
	require.Contains(t, joined, "describe_image")
}
