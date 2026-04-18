package model

import (
	"encoding/json"

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
		if usage.Provisional {
			if summary, ok := latestFinishedSummaryUsageSnapshot(messages, cfg, selected); ok && summary.TotalTokens > usage.TotalTokens {
				return applySessionUsageFloor(summary, sess)
			}
		}
		return applySessionUsageFloor(usage, sess)
	}

	contextWindow := int64(0)
	if selected != nil {
		contextWindow = displayContextWindow(*selected)
		if contextWindow <= 0 {
			contextWindow = selected.CatwalkCfg.ContextWindow
		}
	}

	return applySessionUsageFloor(contextUsageSnapshot{
		TotalTokens:   sess.LastTotalTokens(),
		OutputTokens:  sess.LastOutputTokens(),
		ContextWindow: contextWindow,
	}, sess)
}

func latestAssistantUsageSnapshot(messages []message.Message, cfg *config.Config, selected *agent.Model) (contextUsageSnapshot, bool) {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role != message.Assistant {
			continue
		}
		total := msg.Usage.TotalTokens()
		if total <= 0 {
			continue
		}
		return contextUsageSnapshot{
			TotalTokens:   total,
			OutputTokens:  msg.Usage.CompletionTokens(),
			ContextWindow: contextWindowForUsageMessage(msg, cfg, selected),
			Provisional:   !msg.IsFinished(),
			Summary:       msg.IsSummaryMessage,
		}, true
	}
	return contextUsageSnapshot{}, false
}

func latestFinishedSummaryUsageSnapshot(messages []message.Message, cfg *config.Config, selected *agent.Model) (contextUsageSnapshot, bool) {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role != message.Assistant || !msg.IsSummaryMessage || !msg.IsFinished() {
			continue
		}
		total := msg.Usage.TotalTokens()
		if total <= 0 {
			continue
		}
		return contextUsageSnapshot{
			TotalTokens:   total,
			OutputTokens:  msg.Usage.CompletionTokens(),
			ContextWindow: contextWindowForUsageMessage(msg, cfg, selected),
			Summary:       true,
		}, true
	}
	return contextUsageSnapshot{}, false
}

func contextWindowForUsageMessage(msg message.Message, cfg *config.Config, selected *agent.Model) int64 {
	if selected != nil &&
		selected.ModelCfg.Provider == msg.Provider &&
		selected.ModelCfg.Model == msg.Model {
		return displayContextWindow(*selected)
	}
	if cfg == nil {
		return 0
	}
	providerCfg, ok := cfg.Providers.Get(msg.Provider)
	if !ok {
		return 0
	}
	for _, candidate := range providerCfg.Models {
		if candidate.ID != msg.Model {
			continue
		}
		return displayContextWindow(agent.Model{CatwalkCfg: candidate})
	}
	return 0
}

func applySessionUsageFloor(snapshot contextUsageSnapshot, sess *session.Session) contextUsageSnapshot {
	if sess == nil || snapshot.Provisional {
		return snapshot
	}
	snapshot.TotalTokens = max(snapshot.TotalTokens, sess.LastTotalTokens())
	snapshot.OutputTokens = max(snapshot.OutputTokens, sess.LastOutputTokens())
	return snapshot
}

func displayContextWindow(model agent.Model) int64 {
	window := model.CatwalkCfg.ContextWindow
	if window <= 0 {
		options := model.CatwalkCfg.Options.ProviderOptions
		if options == nil {
			return 0
		}
		value, ok := options["max_prompt_tokens"]
		if !ok {
			return 0
		}
		maxPromptTokens, ok := contextWindowInt64(value)
		if !ok || maxPromptTokens <= 0 {
			return 0
		}
		return maxPromptTokens
	}
	return window
}

func contextWindowInt64(value any) (int64, bool) {
	switch v := value.(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case int32:
		return int64(v), true
	case float64:
		return int64(v), true
	case float32:
		return int64(v), true
	case json.Number:
		parsed, err := v.Int64()
		if err == nil {
			return parsed, true
		}
		f, ferr := v.Float64()
		if ferr != nil {
			return 0, false
		}
		return int64(f), true
	default:
		return 0, false
	}
}
