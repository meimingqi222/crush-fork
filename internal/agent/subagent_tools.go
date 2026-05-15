package agent

import (
	"sort"
	"strings"

	"charm.land/fantasy"
	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
)

func ShapeToolsForSubagent(all []fantasy.AgentTool, profile SubagentToolProfile) []fantasy.AgentTool {
	if len(all) == 0 {
		return nil
	}
	shaped := make([]fantasy.AgentTool, 0, len(all))
	for _, tool := range all {
		if tool == nil {
			continue
		}
		name := strings.TrimSpace(tool.Info().Name)
		if !profile.Allows(name) {
			continue
		}
		shaped = append(shaped, tool)
	}
	sort.Slice(shaped, func(i, j int) bool {
		return shaped[i].Info().Name < shaped[j].Info().Name
	})
	return shaped
}

func shapeDeferredHintsForSubagent(entries []agenttools.RegistryEntry, profile SubagentToolProfile) []agenttools.RegistryEntry {
	if len(entries) == 0 {
		return nil
	}
	shaped := make([]agenttools.RegistryEntry, 0, len(entries))
	for _, entry := range entries {
		if !profile.Allows(entry.Name) {
			continue
		}
		entry.Exposed = false
		shaped = append(shaped, entry)
	}
	sort.Slice(shaped, func(i, j int) bool {
		return shaped[i].Name < shaped[j].Name
	})
	return shaped
}
