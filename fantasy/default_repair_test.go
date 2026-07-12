package fantasy

import (
	"encoding/json"
	"strings"
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

// P0-a: parseToolCallInput tolerates weak-model JSON dialects via jsonrepair
// before rejecting. Covers single quotes, bareword keys, Python literals,
// truncated JSON, and the unrepairable failure path.

func TestParseToolCallInputPassesStrictJSON(t *testing.T) {
	t.Parallel()

	parsed, repaired, err := parseToolCallInput(`{"command":"ls"}`)
	require.NoError(t, err)
	require.Equal(t, "ls", parsed["command"])
	require.Equal(t, `{"command":"ls"}`, repaired)
}

func TestParseToolCallInputRepairsSingleQuotes(t *testing.T) {
	t.Parallel()

	parsed, repaired, err := parseToolCallInput(`{'command': 'ls'}`)
	require.NoError(t, err)
	require.Equal(t, "ls", parsed["command"])

	// Repaired input must be strict JSON (no single quotes).
	require.NotContains(t, repaired, "'")
	var reParsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(repaired), &reParsed))
	require.Equal(t, "ls", reParsed["command"])
}

func TestParseToolCallInputRepairsBarewordKeys(t *testing.T) {
	t.Parallel()

	parsed, _, err := parseToolCallInput(`{command: ls}`)
	require.NoError(t, err)
	require.Equal(t, "ls", parsed["command"])
}

func TestParseToolCallInputRepairsPythonLiterals(t *testing.T) {
	t.Parallel()

	parsed, _, err := parseToolCallInput(`{"verbose": True, "dryrun": False}`)
	require.NoError(t, err)
	require.Equal(t, true, parsed["verbose"])
	require.Equal(t, false, parsed["dryrun"])
}

func TestParseToolCallInputRepairsTruncatedJSON(t *testing.T) {
	t.Parallel()

	parsed, _, err := parseToolCallInput(`{"command": "ls`)
	require.NoError(t, err)
	require.Equal(t, "ls", parsed["command"])
}

func TestParseToolCallInputRejectsUnrepairable(t *testing.T) {
	t.Parallel()

	_, _, err := parseToolCallInput(`{invalid json}`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid JSON input")
	// Raw-input excerpt lets the model self-correct (oh-my-pi __parseError).
	require.Contains(t, err.Error(), "raw:")
	require.Contains(t, err.Error(), "{invalid json}")
}

func TestParseToolCallInputExcerptTruncatedForLongInput(t *testing.T) {
	t.Parallel()

	huge := strings.Repeat("x", 1024)
	_, _, err := parseToolCallInput(huge)
	require.Error(t, err)
	// The raw excerpt must not echo the full 1KB input back.
	require.Less(t, len(err.Error()), rawInputExcerptMax+256)
}

// P0-a integration: validateToolCall propagates repaired JSON so the
// downstream tool call executes on well-formed input.

func TestValidateToolCallRepairsSingleQuotedInput(t *testing.T) {
	t.Parallel()

	ag := NewAgent(&mockLanguageModel{})
	validated, err := ag.(*agent).validateToolCall(ToolCallContent{
		ToolCallID: "call1",
		ToolName:   "test_tool",
		Input:      `{'command': 'ls'}`,
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
	require.NotContains(t, validated.Input, "'")
}

func TestValidateToolCallRepairsBarewordKeys(t *testing.T) {
	t.Parallel()

	ag := NewAgent(&mockLanguageModel{})
	validated, err := ag.(*agent).validateToolCall(ToolCallContent{
		ToolCallID: "call1",
		ToolName:   "test_tool",
		Input:      `{command: ls}`,
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
}

// P0-b: truncateArgsForError caps string fields recursively so a failed
// write/edit with a large content field does not round-trip hundreds of KB
// back through the model as a tool result.

func TestTruncateArgsForErrorShortString(t *testing.T) {
	t.Parallel()

	out := truncateArgsForError(map[string]any{"command": "ls -la"})
	require.Equal(t, "ls -la", out["command"])
}

func TestTruncateArgsForErrorLongString(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", maxArgEchoFieldLength*2)
	out := truncateArgsForError(map[string]any{"content": long})
	s, ok := out["content"].(string)
	require.True(t, ok)
	// Capped at maxArgEchoFieldLength runes plus an ellipsis marker.
	require.Equal(t, maxArgEchoFieldLength+1, len([]rune(s)))
	require.True(t, strings.HasSuffix(s, "…"))
}

func TestTruncateArgsForErrorNestedMap(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("y", maxArgEchoFieldLength*2)
	in := map[string]any{
		"outer": map[string]any{"inner": long},
	}
	out := truncateArgsForError(in)
	outer, ok := out["outer"].(map[string]any)
	require.True(t, ok)
	inner, ok := outer["inner"].(string)
	require.True(t, ok)
	require.Equal(t, maxArgEchoFieldLength+1, len([]rune(inner)))
}

func TestTruncateArgsForErrorArray(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("z", maxArgEchoFieldLength*2)
	in := map[string]any{
		"items": []any{long, "short"},
	}
	out := truncateArgsForError(in)
	items, ok := out["items"].([]any)
	require.True(t, ok)
	require.Len(t, items, 2)
	first, ok := items[0].(string)
	require.True(t, ok)
	require.Equal(t, maxArgEchoFieldLength+1, len([]rune(first)))
	require.Equal(t, "short", items[1])
}

func TestTruncateArgsForErrorNonString(t *testing.T) {
	t.Parallel()

	in := map[string]any{
		"count":   float64(42),
		"flag":    true,
		"nothing": nil,
	}
	out := truncateArgsForError(in)
	require.Equal(t, float64(42), out["count"])
	require.Equal(t, true, out["flag"])
	require.Nil(t, out["nothing"])
}

// P0-b integration: a 300KB content field must not round-trip through the
// validation error message — the error stays under a few KB.

func TestFormatToolValidationErrorTruncatesLargeContent(t *testing.T) {
	t.Parallel()

	toolInfo := ToolInfo{
		Name: "write",
		Parameters: map[string]any{
			"path":    map[string]any{"type": "string"},
			"content": map[string]any{"type": "string"},
		},
		Required: []string{"path", "content"},
	}

	huge := strings.Repeat("A", 300*1024)
	// Missing required "path" triggers validation failure.
	args := map[string]any{"content": huge}

	_, err := validateAndNormalizeToolArguments(toolInfo, args)
	require.Error(t, err)
	require.Contains(t, err.Error(), "Received arguments:")
	// Error must stay under a few KB despite the 300KB content field.
	require.Less(t, len(err.Error()), 4*1024, "validation error must be truncated")
	// Original content must NOT appear in full in the error.
	require.NotContains(t, err.Error(), huge)
}
