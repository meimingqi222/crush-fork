package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// exploreOutputSchema mirrors the Explore subagent's OutputSchema
// (internal/config/config.go) closely enough to exercise schema-aware field
// ordering without importing the config package.
func exploreOutputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"summary": map[string]any{
				"type":        "string",
				"description": "Brief summary of findings and conclusions.",
			},
			"files": map[string]any{
				"type":        "array",
				"description": "Files examined with relevant code references.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":        map[string]any{"type": "string"},
						"description": map[string]any{"type": "string"},
					},
					"required": []string{"path", "description"},
				},
			},
			"architecture": map[string]any{
				"type":        "string",
				"description": "Brief explanation of how the discovered pieces connect.",
			},
		},
		"required": []string{"summary", "files"},
	}
}

func TestProjectYieldPayload_FullExplorePayload(t *testing.T) {
	t.Parallel()

	payload, err := json.Marshal(map[string]any{
		"summary": "IRC reply bypasses the recipient session.",
		"files": []any{
			map[string]any{"path": "internal/agent/tools/irc.go:117-135", "description": "synchronous call to global responder"},
			map[string]any{"path": "internal/agent/agent.go:3801-3849", "description": "fire-and-forget Generate, no history"},
		},
		"architecture": "The IRC tool calls the responder directly instead of routing through the recipient's own session.",
	})
	require.NoError(t, err)

	text := projectYieldPayload(payload, exploreOutputSchema())
	require.NotEmpty(t, text)

	require.True(t, strings.HasPrefix(text, "IRC reply bypasses the recipient session."))
	require.Contains(t, text, "Files:")
	require.Contains(t, text, "internal/agent/tools/irc.go:117-135 — synchronous call to global responder")
	require.Contains(t, text, "internal/agent/agent.go:3801-3849 — fire-and-forget Generate, no history")
	require.Contains(t, text, "Architecture:")
	require.Contains(t, text, "The IRC tool calls the responder directly")

	// Summary comes first, then the schema's "required" fields in their
	// declared order ("files"), then the optional properties alphabetically
	// ("architecture"). See projectionFieldOrder: "required" is a JSON array
	// so its order survives, unlike "properties" which is a map.
	require.Less(t, strings.Index(text, "IRC reply"), strings.Index(text, "Files:"))
	require.Less(t, strings.Index(text, "Files:"), strings.Index(text, "Architecture:"))
}

func TestProjectYieldPayload_MissingFiles(t *testing.T) {
	t.Parallel()

	// "files" is required by the schema but the model omitted it. Projection
	// must still succeed with whatever fields are present -- missing fields
	// are not a failure.
	payload, err := json.Marshal(map[string]any{
		"summary":      "Found the root cause in the retry loop.",
		"architecture": "Retry loop re-enters without resetting state.",
	})
	require.NoError(t, err)

	text := projectYieldPayload(payload, exploreOutputSchema())
	require.NotEmpty(t, text)
	require.Contains(t, text, "Found the root cause in the retry loop.")
	require.Contains(t, text, "Architecture:")
	require.NotContains(t, text, "Files:")
}

func TestProjectYieldPayload_MissingSummary(t *testing.T) {
	t.Parallel()

	// No top-level "summary" field: projection falls back to rendering the
	// other fields directly, best-effort.
	payload, err := json.Marshal(map[string]any{
		"files": []any{
			map[string]any{"path": "foo.go", "description": "does the thing"},
		},
	})
	require.NoError(t, err)

	text := projectYieldPayload(payload, exploreOutputSchema())
	require.NotEmpty(t, text)
	require.Contains(t, text, "Files:")
	require.Contains(t, text, "foo.go — does the thing")
}

func TestProjectYieldPayload_EmptyObject(t *testing.T) {
	t.Parallel()

	text := projectYieldPayload(json.RawMessage(`{}`), exploreOutputSchema())
	require.Equal(t, "{}", text)
}

func TestProjectYieldPayload_NonObjectJSON(t *testing.T) {
	t.Parallel()

	t.Run("array", func(t *testing.T) {
		t.Parallel()
		text := projectYieldPayload(json.RawMessage(`[1,2,3]`), nil)
		require.Equal(t, "[1,2,3]", text)
	})

	t.Run("string", func(t *testing.T) {
		t.Parallel()
		text := projectYieldPayload(json.RawMessage(`"just a string"`), nil)
		require.Equal(t, `"just a string"`, text)
	})

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, "", projectYieldPayload(json.RawMessage(``), nil))
		require.Equal(t, "", projectYieldPayload(nil, nil))
	})
}

func TestProjectYieldPayload_OversizedArrayIsCapped(t *testing.T) {
	t.Parallel()

	files := make([]any, 0, 200)
	for i := range 200 {
		files = append(files, map[string]any{
			"path":        "file.go",
			"description": "entry",
			"index":       i,
		})
	}
	payload, err := json.Marshal(map[string]any{
		"summary": "Scanned many files.",
		"files":   files,
	})
	require.NoError(t, err)

	text := projectYieldPayload(payload, exploreOutputSchema())
	require.NotEmpty(t, text)

	lines := strings.Split(text, "\n")
	bulletCount := 0
	sawOmittedMarker := false
	for _, line := range lines {
		if strings.HasPrefix(line, "- … ") {
			sawOmittedMarker = true
			continue
		}
		if strings.HasPrefix(line, "- ") {
			bulletCount++
		}
	}
	require.Equal(t, subagentYieldProjectionMaxArrayItems, bulletCount)
	require.True(t, sawOmittedMarker, "expected an omitted-items marker for the truncated array")
}

func TestProjectYieldPayload_DeterministicAcrossRepeatedCalls(t *testing.T) {
	t.Parallel()

	// Guards against Go's randomized map iteration order leaking into the
	// projected text: the same payload must produce byte-identical output
	// every time, regardless of how many top-level fields it has.
	payload, err := json.Marshal(map[string]any{
		"summary":      "Stable across calls.",
		"architecture": "Some architecture notes.",
		"files": []any{
			map[string]any{"path": "a.go", "description": "a"},
			map[string]any{"path": "b.go", "description": "b"},
		},
		"extra_field_one": "value one",
		"extra_field_two": "value two",
		"z_last_field":    "zzz",
		"a_first_field":   "aaa",
	})
	require.NoError(t, err)

	schema := exploreOutputSchema()
	first := projectYieldPayload(payload, schema)
	for range 20 {
		require.Equal(t, first, projectYieldPayload(payload, schema))
	}

	// Also stable with no schema (payload-key-only ordering).
	firstNoSchema := projectYieldPayload(payload, nil)
	for range 20 {
		require.Equal(t, firstNoSchema, projectYieldPayload(payload, nil))
	}
}

func TestYieldContentText(t *testing.T) {
	t.Parallel()

	t.Run("prefers Data over Payload", func(t *testing.T) {
		t.Parallel()
		text := yieldContentText(message.ToolResultYield{
			Data:    "explicit data",
			Payload: json.RawMessage(`{"summary":"from payload"}`),
		})
		require.Equal(t, "explicit data", text)
	})

	t.Run("falls back to Payload projection when Data is empty", func(t *testing.T) {
		t.Parallel()
		text := yieldContentText(message.ToolResultYield{
			Payload: json.RawMessage(`{"summary":"from payload"}`),
		})
		require.Equal(t, "from payload", text)
	})

	t.Run("empty when both are empty", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, "", yieldContentText(message.ToolResultYield{}))
	})
}

func TestProjectYieldPayload_RequiredOrderSurvivesJSONDecodedSchema(t *testing.T) {
	t.Parallel()

	// A crush.json override arrives via json.Unmarshal, so "required"
	// decodes as []any rather than []string. Both shapes must yield the same
	// field order.
	raw := `{
      "type": "object",
      "properties": {
        "summary": {"type": "string"},
        "files": {"type": "array", "items": {"type": "string"}},
        "architecture": {"type": "string"}
      },
      "required": ["summary", "files"]
    }`
	var schema map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &schema))

	payload, err := json.Marshal(map[string]any{
		"summary":      "Root cause is the retry loop.",
		"files":        []any{"internal/agent/retry.go:44"},
		"architecture": "Retry re-enters without resetting state.",
	})
	require.NoError(t, err)

	text := projectYieldPayload(payload, schema)
	require.Less(t, strings.Index(text, "Files:"), strings.Index(text, "Architecture:"))
}
