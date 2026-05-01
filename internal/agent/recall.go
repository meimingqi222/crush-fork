package agent

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/memory"
	"github.com/charmbracelet/crush/internal/oauth/copilot"
)

const (
	autoRecallMemoryLimit        = 5
	autoRecallSectionCharLimit   = 4096
	maxSessionRecallBytes        = 60 * 1024 // 60KB cumulative cap, matching claude-code's MAX_SESSION_BYTES
	shortQueryWordThreshold      = 5
	shortQueryExpansionMaxTokens = 256
	shortQueryExpansionMaxTurns  = 6
	shortQueryExpansionPrompt    = `You are expanding a short user query into a richer search query for memory retrieval. Given the user's current query and the recent conversation history, produce an expanded query that captures the user's actual intent.

Rules:
- Keep the expansion concise (under 20 words).
- Preserve the user's language (do not translate).
- Focus on the core topic, not conversational filler.
- If the query is already specific enough, return it unchanged.

Return ONLY the expanded query text, nothing else.`
)

type backgroundModel struct {
	model    Model
	provider config.ProviderConfig
}

func (b *backgroundModel) semanticSearch(ctx context.Context, memorySvc memory.Service, params memory.SearchParams) ([]memory.Entry, error) {
	if b == nil {
		return memorySvc.Search(ctx, params)
	}
	entries, err := semanticSearchMemories(ctx, memorySvc, b.model, b.provider, params, nil, nil)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 && strings.TrimSpace(params.Query) == "" {
		return memorySvc.Search(ctx, params)
	}
	return entries, nil
}

func buildAutoRecallBlock(ctx context.Context, memorySvc memory.Service, bgModel *backgroundModel, sessionID, prompt string, recentTools []string, alreadySurfaced map[string]bool, recentConversation string) string {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return ""
	}

	// Short prompts lack enough context for meaningful term extraction.
	// When a background model and recent conversation are available,
	// expand the query into a semantically richer search query.
	// If expansion fails and the query still has at least two words,
	// fall back to the original query rather than skipping entirely.
	wordCount := len(strings.Fields(prompt))
	if wordCount < shortQueryWordThreshold {
		if bgModel != nil && strings.TrimSpace(recentConversation) != "" {
			expanded, err := expandShortQuery(ctx, bgModel, prompt, recentConversation)
			if err == nil && strings.TrimSpace(expanded) != "" {
				prompt = strings.TrimSpace(expanded)
				wordCount = len(strings.Fields(prompt))
			}
		}
		// Single-word queries remain too ambiguous even after expansion.
		if wordCount < 2 {
			return ""
		}
	}

	sections := make([]string, 0, 1)

	if memorySvc != nil {
		scope, includeMemory := autoRecallMemoryScope(ctx)
		if includeMemory {
			entries := recallEntriesForSession(ctx, memorySvc, bgModel, sessionID, prompt, scope, recentTools, alreadySurfaced)
			if len(entries) > 0 {
				sections = append(sections, formatAutoRecallMemory(entries))
			}
		}
	}

	if len(sections) == 0 {
		return ""
	}
	return strings.Join(sections, "\n\n")
}

// expandShortQuery uses the background model to expand a terse user query into
// a richer search query by incorporating recent conversation context.
// This mirrors claude-code's approach of surfacing more relevant memories
// when the user says things like "continue" or "fix it".
func expandShortQuery(ctx context.Context, bgModel *backgroundModel, query, recentConversation string) (string, error) {
	if bgModel == nil {
		return "", nil
	}

	prompt := fmt.Sprintf("Recent conversation:\n%s\n\nCurrent query: %s\n\nExpanded query:", recentConversation, query)

	agent := fantasy.NewAgent(
		bgModel.model.Model,
		fantasy.WithSystemPrompt(shortQueryExpansionPrompt),
		fantasy.WithMaxOutputTokens(shortQueryExpansionMaxTokens),
		fantasy.WithUserAgent(memoryUserAgent),
	)

	resp, err := agent.Stream(copilot.ContextWithInitiatorType(ctx, copilot.InitiatorAgent), fantasy.AgentStreamCall{
		Prompt:          prompt,
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
		return "", err
	}
	if resp == nil {
		return "", nil
	}
	return strings.TrimSpace(resp.Response.Content.Text()), nil
}

func recallEntriesForSession(ctx context.Context, memorySvc memory.Service, bgModel *backgroundModel, sessionID, query, scope string, recentTools []string, alreadySurfaced map[string]bool) []memory.Entry {
	entries := make([]memory.Entry, 0, autoRecallMemoryLimit)
	seen := make(map[string]bool)

	if shouldInjectSessionMemory(scope) && strings.TrimSpace(sessionID) != "" {
		if entry, err := memorySvc.Get(ctx, sessionMemoryKey(sessionID)); err == nil {
			entries = append(entries, entry)
			seen[entry.Key] = true
		}
	}

	if bgModel == nil {
		return entries
	}

	matched := selectRelevantMemories(ctx, memorySvc, bgModel.model, bgModel.provider, query, scope, recentTools, alreadySurfaced)
	for _, entry := range matched {
		if seen[entry.Key] {
			continue
		}
		entries = append(entries, entry)
		seen[entry.Key] = true
		if len(entries) >= autoRecallMemoryLimit {
			break
		}
	}

	return entries
}

func shouldInjectSessionMemory(scope string) bool {
	scope = strings.TrimSpace(scope)
	return scope == "" || strings.EqualFold(scope, "session")
}

func formatAutoRecallMemory(entries []memory.Entry) string {
	lines := make([]string, 0, len(entries)+1)
	lines = append(lines, "Relevant long-term memory:")
	for _, entry := range entries {
		value := truncateRecallText(strings.TrimSpace(entry.Value), autoRecallSectionCharLimit)
		line := fmt.Sprintf("- %s: %s", formatAutoRecallMemoryLabel(entry), value)
		if caveat := memoryFreshnessText(entry.UpdatedAt); caveat != "" {
			line += "\n  " + caveat
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func formatAutoRecallMemoryLabel(entry memory.Entry) string {
	label := strings.TrimSpace(entry.Key)
	qualifiers := make([]string, 0, 2)

	switch {
	case entry.Category != "" && entry.Type != "":
		qualifiers = append(qualifiers, fmt.Sprintf("%s/%s", strings.TrimSpace(entry.Category), strings.TrimSpace(entry.Type)))
	case entry.Category != "":
		qualifiers = append(qualifiers, strings.TrimSpace(entry.Category))
	case entry.Type != "":
		qualifiers = append(qualifiers, strings.TrimSpace(entry.Type))
	}

	if len(entry.Tags) > 0 {
		tags := make([]string, 0, len(entry.Tags))
		for _, tag := range entry.Tags {
			trimmed := strings.TrimSpace(tag)
			if trimmed == "" {
				continue
			}
			tags = append(tags, "#"+trimmed)
		}
		if len(tags) > 0 {
			qualifiers = append(qualifiers, strings.Join(tags, " "))
		}
	}

	if entry.UpdatedAt > 0 {
		qualifiers = append(qualifiers, fmt.Sprintf("updated %s", time.Unix(0, entry.UpdatedAt).UTC().Format(time.RFC3339)))
	}

	if len(qualifiers) == 0 {
		return label
	}
	return fmt.Sprintf("%s (%s)", label, strings.Join(qualifiers, "; "))
}

func autoRecallMemoryScope(ctx context.Context) (string, bool) {
	memoryPolicy := strings.ToLower(strings.TrimSpace(tools.GetAgentMemoryFromContext(ctx)))
	isolationPolicy := strings.ToLower(strings.TrimSpace(tools.GetAgentIsolationFromContext(ctx)))

	switch memoryPolicy {
	case "ephemeral":
		return "", false
	case "isolated", "session":
		return "session", true
	case "project":
		return "project", true
	}

	switch isolationPolicy {
	case "session", "process":
		return "session", true
	case "workspace":
		return "project", true
	}

	return "", true
}

// memoryFreshnessText returns a staleness caveat for memories older than 1 day.
// Models are poor at date arithmetic — a raw ISO timestamp doesn't trigger
// staleness reasoning the way "47 days ago" does. This mirrors claude-code's
// memoryFreshnessText approach.
func memoryFreshnessText(updatedAtNano int64) string {
	if updatedAtNano <= 0 {
		return ""
	}
	updatedAt := time.Unix(0, updatedAtNano)
	days := int(time.Since(updatedAt).Hours() / 24)
	if days <= 1 {
		return ""
	}
	return fmt.Sprintf("[This memory is %d days old. Memories are point-in-time observations, not live state — claims about code behavior or file:line citations may be outdated. Verify against current code before asserting as fact.]", days)
}

func truncateRecallText(text string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return string(runes[:limit]) + "…"
}

// FormatAutoRecallMessage wraps memory content in a system-reminder tag.
// This approach mirrors Claude Code's design: memories are injected as user
// messages wrapped in <system-reminder> tags, preserving prompt cache.
func FormatAutoRecallMessage(content string) string {
	if content == "" {
		return ""
	}
	return "<system-reminder>\n" + content + "\n</system-reminder>"
}
