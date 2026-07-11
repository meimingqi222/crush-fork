package agent

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"unicode/utf8"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openrouter"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/oauth/copilot"
)

// titleMaxOutputTokens is the max output token budget for session title
// generation. All models use this single value to avoid issues with thinking
// tokens, non-English titles, model-specific prefixes, and models that lack
// metadata to signal their capabilities.
const titleMaxOutputTokens int64 = 1200

func shouldGenerateSessionTitle(title string) bool {
	title = strings.TrimSpace(title)
	if title == "" {
		return true
	}
	if strings.EqualFold(title, "New Session") {
		return true
	}
	return strings.EqualFold(title, DefaultSessionName)
}

func titlePromptFromCallOrHistory(prompt string, history []message.Message) string {
	if titlePrompt := titleUserPromptFromCall(prompt); titlePrompt != "" {
		return titlePrompt
	}
	for i := len(history) - 1; i >= 0; i-- {
		msg := history[i]
		if msg.Role != message.User {
			continue
		}
		if titlePrompt := titleUserPromptFromCall(msg.Content().Text); titlePrompt != "" {
			return titlePrompt
		}
	}
	return ""
}

// generateTitle generates a session titled based on the initial prompt.
func titleUserPromptFromCall(prompt string) string {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return ""
	}
	for _, prefix := range []string{autoResumePromptPrefix, contextWindowResumePromptPrefix} {
		if !strings.HasPrefix(prompt, prefix) {
			continue
		}
		trimmed := strings.TrimPrefix(prompt, prefix)
		if end := strings.LastIndex(trimmed, "`"); end >= 0 {
			trimmed = trimmed[:end]
		}
		return strings.TrimSpace(trimmed)
	}
	return prompt
}

func cleanGeneratedTitle(raw string) string {
	raw = thinkTagRegex.ReplaceAllString(raw, "")
	lines := strings.Split(raw, "\n")
	title := ""
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		title = line
		break
	}
	if title == "" {
		title = strings.TrimSpace(raw)
	}
	title = strings.NewReplacer(`"`, "", `'`, "", ":", "").Replace(title)
	title = titleWhitespaceRegex.ReplaceAllString(strings.TrimSpace(title), " ")
	title = strings.Trim(title, " -\t\r\n")
	if utf8.RuneCountInString(title) > 50 {
		runes := []rune(title)
		title = strings.TrimSpace(string(runes[:50]))
	}
	return cmp.Or(title, DefaultSessionName)
}

func (a *sessionAgent) generateTitle(ctx context.Context, sessionID string, userPrompt string, sessionLock *sync.Mutex) {
	userPrompt = titleUserPromptFromCall(userPrompt)
	if userPrompt == "" {
		return
	}

	smallModel := a.smallModel.Get()
	largeModel := a.largeModel.Get()
	systemPromptPrefix := a.systemPromptPrefix.Get()

	newAgent := func(m fantasy.LanguageModel, p []byte) fantasy.Agent {
		return fantasy.NewAgent(m,
			fantasy.WithSystemPrompt(string(p)+"\n/no_think"),
			fantasy.WithMaxOutputTokens(titleMaxOutputTokens),
			fantasy.WithUserAgent(userAgent),
		)
	}

	var streamedTitle strings.Builder
	streamCall := fantasy.AgentStreamCall{
		Prompt: userPrompt,
		PrepareStep: func(callCtx context.Context, opts fantasy.PrepareStepFunctionOptions) (_ context.Context, prepared fantasy.PrepareStepResult, err error) {
			// Title generation is always agent-initiated (never billable).
			callCtx = copilot.ContextWithInitiatorType(callCtx, copilot.InitiatorAgent)
			prepared.Messages = opts.Messages
			if systemPromptPrefix != "" {
				prepared.Messages = append([]fantasy.Message{
					fantasy.NewSystemMessage(systemPromptPrefix),
				}, prepared.Messages...)
			}
			return callCtx, prepared, nil
		},
		OnTextDelta: func(_ string, text string) error {
			streamedTitle.WriteString(text)
			return nil
		},
	}

	// Use the small model to generate the title.
	model := smallModel
	agent := newAgent(model.Model, titlePrompt)
	titleCtx := copilot.ContextWithInitiatorType(ctx, copilot.InitiatorAgent)
	resp, err := agent.Stream(titleCtx, streamCall)
	if err == nil {
		slog.Debug("Generated title with small model")
	} else {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			slog.Debug("Title generation cancelled (small model)", "err", err)
		} else {
			slog.Warn("Error generating title with small model; trying big model", "err", err)
		}
		model = largeModel
		agent = newAgent(model.Model, titlePrompt)
		streamedTitle.Reset()
		resp, err = agent.Stream(titleCtx, streamCall)
		if err == nil {
			slog.Debug("Generated title with large model")
		} else {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				slog.Debug("Title generation cancelled (large model)", "err", err)
			} else {
				slog.Warn("Error generating title with large model", "err", err)
			}
			if sessionLock != nil {
				sessionLock.Lock()
				defer sessionLock.Unlock()
			}
			saveErr := a.sessions.Rename(context.Background(), sessionID, DefaultSessionName)
			if saveErr != nil {
				slog.Warn("Failed to save session title", "error", saveErr)
			}
			return
		}
	}

	if resp == nil {
		slog.Error("Response is nil; can't generate title")
		if sessionLock != nil {
			sessionLock.Lock()
			defer sessionLock.Unlock()
		}
		saveErr := a.sessions.Rename(context.Background(), sessionID, DefaultSessionName)
		if saveErr != nil {
			slog.Warn("Failed to save session title", "error", saveErr)
		}
		return
	}

	// Clean up title.
	title := streamedTitle.String()
	if strings.TrimSpace(title) == "" {
		title = resp.Response.Content.Text()
	}
	title = cleanGeneratedTitle(title)

	// Calculate usage and cost.
	var openrouterCost *float64
	for _, step := range resp.Steps {
		stepCost := a.openrouterCost(step.ProviderMetadata)
		if stepCost != nil {
			newCost := *stepCost
			if openrouterCost != nil {
				newCost += *openrouterCost
			}
			openrouterCost = &newCost
		}
	}

	modelConfig := model.CatwalkCfg
	// CostPer1MInCached is the rate for cache reads, CostPer1MOutCached is the
	// rate for cache writes (creation). They are not named after the token
	// direction but after the cache operation: read-in vs write-out.
	cost := modelConfig.CostPer1MInCached/1e6*float64(resp.TotalUsage.CacheReadTokens) +
		modelConfig.CostPer1MOutCached/1e6*float64(resp.TotalUsage.CacheCreationTokens) +
		modelConfig.CostPer1MIn/1e6*float64(resp.TotalUsage.InputTokens) +
		modelConfig.CostPer1MOut/1e6*float64(resp.TotalUsage.OutputTokens)

	// Use override cost if available (e.g., from OpenRouter).
	if openrouterCost != nil {
		cost = *openrouterCost
	}

	// Use OutputTokens only (not OutputTokens + ReasoningTokens) to avoid
	// double-counting reasoning tokens for OpenAI-style providers where
	// OutputTokens already includes ReasoningTokens.
	completionTokens := resp.TotalUsage.OutputTokens

	// Atomically update only title and usage fields to avoid overriding other
	// concurrent session updates. Title generation is an auxiliary call: add
	// its output tokens and cost, but do not touch prompt_tokens. That field
	// tracks the latest main-conversation context length, and the SQL path
	// increments prompt_tokens rather than setting an absolute value.
	if sessionLock != nil {
		sessionLock.Lock()
		defer sessionLock.Unlock()
	}
	saveErr := a.sessions.UpdateTitleAndUsage(context.Background(), sessionID, title, 0, completionTokens, cost)
	if saveErr != nil {
		slog.Warn("Failed to save session title and usage", "error", saveErr)
		return
	}
}

func (a *sessionAgent) openrouterCost(metadata fantasy.ProviderMetadata) *float64 {
	openrouterMetadata, ok := metadata[openrouter.Name]
	if !ok {
		return nil
	}

	opts, ok := openrouterMetadata.(*openrouter.ProviderMetadata)
	if !ok {
		return nil
	}
	return &opts.Usage.Cost
}
