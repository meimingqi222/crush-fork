package model

import (
	"fmt"
	"image"
	"strings"

	"charm.land/lipgloss/v2"
	sessionpkg "github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/logo"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/ultraviolet/layout"
)

// invalidateSidebarCache marks the sidebar cache as dirty so the next
// drawSidebar call will re-render the sidebar content.
func (m *UI) invalidateSidebarCache() {
	m.sidebarCacheDirty = true
}

// modelInfo renders the current model information including reasoning
// settings and context usage/cost for the sidebar.
func (m *UI) modelInfo(width int) string {
	model := m.selectedLargeModel()
	reasoningInfo := ""
	providerName := ""

	if model != nil {
		// Get provider name first
		providerConfig, ok := m.com.Config().Providers.Get(model.ModelCfg.Provider)
		if ok {
			providerName = providerConfig.Name

			// Only check reasoning if model can reason.
			if model.CatwalkCfg.CanReason {
				effectiveEffort := model.CatwalkCfg.DefaultReasoningEffort
				// Check for user-selected reasoning effort in ProviderOptions.
				if model.ModelCfg.ProviderOptions != nil {
					if effort, ok := model.ModelCfg.ProviderOptions["reasoning_effort"].(string); ok && effort != "" {
						effectiveEffort = effort
					}
				}
				if len(model.CatwalkCfg.ReasoningLevels) == 0 {
					// Anthropic-style thinking models. Think==nil or true means on
					// (default), Think==&false means explicitly disabled.
					thinkingDisabled := model.ModelCfg.Think != nil && !*model.ModelCfg.Think
					if thinkingDisabled {
						reasoningInfo = "Thinking Off"
					} else {
						displayEffort := effectiveEffort
						if displayEffort == "" {
							displayEffort = "high"
						}
						reasoningInfo = fmt.Sprintf("Thinking On (%s)", common.FormatReasoningEffort(displayEffort))
					}
				} else {
					// Models with explicit reasoning levels (e.g. OpenAI).
					thinkingDisabled := model.ModelCfg.Think != nil && !*model.ModelCfg.Think
					if thinkingDisabled {
						reasoningInfo = "Reasoning Off"
					} else {
						displayEffort := effectiveEffort
						if displayEffort == "" {
							displayEffort = "high"
						}
						reasoningInfo = fmt.Sprintf("Reasoning %s", common.FormatReasoningEffort(displayEffort))
					}
				}
			}
		}
	}

	var modelContext *common.ModelContextInfo
	if model != nil && m.session != nil {
		// Use the per-frame cached snapshot when available to avoid
		// walking the message list a second time.
		usage := m.frameUsageSnapshotCached()
		modelContext = &common.ModelContextInfo{
			OutputTokens: usage.OutputTokens,
			TotalTokens:  usage.TotalTokens,
			Cost:         m.session.Cost,
			ModelContext: usage.ContextWindow,
		}
	}
	modelName := ""
	if model != nil {
		modelName = model.CatwalkCfg.Name
	}
	info := common.ModelInfo(m.com.Styles, modelName, providerName, reasoningInfo, modelContext, width)
	modeLine := m.modeInfo(width)
	if modeLine == "" {
		return info
	}
	return lipgloss.JoinVertical(lipgloss.Left, info, modeLine)
}

func (m *UI) modeInfo(width int) string {
	if m.session == nil || m.com.App == nil {
		return ""
	}

	modes := make([]string, 0, 3)
	roleLabel := strings.ToUpper(m.sessionRoleLabel(m.session))
	if m.isSubagentSession() {
		if roleLabel != "" {
			modes = append(modes, "SUBAGENT "+roleLabel)
		} else {
			modes = append(modes, "SUBAGENT")
		}
	} else {
		modes = append(modes, "SESSION "+roleLabel)
	}

	switch m.session.CollaborationMode {
	case sessionpkg.CollaborationModePlan:
		modes = append(modes, "PLAN")
	case sessionpkg.CollaborationModePlanPaused:
		modes = append(modes, "PLAN PAUSED")
	case sessionpkg.CollaborationModeOrchestrate:
		modes = append(modes, "ORCHESTRATE")
	default:
		modes = append(modes, "STANDARD")
	}

	if !m.session.IsPlanFlow() {
		switch m.currentExecutionMode() {
		case executionModeAuto:
			modes = append(modes, "AUTO")
		case executionModeYolo:
			modes = append(modes, "YOLO")
		default:
			modes = append(modes, "ASK")
		}
	}

	if goal := m.session.Goal; goal.Status != "" && goal.Status != sessionpkg.GoalStatusComplete && goal.Status != sessionpkg.GoalStatusDropped {
		goalLabel := fmt.Sprintf("GOAL %s", goal.Status)
		if text := strings.TrimSpace(goal.Text); text != "" {
			runes := []rune(text)
			if len(runes) > 18 {
				text = string(runes[:18]) + "…"
			}
			goalLabel += " · " + text
		}
		if goal.HasBudget() {
			goalLabel += fmt.Sprintf(" · %d/%d", goal.TokensUsed, goal.TokenBudget)
		}
		modes = append(modes, goalLabel)
	}

	if len(modes) == 0 {
		return ""
	}

	text := fmt.Sprintf("Modes: %s", strings.Join(modes, " | "))
	return m.com.Styles.Subtle.PaddingLeft(2).Width(width).Render(text)
}

// getDynamicHeightLimits will give us the num of items to show in each section based on the height
// some items are more important than others.
func getDynamicHeightLimits(availableHeight int) (maxFiles, maxLSPs, maxMCPs, maxSkills, maxTimeline int) {
	const (
		minItemsPerSection      = 2
		defaultMaxFilesShown    = 10
		defaultMaxLSPsShown     = 8
		defaultMaxMCPsShown     = 8
		defaultMaxSkillsShown   = 6
		defaultMaxTimelineShown = 6
		minAvailableHeightLimit = 15
	)

	// If we have very little space, use minimum values.
	if availableHeight < minAvailableHeightLimit {
		return minItemsPerSection, minItemsPerSection, minItemsPerSection, minItemsPerSection, minItemsPerSection
	}

	// Distribute available height among the five sections.
	totalSections := 5
	heightPerSection := availableHeight / totalSections

	// Calculate limits for each section, ensuring minimums.
	maxFiles = max(minItemsPerSection, min(defaultMaxFilesShown, heightPerSection))
	maxLSPs = max(minItemsPerSection, min(defaultMaxLSPsShown, heightPerSection))
	maxMCPs = max(minItemsPerSection, min(defaultMaxMCPsShown, heightPerSection))
	maxSkills = max(minItemsPerSection, min(defaultMaxSkillsShown, heightPerSection))
	maxTimeline = max(minItemsPerSection, min(defaultMaxTimelineShown, heightPerSection))

	// If we have extra space, give it to files first.
	remainingHeight := availableHeight - (maxFiles + maxLSPs + maxMCPs + maxSkills + maxTimeline)
	if remainingHeight > 0 {
		extraForFiles := min(remainingHeight, defaultMaxFilesShown-maxFiles)
		maxFiles += extraForFiles
		remainingHeight -= extraForFiles

		if remainingHeight > 0 {
			extraForLSPs := min(remainingHeight, defaultMaxLSPsShown-maxLSPs)
			maxLSPs += extraForLSPs
			remainingHeight -= extraForLSPs

			if remainingHeight > 0 {
				extraForMCPs := min(remainingHeight, defaultMaxMCPsShown-maxMCPs)
				maxMCPs += extraForMCPs
				remainingHeight -= extraForMCPs

				if remainingHeight > 0 {
					extraForSkills := min(remainingHeight, defaultMaxSkillsShown-maxSkills)
					maxSkills += extraForSkills
					remainingHeight -= extraForSkills

					if remainingHeight > 0 {
						maxTimeline += min(remainingHeight, defaultMaxTimelineShown-maxTimeline)
					}
				}
			}
		}
	}

	return maxFiles, maxLSPs, maxMCPs, maxSkills, maxTimeline
}

// sidebar renders the chat sidebar containing session title, working
// directory, model info, file list, LSP status, MCP status, and Skills status.
func (m *UI) drawSidebar(scr uv.Screen, area uv.Rectangle) {
	if m.session == nil {
		return
	}

	width := area.Dx()
	height := area.Dy()

	// Return cached sidebar when nothing changed and dimensions match.
	if !m.sidebarCacheDirty && m.sidebarCacheContent != "" &&
		m.sidebarCacheWidth == width && m.sidebarCacheHeight == height {
		uv.NewStyledString(m.sidebarCacheContent).Draw(scr, area)
		return
	}

	rendered := m.renderSidebarContent(width, height)
	m.sidebarCacheContent = rendered
	m.sidebarCacheWidth = width
	m.sidebarCacheHeight = height
	m.sidebarCacheDirty = false

	uv.NewStyledString(rendered).Draw(scr, area)
}

// renderSidebarContent builds the full sidebar string.
func (m *UI) renderSidebarContent(width, height int) string {
	const logoHeightBreakpoint = 30

	t := m.com.Styles

	title := t.Muted.Width(width).MaxHeight(2).Render(m.session.Title)
	cwd := common.PrettyPath(t, m.com.Store().WorkingDir(), width)
	sidebarLogo := m.sidebarLogo
	if height < logoHeightBreakpoint {
		sidebarLogo = logo.SmallRender(m.com.Styles, width)
	}
	blocks := []string{
		sidebarLogo,
		title,
		"",
		cwd,
		"",
		m.modelInfo(width),
		"",
	}

	sidebarHeader := lipgloss.JoinVertical(
		lipgloss.Left,
		blocks...,
	)

	var remainingHeightArea image.Rectangle
	layout.Vertical(
		layout.Len(lipgloss.Height(sidebarHeader)),
		layout.Fill(1),
	).Split(m.layout.sidebar).Assign(new(image.Rectangle), &remainingHeightArea)
	remainingHeight := remainingHeightArea.Dy() - 14
	maxFiles, maxLSPs, maxMCPs, maxSkills, maxTimeline := getDynamicHeightLimits(remainingHeight)

	lspSection := m.lspInfo(width, maxLSPs, true)
	mcpSection := m.mcpInfo(width, maxMCPs, true)
	skillsSection := m.skillsInfo(width, maxSkills, true)
	filesSection := m.filesInfo(m.com.Store().WorkingDir(), width, maxFiles, true)
	timelineSection := m.timelineInfo(width, maxTimeline, true)

	return lipgloss.NewStyle().
		MaxWidth(width).
		MaxHeight(height).
		Render(
			lipgloss.JoinVertical(
				lipgloss.Left,
				sidebarHeader,
				filesSection,
				"",
				lspSection,
				"",
				mcpSection,
				"",
				skillsSection,
				"",
				timelineSection,
			),
		)
}
