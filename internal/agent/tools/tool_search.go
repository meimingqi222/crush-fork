package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
)

// ─── BM25 Constants ──────────────────────────────────────────────────────────

const (
	bm25K1    = 1.2
	bm25B     = 0.75
	bm25Delta = 1.0
)

// Field weights for BM25 index building. Higher weight = more important for
// relevance ranking.
var fieldWeights = map[string]float64{
	"name":        6.0,
	"searchHint":  4.0,
	"mcpToolName": 4.0,
	"serverName":  2.0,
	"description": 2.0,
	"schemaKey":   1.0,
	"tag":         1.5,
}

// ─── Parameter Types ─────────────────────────────────────────────────────────

type ToolSearchParams struct {
	Query string `json:"query" description:"Natural language or keyword query to search tool metadata, or 'select:tool_name' to activate a specific tool"`
	Limit int    `json:"limit,omitempty" description:"Maximum number of tool results to return (default: 8, max: 50)"`
}

type ToolSearchResult struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	SchemaKeys  []string `json:"schema_keys,omitempty"`
	Source      string   `json:"source,omitempty"`
	Score       float64  `json:"score,omitempty"`
	Activated   bool     `json:"activated,omitempty"`
}

type ToolSearchResponse struct {
	Query             string             `json:"query"`
	MatchCount        int                `json:"match_count"`
	TotalTools        int                `json:"total_tools"`
	ActivatedTools    []string           `json:"activated_tools,omitempty"`
	Results           []ToolSearchResult `json:"results,omitempty"`
	PendingMCPServers []string           `json:"pending_mcp_servers,omitempty"`
	ActivationHint    string             `json:"activation_hint,omitempty"`
}

type (
	DeferredToolActivator  func(ctx context.Context, toolNames []string) []string
	PendingServersProvider func() []string
)

// ─── BM25 Search Document ────────────────────────────────────────────────────

type searchDocument struct {
	entry           RegistryEntry
	termFrequencies map[string]float64
	length          float64
}

type searchIndex struct {
	documents           []searchDocument
	averageLength       float64
	documentFrequencies map[string]int
}

// ─── Tokenization ────────────────────────────────────────────────────────────

// tokenize splits text into search tokens by splitting on non-alphanumeric
// characters. The result preserves original casing for downstream camelCase
// splitting.
func tokenize(value string) []string {
	if value == "" {
		return nil
	}

	var tokens []string
	var buf strings.Builder

	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			buf.WriteRune(r)
		} else {
			if buf.Len() > 0 {
				tokens = append(tokens, buf.String())
				buf.Reset()
			}
		}
	}
	if buf.Len() > 0 {
		tokens = append(tokens, buf.String())
	}

	return tokens
}

// tokenizeWithCamelCase splits text into search tokens, including camelCase
// boundary splitting. "MCPTool" → ["mcp", "tool", "mcptool"],
// "fooBar" → ["foo", "bar", "foobar"], "mcp_github_issue" → ["mcp", "github", "issue"].
func tokenizeWithCamelCase(value string) []string {
	base := tokenize(value)
	seen := make(map[string]struct{})
	var result []string

	for _, token := range base {
		parts := splitCamelCase(token)
		for _, part := range parts {
			lower := strings.ToLower(part)
			if _, ok := seen[lower]; !ok {
				seen[lower] = struct{}{}
				result = append(result, lower)
			}
		}
	}

	return result
}

// splitCamelCase splits a single token on camelCase boundaries.
// Handles acronyms: "MCPTool" → ["MCP", "Tool", "MCPTool"],
// "fooBar" → ["foo", "Bar", "fooBar"], "simple" → ["simple"]
func splitCamelCase(token string) []string {
	if token == "" {
		return nil
	}

	var parts []string
	var buf strings.Builder
	runes := []rune(token)

	for i, r := range runes {
		if i > 0 && unicode.IsUpper(r) && !unicode.IsUpper(runes[i-1]) {
			// lowercase→Upper boundary: "fooBar" → split before B.
			if buf.Len() > 0 {
				parts = append(parts, buf.String())
				buf.Reset()
			}
		} else if i > 0 && i < len(runes)-1 && unicode.IsUpper(r) && unicode.IsUpper(runes[i-1]) && unicode.IsLower(runes[i+1]) {
			// Upper→Upper→lower boundary (acronym end): "MCPTool" → split before T.
			if buf.Len() > 0 {
				parts = append(parts, buf.String())
				buf.Reset()
			}
		} else if i > 0 && unicode.IsDigit(r) && !unicode.IsDigit(runes[i-1]) {
			// Digit after non-digit: flush buffer.
			if buf.Len() > 0 {
				parts = append(parts, buf.String())
				buf.Reset()
			}
		}
		buf.WriteRune(r)
	}
	if buf.Len() > 0 {
		parts = append(parts, buf.String())
	}

	// Also include the full token.
	if len(parts) > 1 {
		parts = append(parts, token)
	}

	return parts
}

// ─── BM25 Index Building ─────────────────────────────────────────────────────

// addWeightedTokens adds tokens from value to the term frequency map with the
// given weight.
func addWeightedTokens(tf map[string]float64, value string, weight float64) {
	for _, token := range tokenizeWithCamelCase(value) {
		tf[token] += weight
	}
}

// buildSearchDocument creates a BM25 search document from a registry entry.
func buildSearchDocument(entry RegistryEntry) searchDocument {
	tf := make(map[string]float64)

	// Name (highest weight).
	addWeightedTokens(tf, entry.Name, fieldWeights["name"])

	// Search hint.
	addWeightedTokens(tf, entry.Metadata.SearchHint, fieldWeights["searchHint"])

	// MCP tool name (extract from source pattern).
	if strings.HasPrefix(entry.Name, "mcp_") || strings.HasPrefix(entry.Name, "mcp__") {
		addWeightedTokens(tf, entry.Name, fieldWeights["mcpToolName"])
		// Extract server name from source.
		if strings.HasPrefix(entry.Source, "mcp:") {
			serverName := strings.TrimPrefix(entry.Source, "mcp:")
			addWeightedTokens(tf, serverName, fieldWeights["serverName"])
		}
	}

	// Description.
	addWeightedTokens(tf, entry.Description, fieldWeights["description"])

	// Schema keys (parameter names).
	if entry.Parameters != nil {
		if props, ok := entry.Parameters["properties"]; ok {
			if propsMap, ok := props.(map[string]any); ok {
				for key := range propsMap {
					addWeightedTokens(tf, key, fieldWeights["schemaKey"])
				}
			}
		}
	}

	// Search tags.
	for _, tag := range entry.Metadata.SearchTags {
		addWeightedTokens(tf, tag, fieldWeights["tag"])
	}

	// Compute document length (sum of weighted term frequencies).
	var length float64
	for _, freq := range tf {
		length += freq
	}

	return searchDocument{
		entry:           entry,
		termFrequencies: tf,
		length:          length,
	}
}

// buildSearchIndex creates a BM25 search index from registry entries.
func buildSearchIndex(entries []RegistryEntry) searchIndex {
	documents := make([]searchDocument, 0, len(entries))
	for _, entry := range entries {
		documents = append(documents, buildSearchDocument(entry))
	}

	var totalLength float64
	for _, doc := range documents {
		totalLength += doc.length
	}
	avgLength := float64(1)
	if len(documents) > 0 {
		avgLength = totalLength / float64(len(documents))
	}

	// Compute document frequencies.
	docFreq := make(map[string]int)
	for _, doc := range documents {
		seen := make(map[string]struct{})
		for token := range doc.termFrequencies {
			if _, ok := seen[token]; !ok {
				docFreq[token]++
				seen[token] = struct{}{}
			}
		}
	}

	return searchIndex{
		documents:           documents,
		averageLength:       avgLength,
		documentFrequencies: docFreq,
	}
}

// ─── BM25 Search ─────────────────────────────────────────────────────────────

// searchBM25 performs a BM25 search over the index and returns ranked results.
func searchBM25(index searchIndex, query string, limit int, excludeNames map[string]struct{}) []ToolSearchResult {
	queryTokens := tokenizeWithCamelCase(query)
	if len(queryTokens) == 0 || len(index.documents) == 0 {
		return nil
	}

	// Count query term frequencies.
	queryTermCounts := make(map[string]int)
	for _, token := range queryTokens {
		queryTermCounts[token]++
	}

	type scoredResult struct {
		result ToolSearchResult
		score  float64
	}

	var ranked []scoredResult
	for _, doc := range index.documents {
		// Skip excluded (already activated) tools.
		if _, ok := excludeNames[doc.entry.Name]; ok {
			continue
		}

		var score float64
		for queryToken, queryTermCount := range queryTermCounts {
			tf, ok := doc.termFrequencies[queryToken]
			if !ok || tf == 0 {
				continue
			}

			df := index.documentFrequencies[queryToken]
			// IDF with BM25+ delta.
			idf := math.Log(1 + (float64(len(index.documents))-float64(df)+0.5)/(float64(df)+0.5))
			// BM25 normalization.
			norm := bm25K1 * (1 - bm25B + bm25B*(doc.length/index.averageLength))
			// BM25+ scoring.
			score += float64(queryTermCount) * idf * ((tf*(bm25K1+1))/(tf+norm) + bm25Delta)
		}

		if score > 0 {
			ranked = append(ranked, scoredResult{
				result: ToolSearchResult{
					Name:        doc.entry.Name,
					Description: doc.entry.Description,
					Source:      doc.entry.Source,
					Score:       math.Round(score*1000) / 1000,
				},
				score: score,
			})
		}
	}

	// Sort by score descending, then by name ascending for stability.
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].result.Name < ranked[j].result.Name
	})

	if limit > 0 && len(ranked) > limit {
		ranked = ranked[:limit]
	}

	results := make([]ToolSearchResult, 0, len(ranked))
	for _, r := range ranked {
		results = append(results, r.result)
	}
	return results
}

// ─── Tool Search Tool ────────────────────────────────────────────────────────

// NewToolSearchTool creates a tool for discovering and activating deferred tools
// using BM25 relevance ranking. It is only registered when deferred tools exist.
func NewToolSearchTool(registry Registry, activateDeferred DeferredToolActivator, pendingProviders ...PendingServersProvider) fantasy.AgentTool {
	pendingServersProvider := detectPendingMCPServers
	if len(pendingProviders) > 0 && pendingProviders[0] != nil {
		pendingServersProvider = pendingProviders[0]
	}

	return fantasy.NewParallelAgentTool(
		ToolSearchToolName,
		"Discovers and activates deferred MCP and external integration tools using BM25 relevance ranking. Use when the task involves external systems, APIs, databases, or deployments that require tools not in the default set. Use 'select:tool_name' for direct activation, or keywords to search by name, description, and tags. Deferred matches from keyword queries are activated automatically. Returns {query, match_count, total_tools, activated_tools}.",
		func(ctx context.Context, params ToolSearchParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			_ = call
			if registry == nil {
				return fantasy.NewTextErrorResponse("tool registry is unavailable"), nil
			}

			limit := params.Limit
			if limit <= 0 {
				limit = 8
			} else if limit > 50 {
				limit = 50
			}

			query := strings.TrimSpace(params.Query)
			if query == "" {
				return fantasy.NewTextErrorResponse("query is required and must not be empty"), nil
			}

			// Get all searchable entries for the index.
			allEntries := searchableRegistryEntries(registry, RegistrySearchOptions{
				Limit:           10_000,
				IncludeDeferred: true,
			})

			totalDeferred := countDeferredEntries(allEntries)

			// Build BM25 search index.
			index := buildSearchIndex(allEntries)

			// Handle "select:tool_name" direct activation (works even with 0 deferred tools).
			if selectedNames, ok := parseToolSelectQuery(query); ok {
				return handleSelectQuery(ctx, registry, activateDeferred, index, selectedNames, totalDeferred, pendingServersProvider)
			}

			// Handle exact name match.
			if entry, ok := resolveRegistryEntryByName(registry, query); ok && isRegistryEntrySearchable(entry) {
				return handleSelectQuery(ctx, registry, activateDeferred, index, []string{entry.Name}, totalDeferred, pendingServersProvider)
			}

			// BM25 keyword search: only meaningful when there are deferred tools.
			if totalDeferred == 0 {
				return fantasy.NewTextResponse(`{"query":"","match_count":0,"total_tools":0}`), nil
			}

			// BM25 keyword search over deferred tools only.
			// Exclude already-activated tools from results.
			activatedSet := getActivatedSet(ctx, activateDeferred)
			results := searchBM25(index, query, limit, activatedSet)

			// Activate matching deferred tools.
			var activatedTools []string
			if len(results) > 0 && activateDeferred != nil {
				toActivate := make([]string, 0, len(results))
				for _, r := range results {
					if entry, ok := registry.Resolve(r.Name); ok && entry.Metadata.IsDeferred() {
						toActivate = append(toActivate, r.Name)
					}
				}
				if len(toActivate) > 0 {
					activated := activateDeferred(ctx, toActivate)
					activatedSet := make(map[string]struct{}, len(activated))
					for _, name := range activated {
						activatedSet[name] = struct{}{}
					}
					for i := range results {
						if _, ok := activatedSet[results[i].Name]; ok {
							results[i].Activated = true
						}
					}
					activatedTools = activated
				}
			}

			// Add schema keys to results.
			for i, r := range results {
				if entry, ok := registry.Resolve(r.Name); ok {
					results[i].SchemaKeys = extractSchemaKeys(entry)
				}
			}

			response := ToolSearchResponse{
				Query:          query,
				MatchCount:     len(results),
				TotalTools:     totalDeferred,
				ActivatedTools: activatedTools,
				Results:        results,
			}

			if len(activatedTools) > 0 {
				response.ActivationHint = fmt.Sprintf(
					"Deferred tools activated: %s. These tools are now available for use in your NEXT response.",
					strings.Join(activatedTools, ", "),
				)
			}

			pendingMCP := detectPendingIfNoResults(results, pendingServersProvider)
			if len(pendingMCP) > 0 {
				response.PendingMCPServers = pendingMCP
			}

			return marshalToolSearchResponse(response)
		},
	)
}

// handleSelectQuery handles the "select:tool_name" direct activation path.
func handleSelectQuery(
	ctx context.Context,
	registry Registry,
	activateDeferred DeferredToolActivator,
	index searchIndex,
	selectedNames []string,
	totalDeferred int,
	pendingProvider PendingServersProvider,
) (fantasy.ToolResponse, error) {
	results := make([]ToolSearchResult, 0, len(selectedNames))
	deferredToActivate := make([]string, 0, len(selectedNames))

	for _, name := range selectedNames {
		entry, ok := resolveRegistryEntryByName(registry, name)
		if !ok || !isRegistryEntrySearchable(entry) {
			continue
		}
		results = append(results, ToolSearchResult{
			Name:        entry.Name,
			Description: entry.Description,
			Source:      entry.Source,
			SchemaKeys:  extractSchemaKeys(entry),
		})
		if entry.Metadata.IsDeferred() {
			deferredToActivate = append(deferredToActivate, entry.Name)
		}
	}

	var activatedTools []string
	if len(deferredToActivate) > 0 && activateDeferred != nil {
		activated := activateDeferred(ctx, deferredToActivate)
		activatedSet := make(map[string]struct{}, len(activated))
		for _, name := range activated {
			activatedSet[name] = struct{}{}
		}
		for i := range results {
			if _, ok := activatedSet[results[i].Name]; ok {
				results[i].Activated = true
			}
		}
		activatedTools = activated
	}

	response := ToolSearchResponse{
		Query:          "select:" + strings.Join(selectedNames, ","),
		MatchCount:     len(results),
		TotalTools:     totalDeferred,
		ActivatedTools: activatedTools,
		Results:        results,
	}

	if len(activatedTools) > 0 {
		response.ActivationHint = fmt.Sprintf(
			"Deferred tools activated: %s. These tools are now available for use in your NEXT response.",
			strings.Join(activatedTools, ", "),
		)
	}

	pendingMCP := detectPendingIfNoResults(results, pendingProvider)
	if len(pendingMCP) > 0 {
		response.PendingMCPServers = pendingMCP
	}

	return marshalToolSearchResponse(response)
}

// ─── Helper Functions ────────────────────────────────────────────────────────

// getActivatedSet returns the set of tool names that are already activated for
// the current session, so they can be excluded from search results.
func getActivatedSet(ctx context.Context, activateDeferred DeferredToolActivator) map[string]struct{} {
	if activateDeferred == nil {
		return nil
	}
	// Activate with empty list returns already-activated tools.
	activated := activateDeferred(ctx, nil)
	if len(activated) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(activated))
	for _, name := range activated {
		set[name] = struct{}{}
	}
	return set
}

// extractSchemaKeys extracts parameter property keys from a registry entry.
func extractSchemaKeys(entry RegistryEntry) []string {
	if entry.Parameters == nil {
		return nil
	}
	props, ok := entry.Parameters["properties"]
	if !ok {
		return nil
	}
	propsMap, ok := props.(map[string]any)
	if !ok {
		return nil
	}
	keys := make([]string, 0, len(propsMap))
	for k := range propsMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// countDeferredEntries counts the number of deferred entries.
func countDeferredEntries(entries []RegistryEntry) int {
	count := 0
	for _, entry := range entries {
		if entry.Metadata.IsDeferred() {
			count++
		}
	}
	return count
}

func searchableRegistryEntries(registry Registry, opts RegistrySearchOptions) []RegistryEntry {
	entries := registry.Search("", opts)
	filtered := make([]RegistryEntry, 0, len(entries))
	for _, entry := range entries {
		if !isRegistryEntrySearchable(entry) {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

func isRegistryEntrySearchable(entry RegistryEntry) bool {
	if entry.Exposed {
		return true
	}
	return entry.Metadata.IsDeferred()
}

func detectPendingMCPServers() []string {
	states := mcp.GetStates()
	if len(states) == 0 {
		return nil
	}
	pending := make([]string, 0, len(states))
	for name, state := range states {
		if state.State == mcp.StateStarting {
			pending = append(pending, name)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	sort.Strings(pending)
	return pending
}

func detectPendingIfNoResults(results []ToolSearchResult, provider PendingServersProvider) []string {
	if len(results) > 0 || provider == nil {
		return nil
	}
	pending := provider()
	if len(pending) == 0 {
		return nil
	}
	return append([]string(nil), pending...)
}

func parseToolSelectQuery(query string) ([]string, bool) {
	match := strings.TrimSpace(query)
	if !strings.HasPrefix(strings.ToLower(match), "select:") {
		return nil, false
	}
	requested := strings.Split(match[len("select:"):], ",")
	normalized := make([]string, 0, len(requested))
	seen := make(map[string]struct{}, len(requested))
	for _, item := range requested {
		name := strings.TrimSpace(item)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		normalized = append(normalized, name)
	}
	return normalized, true
}

func resolveRegistryEntryByName(registry Registry, requestedName string) (RegistryEntry, bool) {
	name := strings.TrimSpace(requestedName)
	if name == "" {
		return RegistryEntry{}, false
	}
	candidates := lookupNameCandidates(name)
	for _, candidate := range candidates {
		if entry, ok := registry.Resolve(candidate); ok {
			return entry, true
		}
	}
	entries := registry.Search("", RegistrySearchOptions{Limit: 10_000, IncludeDeferred: true})
	for _, entry := range entries {
		for _, candidate := range candidates {
			if strings.EqualFold(entry.Name, candidate) {
				return entry, true
			}
		}
	}
	return RegistryEntry{}, false
}

func lookupNameCandidates(requested string) []string {
	trimmed := strings.TrimSpace(requested)
	if trimmed == "" {
		return nil
	}

	candidates := make([]string, 0, 4)
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		for _, existing := range candidates {
			if strings.EqualFold(existing, value) {
				return
			}
		}
		candidates = append(candidates, value)
	}

	add(trimmed)
	add(strings.ReplaceAll(trimmed, "__", "_"))

	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "mcp__") {
		rest := trimmed[len("mcp__"):]
		add("mcp_" + strings.ReplaceAll(rest, "__", "_"))
	}
	if strings.HasPrefix(lower, "mcp_") {
		rest := trimmed[len("mcp_"):]
		add("mcp__" + strings.ReplaceAll(rest, "_", "__"))
	}

	return candidates
}

func marshalToolSearchResponse(response ToolSearchResponse) (fantasy.ToolResponse, error) {
	data, err := json.Marshal(response)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	return fantasy.NewTextResponse(string(data)), nil
}
