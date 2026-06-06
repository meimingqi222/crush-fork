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
			usage.TotalTokens = max(usage.TotalTokens, sess.LastTotalTokens())
		} else {
			usage.OutputTokens = sess.CompletionTokens
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
					return agent.EffectiveContextWindow(candidate)
				}
			}
		}
	}
	if selected != nil {
		return agent.EffectiveContextWindow(selected.CatwalkCfg)
	}
	return 0
}
