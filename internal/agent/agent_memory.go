package agent

import (
	"context"

	"github.com/charmbracelet/crush/internal/message"
)

func (a *sessionAgent) getHistoryForMemoryExtraction(ctx context.Context, sessionID string) []string {
	msgs, err := a.messages.List(ctx, sessionID)
	if err != nil {
		return nil
	}

	var history []string
	for _, msg := range msgs {
		switch msg.Role {
		case message.User:
			if text := msg.Content().Text; text != "" {
				history = append(history, "USER: "+text)
			}
		case message.Assistant:
			if text := msg.Content().Text; text != "" {
				history = append(history, "ASSISTANT: "+text)
			}
		}
	}
	return history
}

func (a *sessionAgent) trackPendingExtractionLocked(sessionID string, cancel context.CancelFunc) uint64 {
	a.nextExtractionID++
	pendingID := a.nextExtractionID
	a.pendingExtractions[sessionID] = append(a.pendingExtractions[sessionID], pendingExtraction{
		id:     pendingID,
		cancel: cancel,
	})
	return pendingID
}

func (a *sessionAgent) finishPendingExtraction(sessionID string, pendingID uint64) {
	a.extractionMu.Lock()
	defer a.extractionMu.Unlock()
	pending := a.pendingExtractions[sessionID]
	filtered := make([]pendingExtraction, 0, len(pending))
	for _, candidate := range pending {
		if candidate.id == pendingID {
			continue
		}
		filtered = append(filtered, candidate)
	}
	if len(filtered) == 0 {
		delete(a.pendingExtractions, sessionID)
		return
	}
	a.pendingExtractions[sessionID] = filtered
}

func (a *sessionAgent) enableSessionMemory() bool {
	return a.sessionMemoryEnabled && a.memory != nil && a.backgroundModel != nil
}
