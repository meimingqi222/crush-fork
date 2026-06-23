package agent

import (
	"context"
	"strings"
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

	block := buildAutoRecallBlock(context.Background(), staticRetriever{recall: "Memory Summary"}, "", "", "sess-1", "local")
	require.Equal(t, "Memory Summary", block)
}

func TestBuildAutoRecallBlockSkipsNilRetriever(t *testing.T) {
	t.Parallel()

	require.Empty(t, buildAutoRecallBlock(context.Background(), nil, "", "", "sess-1", "local"))
}

func TestBuildAutoRecallBlockRespectsEphemeralMemoryPolicy(t *testing.T) {
	t.Parallel()

	ctx := context.WithValue(context.Background(), tools.AgentMemoryContextKey, "ephemeral")
	require.Empty(t, buildAutoRecallBlock(ctx, staticRetriever{recall: "Memory Summary"}, "", "", "sess-1", "local"))
}

func TestBuildAutoRecallBlockHindsightNoFallback(t *testing.T) {
	t.Parallel()

	// In hindsight mode, retrieve yielding empty results returns empty without fallback to Recall
	block := buildAutoRecallBlock(context.Background(), staticRetriever{recall: "Memory Summary"}, "non-empty-query", "", "sess-1", "hindsight")
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

func TestComposeRecallQueryLatestOnly(t *testing.T) {
	t.Parallel()

	// No recent context: query is just the trimmed prompt.
	require.Equal(t, "hello world", composeRecallQuery("  hello world  ", ""))
}

func TestComposeRecallQueryRecentOnly(t *testing.T) {
	t.Parallel()

	// No prompt (compaction-rescue path): query is just the trimmed recent block.
	require.Equal(t, "user: earlier", composeRecallQuery("", "  user: earlier  "))
}

func TestComposeRecallQueryStructured(t *testing.T) {
	t.Parallel()

	// Both present: recent framed as "Prior context:" ahead of the prompt.
	got := composeRecallQuery("what now", "user: earlier\nassistant: reply")
	require.Equal(t, "Prior context:\n\nuser: earlier\nassistant: reply\n\nwhat now", got)
}

func TestTruncateRecallQueryUnderLimit(t *testing.T) {
	t.Parallel()

	// Under the budget is returned unchanged.
	query := composeRecallQuery("latest", "context")
	require.Equal(t, query, truncateRecallQuery(query, "latest", 1000))
}

func TestTruncateRecallQueryDropsOldestContext(t *testing.T) {
	t.Parallel()

	// Build a query with three context lines plus the prompt, then budget it
	// so only the most recent context line + prompt fit. Oldest lines drop first.
	recent := "line one\nline two\nline three"
	query := composeRecallQuery("the prompt", recent)

	// Budget: marker(len 17) + "\n\nthe prompt"(12) = 29 consumed by framing,
	// leaving room for exactly "line three" (10) but not "line two" (8) too.
	truncated := truncateRecallQuery(query, "the prompt", 29+len("line three"))
	require.Contains(t, truncated, "line three")
	require.Contains(t, truncated, "the prompt")
	require.NotContains(t, truncated, "line one")
	require.NotContains(t, truncated, "line two")
	require.True(t, runeLen(truncated) <= 29+len("line three"))
}

func TestTruncateRecallQueryLatestExceeds(t *testing.T) {
	t.Parallel()

	// The prompt itself is over budget: fall back to a tail slice of the prompt.
	long := strings.Repeat("x", 50)
	truncated := truncateRecallQuery(long, long, 10)
	require.Equal(t, 10, runeLen(truncated))
	require.Equal(t, strings.Repeat("x", 10), truncated)
}

func TestTruncateRecallQueryTailTruncateNoMarker(t *testing.T) {
	t.Parallel()

	// No structured marker and no prompt: tail-truncate to the budget.
	long := strings.Repeat("ab", 50) // 100 runes
	truncated := truncateRecallQuery(long, "", 10)
	require.Equal(t, 10, runeLen(truncated))
	// Tail of "ab"*50 is the last 10 runes = "ab"*5.
	require.Equal(t, strings.Repeat("ab", 5), truncated)
}

func TestTruncateRecallQueryNoMarkerPrefersPrompt(t *testing.T) {
	t.Parallel()

	// No marker but a prompt is supplied: return the (possibly truncated) prompt
	// rather than truncating the raw query.
	raw := strings.Repeat("z", 100)
	truncated := truncateRecallQuery(raw, "keep me", 10)
	require.Equal(t, "keep me", truncated)
}

func TestTruncateRecallQueryMultibyteSafe(t *testing.T) {
	t.Parallel()

	// Truncation must not split a multi-byte rune. Each 中 is one rune.
	long := strings.Repeat("中", 10)
	truncated := truncateRecallQuery(long, "", 3)
	require.Equal(t, "中中中", truncated)
}
