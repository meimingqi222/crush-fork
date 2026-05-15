package chat

import (
	"encoding/json"
	"testing"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func TestReadToolRenderContextRendersSanitizedResultAsPlainText(t *testing.T) {
	t.Parallel()

	sty := styles.DefaultStyles()
	renderer := &ReadToolRenderContext{}

	viewMeta, err := json.Marshal(tools.ReadResponseMetadata{
		Path:    "/tmp/test.go",
		Content: "<file>\n     1|package main\n</file>",
	})
	require.NoError(t, err)

	result := (&message.ToolResult{
		Name:     tools.ReadToolName,
		Content:  "<file>\n     1|package main\n</file>",
		Metadata: string(viewMeta),
	}).WithAutoReview(message.ToolResultAutoReview{
		Suspicious: true,
		Sanitized:  true,
		Reason:     "Tool output matched local prompt-injection heuristics (assistant message).",
	})

	out := renderer.RenderTool(&sty, 120, &ToolRenderOpts{
		ToolCall: message.ToolCall{
			Name:  tools.ReadToolName,
			Input: `{"path":"/tmp/test.go","offset":0,"limit":20}`,
		},
		Result:     &result,
		Status:     ToolStatusSuccess,
		IsSpinning: false,
	})

	require.Contains(t, out, "Tool output was withheld from the model")
	require.Contains(t, out, "Reason: Tool output matched local prompt-injection heuristics")
	require.NotContains(t, out, "<file>")
	require.NotContains(t, out, "|package main")
}

func TestReadToolRenderContextRendersSanitizedResultWithWindowsPath(t *testing.T) {
	t.Parallel()

	sty := styles.DefaultStyles()
	renderer := &ReadToolRenderContext{}

	viewMeta, err := json.Marshal(tools.ReadResponseMetadata{
		Path:    `C:\\Users\\dev\\project\\test.go`,
		Content: "<file>\n     1|package main\n</file>",
	})
	require.NoError(t, err)

	result := (&message.ToolResult{
		Name:     tools.ReadToolName,
		Content:  "<file>\n     1|package main\n</file>",
		Metadata: string(viewMeta),
	}).WithAutoReview(message.ToolResultAutoReview{
		Suspicious: true,
		Sanitized:  true,
		Reason:     "windows path case",
	})

	out := renderer.RenderTool(&sty, 120, &ToolRenderOpts{
		ToolCall: message.ToolCall{
			Name:  tools.ReadToolName,
			Input: `{"path":"C:\\Users\\dev\\project\\test.go","offset":0,"limit":20}`,
		},
		Result:     &result,
		Status:     ToolStatusSuccess,
		IsSpinning: false,
	})

	require.Contains(t, out, "Tool output was withheld from the model")
	require.Contains(t, out, "Reason: windows path case")
	require.Contains(t, out, `C:\Users\dev\project\test.go`)
	require.NotContains(t, out, "|package main")
}
