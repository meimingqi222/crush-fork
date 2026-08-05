package plugin

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewCustomToolAgentToolUnwrapsJSONSchema(t *testing.T) {
	t.Parallel()

	// The documented tool definition format wraps parameters in a full JSON
	// Schema. The wrapper must not leak into the advertised parameter list.
	tool := NewCustomToolAgentTool(ToolDefinition{
		Name:        "github_issue",
		Description: "Create or update GitHub issues",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"title": map[string]any{"type": "string", "description": "Issue title"},
				"body":  map[string]any{"type": "string", "description": "Issue body"},
			},
			"required": []any{"title"},
		},
	}, t.TempDir())

	info := tool.Info()
	require.ElementsMatch(t, []string{"title", "body"}, mapKeys(info.Parameters))
	require.Equal(t, []string{"title"}, info.Required)
}

func TestNewCustomToolAgentToolAcceptsBarePropertyMap(t *testing.T) {
	t.Parallel()

	tool := NewCustomToolAgentTool(ToolDefinition{
		Name: "bare",
		Parameters: map[string]any{
			"title": map[string]any{"type": "string"},
		},
	}, t.TempDir())

	info := tool.Info()
	require.ElementsMatch(t, []string{"title"}, mapKeys(info.Parameters))
	require.Empty(t, info.Required)
}

func TestNewCustomToolAgentToolHandlesNoParameters(t *testing.T) {
	t.Parallel()

	tool := NewCustomToolAgentTool(ToolDefinition{Name: "none"}, t.TempDir())
	require.Empty(t, tool.Info().Parameters)
	require.Empty(t, tool.Info().Required)

	objectOnly := NewCustomToolAgentTool(ToolDefinition{
		Name:       "object-only",
		Parameters: map[string]any{"type": "object"},
	}, t.TempDir())
	require.Empty(t, objectOnly.Info().Parameters)
}

func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
