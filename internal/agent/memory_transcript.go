package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/memory"
)

// MemoryBackendType identifies which memory pipeline backend is active.
type MemoryBackendType string

const (
	// MemoryBackendLocal uses the file-based local memory backend.
	MemoryBackendLocal MemoryBackendType = "local"
	// MemoryBackendTranscript uses the transcript-retain memory backend.
	MemoryBackendTranscript MemoryBackendType = "transcript"

	// defaultRetainEveryNTurns controls how often a transcript window is
	// persisted during a session.
	defaultRetainEveryNTurns = 3

	// transcriptMemoryType is the type tag written to the memory store for
	// transcript window entries.
	transcriptMemoryType = "transcript"

	// transcriptMemoryScope is the scope tag written to the memory store for
	// transcript window entries.
	transcriptMemoryScope = "session"

	// maxTranscriptWindowChars is the maximum number of characters retained in
	// a single transcript window. Content beyond this limit is dropped from
	// the head (oldest turns) so that the most recent context is preserved.
	maxTranscriptWindowChars = 12_000
)

// transcriptWindowKey returns the memory key used to store the transcript
// window for the given session.
func transcriptWindowKey(sessionID string) string {
	return fmt.Sprintf("transcript/%s/window", strings.TrimSpace(sessionID))
}

// buildTranscriptWindow joins history entries with double newlines and
// truncates from the head when the result exceeds maxTranscriptWindowChars,
// preserving the most recent content.
func buildTranscriptWindow(history []string) string {
	if len(history) == 0 {
		return ""
	}
	joined := strings.Join(history, "\n\n")
	runes := []rune(joined)
	if len(runes) > maxTranscriptWindowChars {
		runes = runes[len(runes)-maxTranscriptWindowChars:]
	}
	return strings.TrimSpace(string(runes))
}

// retainTranscriptWindow builds a transcript window from history and persists
// it to the memory service under the session-scoped transcript key.
func retainTranscriptWindow(ctx context.Context, memorySvc memory.Service, sessionID, prompt string, history []string) {
	window := buildTranscriptWindow(history)
	if window == "" {
		return
	}

	var desc string
	promptRunes := []rune(strings.TrimSpace(prompt))
	if len(promptRunes) == 0 {
		desc = fmt.Sprintf("Transcript window at %s.", time.Now().Format(time.RFC3339))
	} else {
		if len(promptRunes) > 80 {
			desc = string(promptRunes[:80]) + "…"
		} else {
			desc = string(promptRunes)
		}
	}

	key := transcriptWindowKey(sessionID)
	err := memorySvc.Store(ctx, memory.StoreParams{
		Key:         key,
		Value:       window,
		Description: desc,
		Scope:       transcriptMemoryScope,
		Type:        transcriptMemoryType,
		Importance:  0.7,
	})
	if err != nil {
		slog.Warn("Failed to retain transcript window.", "session_id", sessionID, "error", err)
		return
	}
	slog.Debug("Retained transcript window.", "session_id", sessionID, "key", key)
}

// buildTranscriptRecallBlock retrieves the stored transcript window for the
// session and formats it as a context block for injection into prompts.
// Returns an empty string when no window is found or when memorySvc is nil.
func buildTranscriptRecallBlock(ctx context.Context, memorySvc memory.Service, sessionID string) string {
	if memorySvc == nil || strings.TrimSpace(sessionID) == "" {
		return ""
	}
	entry, err := memorySvc.Get(ctx, transcriptWindowKey(sessionID))
	if err != nil {
		if !errors.Is(err, memory.ErrNotFound) {
			slog.Warn("Failed to retrieve transcript window.", "session_id", sessionID, "error", err)
		}
		return ""
	}
	value := strings.TrimSpace(entry.Value)
	if value == "" {
		return ""
	}
	return "Session transcript context (retained):\n" + value
}
