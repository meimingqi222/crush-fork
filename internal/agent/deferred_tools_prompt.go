package agent

import (
	"sort"
	"strings"

	"github.com/charmbracelet/crush/internal/agent/tools"
)

const maxDeferredToolHintLength = 96

// collectDeferredToolHints collects deferred tool entries for prompt inclusion.
func collectDeferredToolHints(entries map[string]tools.RegistryEntry, disabledSet map[string]struct{}) []tools.RegistryEntry {
	if len(entries) == 0 {
		return nil
	}

	hints := make([]tools.RegistryEntry, 0, len(entries))
	for _, entry := range entries {
		if !entry.Metadata.IsDeferred() {
			continue
		}
		if entry.Exposed {
			continue
		}
		if _, disabled := disabledSet[entry.Name]; disabled {
			continue
		}
		hints = append(hints, entry)
	}

	sort.Slice(hints, func(i, j int) bool {
		return hints[i].Name < hints[j].Name
	})
	return hints
}

// appendDeferredToolsPromptSection appends a compact deferred tool discovery section.
// It includes short hints so the model can decide whether tool_search is relevant
// without paying the cost of full tool schemas up front.
//
// Unlike the previous unconditional approach, the guidance now includes scenario
// qualifiers to reduce tool_search misuse: LLMs should only call tool_search
// when the task actually involves external integrations (MCP tools, APIs, etc.),
// not for routine local coding tasks.
func appendDeferredToolsPromptSection(basePrompt string, deferredEntries []tools.RegistryEntry) string {
	if len(deferredEntries) == 0 {
		return basePrompt
	}

	toolLines := make([]string, len(deferredEntries))
	for i, entry := range deferredEntries {
		toolLines[i] = "- " + entry.Name + " — " + deferredToolPromptHint(entry)
	}

	// Derive the list of deferred built-in (non-MCP) tools from the actual
	// entries so the prose stays in sync with the toolset.
	var builtinNames []string
	for _, entry := range deferredEntries {
		if !strings.HasPrefix(entry.Source, "mcp:") {
			builtinNames = append(builtinNames, entry.Name)
		}
	}
	sort.Strings(builtinNames)

	lines := []string{
		"<available_deferred_tools>",
		"Some tools are deferred (not loaded yet) to reduce context size. Two kinds of tools are deferred:",
		"- MCP/external integration tools (all MCP tools are deferred and NOT in your default tool set).",
		"- Low-frequency built-in tools: " + strings.Join(builtinNames, ", "),
		"",
		"When to use tool_search:",
		"- When the task involves external systems, APIs, databases, deployments, or other non-local integrations",
		"- When you need an MCP tool that isn't in your default tool set",
		"- When a task matches a listed deferred built-in tool's purpose",
		"",
		"Usage workflow:",
		"1. Call tool_search with query \"select:tool_name\" to get the full schema",
		"2. The tool will be activated and available in your NEXT response",
		"3. Call the tool with the correct parameters from the schema",
		"",
		"Available tools:",
		strings.Join(toolLines, "\n"),
		"</available_deferred_tools>",
	}

	section := strings.Join(lines, "\n")
	trimmedBase := strings.TrimSpace(basePrompt)
	if trimmedBase == "" {
		return section
	}
	return trimmedBase + "\n\n" + section
}

func deferredToolPromptHint(entry tools.RegistryEntry) string {
	hint := strings.TrimSpace(entry.Metadata.SearchHint)
	if hint == "" && len(entry.Metadata.SearchTags) > 0 {
		tags := make([]string, 0, len(entry.Metadata.SearchTags))
		for _, tag := range entry.Metadata.SearchTags {
			tag = strings.TrimSpace(tag)
			if tag == "" {
				continue
			}
			tags = append(tags, tag)
		}
		hint = strings.Join(tags, ", ")
	}
	if hint == "" {
		hint = strings.TrimSpace(entry.Description)
	}
	if hint == "" {
		hint = strings.TrimSpace(entry.Source)
	}
	if hint == "" {
		return "Use tool_search for details"
	}
	return truncateDeferredToolHint(hint)
}

func truncateDeferredToolHint(hint string) string {
	hint = strings.Join(strings.Fields(hint), " ")
	runes := []rune(hint)
	if len(runes) <= maxDeferredToolHintLength {
		return hint
	}
	return strings.TrimSpace(string(runes[:maxDeferredToolHintLength-1])) + "…"
}
