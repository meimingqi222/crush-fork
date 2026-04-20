package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
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
	memoryRelevanceMaxSelected = 5
	memoryRelevanceMaxFiles    = 50
)

// memoryRelevancePrompt is the system prompt for the memory relevance selection model.
const memoryRelevancePrompt = `You are a memory relevance selector. Given a user query and a manifest of available memory entries, select up to 5 most relevant memories that would help answer the query.

Rules:
- Only select memories that are directly relevant to the query
- Prefer recent memories (higher updated_at timestamps)
- Prefer specific, actionable memories over vague ones
- Return ONLY a JSON array of memory keys, nothing else
- Return an empty array [] if no memories are relevant

Example output:
["project/goal", "user/preferred-language"]`

// selectRelevantMemories uses the background model to select the most relevant
// memories for a given query, replacing simple string matching.
func selectRelevantMemories(ctx context.Context, memorySvc memory.Service, model Model, providerCfg config.ProviderConfig, query string, scope string) []memory.Entry {
	if memorySvc == nil {
		return nil
	}

	infos, err := memorySvc.ListMemoryFiles()
	if err != nil {
		slog.Warn("Failed to list memory files for relevance selection", "error", err)
		return nil
	}

	if len(infos) == 0 {
		return nil
	}

	filteredInfos := make([]memory.MemoryFileInfo, 0, len(infos))
	for _, info := range infos {
		if matchesRequestedMemoryScope(info.Scope, scope) {
			filteredInfos = append(filteredInfos, info)
		}
	}
	infos = filteredInfos

	if len(infos) == 0 {
		return nil
	}

	if len(infos) > memoryRelevanceMaxFiles {
		infos = infos[:memoryRelevanceMaxFiles]
	}

	manifest := buildMemoryManifest(infos)

	prompt := fmt.Sprintf("Query: %s\n\nMemory manifest:\n%s\n\nSelect relevant memory keys:", query, manifest)

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
		slog.Warn("LLM memory relevance selection failed, falling back to string matching", "error", err)
		return fallbackMemorySearch(ctx, memorySvc, query, scope)
	}
	if resp == nil {
		return fallbackMemorySearch(ctx, memorySvc, query, scope)
	}

	content := resp.Response.Content.Text()
	selectedKeys := parseMemorySelectionResponse(content)
	if len(selectedKeys) == 0 {
		return fallbackMemorySearch(ctx, memorySvc, query, scope)
	}

	entries := make([]memory.Entry, 0, len(selectedKeys))
	for _, key := range selectedKeys {
		entry, err := memorySvc.Get(ctx, key)
		if err == nil && matchesRequestedMemoryScope(entry.Scope, scope) {
			entries = append(entries, entry)
		}
	}

	if len(entries) == 0 {
		return fallbackMemorySearch(ctx, memorySvc, query, scope)
	}

	return entries
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

func fallbackMemorySearch(ctx context.Context, memorySvc memory.Service, query, scope string) []memory.Entry {
	params := memory.SearchParams{
		Query: query,
		Limit: autoRecallMemoryLimit,
	}
	if scope != "" {
		params.Scope = scope
	}
	entries, err := memorySvc.Search(ctx, params)
	if err != nil {
		return nil
	}
	return entries
}

const memoryExtractionThrottleTurns = 1

var memoryStoreActionPattern = regexp.MustCompile(`(?i)(^|[^a-z0-9_])["']?action["']?\s*[:=]\s*["']?store["']?([^a-z0-9_]|$)`)

const memoryExtractPrompt = `You are a memory extraction agent. Analyze the conversation transcript and maintain long-term memory files for future sessions.

Memory types:
- user: persistent user preferences, identity, constraints, working style.
- feedback: corrections about how the assistant should work.
- project: durable project context, architecture, repeated workflows, key decisions.
- reference: pointers to external systems or resources that are costly to rediscover.

Rules:
- Extract only durable knowledge. Prefer stable preferences, project context, commands that were confirmed to work, and repeated decisions.
- Do NOT save transient task state, one-off logs, temporary file contents, or information already obvious in the codebase.
- Before creating a new key, prefer updating an existing durable memory when the idea matches.
- If an existing memory is now stale, contradicted, or superseded, you may delete it instead of creating another overlapping memory.
- Return JSON with array entries shaped like {"action":"store|update|delete|noop","key":"...","description":"...","content":"...","type":"user|feedback|project|reference","scope":"project|session"}.
- store/update require key + description + content. delete requires key only. noop is optional and will be ignored.
- Return [] if nothing should change.

Example output:
[{"action":"update","key":"user/preferred-style","description":"User prefers concise code","content":"The user prefers concise code and short explanations.","type":"user"},{"action":"delete","key":"project/old-workflow"}]`

func shouldExtractMemories(turnsSinceLastExtraction int) bool {
	return turnsSinceLastExtraction >= memoryExtractionThrottleTurns
}

func isMemoryStoreToolCallLine(line string) bool {
	if line == "" {
		return false
	}

	if !strings.Contains(strings.ToLower(line), "long_term_memory") {
		return false
	}

	return memoryStoreActionPattern.MatchString(line)
}

func hasMemoryWritesInHistory(history []string) bool {
	for _, h := range history {
		if isMemoryStoreToolCallLine(h) {
			return true
		}
	}
	return false
}

func extractMemories(ctx context.Context, memorySvc memory.Service, bgModel *backgroundModel, sessionID, prompt string, history []string) {
	if memorySvc == nil || bgModel == nil {
		return
	}

	if len(history) < 2 {
		slog.Debug("Not enough conversation history for memory extraction", "session_id", sessionID)
		return
	}

	if hasMemoryWritesInHistory(history) {
		slog.Debug("Skipping extraction - memory writes detected in conversation", "session_id", sessionID)
		return
	}

	historyStr := strings.Join(history, "\n\n")
	if len(historyStr) < 200 {
		slog.Debug("Conversation too short for memory extraction", "session_id", sessionID, "chars", len(historyStr))
		return
	}

	existingMemories, err := memorySvc.ListMemoryFiles()
	if err != nil {
		slog.Warn("Failed to list existing memories for context", "error", err)
	} else if len(existingMemories) > 0 {
		manifest := buildMemoryManifest(existingMemories[:min(len(existingMemories), 20)])
		historyStr = "Existing memories:\n" + manifest + "\n\nConversation:\n" + historyStr
	}

	extractPrompt := fmt.Sprintf("Initial prompt: %s\n\nConversation transcript:\n%s\n\nExtract any durable memories worth saving (avoid duplicates with existing memories):", prompt, historyStr)

	agent := fantasy.NewAgent(
		bgModel.model.Model,
		fantasy.WithSystemPrompt(memoryExtractPrompt),
		fantasy.WithMaxOutputTokens(2048),
		fantasy.WithUserAgent(memoryUserAgent),
	)

	extractCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := agent.Stream(copilot.ContextWithInitiatorType(extractCtx, copilot.InitiatorAgent), fantasy.AgentStreamCall{
		Prompt:          extractPrompt,
		ProviderOptions: getProviderOptions(bgModel.model, bgModel.provider),
		PrepareStep: func(callCtx context.Context, options fantasy.PrepareStepFunctionOptions) (_ context.Context, prepared fantasy.PrepareStepResult, err error) {
			callCtx = copilot.ContextWithInitiatorType(callCtx, copilot.InitiatorAgent)
			prepared.Messages = options.Messages
			if bgModel.provider.SystemPromptPrefix != "" {
				prepared.Messages = append([]fantasy.Message{
					fantasy.NewSystemMessage(bgModel.provider.SystemPromptPrefix),
				}, prepared.Messages...)
			}
			return callCtx, prepared, nil
		},
	})
	if err != nil {
		slog.Warn("Memory extraction failed", "error", err, "session_id", sessionID)
		return
	}
	if resp == nil {
		return
	}

	content := resp.Response.Content.Text()
	memories := parseExtractedMemories(content)
	if len(memories) == 0 {
		slog.Debug("No memories extracted from conversation", "session_id", sessionID)
		return
	}

	if err := applyExtractedMemories(ctx, memorySvc, memories, "session_id", sessionID); err != nil {
		slog.Warn("Failed to apply extracted memories", "error", err, "session_id", sessionID)
	}
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
