package agent

import (
	"context"
	"testing"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/memory/engine"
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

	block := buildAutoRecallBlock(context.Background(), staticRetriever{recall: "Memory Summary"}, "sess-1")
	require.Equal(t, "Memory Summary", block)
}

func TestBuildAutoRecallBlockSkipsNilRetriever(t *testing.T) {
	t.Parallel()

	require.Empty(t, buildAutoRecallBlock(context.Background(), nil, "sess-1"))
}

func TestBuildAutoRecallBlockRespectsEphemeralMemoryPolicy(t *testing.T) {
	t.Parallel()

	ctx := context.WithValue(context.Background(), tools.AgentMemoryContextKey, "ephemeral")
	require.Empty(t, buildAutoRecallBlock(ctx, staticRetriever{recall: "Memory Summary"}, "sess-1"))
}

func TestMaxSessionRecallBytes(t *testing.T) {
	t.Parallel()

	require.Equal(t, 60*1024, maxSessionRecallBytes)
}
