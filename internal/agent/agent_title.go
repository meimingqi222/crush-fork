package agent

import (
	"cmp"
	"context"
	"log/slog"
	"strings"
	"sync"
	"unicode/utf8"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openrouter"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/oauth/copilot"
)

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

	// Thinking-capable models require a larger token budget because thinking
	// tokens count against max_tokens. 40 tokens is too small for a model that
	// reasons: the thinking content fills the budget, leaving nothing for the
	// visible title text. Use 1200 when the model can reason (1000 for thinking
	// + ~200 for the title), unless thinking is explicitly disabled by config.
	titleMaxOutputTokens := func(m Model) int64 {
		if m.CatwalkCfg.CanReason {
			thinkingDisabled := m.ModelCfg.Think != nil && !*m.ModelCfg.Think
			if !thinkingDisabled {
				return 1200
			}
		}
		return 40
	}

	newAgent := func(m fantasy.LanguageModel, p []byte, tok int64) fantasy.Agent {
		return fantasy.NewAgent(m,
			fantasy.WithSystemPrompt(string(p)+"\n/no_think"),
			fantasy.WithMaxOutputTokens(tok),
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
	agent := newAgent(model.Model, titlePrompt, titleMaxOutputTokens(model))
	titleCtx := copilot.ContextWithInitiatorType(ctx, copilot.InitiatorAgent)
	resp, err := agent.Stream(titleCtx, streamCall)
	if err == nil {
		// We successfully generated a title with the small model.
		slog.Debug("Generated title with small model")
	} else {
		// It didn't work. Let's try with the big model.
		slog.Error("Error generating title with small model; trying big model", "err", err)
		model = largeModel
		agent = newAgent(model.Model, titlePrompt, titleMaxOutputTokens(model))
		streamedTitle.Reset()
		resp, err = agent.Stream(titleCtx, streamCall)
		if err == nil {
			slog.Debug("Generated title with large model")
		} else {
			// Welp, the large model didn't work either. Use the default
			// session name and return.
			slog.Error("Error generating title with large model", "err", err)
			if sessionLock != nil {
				sessionLock.Lock()
				defer sessionLock.Unlock()
			}
			saveErr := a.sessions.Rename(ctx, sessionID, DefaultSessionName)
			if saveErr != nil {
				slog.Error("Failed to save session title", "error", saveErr)
			}
			return
		}
	}

	if resp == nil {
		// Actually, we didn't get a response so we can't. Use the default
		// session name and return.
		slog.Error("Response is nil; can't generate title")
		if sessionLock != nil {
			sessionLock.Lock()
			defer sessionLock.Unlock()
		}
		saveErr := a.sessions.Rename(ctx, sessionID, DefaultSessionName)
		if saveErr != nil {
			slog.Error("Failed to save session title", "error", saveErr)
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
	cost := modelConfig.CostPer1MInCached/1e6*float64(resp.TotalUsage.CacheCreationTokens) +
		modelConfig.CostPer1MOutCached/1e6*float64(resp.TotalUsage.CacheReadTokens) +
		modelConfig.CostPer1MIn/1e6*float64(resp.TotalUsage.InputTokens) +
		modelConfig.CostPer1MOut/1e6*float64(resp.TotalUsage.OutputTokens)

	// Use override cost if available (e.g., from OpenRouter).
	if openrouterCost != nil {
		cost = *openrouterCost
	}

	promptTokens := promptTokensForUsage(resp.TotalUsage, usageProvider(model))
	// Use OutputTokens only (not OutputTokens + ReasoningTokens) to avoid
	// double-counting reasoning tokens for OpenAI-style providers where
	// OutputTokens already includes ReasoningTokens.
	completionTokens := resp.TotalUsage.OutputTokens

	// Atomically update only title and usage fields to avoid overriding other
	// concurrent session updates.
	if sessionLock != nil {
		sessionLock.Lock()
		defer sessionLock.Unlock()
	}
	saveErr := a.sessions.UpdateTitleAndUsage(ctx, sessionID, title, promptTokens, completionTokens, cost)
	if saveErr != nil {
		slog.Error("Failed to save session title and usage", "error", saveErr)
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
