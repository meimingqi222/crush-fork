package agent

import (
	"cmp"
	"context"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/stringext"
)

func applyRuntimeConfig(call *SessionAgentCall, runtimeConfig sessionAgentRuntimeConfig) {
	if runtimeConfig.ProviderOptions != nil {
		call.ProviderOptions = runtimeConfig.ProviderOptions
	}
	if runtimeConfig.MaxOutputTokens > 0 {
		call.MaxOutputTokens = runtimeConfig.MaxOutputTokens
	}
	if runtimeConfig.Temperature != nil {
		call.Temperature = runtimeConfig.Temperature
	}
	if runtimeConfig.TopP != nil {
		call.TopP = runtimeConfig.TopP
	}
	if runtimeConfig.TopK != nil {
		call.TopK = runtimeConfig.TopK
	}
	if runtimeConfig.FrequencyPenalty != nil {
		call.FrequencyPenalty = runtimeConfig.FrequencyPenalty
	}
	if runtimeConfig.PresencePenalty != nil {
		call.PresencePenalty = runtimeConfig.PresencePenalty
	}
}

func (a *sessionAgent) refreshCallConfigIfNeeded(ctx context.Context, call *SessionAgentCall) (*sessionAgentRuntimeConfig, error) {
	if runtimeConfig, ok := ctx.Value(sessionAgentRuntimeConfigContextKey{}).(*sessionAgentRuntimeConfig); ok && runtimeConfig != nil {
		applyRuntimeConfig(call, *runtimeConfig)
		return runtimeConfig, nil
	}
	if runtimeConfig, ok := ctx.Value(sessionAgentRuntimeConfigContextKey{}).(sessionAgentRuntimeConfig); ok {
		applyRuntimeConfig(call, runtimeConfig)
		return &runtimeConfig, nil
	}
	if a.refreshCallConfig == nil {
		return nil, nil
	}
	runtimeConfig, err := a.refreshCallConfig(ctx)
	if err != nil {
		return nil, err
	}
	applyRuntimeConfig(call, runtimeConfig)
	return &runtimeConfig, nil
}

func (a *sessionAgent) resetRetriedStep(ctx context.Context, assistant *message.Message, toolMessageIDs []string) error {
	for _, toolMessageID := range toolMessageIDs {
		if err := a.messages.Delete(ctx, toolMessageID); err != nil {
			return err
		}
	}
	assistant.Parts = nil
	return a.messages.Update(ctx, *assistant)
}

func (a *sessionAgent) SetModels(large Model, small Model) {
	a.largeModel.Set(large)
	a.smallModel.Set(small)
}

func (a *sessionAgent) SetTools(tools []fantasy.AgentTool) {
	a.tools.SetSlice(tools)
}

func (a *sessionAgent) SetSystemPrompt(systemPrompt string) {
	a.systemPrompt.Set(systemPrompt)
}

func (a *sessionAgent) SetSystemPromptPrefix(systemPromptPrefix string) {
	a.systemPromptPrefix.Set(systemPromptPrefix)
}

func (a *sessionAgent) Model() Model {
	return a.largeModel.Get()
}

func withRetryFailureDetails(details string, retryAttempt int) string {
	details = strings.TrimSpace(details)
	if retryAttempt <= 0 {
		return details
	}

	summary := fmt.Sprintf("Retried %d %s, but the request still failed.", retryAttempt, cmp.Or(pluralizeRetryAttempt(retryAttempt), "times"))
	if details == "" {
		return summary
	}
	return summary + " " + details
}

func pluralizeRetryAttempt(retryAttempt int) string {
	if retryAttempt == 1 {
		return "time"
	}
	return "times"
}

// providerErrorTitle returns a human-readable title for a provider error.
// It overrides the HTTP status text when the message indicates an upstream
// aggregator failure (no available route / all accounts exhausted) rather
// than a genuine client-side rate limit, so the displayed title matches the
// actual failure reason instead of the misleading "Too Many Requests".
func providerErrorTitle(providerErr *fantasy.ProviderError) string {
	if providerErr != nil && providerErr.Title != "" {
		msg := strings.ToLower(providerErr.Message)
		for _, pattern := range noRoutePatterns {
			if strings.Contains(msg, pattern) {
				return "No available route"
			}
		}
		return stringext.Capitalize(providerErr.Title)
	}
	return ""
}

// noRoutePatterns are message fragments emitted by upstream aggregators
// (e.g. copilot-api, openrouter) when they have no account/route available
// to serve a request. These are typically returned with HTTP 429 even
// though the failure is server-side, not a client rate limit.
var noRoutePatterns = []string{
	"no available route",
	"all accounts exhausted",
	"no available model",
	"no upstream available",
}
