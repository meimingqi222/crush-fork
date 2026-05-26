package agent

import (
	"context"
	"testing"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/memory/engine"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

type staticRetriever struct {
	recall string
}

func (r staticRetriever) Recall(context.Context, map[string]any) (string, error) {
	return r.recall, nil
}

func (r staticRetriever) Retrieve(context.Context, string, map[string]any) ([]engine.MemoryEvent, error) {
	return nil, nil
}

func (r staticRetriever) Reflect(context.Context, string, map[string]any) (string, error) {
	return "", nil
}

func TestBuildAutoRecallBlockUsesRetriever(t *testing.T) {
	t.Parallel()

	block := buildAutoRecallBlock(context.Background(), staticRetriever{recall: "Memory Summary"}, "", "sess-1", "local")
	require.Equal(t, "Memory Summary", block)
}

func TestBuildAutoRecallBlockSkipsNilRetriever(t *testing.T) {
	t.Parallel()

	require.Empty(t, buildAutoRecallBlock(context.Background(), nil, "", "sess-1", "local"))
}

func TestBuildAutoRecallBlockRespectsEphemeralMemoryPolicy(t *testing.T) {
	t.Parallel()

	ctx := context.WithValue(context.Background(), tools.AgentMemoryContextKey, "ephemeral")
	require.Empty(t, buildAutoRecallBlock(ctx, staticRetriever{recall: "Memory Summary"}, "", "sess-1", "local"))
}

func TestBuildAutoRecallBlockHindsightNoFallback(t *testing.T) {
	t.Parallel()

	// In hindsight mode, retrieve yielding empty results returns empty without fallback to Recall
	block := buildAutoRecallBlock(context.Background(), staticRetriever{recall: "Memory Summary"}, "non-empty-query", "sess-1", "hindsight")
	require.Empty(t, block)
}

func TestMaxSessionRecallBytes(t *testing.T) {
	t.Parallel()

	require.Equal(t, 60*1024, maxSessionRecallBytes)
}

type mockMessageService struct {
	message.Service
	messages []message.Message
}

func (m mockMessageService) List(ctx context.Context, sessionID string) ([]message.Message, error) {
	return m.messages, nil
}

func TestBuildRecentConversation(t *testing.T) {
	t.Parallel()

	msgs := []message.Message{
		{
			Role:  message.User,
			Parts: []message.ContentPart{message.TextContent{Text: "Hello there"}},
		},
		{
			Role:  message.Assistant,
			Parts: []message.ContentPart{message.TextContent{Text: "Hi! How can I help you today?"}},
		},
		{
			Role:  message.User,
			Parts: []message.ContentPart{message.TextContent{Text: "Tell me about Go"}},
		},
		{
			Role:  message.Assistant,
			Parts: []message.ContentPart{message.TextContent{Text: "Go is a compiled programming language."}},
		},
	}
	svc := mockMessageService{messages: msgs}

	// Max turns = 2. Should capture the last 2 user turns and corresponding assistant replies.
	recent := buildRecentConversation(context.Background(), svc, "sess-1", 2)
	require.Contains(t, recent, "user: Tell me about Go")
	require.Contains(t, recent, "assistant: Go is a compiled programming language.")
	require.Contains(t, recent, "user: Hello there")
	require.Contains(t, recent, "assistant: Hi! How can I help you today?")
}
