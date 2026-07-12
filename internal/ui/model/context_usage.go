package model

import (
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
			usage.OutputTokens = max(usage.OutputTokens, sess.CompletionTokens)
			// During streaming the current exchange has not been committed to
			// session totals yet. Use the live provisional usage but floor it
			// at the last known exchange total so the display never drops
			// below the confirmed context length.
			usage.TotalTokens = max(usage.TotalTokens, sess.LastTotalTokens())
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
