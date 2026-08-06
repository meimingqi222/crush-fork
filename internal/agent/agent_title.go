package agent

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"unicode/utf8"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openrouter"

	"github.com/charmbracelet/crush/internal/config"
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

// reasoningLevelRank orders reasoning levels so the lowest supported level can
// be chosen for fast background tasks like title generation. "none" means
// thinking is disabled entirely (the fastest path).
func reasoningLevelRank(l string) int {
	switch l {
	case "", "none":
		return 0
	case "minimal":
		return 1
	case "low":
		return 2
	case "medium":
		return 3
	case "high":
		return 4
	case "xhigh", "max":
		return 5
	default:
		// Unknown levels are treated as "medium" (a safe middle point) rather
		// than being picked blindly as the lowest.
		return 3
	}
}

// lowestReasoningLevel returns the lowest reasoning level a model advertises,
// or "" when the model does not reason or lacks advertised levels.
func lowestReasoningLevel(cfg catwalk.Model) string {
	if !cfg.CanReason || len(cfg.ReasoningLevels) == 0 {
		return ""
	}
	best := ""
	bestRank := int(^uint(0) >> 1) // max int
	for _, lvl := range cfg.ReasoningLevels {
		if rank := reasoningLevelRank(lvl); rank < bestRank {
			best, bestRank = lvl, rank
		}
	}
	return best
}

// reasoningOverrideKeys are provider-option keys that force a specific
// reasoning/thinking value. Title generation strips them from the local option
// copies so the lowest level picked by the titleProviderOptions branch wins
// instead of user/provider-configured effort.
var reasoningOverrideKeys = []string{
	"reasoning_effort", "effort", "thinking",
	"reasoning", "thinking_config", "reasoning_summary", "include",
}

// titleProviderOptions builds provider options for title generation using the
// model's lowest supported reasoning level. Some models can no longer disable
// thinking entirely, so this requests the lowest advertised level (e.g.
// "minimal"/"low") instead of hard-coding a fixed level. When the model can
// disable thinking ("none" or no reasoning support) it takes the fastest
// disabling path.
func titleProviderOptions(model Model, providerCfg config.ProviderConfig) fantasy.ProviderOptions {
	if lvl := lowestReasoningLevel(model.CatwalkCfg); lvl != "" && lvl != "none" {
		// getProviderOptions resolves reasoning effort from user config first,
		// then model config, then the merged provider options — all of which
		// would override the model's default. For a latency-sensitive title
		// call we want the lowest advertised level unconditionally, so set it
		// at the highest-precedence fields and strip reasoning keys from the
		// (copied) option maps.
		model.ModelCfg.ReasoningEffort = lvl
		model.CatwalkCfg.DefaultReasoningEffort = lvl
		model, providerCfg = stripReasoningProviderOptions(model, providerCfg)
		return getProviderOptions(model, providerCfg)
	}
	return bgProviderOptionsNoThink(model, providerCfg)
}

// stripReasoningProviderOptions removes reasoning keys from the model and
// provider option maps so they cannot override the chosen low level. The maps
// are copied first because they are shared with the long-lived model/provider
// configs; deleting in place would corrupt them.
func stripReasoningProviderOptions(model Model, providerCfg config.ProviderConfig) (Model, config.ProviderConfig) {
	model.CatwalkCfg.Options.ProviderOptions = stripReasoningKeys(model.CatwalkCfg.Options.ProviderOptions)
	model.ModelCfg.ProviderOptions = stripReasoningKeys(model.ModelCfg.ProviderOptions)
	providerCfg.ProviderOptions = stripReasoningKeys(providerCfg.ProviderOptions)
	return model, providerCfg
}

func stripReasoningKeys(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	out := make(map[string]any, len(src))
	for k, v := range src {
		out[k] = v
	}
	for _, k := range reasoningOverrideKeys {
		delete(out, k)
	}
	return out
}

func (a *sessionAgent) generateTitle(ctx context.Context, sessionID string, userPrompt string, sessionLock *sync.Mutex) {
	userPrompt = titleUserPromptFromCall(userPrompt)
	if userPrompt == "" {
		return
	}

	smallModel := a.smallModel.Get()
	largeModel := a.largeModel.Get()
	systemPromptPrefix := a.systemPromptPrefix.Get()
	providerCfg := a.providerCfg

	// Title generation is a short, single-shot task: use non-streaming
	// Generate (no per-token accumulation, fewer round trips) at the model's
	// lowest supported reasoning level.
	newAgent := func(m fantasy.LanguageModel) fantasy.Agent {
		return fantasy.NewAgent(m,
			fantasy.WithSystemPrompt(string(titlePrompt)),
			fantasy.WithMaxOutputTokens(titleMaxOutputTokens),
			fantasy.WithUserAgent(userAgent),
		)
	}

	genCall := func(model Model) fantasy.AgentCall {
		return fantasy.AgentCall{
			Prompt:          userPrompt,
			ProviderOptions: titleProviderOptions(model, providerCfg),
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
		}
	}

	// Use the small model to generate the title.
	model := smallModel
	titleCtx := copilot.ContextWithInitiatorType(ctx, copilot.InitiatorAgent)
	resp, err := newAgent(model.Model).Generate(titleCtx, genCall(model))
	if err == nil {
		slog.Debug("Generated title with small model")
	} else {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			slog.Debug("Title generation cancelled (small model)", "err", err)
		} else {
			slog.Warn("Error generating title with small model; trying big model", "err", err)
		}
		model = largeModel
		resp, err = newAgent(model.Model).Generate(titleCtx, genCall(model))
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
	title := cleanGeneratedTitle(resp.Response.Content.Text())

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
