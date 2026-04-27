package agent

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/oauth/copilot"
)

// enhancedPromptRegex extracts the enhanced prompt from the LLM response.
// Matches the pattern used by Augment's V0o template (deobfuscated.js:338329).
var enhancedPromptRegex = regexp.MustCompile(`(?i)<augment-enhanced-prompt>([\s\S]*?)</augment-enhanced-prompt>`)

const (
	augmentPromptEnhancerMaxPromptChars  = 100_000
	augmentPromptEnhancerKeepMinMessages = 2
)

// enhancePromptTemplate is the user-message template for prompt enhancement,
// ported from Augment's V0o function (deobfuscated.js:338200) with one
// addition: an explicit instruction to preserve the original language of the
// user's prompt. Without this clause the enhancement model tends to translate
// Chinese (or other non-English) input into English.
const enhancePromptTemplate = "⚠️ NO TOOLS ALLOWED ⚠️\n\n" +
	"Here is an instruction that I'd like to give you, but it needs to be improved. " +
	"Rewrite and enhance this instruction to make it clearer, more specific, less ambiguous, " +
	"and correct any mistakes. Do not use any tools: reply immediately with your answer, even if you're not sure. " +
	"Consider the context of our conversation history when enhancing the prompt. " +
	"If there is code in triple backticks (```) consider whether it is a code sample and should remain unchanged. " +
	"IMPORTANT: Write the enhanced prompt in the SAME natural language as the original instruction below " +
	"(e.g. if the original is in Chinese, the enhanced version must also be in Chinese). " +
	"Do not translate the instruction into another language.\n\n" +
	"Reply with the following format:\n\n" +
	"### BEGIN RESPONSE ###\n" +
	"Here is an enhanced version of the original instruction that is more specific and clear:\n" +
	"<augment-enhanced-prompt>enhanced prompt goes here</augment-enhanced-prompt>\n\n" +
	"### END RESPONSE ###\n\n" +
	"Here is my original instruction:\n\n"

// EnhancePrompt implements [Coordinator.EnhancePrompt].
//
// The request shape intentionally tracks Augment's old method closely:
//   - prefers the configured small model with normal chat semantics (not the background model),
//   - disables tools,
//   - passes sanitized chat history,
//   - uses strict XML extraction only.
func (c *coordinator) EnhancePrompt(ctx context.Context, sessionID, userPrompt string) (string, error) {
	userPrompt = strings.TrimSpace(userPrompt)
	if userPrompt == "" {
		return "", fmt.Errorf("cannot enhance empty prompt")
	}

	model, providerCfg, err := c.enhancePromptModel(ctx)
	if err != nil {
		return "", err
	}

	maxOutputTokens := int64(1024)
	if model.CatwalkCfg.DefaultMaxTokens > 0 && model.CatwalkCfg.DefaultMaxTokens < maxOutputTokens {
		maxOutputTokens = model.CatwalkCfg.DefaultMaxTokens
	}

	historyMessages := c.loadEnhancePromptHistory(ctx, sessionID, model)
	preparedMessages := make([]fantasy.Message, 0, len(historyMessages)+1)
	preparedMessages = append(preparedMessages, historyMessages...)
	preparedMessages = append(preparedMessages, fantasy.NewUserMessage(enhancePromptTemplate+userPrompt))
	preparedMessages = truncateMessagesToFit(preparedMessages, augmentEnhancePromptBudget(model, maxOutputTokens))

	ag := fantasy.NewAgent(
		model.Model,
		fantasy.WithMaxOutputTokens(maxOutputTokens),
		fantasy.WithUserAgent(userAgent),
	)

	var result strings.Builder
	_, err = ag.Stream(
		copilot.ContextWithInitiatorType(ctx, copilot.InitiatorAgent),
		fantasy.AgentStreamCall{
			Messages:        append([]fantasy.Message(nil), preparedMessages...),
			ProviderOptions: getProviderOptions(model, providerCfg),
			PrepareStep: func(callCtx context.Context, options fantasy.PrepareStepFunctionOptions) (_ context.Context, prepared fantasy.PrepareStepResult, err error) {
				callCtx = copilot.ContextWithInitiatorType(callCtx, copilot.InitiatorAgent)
				if options.StepNumber > 0 {
					prepared.Messages = truncateMessagesToFit(options.Messages, augmentEnhancePromptBudget(model, maxOutputTokens))
				}
				return callCtx, prepared, nil
			},
			OnTextDelta: func(_ string, text string) error {
				result.WriteString(text)
				return nil
			},
		},
	)
	if err != nil {
		return "", fmt.Errorf("prompt enhancement failed: %w", err)
	}

	matches := enhancedPromptRegex.FindStringSubmatch(result.String())
	if len(matches) >= 2 {
		if enhanced := strings.TrimSpace(matches[1]); enhanced != "" {
			return enhanced, nil
		}
	}
	slog.Warn("EnhancePrompt: failed to parse enhanced prompt from response",
		"raw_response_len", result.Len())
	return "", fmt.Errorf("failed to parse enhanced prompt from response")
}

func (c *coordinator) enhancePromptModel(ctx context.Context) (Model, config.ProviderConfig, error) {
	model, providerCfg, err := c.selectedModel(ctx, config.SelectedModelTypeSmall, false)
	if err == nil {
		return model, providerCfg, nil
	}
	model, providerCfg, fallbackErr := c.selectedModel(ctx, config.SelectedModelTypeLarge, false)
	if fallbackErr != nil {
		return Model{}, config.ProviderConfig{}, fmt.Errorf("no model available for prompt enhancement: %w", fallbackErr)
	}
	return model, providerCfg, nil
}

func augmentEnhancePromptBudget(model Model, maxOutputTokens int64) int64 {
	budget := EffectiveContextWindow(model.CatwalkCfg)
	if budget <= 0 {
		budget = augmentPromptEnhancerMaxPromptChars / 4
	}
	if maxOutputTokens > 0 && budget > maxOutputTokens {
		budget -= maxOutputTokens
	}
	if floor := int64(augmentPromptEnhancerKeepMinMessages * 16); budget < floor {
		budget = floor
	}
	return budget
}

// loadEnhancePromptHistory returns sanitized fantasy messages for the given
// session. Returns nil when sessionID is empty or loading fails.
func (c *coordinator) loadEnhancePromptHistory(ctx context.Context, sessionID string, model Model) []fantasy.Message {
	if sessionID == "" {
		return nil
	}

	msgs, err := c.messages.List(ctx, sessionID)
	if err != nil {
		slog.Warn("EnhancePrompt: failed to load session history, continuing without context",
			"session_id", sessionID, "err", err)
		return nil
	}

	if sess, sessErr := c.sessions.Get(ctx, sessionID); sessErr == nil && sess.SummaryMessageID != "" {
		for i, m := range msgs {
			if m.ID == sess.SummaryMessageID {
				msgs = msgs[i:]
				break
			}
		}
	}

	history := make([]fantasy.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role != message.User && m.Role != message.Assistant {
			continue
		}
		for _, fm := range m.ToAIMessage() {
			if fm.Role == fantasy.MessageRoleUser || fm.Role == fantasy.MessageRoleAssistant {
				history = append(history, fm)
			}
		}
	}
	if len(history) == 0 {
		return nil
	}

	// Drop tool-call parts from assistant messages: prompt enhancement does not
	// include tool result messages, and orphan tool_calls are rejected by
	// strict OpenAI-compatible providers (HTTP 400).
	history = stripToolCallPartsFromFantasyMessages(history)

	if !model.CatwalkCfg.SupportsImages {
		history = stripImagePartsFromFantasyMessages(history)
	}

	historyBudget := augmentEnhancePromptBudget(model, 0)
	history = truncateMessagesToFit(history, historyBudget)
	if len(history) > 0 && estimatePromptTokens(history, nil) > historyBudget {
		history = history[max(0, len(history)-augmentPromptEnhancerKeepMinMessages):]
	}

	slog.Debug("EnhancePrompt: loaded truncated history",
		"session_id", sessionID,
		"history_messages", len(history),
		"history_tokens", estimatePromptTokens(history, nil),
		"history_budget", historyBudget)
	return history
}
