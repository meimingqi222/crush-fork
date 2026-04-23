package agent

import (
	"encoding/json"

	"charm.land/catwalk/pkg/catwalk"
)

// ContextWindowLimits describes the raw and effective context budget for a model.
// EffectiveContextWindow is what UI / auto-compaction / plugin thresholds should all use.
type ContextWindowLimits struct {
	ContextWindow          int64 // raw model context_window
	MaxPromptTokens        int64 // provider option max_prompt_tokens, 0 if unset
	EffectiveContextWindow int64 // min(ContextWindow, MaxPromptTokens) with 0-fallbacks
}

// ContextWindowLimitsFor computes limits from a catwalk.Model (UI passes
// CatwalkCfg directly; agent passes Model.CatwalkCfg).
func ContextWindowLimitsFor(m catwalk.Model) ContextWindowLimits {
	window := int64(m.ContextWindow)
	var maxPrompt int64
	if m.Options.ProviderOptions != nil {
		if v, ok := m.Options.ProviderOptions["max_prompt_tokens"]; ok {
			if n, ok := contextWindowInt64(v); ok && n > 0 {
				maxPrompt = n
			}
		}
	}
	effective := window
	if maxPrompt > 0 {
		if window <= 0 {
			effective = maxPrompt
		} else if maxPrompt < window {
			effective = maxPrompt
		}
	}
	return ContextWindowLimits{
		ContextWindow:          window,
		MaxPromptTokens:        maxPrompt,
		EffectiveContextWindow: effective,
	}
}

// EffectiveContextWindow is a convenience wrapper.
func EffectiveContextWindow(m catwalk.Model) int64 {
	return ContextWindowLimitsFor(m).EffectiveContextWindow
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
		if parsed, err := v.Int64(); err == nil {
			return parsed, true
		}
		if f, ferr := v.Float64(); ferr == nil {
			return int64(f), true
		}
		return 0, false
	default:
		return 0, false
	}
}
