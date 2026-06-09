package agent

import (
	"strings"
	"testing"

	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/stretchr/testify/require"
)

func TestCollectDeferredToolHintsFiltersExpectedEntries(t *testing.T) {
	t.Parallel()

	entries := map[string]agenttools.RegistryEntry{
		"sourcegraph": {
			Name:     "sourcegraph",
			Source:   "builtin",
			Metadata: agenttools.ToolMetadata{Exposure: agenttools.ToolExposureDeferred},
		},
		"mcp_github_issue_list": {
			Name:     "mcp_github_issue_list",
			Source:   "mcp:github",
			Metadata: agenttools.ToolMetadata{Exposure: agenttools.ToolExposureDeferred},
		},
		"read": {
			Name:     "read",
			Source:   "builtin",
			Metadata: agenttools.ToolMetadata{Exposure: agenttools.ToolExposureDefault},
			Exposed:  true,
		},
		"disabled_deferred": {
			Name:     "disabled_deferred",
			Source:   "plugin:custom",
			Metadata: agenttools.ToolMetadata{Exposure: agenttools.ToolExposureDeferred},
		},
	}

	hints := collectDeferredToolHints(entries, map[string]struct{}{"disabled_deferred": {}})
	require.Len(t, hints, 2)
	// Sorted by name
	require.Equal(t, "mcp_github_issue_list", hints[0].Name)
	require.Equal(t, "sourcegraph", hints[1].Name)
}

func TestAppendDeferredToolsPromptSection(t *testing.T) {
	t.Parallel()

	base := "Base prompt"
	entries := []agenttools.RegistryEntry{
		{
			Name:        "sourcegraph",
			Description: "search public repositories",
			Source:      "builtin",
			Metadata: agenttools.ToolMetadata{
				Exposure:   agenttools.ToolExposureDeferred,
				SearchHint: "search public repositories",
			},
		},
	}

	result := appendDeferredToolsPromptSection(base, entries)
	require.Contains(t, result, "<available_deferred_tools>")
	require.Contains(t, result, "tool_search")
	require.Contains(t, result, "- sourcegraph — search public repositories")
	require.True(t, strings.HasPrefix(result, base))
}

func TestAppendDeferredToolsPromptSectionMultipleTools(t *testing.T) {
	t.Parallel()

	entries := []agenttools.RegistryEntry{
		{Name: "mcp_notion_read", Source: "mcp:notion", Metadata: agenttools.ToolMetadata{Exposure: agenttools.ToolExposureDeferred}},
		{Name: "mcp_notion_write", Source: "mcp:notion", Metadata: agenttools.ToolMetadata{Exposure: agenttools.ToolExposureDeferred}},
		{Name: "mcp_slack_send", Source: "mcp:slack", Metadata: agenttools.ToolMetadata{Exposure: agenttools.ToolExposureDeferred}},
		{Name: "sourcegraph", Source: "builtin", Metadata: agenttools.ToolMetadata{Exposure: agenttools.ToolExposureDeferred}},
	}

	result := appendDeferredToolsPromptSection("Base", entries)

	// All tool names should be present (no 24-line limit).
	require.Contains(t, result, "- mcp_notion_read — mcp:notion")
	require.Contains(t, result, "- mcp_notion_write — mcp:notion")
	require.Contains(t, result, "- mcp_slack_send — mcp:slack")
	require.Contains(t, result, "- sourcegraph — builtin")
	require.NotContains(t, result, "... and")
}

func TestDeferredToolPromptHintFallbackOrder(t *testing.T) {
	t.Parallel()

	require.Equal(t, "search repositories", deferredToolPromptHint(agenttools.RegistryEntry{
		Description: "fallback description",
		Source:      "builtin",
		Metadata: agenttools.ToolMetadata{
			SearchHint: "search repositories",
			SearchTags: []string{"code", "search"},
		},
	}))
	require.Equal(t, "code, search", deferredToolPromptHint(agenttools.RegistryEntry{
		Description: "fallback description",
		Source:      "builtin",
		Metadata: agenttools.ToolMetadata{
			SearchTags: []string{"code", "", "search"},
		},
	}))
	require.Equal(t, "fallback description", deferredToolPromptHint(agenttools.RegistryEntry{
		Description: "fallback description",
		Source:      "builtin",
	}))
	require.Equal(t, "builtin", deferredToolPromptHint(agenttools.RegistryEntry{
		Source: "builtin",
	}))
	require.Equal(t, "Use tool_search for details", deferredToolPromptHint(agenttools.RegistryEntry{}))
}

func TestDeferredToolPromptHintTruncatesLongHints(t *testing.T) {
	t.Parallel()

	longHint := strings.Repeat("a", maxDeferredToolHintLength+10)
	hint := deferredToolPromptHint(agenttools.RegistryEntry{
		Metadata: agenttools.ToolMetadata{SearchHint: longHint},
	})

	require.Len(t, []rune(hint), maxDeferredToolHintLength)
	require.True(t, strings.HasSuffix(hint, "…"))
}
