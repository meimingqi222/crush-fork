package tools

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	fschema "charm.land/fantasy/schema"
	"github.com/stretchr/testify/require"
)

// toolParamTypes lists every parameter struct whose JSON schema is reflected
// by fantasy and shipped to the model. Add new tool parameter structs here.
func toolParamTypes() map[string]any {
	return map[string]any{
		"AgenticFetchParams":     AgenticFetchParams{},
		"BashParams":             BashParams{},
		"CrushLogsParams":        CrushLogsParams{},
		"CrushParams":            CrushParams{},
		"DescribeImageParams":    DescribeImageParams{},
		"DiagnosticsParams":      DiagnosticsParams{},
		"DownloadParams":         DownloadParams{},
		"EditParams":             EditParams{},
		"GlobParams":             GlobParams{},
		"GoalParams":             GoalParams{},
		"GraphParams":            GraphParams{},
		"GrepParams":             GrepParams{},
		"HashlineEditParams":     HashlineEditParams{},
		"IrcParams":              IrcParams{},
		"JobKillParams":          JobKillParams{},
		"JobOutputParams":        JobOutputParams{},
		"JobParams":              JobParams{},
		"JobWaitParams":          JobWaitParams{},
		"LSPParams":              LSPParams{},
		"LSParams":               LSParams{},
		"ReadParams":             ReadParams{},
		"RecallParams":           RecallParams{},
		"ReferencesParams":       ReferencesParams{},
		"ReflectParams":          ReflectParams{},
		"RequestUserInputParams": RequestUserInputParams{},
		"ResolveParams":          ResolveParams{},
		"RetainParams":           RetainParams{},
		"SendMessageParams":      SendMessageParams{},
		"SourcegraphParams":      SourcegraphParams{},
		"TaskStopParams":         TaskStopParams{},
		"TodoParams":             TodoParams{},
		"ToolSearchParams":       ToolSearchParams{},
		"WebFetchParams":         WebFetchParams{},
		"WebSearchParams":        WebSearchParams{},
		"WriteParams":            WriteParams{},
		"YieldParams":            YieldParams{},
	}
}

// TestToolSchemasHaveNoReflectionTraps guards against parameter field types
// that reflect into a schema the model cannot satisfy:
//
//   - []byte / json.RawMessage reflects to an integer array, so the model is
//     told to send [72, 105] where the tool wants an object. Use `any`.
//   - map[...]... reflects to a literal property named "*", which is not a
//     JSON Schema wildcard and just confuses the model. Use a named struct.
func TestToolSchemasHaveNoReflectionTraps(t *testing.T) {
	t.Parallel()

	for name, v := range toolParamTypes() {
		raw, err := json.Marshal(fschema.Generate(reflect.TypeOf(v)))
		require.NoError(t, err)
		require.NotContains(t, string(raw), `"items":{"type":"integer"}`,
			"%s: a []byte/json.RawMessage field reflects to an integer array", name)
		require.NotContains(t, string(raw), `"*"`,
			"%s: a map field reflects to a bogus \"*\" property", name)
	}
}

// TestToolSchemasDoNotRequireDefaultedFields asserts that fields the tool
// itself treats as optional are not advertised as required. A required field
// the handler happily defaults means the model gets a hard validation error
// for a call the tool would have accepted.
func TestToolSchemasDoNotRequireDefaultedFields(t *testing.T) {
	t.Parallel()

	optionalAtRuntime := map[string][]string{
		// glob defaults path to the working directory.
		"GlobParams": {"path"},
		// graph defaults mode to "path".
		"GraphParams": {"mode"},
	}

	for name, fields := range optionalAtRuntime {
		s := fschema.Generate(reflect.TypeOf(toolParamTypes()[name]))
		for _, field := range fields {
			require.NotContains(t, s.Required, field,
				"%s.%s is defaulted by the handler and must not be advertised as required", name, field)
		}
	}

	// Hashline operations delete lines when content is omitted, so content
	// must stay optional in the nested operation schema.
	ops := fschema.Generate(reflect.TypeFor[HashlineEditParams]()).Properties["operations"]
	require.NotNil(t, ops)
	require.NotNil(t, ops.Items)
	require.NotContains(t, ops.Items.Required, "content",
		"omitting content is a valid delete operation")
}

// TestToolSchemaDescriptionsDocumentRequiredness is a lint-style check: a
// field advertised as required must not describe itself as optional or
// defaulted, since the two contradict each other in the model's context.
func TestToolSchemaDescriptionsDocumentRequiredness(t *testing.T) {
	t.Parallel()

	contradictions := []string{"optional", "defaults to", "if omitted", "leave empty"}
	for name, v := range toolParamTypes() {
		s := fschema.Generate(reflect.TypeOf(v))
		for _, field := range s.Required {
			prop := s.Properties[field]
			if prop == nil {
				continue
			}
			desc := strings.ToLower(prop.Description)
			for _, phrase := range contradictions {
				require.NotContains(t, desc, phrase,
					"%s.%s is required but its description says %q", name, field, phrase)
			}
		}
	}
}
