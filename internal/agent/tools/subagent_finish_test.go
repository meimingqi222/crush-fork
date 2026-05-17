package tools

import (
	"context"
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestSubagentFinishToolReturnsStructuredMetadata(t *testing.T) {
	t.Parallel()

	tool := NewSubagentFinishTool(nil)
	input, err := json.Marshal(SubagentFinishParams{
		Status:       string(message.ToolResultSubtaskStatusCompletedWithWarnings),
		Summary:      "done with warning",
		Artifacts:    []string{"artifact.txt"},
		FilesTouched: []string{"a.go"},
		PatchPlan:    []string{"update a.go"},
		TestResults:  []string{"go test ./..."},
		Followups:    []string{"review"},
		Risks:        []string{"warning"},
		NextActions:  []string{"merge"},
		Confidence:   "medium",
		Error:        "minor warning",
	})
	require.NoError(t, err)

	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "call-1", Name: SubagentFinishToolName, Input: string(input)})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	finish, ok := message.ParseToolResultSubagentFinish(resp.Metadata)
	require.True(t, ok)
	require.Equal(t, message.ToolResultSubtaskStatusCompletedWithWarnings, finish.Status)
	require.Equal(t, "done with warning", finish.Summary)
}

func TestSubagentFinishToolRejectsInvalidStatuses(t *testing.T) {
	t.Parallel()

	tool := NewSubagentFinishTool(nil)
	input, err := json.Marshal(SubagentFinishParams{Status: string(message.ToolResultSubtaskStatusRunning)})
	require.NoError(t, err)

	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "call-1", Name: SubagentFinishToolName, Input: string(input)})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "status must be one of")
}
