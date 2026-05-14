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

func resolveContextUsageSnapshot(sess *session.Session, messages []message.Message, cfg *config.Config, selected *agent.Model) contextUsageSnapshot {
	if sess == nil {
		return contextUsageSnapshot{}
	}

	if usage, ok := latestAssistantUsageSnapshot(messages, cfg, selected); ok {
		// Provisional snapshots (streaming in progress) may have incomplete
		// token counts from the provider. Floor to the session's last
		// confirmed totals so the display never drops below known history.
		if usage.Provisional && sess != nil {
			usage.TotalTokens = max(usage.TotalTokens, sess.LastTotalTokens())
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
		OutputTokens:  sess.LastOutputTokens(),
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
