package mcplifecycle

import (
	"context"
	"testing"

	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestDefaultBackendScopedMarkerDeniesUnscopedStaleTools(t *testing.T) {
	t.Parallel()

	name := "test-scoped-" + uuid.NewString()
	NewBackend().MarkScoped(name)
	require.False(t, agenttools.MCPServerAllowed(context.Background(), name))
}
