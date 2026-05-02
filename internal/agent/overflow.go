package agent

import "log/slog"

type promptTokenBudget struct {
	ContextWindow          int64
	InputLimit             int64
	MaxOutputTokens        int64
	ReservedInputTokens    int64
	UsableInputTokens      int64
	UsesExplicitInputLimit bool
}

func effectivePromptInputLimit(model Model) (int64, bool) {
	limits := ContextWindowLimitsFor(model.CatwalkCfg)
	if limits.MaxPromptTokens <= 0 {
		return 0, false
	}
	return limits.EffectiveContextWindow, true
}

func effectiveAutoSummarizeMaxOutputTokens(model Model, maxOutputTokens int64) int64 {
	if maxOutputTokens > 0 {
		return maxOutputTokens
	}
	if model.CatwalkCfg.DefaultMaxTokens > 0 {
		return int64(model.CatwalkCfg.DefaultMaxTokens)
	}
	return 0
}

func promptTokenBudgetForModel(model Model, maxOutputTokens int64) promptTokenBudget {
	contextWindow := int64(model.CatwalkCfg.ContextWindow)
	maxOutputTokens = effectiveAutoSummarizeMaxOutputTokens(model, maxOutputTokens)
	reserved := autoSummarizeReservedTokens(maxOutputTokens)

	if inputLimit, ok := effectivePromptInputLimit(model); ok {
		return promptTokenBudget{
			ContextWindow:          contextWindow,
			InputLimit:             inputLimit,
			MaxOutputTokens:        maxOutputTokens,
			ReservedInputTokens:    reserved,
			UsableInputTokens:      inputLimit - reserved,
			UsesExplicitInputLimit: true,
		}
	}

	effectiveWindow := EffectiveContextWindow(model.CatwalkCfg)
	return promptTokenBudget{
		ContextWindow:     effectiveWindow,
		MaxOutputTokens:   maxOutputTokens,
		UsableInputTokens: effectiveWindow - maxOutputTokens,
	}
}

func shouldAutoSummarize(model Model, contextUsed, maxOutputTokens int64) bool {
	budget := promptTokenBudgetForModel(model, maxOutputTokens)
	if budget.UsableInputTokens <= 0 {
		slog.Warn("ShouldAutoSummarize: usable input budget <= 0", "context_window", budget.ContextWindow, "input_limit", budget.InputLimit, "max_output_tokens", budget.MaxOutputTokens)
		return budget.ContextWindow > 0 || budget.InputLimit > 0
	}

	shouldSummarize := contextUsed >= budget.UsableInputTokens
	slog.Info("ShouldAutoSummarize calculation",
		"context_used", contextUsed,
		"context_window", budget.ContextWindow,
		"input_limit", budget.InputLimit,
		"max_output_tokens", budget.MaxOutputTokens,
		"reserved_input_tokens", budget.ReservedInputTokens,
		"usable_input_tokens", budget.UsableInputTokens,
		"uses_explicit_input_limit", budget.UsesExplicitInputLimit,
		"should_summarize", shouldSummarize,
	)
	return shouldSummarize
}
