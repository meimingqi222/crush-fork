package model

import (
	"unicode/utf8"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
)

type contextUsageSnapshot struct {
	TotalTokens   int64
	OutputTokens  int64
	ContextWindow int64
	Provisional   bool
	Estimated     bool
	Summary       bool
}

func (m *UI) currentContextUsageSnapshot() contextUsageSnapshot {
	if m == nil {
		return contextUsageSnapshot{}
	}

	var cfg *config.Config
	selected := m.selectedLargeModel()
	if m.com != nil && m.com.App != nil {
		cfg = m.com.Config()
	}
	return resolveContextUsageSnapshot(m.session, m.sessionMessages, cfg, selected)
}

// frameUsageSnapshotCached returns the per-frame cached usage snapshot.
// If the cache has not been computed yet for this frame (i.e. Draw() has
// not set it), it falls back to computing it on the fly.
func (m *UI) frameUsageSnapshotCached() contextUsageSnapshot {
	if m != nil && m.frameUsageSnapshotValid {
		return m.frameUsageSnapshot
	}
	return m.currentContextUsageSnapshot()
}

// refreshSiblingIndex recomputes the cached position (1-based index) and
// count of the current session among its parent's child sessions, storing
// the result in m.siblingIndex/m.siblingCount. It is a no-op (zeroing the
// cache) when not viewing a child session.
//
// Unlike a per-frame cache, this is event-driven: it must be called
// explicitly whenever the set of siblings could have changed for the
// currently viewed session -- on session switch (loadSessionMsg) and on
// session pubsub events for the current session or one of its siblings.
// This avoids a full DB walk (childSessions hits Messages.List,
// Sessions.ListChildren, and per-tool-call Sessions.Get) on every redraw,
// which matters because sibling subagent streaming can trigger dozens of
// redraws per second.
func (m *UI) refreshSiblingIndex() {
	m.siblingIndex = 0
	m.siblingCount = 0
	if m == nil || m.session == nil || m.session.ParentSessionID == "" {
		return
	}
	parentID := m.session.ParentSessionID
	currentID := m.session.ID
	children, err := m.childSessions(parentID)
	if err != nil || len(children) == 0 {
		return
	}
	m.siblingIndex, m.siblingCount = siblingPosition(children, currentID)
}

// siblingPosition returns the 1-based index and count of currentID within
// children. children is expected in creation order (ascending), matching
// childSessions' contract. If currentID is not found among children (e.g. a
// stale view), index is 0 but count still reflects len(children).
func siblingPosition(children []session.Session, currentID string) (index, count int) {
	count = len(children)
	for i, child := range children {
		if child.ID == currentID {
			return i + 1, count
		}
	}
	return 0, count
}

func resolveContextUsageSnapshot(sess *session.Session, messages []message.Message, cfg *config.Config, selected *agent.Model) contextUsageSnapshot {
	if sess == nil {
		return contextUsageSnapshot{}
	}

	if usage, ok := latestAssistantUsageSnapshot(messages, cfg, selected); ok {
		// OutputTokens represents cumulative output tokens across all
		// exchanges in this session. For finished messages, use the
		// session's cumulative CompletionTokens. For provisional (streaming)
		// messages, take the max of the streaming value and CompletionTokens
		// so the display reflects live growth without dropping below known history.
		if usage.Provisional {
			// During streaming the current exchange has not been committed to
			// session totals yet. The message's PromptTokens may be a local
			// character-based estimate (set in agent.go before the API call),
			// which can overshoot the real value by 30-50%. Using it as the
			// TotalTokens base causes a visible drop when the step finishes
			// and the API's authoritative usage replaces it.
			//
			// Anchor TotalTokens at sess.LastTotalTokens() plus a live
			// estimate of the streamed output (reasoning + text). This is
			// monotonically non-decreasing during streaming and never
			// exceeds the finished value: the real prompt is always >=
			// lastTotal (it includes all prior history), and the real
			// output is always >= the streaming estimate.
			//
			// Summary messages are excluded: their lastTotal reflects the
			// pre-compaction context, which is stale and would push the
			// display above 100%. For summaries, keep the raw usage total
			// (output-only) as before.
			//
			// When lastTotal is zero (first exchange in the session), fall
			// back to the message's estimated total so the display is not
			// stuck at zero until the first step finishes.
			if !usage.Summary {
				lastTotal := sess.LastTotalTokens()
				// Use the higher of the text-based streaming estimate and
				// any output tokens already reported by the provider.
				streamingOutput := max(estimateStreamedOutput(messages), usage.OutputTokens)
				if lastTotal > 0 {
					usage.TotalTokens = lastTotal + streamingOutput
				}
				// OutputTokens (displayed as "Xk out") tracks cumulative
				// output across all exchanges. During streaming, the current
				// exchange's output has not been committed to
				// sess.CompletionTokens yet, so add the streaming estimate
				// on top of the committed cumulative total.
				usage.OutputTokens = sess.CompletionTokens + streamingOutput
			} else {
				usage.OutputTokens = max(usage.OutputTokens, sess.CompletionTokens)
				usage.TotalTokens = max(usage.TotalTokens, sess.LastTotalTokens())
			}
			usage.Estimated = sess.EstimatedUsage
		} else {
			usage.OutputTokens = sess.CompletionTokens
			// For finished messages the latest assistant message's usage is
			// the authoritative current context length: it is the total input
			// (including history) plus output for the last exchange. Session
			// cumulative totals double-count history across multi-step runs,
			// so they must not be used for the context-usage percentage.
			usage.TotalTokens = sess.LastTotalTokens()
		}
		return usage
	}

	contextWindow := int64(0)
	if selected != nil {
		contextWindow = agent.EffectiveContextWindow(selected.CatwalkCfg)
		if contextWindow <= 0 {
			contextWindow = selected.CatwalkCfg.ContextWindow
		}
	}

	return contextUsageSnapshot{
		TotalTokens:   sess.LastTotalTokens(),
		OutputTokens:  sess.CompletionTokens,
		ContextWindow: contextWindow,
	}
}

func latestAssistantUsageSnapshot(messages []message.Message, cfg *config.Config, selected *agent.Model) (contextUsageSnapshot, bool) {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role != message.Assistant {
			continue
		}
		// Use PromptTokens + OutputTokens for context usage, matching
		// opencode's approach of inputTokens + outputTokens.
		// PromptTokens() includes InputTokens + CacheReadTokens + CacheWriteTokens,
		// which equals the full input sent to the model.
		// OutputTokens already includes ReasoningTokens for OpenAI-style providers
		// (and Anthropic output_tokens includes thinking tokens), so we must
		// NOT add ReasoningTokens again to avoid double-counting.
		total := msg.Usage.PromptTokens() + msg.Usage.OutputTokens
		if msg.IsSummaryMessage {
			// A summary message's own PromptTokens reflects the
			// *pre-compaction* history sent to the summarizer, not the
			// current context -- adding it here would make the display
			// jump above 100% while/after compacting. Only the summary's
			// output length is meaningful; resolveContextUsageSnapshot
			// falls back to sess.LastTotalTokens() (the post-compaction
			// baseline written by Summarize) to size the actual context.
			total = msg.Usage.OutputTokens
		}
		if total <= 0 {
			continue
		}
		return contextUsageSnapshot{
			TotalTokens:   total,
			OutputTokens:  msg.Usage.OutputTokens,
			ContextWindow: contextWindowForUsageMessage(msg, cfg, selected),
			Provisional:   !msg.IsFinished(),
			Summary:       msg.IsSummaryMessage,
		}, true
	}
	return contextUsageSnapshot{}, false
}

func contextWindowForUsageMessage(msg message.Message, cfg *config.Config, selected *agent.Model) int64 {
	if cfg != nil {
		if providerCfg, ok := cfg.Providers.Get(msg.Provider); ok {
			for _, candidate := range providerCfg.Models {
				if candidate.ID == msg.Model {
					return agent.EffectiveContextWindow(candidate.Model)
				}
			}
		}
	}
	if selected != nil {
		return agent.EffectiveContextWindow(selected.CatwalkCfg)
	}
	return 0
}

// estimateStreamedOutput estimates the output token count of the latest
// assistant message's streamed text and reasoning. This mirrors the
// heuristic used by renderThinkingStatusLine: ~4 ASCII bytes per token,
// one token per non-ASCII rune. Used during provisional (streaming)
// display to provide a live, monotonically growing output estimate that
// never exceeds the final API-reported output.
func estimateStreamedOutput(messages []message.Message) int64 {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role != message.Assistant {
			continue
		}
		var asciiBytes, nonASCIIRunes int64
		for _, r := range msg.Content().Text {
			if r < utf8.RuneSelf {
				asciiBytes++
			} else {
				nonASCIIRunes++
			}
		}
		for _, r := range msg.ReasoningContent().Thinking {
			if r < utf8.RuneSelf {
				asciiBytes++
			} else {
				nonASCIIRunes++
			}
		}
		return asciiBytes/4 + nonASCIIRunes
	}
	return 0
}
