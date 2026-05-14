package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/memory/engine"
	"github.com/charmbracelet/crush/internal/message"
)

const (
	// transcriptWindowMaxRunes limits transcript windows by Unicode code points.
	transcriptWindowMaxRunes = 12_000

	// Transcript windows are raw source context, not model-extracted memories.
	defaultTranscriptConfidence = 0.9
	defaultTranscriptImportance = 0.5

	// transcriptMemoryKind is the kind tag for transcript window events.
	transcriptMemoryKind = "transcript_window"
)

// TranscriptRetainer retains bounded transcript windows for a session.
type TranscriptRetainer struct {
	store engine.EventStore
}

// NewTranscriptRetainer creates a transcript retainer using the given store.
func NewTranscriptRetainer(store engine.EventStore) *TranscriptRetainer {
	return &TranscriptRetainer{store: store}
}

// RetainTranscript retains the provided bounded transcript window for a session.
func (r *TranscriptRetainer) RetainTranscript(ctx context.Context, sessionID string, turnCount int, content string) error {
	if r == nil || r.store == nil {
		return nil
	}
	return retainTranscriptContent(ctx, r.store, sessionID, content, turnCount)
}

// buildTranscriptWindow constructs a transcript window from recent messages.
// It truncates to transcriptWindowMaxRunes to keep the retained window bounded.
func buildTranscriptWindow(msgs []message.Message) string {
	var lines []string
	totalRunes := 0

	// Walk backwards from most recent messages.
	for i := len(msgs) - 1; i >= 0; i-- {
		msg := msgs[i]
		var line string
		switch msg.Role {
		case message.User:
			if text := strings.TrimSpace(msg.Content().Text); text != "" {
				line = "USER: " + text
			}
		case message.Assistant:
			if text := strings.TrimSpace(msg.Content().Text); text != "" {
				line = "ASSISTANT: " + text
			}
		}
		if line == "" {
			continue
		}
		lineRunes := []rune(line)
		lineLen := len(lineRunes)
		if len(lines) > 0 {
			lineLen++
		}
		if totalRunes+lineLen > transcriptWindowMaxRunes {
			if len(lines) == 0 {
				line = string(lineRunes[:transcriptWindowMaxRunes])
				lineLen = transcriptWindowMaxRunes
			} else {
				break
			}
		}
		// Prepend to maintain order.
		lines = append([]string{line}, lines...)
		totalRunes += lineLen
	}

	return strings.Join(lines, "\n")
}

// retainTranscriptWindow stores the current transcript window as a session-scoped
// memory event. This is called every N turns when using the transcript backend.
// It does NOT trigger LLM extraction - the raw transcript is stored directly.
func retainTranscriptWindow(ctx context.Context, store engine.EventStore, sessionID string, msgs []message.Message, turnCount int) error {
	return retainTranscriptContent(ctx, store, sessionID, buildTranscriptWindow(msgs), turnCount)
}

func retainTranscriptContent(ctx context.Context, store engine.EventStore, sessionID, content string, turnCount int) error {
	if store == nil || strings.TrimSpace(sessionID) == "" || strings.TrimSpace(content) == "" {
		return nil
	}

	now := time.Now()
	eventID := fmt.Sprintf("tw-%s-%d-%d", sessionID, now.UnixNano(), turnCount)
	event := engine.MemoryEvent{
		ID:      eventID,
		Scope:   engine.MemoryScopeSession,
		Kind:    engine.MemoryKindReference,
		Content: content,
		Summary: fmt.Sprintf("Transcript window at turn %d", turnCount),
		Source: engine.MemorySourceRef{
			SessionID: sessionID,
		},
		Confidence: defaultTranscriptConfidence,
		Importance: defaultTranscriptImportance,
		CreatedAt:  now,
		UpdatedAt:  now,
		Tags:       []string{transcriptMemoryKind, "turn:" + fmt.Sprint(turnCount)},
	}

	if err := store.Append(ctx, event); err != nil {
		return fmt.Errorf("appending transcript window: %w", err)
	}

	slog.Debug("Transcript window retained", "session_id", sessionID, "turn", turnCount, "runes", len([]rune(content)))
	return nil
}

// buildTranscriptRecallBlock retrieves the latest retained transcript window
// for session injection. Returns empty string if not found or on error.
func buildTranscriptRecallBlock(ctx context.Context, store engine.EventStore, sessionID string) string {
	if store == nil || strings.TrimSpace(sessionID) == "" {
		return ""
	}

	// Query for transcript window events.
	kind := engine.MemoryKindReference
	events, err := store.Query(ctx, engine.EventFilter{
		Scope:     ptrScope(engine.MemoryScopeSession),
		Kind:      &kind,
		SessionID: &sessionID,
		Tags:      []string{transcriptMemoryKind},
		Limit:     1,
		OrderDesc: true,
	})
	if err != nil {
		slog.Warn("Failed to query transcript windows", "error", err, "session_id", sessionID)
		return ""
	}
	if len(events) == 0 {
		return ""
	}

	// Get the most recent transcript window.
	latest := events[0]
	if latest.Content == "" {
		return ""
	}

	return formatTranscriptRecall(latest.Content)
}

func formatTranscriptRecall(content string) string {
	if content == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("<transcript_context>\n")
	b.WriteString(content)
	b.WriteString("\n</transcript_context>")
	return b.String()
}

// ptrScope is a helper to get a pointer to a MemoryScope.
func ptrScope(s engine.MemoryScope) *engine.MemoryScope {
	return &s
}
