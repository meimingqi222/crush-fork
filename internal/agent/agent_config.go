package agent

import (
	"cmp"
	"context"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/message"
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
