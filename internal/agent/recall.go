package agent

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/memory"
)

const (
	autoRecallMemoryLimit      = 5
	autoRecallSectionCharLimit = 4096
	maxSessionRecallBytes      = 60 * 1024 // 60KB cumulative cap, matching claude-code's MAX_SESSION_BYTES
)

type backgroundModel struct {
	model    Model
	provider config.ProviderConfig
}

func buildAutoRecallBlock(ctx context.Context, memorySvc memory.Service, bgModel *backgroundModel, sessionID, prompt string, recentTools []string, alreadySurfaced map[string]bool) string {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return ""
	}
	// Single-word prompts lack enough context for meaningful term extraction.
	if !strings.Contains(prompt, " ") {
		return ""
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
