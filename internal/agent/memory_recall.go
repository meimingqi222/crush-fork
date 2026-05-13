package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/memory"
	"github.com/charmbracelet/crush/internal/oauth/copilot"
	"github.com/charmbracelet/crush/internal/version"
)

var memoryUserAgent = fmt.Sprintf("Charm-Crush/%s (https://charm.land/crush)", version.Version)

const (
	memoryRelevanceMaxSelected        = 5
	memoryRelevanceMaxFiles           = 200
	memoryRelevancePreFilterThreshold = 30
	memoryRelevancePreFilterMax       = 40
)

type memoryInfoFilters struct {
	Scope    string
	Category string
	Type     string
	Tags     []string
}

// memoryRelevancePrompt is the system prompt for the memory relevance selection model.
const memoryRelevancePrompt = `You are selecting memories that will be useful to the assistant as it processes a user's query. You will be given the user's query and a list of available memory files with their keys and descriptions.

Return a list of memory keys for the memories that will clearly be useful to the assistant as it processes the user's query (up to 5). Only include memories that you are certain will be helpful based on their key and description.
- If you are unsure if a memory will be useful in processing the user's query, then do not include it in your list. Be selective and discerning.
- If there are no memories in the list that would clearly be useful, feel free to return an empty list.
- If a list of recently-used tools is provided, do not select memories that are usage reference or API documentation for those tools (the assistant is already exercising them). DO still select memories containing warnings, gotchas, or known issues about those tools — active use is exactly when those matter.
- Return ONLY a JSON array of memory keys, nothing else.
- Return an empty array [] if no memories are relevant.

Example output:
["project/goal", "user/preferred-language"]`

// selectRelevantMemories uses the background model to select the most relevant
// memories for a given query, replacing simple string matching.
func selectRelevantMemories(ctx context.Context, memorySvc memory.Service, model Model, providerCfg config.ProviderConfig, query string, scope string, recentTools []string, alreadySurfaced map[string]bool) []memory.Entry {
	entries, err := semanticSearchMemories(ctx, memorySvc, model, providerCfg, memory.SearchParams{Query: query, Scope: scope}, recentTools, alreadySurfaced)
	if err != nil {
		slog.Warn("LLM memory relevance selection failed", "error", err)
		return nil
	}
	return entries
}

func semanticSearchMemories(ctx context.Context, memorySvc memory.Service, model Model, providerCfg config.ProviderConfig, params memory.SearchParams, recentTools []string, alreadySurfaced map[string]bool) ([]memory.Entry, error) {
	if memorySvc == nil {
		return nil, nil
	}

	infos, err := memorySvc.ListMemoryFiles()
	if err != nil {
		return nil, fmt.Errorf("listing memory files: %w", err)
	}
	if len(infos) == 0 {
		return nil, nil
	}

	filteredInfos := filterMemoryInfos(infos, memoryInfoFilters{
		Scope:    strings.TrimSpace(params.Scope),
		Category: strings.TrimSpace(params.Category),
		Type:     strings.TrimSpace(params.Type),
		Tags:     normalizeMemorySearchTags(params.Tags),
	}, alreadySurfaced)
	if len(filteredInfos) == 0 {
		return nil, nil
	}

	query := strings.TrimSpace(params.Query)
	if len(filteredInfos) > memoryRelevancePreFilterThreshold && query != "" {
		filteredInfos = preFilterMemoryInfos(filteredInfos, query, memoryRelevancePreFilterMax)
	}

	if len(filteredInfos) > memoryRelevanceMaxFiles {
		filteredInfos = filteredInfos[:memoryRelevanceMaxFiles]
	}

	selectedKeys, err := selectRelevantMemoryKeys(ctx, filteredInfos, model, providerCfg, query, recentTools)
	if err != nil {
		return nil, err
	}
	if len(selectedKeys) == 0 {
		return nil, nil
	}

	entries := make([]memory.Entry, 0, len(selectedKeys))
	for _, key := range selectedKeys {
		entry, err := memorySvc.Get(ctx, key)
		if err != nil || !matchesRequestedMemoryScope(entry.Scope, strings.TrimSpace(params.Scope)) {
			continue
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func filterMemoryInfos(infos []memory.MemoryFileInfo, filters memoryInfoFilters, alreadySurfaced map[string]bool) []memory.MemoryFileInfo {
	filteredInfos := make([]memory.MemoryFileInfo, 0, len(infos))
	for _, info := range infos {
		if !matchesRequestedMemoryScope(info.Scope, filters.Scope) {
			continue
		}
		if !matchesMemoryInfoFilters(info, filters) {
			continue
		}
		if alreadySurfaced != nil && alreadySurfaced[info.Key] {
			continue
		}
		filteredInfos = append(filteredInfos, info)
	}
	return filteredInfos
}

func matchesMemoryInfoFilters(info memory.MemoryFileInfo, filters memoryInfoFilters) bool {
	if filters.Category != "" && !strings.EqualFold(strings.TrimSpace(info.Category), filters.Category) {
		return false
	}
	if filters.Type != "" && !strings.EqualFold(strings.TrimSpace(info.Type), filters.Type) {
		return false
	}
	if len(filters.Tags) == 0 {
		return true
	}
	infoTags := normalizeMemorySearchTags(info.Tags)
	for _, tag := range filters.Tags {
		if !containsNormalizedTag(infoTags, tag) {
			return false
		}
	}
	return true
}

func normalizeMemorySearchTags(tags []string) []string {
	normalized := make([]string, 0, len(tags))
	seen := make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		trimmed := strings.ToLower(strings.TrimSpace(tag))
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		normalized = append(normalized, trimmed)
	}
	return normalized
}

func containsNormalizedTag(tags []string, want string) bool {
	for _, tag := range tags {
		if tag == want {
			return true
		}
	}
	return false
}

func preFilterMemoryInfos(infos []memory.MemoryFileInfo, query string, maxResults int) []memory.MemoryFileInfo {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" || len(infos) <= maxResults {
		return infos
	}

	tokens := strings.FieldsFunc(query, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || (r >= 0x4E00 && r <= 0x9FFF))
	})
	if len(tokens) == 0 {
		return infos[:maxResults]
	}

	type scored struct {
		info  memory.MemoryFileInfo
		score float64
	}
	scoredInfos := make([]scored, 0, len(infos))
	for _, info := range infos {
		score := 0.0
		matched := 0
		for _, token := range tokens {
			found := false
			if strings.Contains(strings.ToLower(info.Key), token) {
				score += 4.0
				found = true
			}
			if strings.Contains(strings.ToLower(info.Description), token) {
				score += 3.0
				found = true
			}
			if strings.Contains(strings.ToLower(info.Type), token) {
				score += 2.0
				found = true
			}
			if strings.Contains(strings.ToLower(info.Category), token) {
				score += 2.0
				found = true
			}
			for _, tag := range info.Tags {
				if strings.Contains(strings.ToLower(tag), token) {
					score += 2.5
					found = true
					break
				}
			}
			if found {
				matched++
			}
		}
		if matched > 0 {
			coverage := float64(matched) / float64(len(tokens))
			score += coverage * 5.0
			scoredInfos = append(scoredInfos, scored{info: info, score: score})
		}
	}

	if len(scoredInfos) == 0 {
		return infos[:maxResults]
	}

	sort.Slice(scoredInfos, func(i, j int) bool {
		if scoredInfos[i].score != scoredInfos[j].score {
			return scoredInfos[i].score > scoredInfos[j].score
		}
		return scoredInfos[i].info.UpdatedAt > scoredInfos[j].info.UpdatedAt
	})

	if len(scoredInfos) > maxResults {
		scoredInfos = scoredInfos[:maxResults]
	}
	result := make([]memory.MemoryFileInfo, len(scoredInfos))
	for i, s := range scoredInfos {
		result[i] = s.info
	}
	return result
}

func selectRelevantMemoryKeys(ctx context.Context, infos []memory.MemoryFileInfo, model Model, providerCfg config.ProviderConfig, query string, recentTools []string) ([]string, error) {
	query = strings.TrimSpace(query)
	if len(infos) == 0 || query == "" {
		return nil, nil
	}

	manifest := buildMemoryManifest(infos)
	toolsSection := ""
	if len(recentTools) > 0 {
		toolsSection = fmt.Sprintf("\n\nRecently used tools: %s", strings.Join(recentTools, ", "))
	}
	prompt := fmt.Sprintf("Query: %s\n\nAvailable memories:\n%s%s", query, manifest, toolsSection)

	agent := fantasy.NewAgent(
		model.Model,
		fantasy.WithSystemPrompt(memoryRelevancePrompt),
		fantasy.WithMaxOutputTokens(512),
		fantasy.WithUserAgent(memoryUserAgent),
	)

	resp, err := agent.Stream(copilot.ContextWithInitiatorType(ctx, copilot.InitiatorAgent), fantasy.AgentStreamCall{
		Prompt:          prompt,
		ProviderOptions: getProviderOptions(model, providerCfg),
		PrepareStep: func(callCtx context.Context, options fantasy.PrepareStepFunctionOptions) (_ context.Context, prepared fantasy.PrepareStepResult, err error) {
			callCtx = copilot.ContextWithInitiatorType(callCtx, copilot.InitiatorAgent)
			prepared.Messages = options.Messages
			if providerCfg.SystemPromptPrefix != "" {
				prepared.Messages = append([]fantasy.Message{
					fantasy.NewSystemMessage(providerCfg.SystemPromptPrefix),
				}, prepared.Messages...)
			}
			return callCtx, prepared, nil
		},
	})
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, nil
	}
	return parseMemorySelectionResponse(resp.Response.Content.Text()), nil
}

func matchesRequestedMemoryScope(entryScope, requestedScope string) bool {
	entryScope = strings.TrimSpace(entryScope)
	requestedScope = strings.TrimSpace(requestedScope)

	switch {
	case requestedScope == "":
		return !strings.EqualFold(entryScope, "session")
	case strings.EqualFold(requestedScope, "session"):
		return strings.EqualFold(entryScope, "session")
	default:
		return strings.EqualFold(entryScope, requestedScope)
	}
}

func buildMemoryManifest(infos []memory.MemoryFileInfo) string {
	var sb strings.Builder
	for i, info := range infos {
		updatedAt := time.Unix(0, info.UpdatedAt).UTC().Format(time.RFC3339)
		fmt.Fprintf(&sb, "%d. [%s] %s — %s (updated %s", i+1, info.Type, info.Key, info.Description, updatedAt)
		if info.Scope != "" {
			fmt.Fprintf(&sb, "; scope=%s", info.Scope)
		}
		if info.Category != "" {
			fmt.Fprintf(&sb, "; category=%s", info.Category)
		}
		sb.WriteString(")")
		if len(info.Tags) > 0 {
			fmt.Fprintf(&sb, " (#%s)", strings.Join(info.Tags, " #"))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func parseMemorySelectionResponse(content string) []string {
	content = strings.TrimSpace(content)

	startIdx := strings.Index(content, "[")
	endIdx := strings.LastIndex(content, "]")
	if startIdx == -1 || endIdx == -1 || endIdx <= startIdx {
		return nil
	}

	jsonArray := content[startIdx : endIdx+1]
	var keys []string
	if err := json.Unmarshal([]byte(jsonArray), &keys); err != nil {
		return nil
	}

	result := make([]string, 0, len(keys))
	seen := make(map[string]bool)
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, key)
		if len(result) >= memoryRelevanceMaxSelected {
			break
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func parseExtractedMemories(content string) []extractedMemory {
	content = strings.TrimSpace(content)

	startIdx := strings.Index(content, "[")
	endIdx := strings.LastIndex(content, "]")
	if startIdx == -1 || endIdx == -1 || endIdx <= startIdx {
		return nil
	}

	jsonArray := content[startIdx : endIdx+1]
	var memories []extractedMemory
	if err := json.Unmarshal([]byte(jsonArray), &memories); err != nil {
		return nil
	}

	return sanitizeExtractedMemories(memories)
}
