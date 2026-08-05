package fantasy

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildToolInputSchemaResolvesSchemaDefs(t *testing.T) {
	t.Parallel()

	info := ToolInfo{
		Name: "mcp_server_configure",
		Parameters: map[string]any{
			"cfg": map[string]any{"$ref": "#/$defs/Cfg"},
		},
		Required: []string{"cfg"},
		SchemaDefs: map[string]any{
			"Cfg": map[string]any{
				"type":       "object",
				"properties": map[string]any{"mode": map[string]any{"type": "string"}},
				"required":   []any{"mode"},
			},
		},
	}

	// With the definitions carried along, the referenced schema is actually
	// enforced instead of being silently skipped.
	_, err := validateAndNormalizeToolArguments(info, map[string]any{
		"cfg": map[string]any{"mode": "fast"},
	})
	require.NoError(t, err)

	_, err = validateAndNormalizeToolArguments(info, map[string]any{
		"cfg": map[string]any{},
	})
	require.Error(t, err)
}
