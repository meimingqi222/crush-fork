package model

import (
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/timeline"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func TestTimelineEventSummary(t *testing.T) {
	t.Parallel()

	t.Run("mode changed shows current modes", func(t *testing.T) {
		t.Parallel()

		title, description := timelineEventSummary(timeline.Event{
			Type:              timeline.EventModeChanged,
			CollaborationMode: "plan",
			PermissionMode:    "auto",
		})

		require.Equal(t, "Mode", title)
		require.Equal(t, "Plan • Auto", description)
	})

	t.Run("tool finished shows status and preview", func(t *testing.T) {
		t.Parallel()

		title, description := timelineEventSummary(timeline.Event{
			Type:     timeline.EventToolFinished,
			ToolName: "bash",
			Status:   "completed",
			Content:  "line one\nline two",
		})

		require.Equal(t, "Bash", title)
		require.Equal(t, "Completed • line one line two", description)
	})

	t.Run("child session blocked shows blocked label", func(t *testing.T) {
		t.Parallel()

		title, description := timelineEventSummary(timeline.Event{
			Type:   timeline.EventChildSessionBlocked,
			Title:  "explore task",
			Status: "blocked",
		})

		require.Equal(t, "Subagent", title)
		require.Equal(t, "Blocked • explore task", description)
	})
}

func TestTimelineListRendersNewestFirst(t *testing.T) {
	t.Parallel()

	theme := styles.DefaultStyles()
	rendered := ansi.Strip(timelineList(&theme, []timeline.Event{
		{Type: timeline.EventChildSessionStarted, Title: "alpha"},
		{Type: timeline.EventChildSessionStarted, Title: "beta"},
		{Type: timeline.EventChildSessionStarted, Title: "gamma"},
		{Type: timeline.EventChildSessionStarted, Title: "delta"},
	}, 120, 3))

	require.Contains(t, rendered, "delta")
	require.Contains(t, rendered, "gamma")
	require.NotContains(t, rendered, "beta")
	require.NotContains(t, rendered, "alpha")
	require.Contains(t, rendered, "…and 2 earlier")
	require.Less(t, strings.Index(rendered, "delta"), strings.Index(rendered, "gamma"))
}

func TestGetDynamicHeightLimitsIncludesTimeline(t *testing.T) {
	t.Parallel()

	maxFiles, maxLSPs, maxMCPs, maxTimeline := getDynamicHeightLimits(24)
	require.GreaterOrEqual(t, maxFiles, 2)
	require.GreaterOrEqual(t, maxLSPs, 2)
	require.GreaterOrEqual(t, maxMCPs, 2)
	require.GreaterOrEqual(t, maxTimeline, 2)
}

func TestTimelineInfoSubagentTasksDisplay(t *testing.T) {
	ui := testExecutionModeUI(t, `{"options":{"disable_provider_auto_update":true},"tools":{}}`)
	ui.timelineEvents = []timeline.Event{
		{Type: timeline.EventToolFinished, ToolName: "bash", Status: "completed", Content: "run build", Timestamp: 100},
		{Type: timeline.EventChildSessionStarted, ChildSessionID: "child-1", Title: "Explore subagent", Status: "running", Timestamp: 110},
		{Type: timeline.EventChildSessionFinished, ChildSessionID: "child-2", Title: "General subagent", Status: "completed", Timestamp: 120},
	}

	rendered := ansi.Strip(ui.timelineInfo(80, 6, false))

	require.Contains(t, rendered, "⏵  Explore subagent")
	require.Contains(t, rendered, "✔  General subagent")
	require.Contains(t, rendered, "┈")
	require.Contains(t, rendered, "Bash")
	require.Contains(t, rendered, "Completed • run build")

	occurrences := strings.Count(rendered, "Explore subagent")
	require.Equal(t, 1, occurrences)
}
