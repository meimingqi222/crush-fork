package agent

import (
	"context"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/crush/internal/guimetrics"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/sessionevent"
)

const (
	maxLiveToolTextBytes = 64 * 1024
	maxLiveDeltaBytes    = 64 * 1024
)

func (a *sessionAgent) publishSessionEvent(ctx context.Context, sessionID string, started time.Time, event sessionevent.NewEvent) {
	if a == nil || a.sessionEvents == nil {
		return
	}
	_, err := a.sessionEvents.Publish(sessionID, event)
	outcome := "success"
	if err != nil {
		outcome = "error"
		slog.Warn("Failed to publish live session event", "error", err, "event_kind", event.Kind, "session_id", sessionID)
	}
	guimetrics.FromContext(ctx).ObserveDuration(
		guimetrics.StreamChunkToEventDuration,
		time.Since(started),
		guimetrics.Labels{Kind: liveMetricKind(event.Kind), Outcome: outcome},
	)
}

func liveMetricKind(kind sessionevent.Kind) string {
	switch kind {
	case sessionevent.KindMessageDelta, sessionevent.KindMessageCreated,
		sessionevent.KindMessageCompleted, sessionevent.KindMessageReset:
		return "message"
	case sessionevent.KindReasoningDelta:
		return "reasoning"
	case sessionevent.KindToolProgress, sessionevent.KindToolCompleted:
		return "tool"
	case sessionevent.KindTurnStarted, sessionevent.KindTurnCompleted,
		sessionevent.KindTurnFailed, sessionevent.KindTurnCancelled, sessionevent.KindTurnSteered,
		sessionevent.KindTurnProgress, sessionevent.KindCancelAcknowledged:
		return "turn"
	case sessionevent.KindUsageUpdated:
		return "usage"
	default:
		return "other"
	}
}

func liveUsage(usage message.Usage) sessionevent.UsageEvent {
	return sessionevent.UsageEvent{
		InputTokens:      usage.InputTokens,
		OutputTokens:     usage.OutputTokens,
		ReasoningTokens:  usage.ReasoningTokens,
		CacheReadTokens:  usage.CacheReadTokens,
		CacheWriteTokens: usage.CacheWriteTokens,
	}
}

func boundedLiveToolText(text string) (string, bool) {
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "�")
	}
	if len(text) <= maxLiveToolTextBytes {
		return text, false
	}
	limit := maxLiveToolTextBytes - len("…")
	for limit > 0 && !utf8.ValidString(text[:limit]) {
		limit--
	}
	return text[:limit] + "…", true
}

func (a *sessionAgent) publishLiveTextDelta(ctx context.Context, sessionID string, started time.Time, kind sessionevent.Kind, messageID, partID, text string) {
	for _, chunk := range splitLiveText(text) {
		a.publishSessionEvent(ctx, sessionID, started, sessionevent.NewEvent{
			Kind:        kind,
			Delivery:    sessionevent.DeliveryMerge,
			CoalesceKey: messageID + ":" + partID,
			Payload: sessionevent.TextDelta{
				MessageID: messageID,
				PartID:    partID,
				Text:      chunk,
			},
		})
	}
}

func splitLiveText(text string) []string {
	if text == "" {
		return nil
	}
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "�")
	}
	if len(text) <= maxLiveDeltaBytes {
		return []string{text}
	}
	chunks := make([]string, 0, len(text)/maxLiveDeltaBytes+1)
	for len(text) > 0 {
		limit := min(len(text), maxLiveDeltaBytes)
		for limit > 0 && !utf8.ValidString(text[:limit]) {
			limit--
		}
		chunks = append(chunks, text[:limit])
		text = text[limit:]
	}
	return chunks
}
