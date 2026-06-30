package tools

import (
	"context"
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

type mockMessageService struct {
	messages []message.Message
	err      error
}

func (m *mockMessageService) Create(context.Context, string, message.CreateMessageParams) (message.Message, error) {
	return message.Message{}, nil
}

func (m *mockMessageService) Update(context.Context, message.Message) error {
	return nil
}

func (m *mockMessageService) Get(context.Context, string) (message.Message, error) {
	return message.Message{}, nil
}

func (m *mockMessageService) List(context.Context, string) ([]message.Message, error) {
	return m.messages, m.err
}

func (m *mockMessageService) ListPage(context.Context, string, int, int) ([]message.Message, error) {
	return m.messages, m.err
}

func (m *mockMessageService) Count(context.Context, string) (int64, error) {
	return int64(len(m.messages)), nil
}

func (m *mockMessageService) ListUserMessages(context.Context, string) ([]message.Message, error) {
	return m.messages, m.err
}

func (m *mockMessageService) ListAllUserMessages(context.Context) ([]message.Message, error) {
	return m.messages, m.err
}

func (m *mockMessageService) Delete(context.Context, string) error {
	return nil
}

func (m *mockMessageService) DeleteSessionMessages(context.Context, string) error {
	return nil
}

func (m *mockMessageService) Subscribe(context.Context) <-chan pubsub.Event[message.Message] {
	return nil
}

func TestLoadImageDataForDescribe_FallsBackToSessionHistory(t *testing.T) {
	t.Parallel()

	svc := &mockMessageService{
		messages: []message.Message{
			{
				ID:   "message-1",
				Role: message.User,
				Parts: []message.ContentPart{
					message.TextContent{Text: "look at this"},
					message.BinaryContent{
						Path:     "/tmp/screenshot.png",
						MIMEType: "image/png",
						Data:     []byte("fake-image-bytes"),
					},
				},
			},
		},
	}

	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-1")
	ctx = context.WithValue(ctx, MessageServiceContextKey, svc)

	image, err := loadImageDataForDescribe(ctx, DescribeImageParams{Path: "screenshot.png"})
	require.NoError(t, err)
	require.Equal(t, "image/png", image.mimeType)
	require.Equal(t, []byte("fake-image-bytes"), image.data)
	require.Equal(t, "message-1", image.messageID)
	require.Equal(t, 1, image.imageIndex)
}

func TestLoadImageDataForDescribe_NotFound(t *testing.T) {
	t.Parallel()

	svc := &mockMessageService{messages: []message.Message{}}
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-1")
	ctx = context.WithValue(ctx, MessageServiceContextKey, svc)

	_, err := loadImageDataForDescribe(ctx, DescribeImageParams{Path: "missing.png"})
	require.Error(t, err)
}

func TestLoadImageDataForDescribe_AmbiguousPathFallback(t *testing.T) {
	t.Parallel()

	svc := &mockMessageService{
		messages: []message.Message{
			{
				ID:   "message-1",
				Role: message.User,
				Parts: []message.ContentPart{
					message.BinaryContent{
						Path:     "paste_1.png",
						MIMEType: "image/png",
						Data:     []byte("first-image"),
					},
				},
			},
			{
				ID:   "message-2",
				Role: message.User,
				Parts: []message.ContentPart{
					message.BinaryContent{
						Path:     "paste_1.png",
						MIMEType: "image/png",
						Data:     []byte("second-image"),
					},
				},
			},
		},
	}

	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-1")
	ctx = context.WithValue(ctx, MessageServiceContextKey, svc)

	_, err := loadImageDataForDescribe(ctx, DescribeImageParams{Path: "paste_1.png"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "ambiguous")
	require.Contains(t, err.Error(), "message_id")
}

func TestLoadImageDataForDescribe_MessageIDAndImageIndex(t *testing.T) {
	t.Parallel()

	svc := &mockMessageService{
		messages: []message.Message{
			{
				ID:   "message-1",
				Role: message.User,
				Parts: []message.ContentPart{
					message.BinaryContent{
						Path:     "paste_1.png",
						MIMEType: "image/png",
						Data:     []byte("wrong-image"),
					},
				},
			},
			{
				ID:   "message-2",
				Role: message.User,
				Parts: []message.ContentPart{
					message.TextContent{Text: "two images"},
					message.BinaryContent{
						Path:     "paste_1.png",
						MIMEType: "image/png",
						Data:     []byte("first-image"),
					},
					message.BinaryContent{
						Path:     "paste_1.png",
						MIMEType: "image/png",
						Data:     []byte("second-image"),
					},
				},
			},
		},
	}

	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session-1")
	ctx = context.WithValue(ctx, MessageServiceContextKey, svc)

	image, err := loadImageDataForDescribe(ctx, DescribeImageParams{
		MessageID:  "message-2",
		ImageIndex: 2,
	})
	require.NoError(t, err)
	require.Equal(t, []byte("second-image"), image.data)
	require.Equal(t, "image/png", image.mimeType)
	require.Equal(t, "message-2", image.messageID)
	require.Equal(t, 2, image.imageIndex)
}

func TestIsSupportedImageMimeType(t *testing.T) {
	t.Parallel()

	require.True(t, isSupportedImageMimeType("image/png"))
	require.True(t, isSupportedImageMimeType("image/jpeg"))
	require.True(t, isSupportedImageMimeType("image/gif"))
	require.True(t, isSupportedImageMimeType("image/webp"))
	require.False(t, isSupportedImageMimeType("image/svg+xml"))
	require.False(t, isSupportedImageMimeType("application/pdf"))
}
