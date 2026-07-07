package agent

import (
	"log/slog"
	"os"
	"strconv"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/message"
)

// contextUsageDiagEnabled reports whether per-step context usage diagnostics
// should be logged at INFO level. Set CRUSH_CONTEXT_USAGE_DIAG=1 to enable
// without turning on full debug logging.
func contextUsageDiagEnabled() bool {
	v, _ := strconv.ParseBool(os.Getenv("CRUSH_CONTEXT_USAGE_DIAG"))
	return v
}

type contextUsageDiagnosticInput struct {
	SessionID             string
	Model                 Model
	ProviderUsage         fantasy.Usage
	NormalizedUsage       message.Usage
	EstimatedPromptTokens int64
	UsageEstimated        bool
	PreparedMessageCount  int
}

// logContextUsageDiagnostic emits a single INFO log line per completed step
// when CRUSH_CONTEXT_USAGE_DIAG=1. It compares provider-reported usage,
// Crush normalization, local estimates, and the number shown in the TUI so
// context growth can be diagnosed without copilot-api access.
func logContextUsageDiagnostic(in contextUsageDiagnosticInput) {
	if !contextUsageDiagEnabled() {
		return
	}

	providerPrompt := in.ProviderUsage.InputTokens +
		in.ProviderUsage.CacheReadTokens +
		in.ProviderUsage.CacheCreationTokens
	displayTotal := in.NormalizedUsage.PromptTokens() + in.NormalizedUsage.OutputTokens

	contextWindow := effectiveContextWindow(in.Model)
	var contextPercent int64
	if contextWindow > 0 {
		contextPercent = displayTotal * 100 / contextWindow
	}

	source := "provider"
	if in.UsageEstimated {
		source = "fallback_estimate"
	} else if shouldFloorPromptTokensToEstimate(
		in.ProviderUsage,
		usageProvider(in.Model),
		providerPrompt,
		in.EstimatedPromptTokens,
	) {
		source = "estimate_floor"
	}

	slog.Info("Context usage diagnostic",
		"session_id", in.SessionID,
		"model", in.Model.ModelCfg.Model,
		"provider", in.Model.ModelCfg.Provider,
		"usage_provider", usageProvider(in.Model),
		"prepared_messages", in.PreparedMessageCount,
		"provider_input_tokens", in.ProviderUsage.InputTokens,
		"provider_output_tokens", in.ProviderUsage.OutputTokens,
		"provider_reasoning_tokens", in.ProviderUsage.ReasoningTokens,
		"provider_cache_read_tokens", in.ProviderUsage.CacheReadTokens,
		"provider_cache_creation_tokens", in.ProviderUsage.CacheCreationTokens,
		"provider_prompt_tokens", providerPrompt,
		"estimated_prompt_tokens", in.EstimatedPromptTokens,
		"usage_estimated", in.UsageEstimated,
		"normalized_input_tokens", in.NormalizedUsage.InputTokens,
		"normalized_cache_read_tokens", in.NormalizedUsage.CacheReadTokens,
		"normalized_cache_write_tokens", in.NormalizedUsage.CacheWriteTokens,
		"normalized_output_tokens", in.NormalizedUsage.OutputTokens,
		"normalized_reasoning_tokens", in.NormalizedUsage.ReasoningTokens,
		"display_total_tokens", displayTotal,
		"context_window", contextWindow,
		"context_percent", contextPercent,
		"usage_source", source,
		"provider_vs_display_delta", displayTotal-providerPrompt-in.NormalizedUsage.OutputTokens,
		"estimate_vs_provider_delta", in.EstimatedPromptTokens-providerPrompt,
	)
}
