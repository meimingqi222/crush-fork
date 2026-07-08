package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/oauth/copilot"
)

const (
	recapMaxOutputTokens     = 128
	recapKeepMinMessages     = 4
	recapMaxPromptCharsGuess = 80_000
)

// recapPrompt is the ephemeral user turn sent to the model to generate an idle
// recap. No tool-warning preamble is needed: RecapSession registers no tools
// with the agent and sets ToolChoice to "none" at the call level, so the
// model cannot invoke any tools regardless of what the prompt says.
const recapPrompt = "The user stepped away and is coming back. " +
	"In 1-2 plain sentences (under 40 words total), no markdown, recap: " +
	"(1) the overall goal and current task, and (2) the immediate next action. " +
	"Skip root-cause narrative, fix internals, and secondary to-dos. " +
	"Reply with only the recap sentences — no preamble, no label."

// RecapSession implements [Coordinator.RecapSession].
func (c *coordinator) RecapSession(ctx context.Context, sessionID string) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("recap: session ID is required")
	}

	model, providerCfg, err := c.recapModel(ctx)
	if err != nil {
		return "", err
	}

	history := c.loadRecapHistory(ctx, sessionID, model)
	if len(history) == 0 {
		return "", nil
	}

	messages := append([]fantasy.Message(nil), history...)
	messages = append(messages, fantasy.NewUserMessage(recapPrompt))

	// No tools are registered; ToolChoiceNone additionally suppresses any
	// provider-side tool injection (e.g. from ProviderOptions).
	toolChoiceNone := fantasy.ToolChoiceNone
	ag := fantasy.NewAgent(
		model.Model,
		fantasy.WithMaxOutputTokens(recapMaxOutputTokens),
		fantasy.WithUserAgent(userAgent),
		fantasy.WithToolChoice(toolChoiceNone),
	)

	var result strings.Builder
	_, err = ag.Stream(
		copilot.ContextWithInitiatorType(ctx, copilot.InitiatorAgent),
		fantasy.AgentStreamCall{
			Messages:        messages,
			ProviderOptions: getProviderOptions(model, providerCfg),
			ToolChoice:      &toolChoiceNone,
			OnTextDelta: func(_ string, text string) error {
				result.WriteString(text)
				return nil
			},
		},
	)
	if err != nil {
		return "", fmt.Errorf("recap: LLM call failed: %w", err)
	}

	recap := strings.TrimSpace(result.String())
	return recap, nil
}

// recapModel returns the model to use for recap generation, preferring the
// small model and falling back to the large model.
func (c *coordinator) recapModel(ctx context.Context) (Model, config.ProviderConfig, error) {
	model, providerCfg, err := c.selectedModel(ctx, config.SelectedModelTypeSmall, false)
	if err == nil {
		return model, providerCfg, nil
	}
	model, providerCfg, fallbackErr := c.selectedModel(ctx, config.SelectedModelTypeLarge, false)
	if fallbackErr != nil {
		return Model{}, config.ProviderConfig{}, fmt.Errorf("recap: no model available: %w", fallbackErr)
	}
	return model, providerCfg, nil
}

// loadRecapHistory returns sanitized fantasy messages for recap. It trims the
// history to start from the summary boundary (if any) and drops tool-call
// parts to keep the context clean for providers that reject orphan tool_calls.
func (c *coordinator) loadRecapHistory(ctx context.Context, sessionID string, model Model) []fantasy.Message {
	msgs, err := c.messages.List(ctx, sessionID)
	if err != nil {
		slog.Warn("Recap: failed to load session history", "session_id", sessionID, "err", err)
		return nil
	}

	// Start from the summary boundary when available.
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

	// Remove tool-call parts so that providers that reject orphan tool_calls
	// (strict OpenAI-compat) do not return HTTP 400.
	history = stripToolCallPartsFromFantasyMessages(history)

	if !model.CatwalkCfg.SupportsImages {
		history = stripImagePartsFromFantasyMessages(history)
	}

	// Budget: leave room for the recap prompt itself and the output.
	// estimatePromptTokens is used for consistency with truncateMessagesToFit.
	recapPromptTokens := estimatePromptTokens(
		[]fantasy.Message{fantasy.NewUserMessage(recapPrompt)}, nil,
	)
	budget := EffectiveContextWindow(model.CatwalkCfg)
	if budget <= 0 {
		// Fallback: pessimistic char/4 estimate, same convention as EnhancePrompt.
		budget = recapMaxPromptCharsGuess / 4
	}
	budget -= recapMaxOutputTokens + recapPromptTokens

	history = truncateMessagesToFit(history, budget)
	if len(history) > 0 && estimatePromptTokens(history, nil) > budget {
		history = history[max(0, len(history)-recapKeepMinMessages):]
	}

	slog.Debug("Recap: loaded truncated history",
		"session_id", sessionID,
		"history_messages", len(history),
		"history_tokens", estimatePromptTokens(history, nil),
		"history_budget", budget)
	return history
}
