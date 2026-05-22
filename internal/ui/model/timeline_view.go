package model

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/timeline"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/x/ansi"
)

type subagentTask struct {
	id        string
	title     string
	eventType timeline.EventType
	status    string
	timestamp int64
}

func isActiveTask(eventType timeline.EventType) bool {
	switch eventType {
	case timeline.EventChildSessionFinished,
		timeline.EventChildSessionFailed,
		timeline.EventChildSessionCanceled:
		return false
	default:
		return true
	}
}

func getSubagentTasks(events []timeline.Event) []subagentTask {
	taskMap := make(map[string]subagentTask)
	for _, event := range events {
		if event.ChildSessionID == "" {
			continue
		}

		task, exists := taskMap[event.ChildSessionID]
		if !exists {
			task = subagentTask{
				id: event.ChildSessionID,
			}
		}

		if event.Title != "" {
			task.title = event.Title
		}
		task.eventType = event.Type
		task.status = event.Status
		task.timestamp = event.Timestamp

		taskMap[event.ChildSessionID] = task
	}

	var tasks []subagentTask
	for _, t := range taskMap {
		tasks = append(tasks, t)
	}

	// Sort: Active tasks first, then by timestamp (newest first).
	sort.Slice(tasks, func(i, j int) bool {
		iActive := isActiveTask(tasks[i].eventType)
		jActive := isActiveTask(tasks[j].eventType)
		if iActive != jActive {
			return iActive
		}
		return tasks[i].timestamp > tasks[j].timestamp
	})

	return tasks
}

func (m *UI) timelineInfo(width, maxItems int, isSection bool) string {
	t := m.com.Styles

	title := t.ResourceGroupTitle.Render("Recent Activity")
	if isSection {
		title = common.Section(t, title, width)
	}

	var sb strings.Builder

	// 1. Get subagent tasks.
	tasks := getSubagentTasks(m.timelineEvents)
	numTasks := len(tasks)

	// Calculate the number of lines required to render the task board.
	taskLines := 0
	if numTasks > 0 {
		numVisible := min(numTasks, 3)
		taskLines += numVisible
		if numTasks > 3 {
			taskLines += 1 // Overflow hint line ("…and N more tasks").
		}
		taskLines += 1 // Divider line ("┈...").
	}

	// 2. Collect regular timeline events (excluding subagent tasks).
	var nonTaskEvents []timeline.Event
	for _, event := range m.timelineEvents {
		if event.ChildSessionID == "" {
			nonTaskEvents = append(nonTaskEvents, event)
		}
	}

	// 3. Render the task board.
	if numTasks > 0 {
		numVisible := min(numTasks, 3)
		for _, task := range tasks[:numVisible] {
			var icon string
			var style lipgloss.Style

			switch task.eventType {
			case timeline.EventChildSessionFinished:
				icon = "✔"
				style = t.Tool.JobIconSuccess
			case timeline.EventChildSessionFailed:
				icon = "✖"
				style = t.Tool.JobIconError
			case timeline.EventChildSessionCanceled:
				icon = "-"
				style = t.Subtle
			case timeline.EventChildSessionBlocked:
				icon = "⏸"
				style = t.Muted
			default:
				// Started or Progress (running).
				icon = "⏵"
				style = t.ResourceName
			}

			iconStr := style.Render(icon)
			name := task.title
			if name == "" {
				name = "Subagent"
			}
			maxNameWidth := max(width-5, 5)
			name = ansi.Truncate(name, maxNameWidth, "…")

			sb.WriteString(fmt.Sprintf(" %s  %s\n", iconStr, t.ResourceName.Render(name)))
		}

		if numTasks > 3 {
			hiddenTasks := numTasks - 3
			sb.WriteString(t.ResourceAdditionalText.Render(fmt.Sprintf("  …and %d more tasks\n", hiddenTasks)))
		}

		dashWidth := width
		if dashWidth <= 0 {
			dashWidth = 30
		}
		sb.WriteString(t.ResourceAdditionalText.Render(strings.Repeat("┈", dashWidth)) + "\n")
	}

	// 4. Render regular timeline events.
	remainingItems := maxItems - taskLines
	if remainingItems > 0 && len(nonTaskEvents) > 0 {
		list := timelineList(t, nonTaskEvents, width, remainingItems)
		sb.WriteString(list)
	} else if numTasks == 0 && len(nonTaskEvents) == 0 {
		sb.WriteString(t.ResourceAdditionalText.Render("None"))
	}

	content := strings.TrimSuffix(sb.String(), "\n")
	return lipgloss.NewStyle().Width(width).Render(fmt.Sprintf("%s\n\n%s", title, content))
}

func timelineList(t *styles.Styles, events []timeline.Event, width, maxItems int) string {
	if maxItems <= 0 {
		return ""
	}

	visibleLimit := min(len(events), maxItems)
	showOverflow := len(events) > maxItems && maxItems > 1
	if showOverflow {
		visibleLimit = maxItems - 1
	}

	rendered := make([]string, 0, visibleLimit+1)
	for i := len(events) - 1; i >= len(events)-visibleLimit; i-- {
		rendered = append(rendered, timelineListItem(t, events[i], width))
	}

	if showOverflow {
		hidden := len(events) - visibleLimit
		rendered = append(rendered, t.ResourceAdditionalText.Render(fmt.Sprintf("…and %d earlier", hidden)))
	}

	return lipgloss.JoinVertical(lipgloss.Left, rendered...)
}

func timelineListItem(t *styles.Styles, event timeline.Event, width int) string {
	title, description := timelineEventSummary(event)
	extra := ""
	if event.Timestamp > 0 {
		extra = t.ResourceAdditionalText.Render(time.UnixMilli(event.Timestamp).Format("15:04"))
	}

	return common.Status(t, common.StatusOpts{
		Title:        t.ResourceName.Render(title),
		Description:  description,
		ExtraContent: extra,
	}, width)
}

func timelineEventSummary(event timeline.Event) (string, string) {
	switch event.Type {
	case timeline.EventModeChanged:
		parts := make([]string, 0, 2)
		if label := timelineLabel(event.CollaborationMode); label != "" {
			parts = append(parts, label)
		}
		if label := timelineLabel(event.PermissionMode); label != "" {
			parts = append(parts, label)
		}
		if len(parts) == 0 {
			return "Mode", "updated"
		}
		return "Mode", strings.Join(parts, " • ")
	case timeline.EventToolStarted:
		title := timelineTitle(event.ToolName, event.Title, "Tool")
		if status := timelineLabel(event.Status); status != "" {
			return title, status
		}
		return title, "started"
	case timeline.EventToolProgress:
		title := timelineTitle(event.ToolName, event.Title, "Tool")
		if content := timelinePreview(event.Content); content != "" {
			return title, content
		}
		if status := timelineLabel(event.Status); status != "" {
			return title, status
		}
		return title, "updated"
	case timeline.EventToolFinished:
		title := timelineTitle(event.ToolName, event.Title, "Tool")
		status := timelineLabel(event.Status)
		content := timelinePreview(event.Content)
		switch {
		case status != "" && content != "":
			return title, status + " • " + content
		case status != "":
			return title, status
		case content != "":
			return title, content
		default:
			return title, "finished"
		}
	case timeline.EventChildSessionStarted:
		return "Subagent", timelineChildDescription("started", event.Title)
	case timeline.EventChildSessionProgress:
		status := timelineLabel(event.Status)
		if status == "" {
			status = "progress"
		}
		return "Subagent", timelineChildDescription(status, event.Title)
	case timeline.EventChildSessionFinished, timeline.EventChildSessionFailed, timeline.EventChildSessionCanceled, timeline.EventChildSessionBlocked:
		status := timelineLabel(event.Status)
		if status == "" {
			status = "finished"
		}
		return "Subagent", timelineChildDescription(status, event.Title)
	default:
		title := timelineTitle(string(event.Type), event.Title, "Event")
		if content := timelinePreview(event.Content); content != "" {
			return title, content
		}
		return title, ""
	}
}

func timelineChildDescription(status, title string) string {
	title = timelinePreview(title)
	if title == "" {
		return status
	}
	return status + " • " + title
}

func timelineTitle(values ...string) string {
	for _, value := range values {
		if label := timelineLabel(value); label != "" {
			return label
		}
	}
	return "Event"
}

func timelineLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	value = strings.ReplaceAll(value, "_", " ")
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return ""
	}
	return strings.ToUpper(value[:1]) + value[1:]
}

func timelinePreview(value string) string {
	value = ansi.Strip(value)
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return ""
	}
	return ansi.Truncate(value, 48, "…")
}
