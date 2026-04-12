package agent

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/memory"
)

const (
	autoRecallHistoryLimit     = 3
	autoRecallSectionCharLimit = 600
	autoRecallQueryMaxWords    = 12
)

// recallFilePattern matches file references like foo.go, bar/baz.ts, etc.
var recallFilePattern = regexp.MustCompile(`\b[\w/.-]+\.(?:go|ts|tsx|js|jsx|py|rs|rb|java|kt|swift|md|json|yaml|yml|toml|sql|sh|bash)\b`)

// recallIdentPattern matches CamelCase identifiers (likely function/type names).
var recallIdentPattern = regexp.MustCompile(`\b[A-Z][A-Za-z0-9]{3,}\b`)

// buildAutoRecall returns a closure that, given a session and prompt, retrieves
// relevant long-term memories and session history to inject into the system prompt.
func buildAutoRecall(historySvc history.Service, memorySvc memory.MemoryClient, bgModel *backgroundModel) func(context.Context, string, string) string {
	if historySvc == nil && memorySvc == nil {
		return nil
	}
	return func(ctx context.Context, sessionID, prompt string) string {
		return buildAutoRecallBlock(ctx, historySvc, memorySvc, bgModel, sessionID, prompt)
	}
}

// backgroundModel holds the resolved model and provider config for background tasks
// like memory relevance selection.
type backgroundModel struct {
	model    Model
	provider config.ProviderConfig
}

func buildAutoRecallBlock(ctx context.Context, historySvc history.Service, memorySvc memory.MemoryClient, bgModel *backgroundModel, sessionID, prompt string) string {
	query := extractRecallQuery(prompt)
	if query == "" {
		return ""
	}

	sections := make([]string, 0, 2)

	if memorySvc != nil {
		scope, includeMemory := autoRecallMemoryScope(ctx)
		if includeMemory {
			lines, err := memorySvc.Recall(ctx, query, scope, sessionID)
			if err == nil && len(lines) > 0 {
				sections = append(sections, formatAutoRecallLines(lines))
			}
		}
	}

	if historySvc != nil {
		results, err := historySvc.SearchMessages(ctx, history.SearchParams{
			Query:     query,
			SessionID: sessionID,
			Limit:     autoRecallHistoryLimit,
		})
		if err == nil && len(results) > 0 {
			sections = append(sections, formatAutoRecallHistory(results))
		}
	}

	if len(sections) == 0 {
		return ""
	}
	return strings.Join(sections, "\n\n")
}

// extractRecallQuery distills a prompt into a short, focused search query.
// It extracts file references and CamelCase identifiers first (most precise),
// then falls back to the first autoRecallQueryMaxWords words of the prompt.
func extractRecallQuery(prompt string) string {
	trimmed := strings.TrimSpace(prompt)
	if trimmed == "" {
		return ""
	}

	var tokens []string
	seen := make(map[string]bool)

	addToken := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		tokens = append(tokens, s)
	}

	for _, f := range recallFilePattern.FindAllString(trimmed, -1) {
		addToken(f)
	}
	for _, id := range recallIdentPattern.FindAllString(trimmed, -1) {
		addToken(id)
	}

	if len(tokens) >= 3 {
		return strings.Join(tokens, " ")
	}

	words := strings.Fields(trimmed)
	if len(words) > autoRecallQueryMaxWords {
		words = words[:autoRecallQueryMaxWords]
	}
	return strings.Join(words, " ")
}

func formatAutoRecallLines(lines []string) string {
	out := make([]string, 0, len(lines)+1)
	out = append(out, "Relevant long-term memory:")
	for _, line := range lines {
		text := truncateRecallText(strings.TrimSpace(line), autoRecallSectionCharLimit)
		out = append(out, "- "+text)
	}
	return strings.Join(out, "\n")
}

func formatAutoRecallHistory(results []history.MessageSearchResult) string {
	lines := make([]string, 0, len(results)+1)
	lines = append(lines, "Relevant session history:")
	for _, result := range results {
		text := truncateRecallText(strings.TrimSpace(result.Text), autoRecallSectionCharLimit)
		lines = append(lines, fmt.Sprintf("- [%s] %s", result.Role, text))
	}
	return strings.Join(lines, "\n")
}

// autoRecallMemoryScope determines the scope for memory retrieval based on the
// context's memory and isolation policies.
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

func truncateRecallText(text string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return string(runes[:limit]) + "…"
}
