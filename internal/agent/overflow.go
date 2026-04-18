package agent

import "log/slog"

type promptTokenBudget struct {
	ContextWindow          int64
	InputLimit             int64
	MaxOutputTokens        int64
	ReservedInputTokens    int64
	ToolReserveTokens      int64
	SafetyReserveTokens    int64
	UsableInputTokens      int64
	UsesExplicitInputLimit bool
}

const (
	autoSummarizeToolReserveMax   int64 = 8_000
	autoSummarizeToolReserveMin   int64 = 2_000
	autoSummarizeSafetyReserveMin int64 = 2_000
)

func effectivePromptInputLimit(model Model) (int64, bool) {
	window := int64(model.CatwalkCfg.ContextWindow)
	options := model.CatwalkCfg.Options.ProviderOptions
	if options == nil {
		return 0, false
	}
	value, ok := options["max_prompt_tokens"]
	if !ok {
		return 0, false
	}
	maxPromptTokens, ok := int64ProviderOptionValue(value)
	if !ok || maxPromptTokens <= 0 {
		return 0, false
	}
	if window <= 0 {
		return maxPromptTokens, true
	}
	return min(window, maxPromptTokens), true
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

func autoSummarizeToolReserveTokens(contextWindow int64) int64 {
	if contextWindow <= 0 {
		return 0
	}
	return min(autoSummarizeToolReserveMax, max(autoSummarizeToolReserveMin, contextWindow/10))
}

func autoSummarizeSafetyReserveTokens(contextWindow int64) int64 {
	if contextWindow <= 0 {
		return 0
	}
	return max(autoSummarizeSafetyReserveMin, contextWindow/50)
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

	toolReserve := autoSummarizeToolReserveTokens(contextWindow)
	safetyReserve := autoSummarizeSafetyReserveTokens(contextWindow)
	return promptTokenBudget{
		ContextWindow:       contextWindow,
		MaxOutputTokens:     maxOutputTokens,
		ReservedInputTokens: reserved,
		ToolReserveTokens:   toolReserve,
		SafetyReserveTokens: safetyReserve,
		UsableInputTokens:   contextWindow - reserved - toolReserve - safetyReserve,
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
		"tool_reserve_tokens", budget.ToolReserveTokens,
		"safety_reserve_tokens", budget.SafetyReserveTokens,
		"usable_input_tokens", budget.UsableInputTokens,
		"uses_explicit_input_limit", budget.UsesExplicitInputLimit,
		"should_summarize", shouldSummarize,
	)
	return shouldSummarize
}
