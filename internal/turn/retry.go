package turn

import (
	"context"

	"github.com/charmbracelet/crush/internal/message"
)

type messageRetryReader interface {
	GetRetrySource(context.Context, string, string) (message.Message, error)
}

type MessageRetrySource struct{ Messages messageRetryReader }

func (s MessageRetrySource) RetryInput(ctx context.Context, sessionID, messageID string) (Input, error) {
	source, err := s.Messages.GetRetrySource(ctx, sessionID, messageID)
	if err != nil {
		return Input{}, err
	}
	attachments := make([]message.Attachment, 0, len(source.BinaryContent()))
	for _, content := range source.BinaryContent() {
		attachments = append(attachments, message.Attachment{
			FilePath: content.Path,
			MimeType: content.MIMEType,
			Content:  append([]byte(nil), content.Data...),
		})
	}
	return Input{Prompt: source.Content().Text, Attachments: attachments}, nil
}
