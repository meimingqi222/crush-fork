package agent

import (
	"context"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/plugin"
)

func (t sessionCompactionTrigger) Purpose() plugin.ChatTransformPurpose {
	switch t {
	case sessionCompactionTriggerRecover:
		return plugin.ChatTransformPurposeRecover
	case sessionCompactionTriggerProactive:
		return plugin.ChatTransformPurposeProactiveCompact
	default:
		return plugin.ChatTransformPurposeSummarize
	}
}

func (a *sessionAgent) proactiveCompactionTrigger(model Model, contextUsed, maxOutputTokens int64, sessionID string) sessionCompactionTrigger {
	if a.shouldAutoSummarizeWithCooldown(model, contextUsed, maxOutputTokens, sessionID) {
		return sessionCompactionTriggerProactive
	}
	return sessionCompactionTriggerNone
}

func shouldReactiveCompactMessages(purpose plugin.ChatTransformPurpose) bool {
	switch purpose {
	case plugin.ChatTransformPurposeRecover,
		plugin.ChatTransformPurposeProactiveCompact:
		return true
	default:
		return false
	}
}

func shouldAutoCompactMessages(purpose plugin.ChatTransformPurpose, msgs []message.Message) bool {
	if len(msgs) < 3 {
		return false
	}
	switch purpose {
	case plugin.ChatTransformPurposeSummarize,
		plugin.ChatTransformPurposeRecover,
		plugin.ChatTransformPurposeProactiveCompact:
		return true
	default:
		return false
	}
}

func (a *sessionAgent) reactiveCompactSessionMessages(ctx context.Context, sessionID string, model Model, providerCtx plugin.ProviderContext, msgs []message.Message) ([]message.Message, error) {
	return a.transformSessionMessages(ctx, chatRequestStateInput{
		SessionID:             sessionID,
		Agent:                 "session",
		Model:                 model,
		Provider:              providerCtx,
		Purpose:               plugin.ChatTransformPurposeReactiveCompact,
		Messages:              msgs,
		Message:               message.Message{SessionID: sessionID, Role: message.User},
		EstimatedPromptTokens: a.estimatePromptForMessages(msgs),
	})
}

func (a *sessionAgent) autoCompactSessionMessages(ctx context.Context, sessionID string, model Model, providerCtx plugin.ProviderContext, msgs []message.Message) ([]message.Message, error) {
	result, err := a.transformSessionMessages(ctx, chatRequestStateInput{
		SessionID:             sessionID,
		Agent:                 "session",
		Model:                 model,
		Provider:              providerCtx,
		Purpose:               plugin.ChatTransformPurposeAutoCompact,
		Messages:              msgs,
		Message:               message.Message{SessionID: sessionID, Role: message.User},
		EstimatedPromptTokens: a.estimatePromptForMessages(msgs),
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (a *sessionAgent) postCompactSessionMessages(ctx context.Context, sessionID string, model Model, providerCtx plugin.ProviderContext, msgs []message.Message) ([]message.Message, error) {
	return a.transformSessionMessages(ctx, chatRequestStateInput{
		SessionID:             sessionID,
		Agent:                 "session",
		Model:                 model,
		Provider:              providerCtx,
		Purpose:               plugin.ChatTransformPurposePostCompact,
		Messages:              msgs,
		Message:               message.Message{SessionID: sessionID, Role: message.User},
		EstimatedPromptTokens: a.estimatePromptForMessages(msgs),
	})
}
