package fantasy

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func testToolInfo() ToolInfo {
	return ToolInfo{
		Name:        "test_tool",
		Description: "Test tool",
		Parameters: map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "The command to execute",
			},
			"description": map[string]any{"type": "string"},
			"timeout":     map[string]any{"type": "integer"},
			"count":       map[string]any{"type": "integer"},
		},
		Required: []string{"command"},
	}
}

// Built-in repair does NOT inject missing required fields: silently running a
// tool with fabricated input (e.g. an empty command) is dangerous.
func TestValidateAndNormalizeDoesNotInjectMissingRequired(t *testing.T) {
	t.Parallel()

	_, err := validateAndNormalizeToolArguments(testToolInfo(), map[string]any{
		"description": "test",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Received arguments:")
}

func TestRepairToolArgumentsDoesNotInjectMissingRequired(t *testing.T) {
	t.Parallel()

	options := ToolCallRepairOptions{
		OriginalToolCall: ToolCallContent{
			ToolCallID: "call1",
			ToolName:   "test_tool",
			Input:      `{"description": "test"}`,
		},
		AvailableTools: []AgentTool{&mockTool{
			name:        "test_tool",
			description: "Test tool",
			parameters:  testToolInfo().Parameters,
			required:    testToolInfo().Required,
		}},
	}

	result, err := repairToolArguments(options)
	require.NoError(t, err)
	require.Nil(t, result)
}

func TestValidateAndNormalizeCoercesStringToNumber(t *testing.T) {
	t.Parallel()

	normalized, err := validateAndNormalizeToolArguments(ToolInfo{
		Name: "test_tool",
		Parameters: map[string]any{
			"count": map[string]any{"type": "integer"},
		},
		Required: []string{"count"},
	}, map[string]any{"count": "42"})
	require.NoError(t, err)
	require.Equal(t, float64(42), normalized["count"])
}

func TestValidateAndNormalizeStripsOptionalNull(t *testing.T) {
	t.Parallel()

	normalized, err := validateAndNormalizeToolArguments(testToolInfo(), map[string]any{
		"command": "ls",
		"timeout": nil,
	})
	require.NoError(t, err)
	_, hasTimeout := normalized["timeout"]
	require.False(t, hasTimeout)
	require.Equal(t, "ls", normalized["command"])
}

func TestValidateAndNormalizeStripsOptionalStringNull(t *testing.T) {
	t.Parallel()

	normalized, err := validateAndNormalizeToolArguments(testToolInfo(), map[string]any{
		"command": "ls",
		"timeout": "null",
	})
	require.NoError(t, err)
	_, hasTimeout := normalized["timeout"]
	require.False(t, hasTimeout)
}

func TestValidateAndNormalizeSubstitutesSchemaDefault(t *testing.T) {
	t.Parallel()

	normalized, err := validateAndNormalizeToolArguments(ToolInfo{
		Name: "test_tool",
		Parameters: map[string]any{
			"command": map[string]any{"type": "string", "default": "fallback"},
		},
		Required: []string{"command"},
	}, map[string]any{"command": nil})
	require.NoError(t, err)
	require.Equal(t, "fallback", normalized["command"])
}

func TestValidateAndNormalizePreservesRequiredStringNull(t *testing.T) {
	t.Parallel()

	normalized, err := validateAndNormalizeToolArguments(ToolInfo{
		Name: "test_tool",
		Parameters: map[string]any{
			"path": map[string]any{"type": "string"},
		},
		Required: []string{"path"},
	}, map[string]any{"path": "null"})
	require.NoError(t, err)
	require.Equal(t, "null", normalized["path"])
}

func TestValidateAndNormalizePreservesUnknownRootFields(t *testing.T) {
	t.Parallel()

	normalized, err := validateAndNormalizeToolArguments(testToolInfo(), map[string]any{
		"command": "ls",
		"async":   true,
	})
	require.NoError(t, err)
	require.Equal(t, "ls", normalized["command"])
	require.Equal(t, true, normalized["async"])
}

func TestRepairToolArgumentsUpdatesInputWhenChanged(t *testing.T) {
	t.Parallel()

	options := ToolCallRepairOptions{
		OriginalToolCall: ToolCallContent{
			ToolCallID: "call1",
			ToolName:   "test_tool",
			Input:      `{"command":"ls","timeout":null}`,
		},
		AvailableTools: []AgentTool{&mockTool{
			name:        "test_tool",
			description: "Test tool",
			parameters:  testToolInfo().Parameters,
			required:    testToolInfo().Required,
		}},
	}

	result, err := repairToolArguments(options)
	require.NoError(t, err)
	require.NotNil(t, result)

	var input map[string]any
	require.NoError(t, json.Unmarshal([]byte(result.Input), &input))
	_, hasTimeout := input["timeout"]
	require.False(t, hasTimeout)
	require.Equal(t, "ls", input["command"])
}

func TestValidateToolCallAppliesNormalizationOnSuccessPath(t *testing.T) {
	t.Parallel()

	ag := NewAgent(&mockLanguageModel{}, WithTools(&mockTool{
		name:        "test_tool",
		description: "Test tool",
		parameters:  testToolInfo().Parameters,
		required:    testToolInfo().Required,
	}))

	validated, err := ag.(*agent).validateToolCall(ToolCallContent{
		ToolCallID: "call1",
		ToolName:   "test_tool",
		Input:      `{"command":"ls","timeout":null}`,
	}, []AgentTool{&mockTool{
		name:        "test_tool",
		description: "Test tool",
		parameters:  testToolInfo().Parameters,
		required:    testToolInfo().Required,
	}}, nil)
	require.NoError(t, err)

	var input map[string]any
	require.NoError(t, json.Unmarshal([]byte(validated.Input), &input))
	_, hasTimeout := input["timeout"]
	require.False(t, hasTimeout)
}

func TestValidateToolCallRejectsRequiredNull(t *testing.T) {
	t.Parallel()

	ag := NewAgent(&mockLanguageModel{})
	_, err := ag.(*agent).validateToolCall(ToolCallContent{
		ToolCallID: "call1",
		ToolName:   "test_tool",
		Input:      `{"command":null}`,
	}, []AgentTool{&mockTool{
		name:        "test_tool",
		description: "Test tool",
		parameters:  testToolInfo().Parameters,
		required:    testToolInfo().Required,
	}}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "Received arguments:")
}

func TestValidateToolCallPreservesUnknownRootFields(t *testing.T) {
	t.Parallel()

	ag := NewAgent(&mockLanguageModel{})
	validated, err := ag.(*agent).validateToolCall(ToolCallContent{
		ToolCallID: "call1",
		ToolName:   "test_tool",
		Input:      `{"command":"ls","async":true}`,
	}, []AgentTool{&mockTool{
		name:        "test_tool",
		description: "Test tool",
		parameters:  testToolInfo().Parameters,
		required:    testToolInfo().Required,
	}}, nil)
	require.NoError(t, err)

	var input map[string]any
	require.NoError(t, json.Unmarshal([]byte(validated.Input), &input))
	require.Equal(t, "ls", input["command"])
	require.Equal(t, true, input["async"])
}
