package guiapi

import (
	"encoding/json"
	"testing"

	"github.com/charmbracelet/crush/internal/sessionevent"
	"github.com/stretchr/testify/require"
)

func TestToolEventPreservesClientFSSourceURIAndRevision(t *testing.T) {
	t.Parallel()
	payload := toWirePayload(sessionevent.ToolEvent{
		ToolCallID: "tool-1", Name: "edit", Status: "completed",
		Files: []sessionevent.ToolFile{{
			Path: "main.go", SourceURI: "vscode-notebook-cell:///main.go", Revision: "buffer:3",
		}},
	})
	wire, err := json.Marshal(payload)
	require.NoError(t, err)
	require.JSONEq(t, `{
      "messageId":"","toolCallId":"tool-1","name":"edit","status":"completed",
      "files":[{"path":"main.go","sourceUri":"vscode-notebook-cell:///main.go","revision":"buffer:3"}]
    }`, string(wire))
}
