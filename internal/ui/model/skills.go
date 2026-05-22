package model

import (
	"fmt"
	"path/filepath"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
)

// skillsInfo renders the Skills status section showing active/loaded skills and errors.
func (m *UI) skillsInfo(width, maxItems int, isSection bool) string {
	t := m.com.Styles

	var items []common.StatusOpts

	// Add loaded skills (success state)
	for _, sk := range m.skillsState.Loaded {
		items = append(items, common.StatusOpts{
			Icon:        t.ResourceOnlineIcon.String(),
			Title:       t.ResourceName.Render(sk.Name),
			Description: t.ResourceStatus.Render("available"),
		})
	}

	// Add errored skills (error state)
	for path, err := range m.skillsState.Errors {
		name := filepath.Base(filepath.Dir(path))
		if name == "" || name == "." {
			name = filepath.Base(path)
		}
		items = append(items, common.StatusOpts{
			Icon:        t.ResourceErrorIcon.String(),
			Title:       t.ResourceName.Render(name),
			Description: t.ResourceStatus.Render(fmt.Sprintf("error: %s", err.Error())),
		})
	}

	title := t.ResourceGroupTitle.Render("Skills")
	if isSection {
		title = common.Section(t, title, width)
	}

	list := t.ResourceAdditionalText.Render("None")
	if len(items) > 0 {
		list = skillsList(t, items, width, maxItems)
	}

	return lipgloss.NewStyle().Width(width).Render(fmt.Sprintf("%s\n\n%s", title, list))
}

func skillsList(t *styles.Styles, items []common.StatusOpts, width, maxItems int) string {
	if maxItems <= 0 {
		return ""
	}
	var rendered []string

	for _, item := range items {
		rendered = append(rendered, common.Status(t, item, width))
	}

	if len(rendered) > maxItems {
		visibleItems := rendered[:maxItems-1]
		remaining := len(rendered) - maxItems + 1
		visibleItems = append(visibleItems, t.ResourceAdditionalText.Render(fmt.Sprintf("…and %d more", remaining)))
		return lipgloss.JoinVertical(lipgloss.Left, visibleItems...)
	}
	return lipgloss.JoinVertical(lipgloss.Left, rendered...)
}
