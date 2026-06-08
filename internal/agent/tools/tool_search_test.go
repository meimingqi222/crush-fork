package tools

import (
	"context"
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

type toolSearchRegistryStub struct {
	entries []RegistryEntry
}

func (s toolSearchRegistryStub) Search(query string, opts RegistrySearchOptions) []RegistryEntry {
	return SearchRegistryEntries(s.entries, query, opts)
}

func (s toolSearchRegistryStub) Resolve(name string) (RegistryEntry, bool) {
	for _, entry := range s.entries {
		if entry.Name == name {
			return entry, true
		}
	}
	return RegistryEntry{}, false
}

func (s toolSearchRegistryStub) Invoke(context.Context, string, map[string]any, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return fantasy.NewTextErrorResponse("not implemented"), nil
}

func runToolSearchResponse(t *testing.T, tool fantasy.AgentTool, params ToolSearchParams) ToolSearchResponse {
	t.Helper()
	input, err := json.Marshal(params)
	require.NoError(t, err)
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{
		ID:    "call-1",
		Name:  ToolSearchToolName,
		Input: string(input),
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)

	var response ToolSearchResponse
	require.NoError(t, json.Unmarshal([]byte(resp.Content), &response))
	return response
}

func runToolSearch(t *testing.T, tool fantasy.AgentTool, params ToolSearchParams) []ToolSearchResult {
	t.Helper()
	response := runToolSearchResponse(t, tool, params)
	return response.Results
}

func TestToolSearchSelectActivatesDeferredTools(t *testing.T) {
	t.Parallel()

	registry := toolSearchRegistryStub{entries: []RegistryEntry{
		{
			Name:        "sourcegraph",
			Description: "search public repositories",
			Metadata:    ToolMetadata{Exposure: ToolExposureDeferred},
		},
		{
			Name:        "read",
			Description: "read file",
			Exposed:     true,
			Metadata:    ToolMetadata{Exposure: ToolExposureDefault},
		},
	}}

	var activated []string
	tool := NewToolSearchTool(registry, func(_ context.Context, toolNames []string) []string {
		activated = append(activated, toolNames...)
		return toolNames
	})

	results := runToolSearch(t, tool, ToolSearchParams{Query: "select:sourcegraph,missing,read"})

	require.Equal(t, []string{"sourcegraph"}, activated)
	require.Len(t, results, 2)
	require.Equal(t, "sourcegraph", results[0].Name)
	require.True(t, results[0].Activated)
	require.Equal(t, "read", results[1].Name)
	require.False(t, results[1].Activated)

	response := runToolSearchResponse(t, tool, ToolSearchParams{Query: "select:sourcegraph,missing,read"})
	require.Equal(t, []string{"sourcegraph"}, response.ActivatedTools)
	require.Contains(t, response.ActivationHint, "sourcegraph")
}

func TestToolSearchSelectSkipsUnexposedNonDeferredTools(t *testing.T) {
	t.Parallel()

	registry := toolSearchRegistryStub{entries: []RegistryEntry{
		{
			Name:        "read",
			Description: "read file",
			Exposed:     true,
			Metadata:    ToolMetadata{Exposure: ToolExposureDefault},
		},
		{
			Name:        "secret_write",
			Description: "internal tool",
			Exposed:     false,
			Metadata:    ToolMetadata{Exposure: ToolExposureDefault},
		},
	}}

	tool := NewToolSearchTool(registry, nil)
	results := runToolSearch(t, tool, ToolSearchParams{Query: "select:read,secret_write"})

	require.Len(t, results, 1)
	require.Equal(t, "read", results[0].Name)
}

func TestToolSearchExactNameActivatesDeferredTool(t *testing.T) {
	t.Parallel()

	registry := toolSearchRegistryStub{entries: []RegistryEntry{
		{
			Name:        "sourcegraph",
			Description: "search public repositories",
			Metadata:    ToolMetadata{Exposure: ToolExposureDeferred},
		},
	}}

	var activated []string
	tool := NewToolSearchTool(registry, func(_ context.Context, toolNames []string) []string {
		activated = append(activated, toolNames...)
		return toolNames
	})

	results := runToolSearch(t, tool, ToolSearchParams{Query: "sourcegraph"})

	require.Equal(t, []string{"sourcegraph"}, activated)
	require.Len(t, results, 1)
	require.Equal(t, "sourcegraph", results[0].Name)
	require.True(t, results[0].Activated)
}

func TestToolSearchBM25KeywordQuery(t *testing.T) {
	t.Parallel()

	registry := toolSearchRegistryStub{entries: []RegistryEntry{
		{
			Name:        "read",
			Description: "read file",
			Exposed:     true,
			Metadata:    ToolMetadata{Exposure: ToolExposureDefault},
		},
		{
			Name:        "sourcegraph",
			Description: "search public repositories",
			Exposed:     false,
			Metadata:    ToolMetadata{Exposure: ToolExposureDeferred},
		},
	}}

	var activated []string
	tool := NewToolSearchTool(registry, func(_ context.Context, toolNames []string) []string {
		activated = append(activated, toolNames...)
		return toolNames
	})

	results := runToolSearch(t, tool, ToolSearchParams{Query: "public repositories"})

	require.Equal(t, []string{"sourcegraph"}, activated)
	require.Len(t, results, 1)
	require.Equal(t, "sourcegraph", results[0].Name)
	require.True(t, results[0].Activated)
}

func TestToolSearchBM25RankingPrefersHintAndTags(t *testing.T) {
	t.Parallel()

	registry := toolSearchRegistryStub{entries: []RegistryEntry{
		{
			Name:        "sourcegraph",
			Description: "search repositories",
			Metadata: ToolMetadata{
				Exposure:   ToolExposureDeferred,
				SearchHint: "search public repositories",
				SearchTags: []string{"code-search", "network"},
			},
		},
		{
			Name:        "read",
			Description: "read local files",
			Exposed:     true,
			Metadata: ToolMetadata{
				Exposure:   ToolExposureDefault,
				SearchHint: "inspect local files",
				SearchTags: []string{"filesystem"},
			},
		},
	}}

	tool := NewToolSearchTool(registry, nil)
	results := runToolSearch(t, tool, ToolSearchParams{Query: "code search"})

	require.Len(t, results, 1)
	require.Equal(t, "sourcegraph", results[0].Name)
}

func TestToolSearchNoMatchIncludesPendingMCPServers(t *testing.T) {
	t.Parallel()

	registry := toolSearchRegistryStub{entries: []RegistryEntry{
		{Name: "read", Description: "read local files", Exposed: true, Metadata: ToolMetadata{Exposure: ToolExposureDefault}},
		{Name: "sourcegraph", Description: "search public repositories", Metadata: ToolMetadata{Exposure: ToolExposureDeferred}},
	}}

	tool := NewToolSearchTool(registry, nil, func() []string {
		return []string{"github", "slack"}
	})
	response := runToolSearchResponse(t, tool, ToolSearchParams{Query: "jira"})

	require.Empty(t, response.Results)
	require.Equal(t, []string{"github", "slack"}, response.PendingMCPServers)
	require.Equal(t, 1, response.TotalTools)
}

func TestToolSearchMCPNameAliasExactMatch(t *testing.T) {
	t.Parallel()

	registry := toolSearchRegistryStub{entries: []RegistryEntry{
		{
			Name:        "mcp_github_issue_list",
			Description: "list github issues",
			Metadata:    ToolMetadata{Exposure: ToolExposureDeferred},
		},
	}}

	var activated []string
	tool := NewToolSearchTool(registry, func(_ context.Context, toolNames []string) []string {
		activated = append(activated, toolNames...)
		return toolNames
	})

	response := runToolSearchResponse(t, tool, ToolSearchParams{Query: "mcp__github__issue__list"})

	require.Equal(t, []string{"mcp_github_issue_list"}, activated)
	require.Len(t, response.Results, 1)
	require.True(t, response.Results[0].Activated)
}

func TestToolSearchMCPKeywordRanking(t *testing.T) {
	t.Parallel()

	registry := toolSearchRegistryStub{entries: []RegistryEntry{
		{
			Name:        "mcp_github_issue_list",
			Description: "list github issues",
			Metadata: ToolMetadata{
				Exposure:   ToolExposureDeferred,
				SearchHint: "github issue listing",
				SearchTags: []string{"mcp", "github", "issue", "list"},
			},
		},
		{
			Name:        "sourcegraph",
			Description: "search public repositories",
			Metadata: ToolMetadata{
				Exposure:   ToolExposureDeferred,
				SearchHint: "search code in repositories",
				SearchTags: []string{"code-search"},
			},
		},
	}}

	tool := NewToolSearchTool(registry, nil)
	response := runToolSearchResponse(t, tool, ToolSearchParams{Query: "github issue"})

	require.NotEmpty(t, response.Results)
	require.Equal(t, "mcp_github_issue_list", response.Results[0].Name)
}

func TestToolSearchEmptyQueryReturnsError(t *testing.T) {
	t.Parallel()

	registry := toolSearchRegistryStub{entries: []RegistryEntry{
		{Name: "read", Description: "read file", Exposed: true, Metadata: ToolMetadata{Exposure: ToolExposureDefault}},
	}}

	tool := NewToolSearchTool(registry, nil)
	input, err := json.Marshal(ToolSearchParams{Query: ""})
	require.NoError(t, err)
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{
		ID:    "call-1",
		Name:  ToolSearchToolName,
		Input: string(input),
	})
	require.NoError(t, err)
	require.True(t, resp.IsError)
}

func TestToolSearchExcludesAlreadyActivatedTools(t *testing.T) {
	t.Parallel()

	registry := toolSearchRegistryStub{entries: []RegistryEntry{
		{
			Name:        "sourcegraph",
			Description: "search public repositories",
			Metadata:    ToolMetadata{Exposure: ToolExposureDeferred},
		},
		{
			Name:        "mcp_github_issue",
			Description: "list github issues",
			Metadata:    ToolMetadata{Exposure: ToolExposureDeferred},
		},
	}}

	// First call activates sourcegraph.
	callCount := 0
	tool := NewToolSearchTool(registry, func(_ context.Context, toolNames []string) []string {
		callCount++
		if len(toolNames) == 0 {
			// getActivatedSet call: return already-activated tools.
			if callCount == 1 {
				return nil
			}
			return []string{"sourcegraph"}
		}
		return toolNames
	})

	// First search should find both.
	results := runToolSearch(t, tool, ToolSearchParams{Query: "search repositories"})
	require.Len(t, results, 1)
	require.Equal(t, "sourcegraph", results[0].Name)
}

func TestTokenizeWithCamelCase(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  []string
	}{
		{"mcp_github_issue_list", []string{"mcp", "github", "issue", "list"}},
		{"fooBar", []string{"foo", "bar", "foobar"}},
		{"MCPTool", []string{"mcp", "tool", "mcptool"}},
		{"simple", []string{"simple"}},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			got := tokenizeWithCamelCase(tt.input)
			require.Equal(t, tt.want, got)
		})
	}
}
