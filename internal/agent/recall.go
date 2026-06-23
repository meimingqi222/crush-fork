package agent

import (
	"context"
	"strings"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/memory/engine"
)

const (
	maxSessionRecallBytes = 60 * 1024
	// maxAutoRecallQueryChars bounds the composed recall query sent to
	// Hindsight. The recall API rejects queries above 500 tokens; ~1000 runes
	// leaves headroom for token-dense scripts (e.g. CJK) before the harder
	// limit in the hindsight package kicks in.
	maxAutoRecallQueryChars = 1000
)

type backgroundModel struct {
	model    Model
	provider config.ProviderConfig
}

func buildAutoRecallBlock(ctx context.Context, retriever engine.Retriever, prompt, recentContext, sessionID, backend string) string {
	if retriever == nil || !autoRecallMemoryEnabled(ctx) {
		return ""
	}

	query := composeRecallQuery(prompt, recentContext)
	if backend == "hindsight" {
		query = truncateRecallQuery(query, prompt, maxAutoRecallQueryChars)
	}
	query = strings.TrimSpace(query)

	if query != "" {
		events, err := retriever.Retrieve(ctx, query, map[string]any{"session_id": sessionID, "limit": 8})
		if err == nil && len(events) > 0 {
			return formatEventsAsRecall(events)
		}
		// Hindsight backend has no fallback to static broad recall to avoid polluting context
		if backend == "hindsight" {
			return ""
		}
	}
	recall, err := retriever.Recall(ctx, map[string]any{"session_id": sessionID})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(recall)
}

// recallContextMarker frames the historical portion of a composed query so it
// can be split back into "context lines" + "latest prompt" for graceful
// truncation. Mirrors oh-my-pi's `Prior context:` convention.
const recallContextMarker = "Prior context:\n\n"

// composeRecallQuery builds a recall query from the latest user prompt and an
// optional recent-conversation block.
//
// When recentContext is empty the query is just the trimmed prompt. When
// prompt is empty (e.g. the compaction-rescue path that has no current
// prompt) the query is just the trimmed recentContext. Otherwise recentContext
// is framed as "Prior context:" ahead of the prompt, giving the Hindsight
// retriever a clear semantic boundary and a stable anchor for truncation.
func composeRecallQuery(prompt, recentContext string) string {
	prompt = strings.TrimSpace(prompt)
	recentContext = strings.TrimSpace(recentContext)
	switch {
	case recentContext == "":
		return prompt
	case prompt == "":
		return recentContext
	default:
		return recallContextMarker + recentContext + "\n\n" + prompt
	}
}

// truncateRecallQuery caps a composed query to maxChars runes, preserving the
// latest user message and dropping the oldest context lines first. With no
// structured marker the query is tail-truncated (keeping the most recent
// content). When maxChars <= 0 the query is returned unchanged.
func truncateRecallQuery(query, prompt string, maxChars int) string {
	query = strings.TrimSpace(query)
	if maxChars <= 0 || runeLen(query) <= maxChars {
		return query
	}

	latest := strings.TrimSpace(prompt)
	latestOnly := latest
	if runeLen(latestOnly) > maxChars {
		latestOnly = tailRunes(latestOnly, maxChars)
	}

	// No structured marker: tail-truncate. Prefer the prompt when one was
	// supplied (it is the most relevant signal).
	if !strings.HasPrefix(query, recallContextMarker) {
		if latest != "" {
			return latestOnly
		}
		return tailRunes(query, maxChars)
	}

	// Structured form: "Prior context:\n\n<context>\n\n<prompt>". Recover the
	// context body and the prompt suffix so we can drop oldest lines first.
	rest := strings.TrimPrefix(query, recallContextMarker)
	suffix := "\n\n" + latest
	suffixIdx := strings.LastIndex(rest, suffix)
	if latest == "" || suffixIdx == -1 || runeLen(suffix) >= maxChars {
		return latestOnly
	}

	contextBody := rest[:suffixIdx]
	contextLines := strings.Split(contextBody, "\n")

	// Keep the most recent context lines; drop the oldest until it fits.
	for len(contextLines) > 0 {
		candidate := recallContextMarker + strings.Join(contextLines, "\n") + suffix
		if runeLen(candidate) <= maxChars {
			return candidate
		}
		contextLines = contextLines[1:]
	}
	return latestOnly
}

// runeLen returns the rune count of s.
func runeLen(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

// tailRunes returns the trailing max runes of s. The latest content sits at
// the end of the query, so a tail slice preserves the most relevant signal.
func tailRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[len(runes)-max:])
}

func formatEventsAsRecall(events []engine.MemoryEvent) string {
	if len(events) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<hindsight_memories>\n")
	for _, e := range events {
		b.WriteString("- ")
		b.WriteString(e.Content)
		b.WriteString("\n")
	}
	b.WriteString("</hindsight_memories>")
	return b.String()
}

func autoRecallMemoryEnabled(ctx context.Context) bool {
	memoryPolicy := strings.ToLower(strings.TrimSpace(tools.GetAgentMemoryFromContext(ctx)))

	switch memoryPolicy {
	case "ephemeral":
		return false
	}

	return true
}

// FormatAutoRecallMessage wraps memory content in a system-reminder tag.
// This approach mirrors Claude Code's design: memories are presented as
// user-message content wrapped in <system-reminder> tags, and merged into
// existing user messages rather than prepended to the message list,
// preserving prompt cache.
func FormatAutoRecallMessage(content string) string {
	if content == "" {
		return ""
	}
	return "<system-reminder>\n" + content + "\n</system-reminder>"
}
