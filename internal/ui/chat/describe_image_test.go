package chat

import (
	"testing"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/anim"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func TestDescribeImageToolPendingShowsAnalyzingImageAnimation(t *testing.T) {
	t.Parallel()

	sty := styles.DefaultStyles()
	renderer := &DescribeImageToolRenderContext{}
	pendingAnim := anim.New(anim.Settings{
		ID:          "describe-image-pending-test",
		Size:        15,
		LabelColor:  sty.FgBase,
		GradColorA:  sty.Primary,
		GradColorB:  sty.Secondary,
		CycleColors: true,
	})
	pendingAnim.Tick()

	out := renderer.RenderTool(&sty, 120, &ToolRenderOpts{
		ToolCall: message.ToolCall{
			Name:     tools.DescribeImageToolName,
			Input:    `{"path":"screenshot.png"}`,
			Finished: true,
		},
		Anim:       pendingAnim,
		IsSpinning: true,
		Status:     ToolStatusRunning,
	})

	plain := ansi.Strip(out)
	require.Contains(t, plain, "Describe Image")
	require.Contains(t, plain, "screenshot.png")
	require.Contains(t, plain, "analyzing image")
}

func TestDescribeImageToolSpinsUntilResultArrives(t *testing.T) {
	t.Parallel()

	sty := styles.DefaultStyles()
	item := NewDescribeImageToolMessageItem(&sty, message.ToolCall{
		ID:       "tool-1",
		Name:     tools.DescribeImageToolName,
		Input:    `{"path":"screenshot.png"}`,
		Finished: true,
	}, nil, false).(*DescribeImageToolMessageItem)

	require.True(t, item.IsAnimating())
	plain := ansi.Strip(item.RawRender(120))
	require.Contains(t, plain, "analyzing image")
}

func TestDescribeImageToolStopsSpinningAfterResult(t *testing.T) {
	t.Parallel()

	sty := styles.DefaultStyles()
	result := message.ToolResult{
		Name:    tools.DescribeImageToolName,
		Content: "A terminal screenshot.",
	}
	item := NewDescribeImageToolMessageItem(&sty, message.ToolCall{
		ID:       "tool-1",
		Name:     tools.DescribeImageToolName,
		Input:    `{"path":"screenshot.png"}`,
		Finished: true,
	}, &result, false).(*DescribeImageToolMessageItem)

	require.False(t, item.IsAnimating())
	plain := ansi.Strip(item.RawRender(120))
	require.NotContains(t, plain, "analyzing image")
	require.Contains(t, plain, "A terminal screenshot.")
}
