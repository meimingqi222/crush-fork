package tools

import (
	"context"
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func newTestServices(t *testing.T) (session.Service, message.Service) {
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	// Close the SQLite connection before TempDir cleanup so Windows can
	// remove the database file without hitting a file-lock error.
	t.Cleanup(func() { _ = conn.Close() })
	q := db.New(conn)
	sessions := session.NewService(q, nil)
	messages := message.NewService(q)
	return sessions, messages
}

func TestYieldToolReturnsYieldMetadata(t *testing.T) {
	t.Parallel()

	tool := NewYieldTool(nil)
	input, err := json.Marshal(YieldParams{
		Data:   "full result text",
		Status: "completed_with_warnings",
	})
	require.NoError(t, err)

	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "call-1", Name: YieldToolName, Input: string(input)})
	require.NoError(t, err)
	require.False(t, resp.IsError)

	yield, ok := message.ParseToolResultYield(resp.Metadata)
	require.True(t, ok)
	require.Equal(t, "full result text", yield.Data)
	require.Equal(t, "completed_with_warnings", yield.Status)
}

func TestYieldToolRejectsEmptyData(t *testing.T) {
	t.Parallel()

	tool := NewYieldTool(nil)
	input, err := json.Marshal(YieldParams{Status: "completed"})
	require.NoError(t, err)

	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "call-1", Name: YieldToolName, Input: string(input)})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "data or payload is required")
}

func TestYieldToolRejectsInvalidStatus(t *testing.T) {
	t.Parallel()

	tool := NewYieldTool(nil)
	input, err := json.Marshal(YieldParams{Data: "result", Status: "done"})
	require.NoError(t, err)

	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "call-1", Name: YieldToolName, Input: string(input)})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "status must be one of")
}

func TestYieldToolDefaultsEmptyStatusToCompleted(t *testing.T) {
	t.Parallel()

	tool := NewYieldTool(nil)
	input, err := json.Marshal(YieldParams{Data: "result"})
	require.NoError(t, err)

	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "call-1", Name: YieldToolName, Input: string(input)})
	require.NoError(t, err)
	require.False(t, resp.IsError)

	yield, ok := message.ParseToolResultYield(resp.Metadata)
	require.True(t, ok)
	require.Equal(t, "completed", yield.Status)
}

func TestYieldToolRejectsDuplicateCalls(t *testing.T) {
	t.Parallel()

	sessions, messages := newTestServices(t)
	tool := NewYieldTool(messages)

	sess, err := sessions.Create(t.Context(), "Test")
	require.NoError(t, err)

	ctx := context.WithValue(context.Background(), SessionIDContextKey, sess.ID)
	input, err := json.Marshal(YieldParams{Data: "first", Status: "completed"})
	require.NoError(t, err)

	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "call-1", Name: YieldToolName, Input: string(input)})
	require.NoError(t, err)
	require.False(t, resp.IsError)

	// Simulate the agent runtime persisting the yield tool result.
	_, err = messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Tool,
		Parts: []message.ContentPart{
			message.ToolResult{Name: YieldToolName}.WithYield(message.ToolResultYield{
				Data:   "first",
				Status: "completed",
			}),
		},
	})
	require.NoError(t, err)

	// Second call should be rejected.
	resp, err = tool.Run(ctx, fantasy.ToolCall{ID: "call-2", Name: YieldToolName, Input: string(input)})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "yield has already been called")
}

func TestYieldToolAllowsParentYieldAfterSubagentYield(t *testing.T) {
	t.Parallel()

	sessions, messages := newTestServices(t)
	tool := NewYieldTool(messages)

	sess, err := sessions.Create(t.Context(), "Test")
	require.NoError(t, err)

	ctx := context.WithValue(context.Background(), SessionIDContextKey, sess.ID)

	// Simulate a subagent yielding by storing a ToolResult with Name = "agent",
	// containing Yield metadata in the parent session messages.
	_, err = messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Tool,
		Parts: []message.ContentPart{
			message.ToolResult{Name: "agent"}.WithYield(message.ToolResultYield{
				Data:   "subagent output",
				Status: "completed",
			}),
		},
	})
	require.NoError(t, err)

	// The parent agent calling yield now should NOT be blocked by the subagent's yield.
	input, err := json.Marshal(YieldParams{Data: "parent output", Status: "completed"})
	require.NoError(t, err)

	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "call-parent", Name: YieldToolName, Input: string(input)})
	require.NoError(t, err)
	require.False(t, resp.IsError)
}

// exploreOutputSchema returns the same schema used in config.go for the
// explore subagent, used by repair tests.
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
		"required": []any{"summary", "files"},
	}
}

func TestRepairPayloadInjectsMissingRequiredString(t *testing.T) {
	t.Parallel()

	// Payload has "files" but is missing required "summary".
	payload := json.RawMessage(`{"files":[{"path":"foo.go","description":"found it"}]}`)
	repaired, err := repairPayloadAgainstSchema(payload, exploreOutputSchema())
	require.NoError(t, err)
	require.NotNil(t, repaired)

	var result map[string]any
	require.NoError(t, json.Unmarshal(repaired, &result))
	// "summary" should have been injected as empty string.
	require.Contains(t, result, "summary")
	require.Equal(t, "", result["summary"])
	// "files" should be preserved.
	require.NotNil(t, result["files"])
}

func TestRepairPayloadInjectsMissingRequiredArray(t *testing.T) {
	t.Parallel()

	// Payload has "summary" but is missing required "files" (array).
	payload := json.RawMessage(`{"summary":"everything is fine"}`)
	repaired, err := repairPayloadAgainstSchema(payload, exploreOutputSchema())
	require.NoError(t, err)
	require.NotNil(t, repaired)

	var result map[string]any
	require.NoError(t, json.Unmarshal(repaired, &result))
	require.Contains(t, result, "files")
	arr, ok := result["files"].([]any)
	require.True(t, ok, "files should be an array")
	require.Empty(t, arr)
}

func TestRepairPayloadRemovesUnknownFields(t *testing.T) {
	t.Parallel()

	// Payload has extra "random" field not in schema.
	payload := json.RawMessage(`{"summary":"ok","files":[],"random":"garbage"}`)
	repaired, err := repairPayloadAgainstSchema(payload, exploreOutputSchema())
	require.NoError(t, err)
	require.NotNil(t, repaired)

	var result map[string]any
	require.NoError(t, json.Unmarshal(repaired, &result))
	_, hasRandom := result["random"]
	require.False(t, hasRandom, "unknown field 'random' should be removed")
	require.Contains(t, result, "summary")
	require.Contains(t, result, "files")
}

func TestRepairPayloadCoercesStringToNumber(t *testing.T) {
	t.Parallel()

	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"count": map[string]any{"type": "number"},
			"name":  map[string]any{"type": "string"},
		},
		"required": []any{"name", "count"},
	}

	payload := json.RawMessage(`{"name":"test","count":"42"}`)
	repaired, err := repairPayloadAgainstSchema(payload, schema)
	require.NoError(t, err)
	require.NotNil(t, repaired)

	var result map[string]any
	require.NoError(t, json.Unmarshal(repaired, &result))
	require.Equal(t, 42.0, result["count"])
}

func TestRepairPayloadReturnsNilWhenNoChangesNeeded(t *testing.T) {
	t.Parallel()

	// Payload is already complete — no repair needed.
	payload := json.RawMessage(`{"summary":"all good","files":[],"architecture":"flat"}`)
	repaired, err := repairPayloadAgainstSchema(payload, exploreOutputSchema())
	require.NoError(t, err)
	require.Nil(t, repaired, "no repair needed when payload already conforms")
}
