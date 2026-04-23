package agent

import (
	"context"
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/stretchr/testify/require"
)

func TestToolRegistryInvokeReturnsDeferredToolRecoveryPayload(t *testing.T) {
	t.Parallel()

	registry := newToolRegistry()
	registry.register(agenttools.RegistryEntry{
		Name:        "mcp_acemcp_search_context",
		Description: "semantic code search",
		Metadata: agenttools.ToolMetadata{
			Exposure: agenttools.ToolExposureDeferred,
		},
	}, nil)

	resp, err := registry.Invoke(context.Background(), "mcp_acemcp_search_context", nil, fantasy.ToolCall{ID: "call-1", Name: "mcp_acemcp_search_context"})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.NotEmpty(t, resp.Metadata)

	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &payload))
	require.Equal(t, "deferred_tool_not_activated", payload["recovered_by"])
	require.Equal(t, "mcp_acemcp_search_context", payload["tool"])
	require.Equal(t, "select:mcp_acemcp_search_context", payload["fallback_tool_query"])
	require.Equal(t, "tool_search", payload["fallback_tool"])
	require.Equal(t, []any{"query"}, payload["recovered_parameters"])
}
