package memory

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestServiceAdapterRecallSearchesFullBody(t *testing.T) {
	t.Parallel()

	svc, err := NewService(t.TempDir())
	require.NoError(t, err)

	body := strings.Repeat("x", 160) + " needle-in-body"
	require.NoError(t, svc.Store(context.Background(), StoreParams{
		Key:   "transcript-memory",
		Value: body,
		Scope: "project",
	}))

	client := NewServiceAdapter(svc, ServiceAdapterOptions{})
	lines, err := client.Recall(context.Background(), "needle-in-body", "project", "")
	require.NoError(t, err)
	require.Len(t, lines, 1)
	require.Contains(t, lines[0], "transcript-memory")
	require.Contains(t, lines[0], "needle-in-body")
}
