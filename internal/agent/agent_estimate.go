package agent

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"charm.land/fantasy"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/plugin"
	"github.com/charmbracelet/crush/internal/session"
)

// estimatedImageTokens is a rough estimate for a compressed image.
// Anthropic's vision token calculation is based on resolution:
// ~1333x1000 pixels ≈ 1500 tokens. Since images are compressed to max
// 2048px dimension, we use 2000 as a reasonable upper bound estimate.
// This is far more accurate than treating base64 bytes as text (which
// would estimate ~250,000 tokens for an 800KB image).
const estimatedImageTokens = 2000

func usageProvider(model Model) string {
	if model.Model != nil {
		if provider := model.Model.Provider(); provider != "" {
			return provider
		}
	}
	return model.ModelCfg.Provider
}

func effectiveContextWindow(model Model) int64 {
	return EffectiveContextWindow(model.CatwalkCfg)
}

func int64ProviderOptionValue(value any) (int64, bool) {
	return contextWindowInt64(value)
}

func isAnthropicStyleUsageProvider(providerID string) bool {
	providerID = strings.ToLower(strings.TrimSpace(providerID))
	if providerID == "" {
		return false
	}
	switch providerID {
	case "anthropic", "anthropic-proxy", "bedrock":
		return true
	default:
		return strings.Contains(providerID, "anthropic") || strings.Contains(providerID, "bedrock")
	}
}

func isOpenAIUsageProvider(providerID string) bool {
	// All providers built on the OpenAI SDK (openai, azure, openai-compat)
	// normalize InputTokens by subtracting CacheReadTokens, so prompt
	// token accounting must add them back.
	providerID = strings.ToLower(strings.TrimSpace(providerID))
	switch providerID {
	case "openai", "azure", "openai-compat":
		return true
	default:
		return false
	}
}

func promptTokensForUsage(usage fantasy.Usage, providerID string) int64 {
	// Anthropic and Bedrock report InputTokens WITHOUT cached tokens.
	if isAnthropicStyleUsageProvider(providerID) {
		return usage.InputTokens + usage.CacheCreationTokens + usage.CacheReadTokens
	}
	// fantasy/providers/openai normalizes InputTokens by subtracting
	// CacheReadTokens for both Chat Completions and Responses usage.
	// It does NOT populate CacheCreationTokens for OpenAI/Response providers,
	// so adding CacheCreationTokens here is a no-op today. If it ever starts
	// filling CacheCreationTokens, this formula must be updated to avoid
	// double-counting cache creation.
	if isOpenAIUsageProvider(providerID) {
		return usage.InputTokens + usage.CacheCreationTokens + usage.CacheReadTokens
	}
	// Other providers generally report InputTokens INCLUDING cached tokens.
	// Only add CacheCreationTokens (rare).
	// Note: ReasoningTokens are output tokens (part of completion_tokens), not input tokens.
	return usage.InputTokens + usage.CacheCreationTokens
}

// shouldFloorPromptTokensToEstimate reports whether a local character-based
// prompt estimate should replace provider-reported prompt tokens.
//
// The estimate floor exists for providers that omit or severely under-report
// input usage (some Anthropic responses only carry output deltas). It must
// not run when the provider already returned a trustworthy prompt total —
// especially OpenAI/xAI-style usage with cache accounting, where character
// estimates often overshoot by 30–50% because they ignore tokenizer efficiency
// and treat cached prefixes as fully billable text.
func shouldFloorPromptTokensToEstimate(usage fantasy.Usage, providerID string, promptTokens, estimatedPromptTokens int64) bool {
	if estimatedPromptTokens <= 0 || promptTokens >= estimatedPromptTokens {
		return false
	}
	if promptTokens <= 0 {
		return true
	}
	if usage.CacheReadTokens > 0 || usage.CacheCreationTokens > 0 {
		return false
	}
	if isOpenAIUsageProvider(providerID) {
		return false
	}
	return true
}

func normalizedMessageUsage(usage fantasy.Usage, providerID string, estimatedPromptTokens int64) message.Usage {
	normalized := message.Usage{
		OutputTokens:     usage.OutputTokens,
		ReasoningTokens:  usage.ReasoningTokens,
		CacheReadTokens:  usage.CacheReadTokens,
		CacheWriteTokens: usage.CacheCreationTokens,
	}

	promptTokens := promptTokensForUsage(usage, providerID)
	if shouldFloorPromptTokensToEstimate(usage, providerID, promptTokens, estimatedPromptTokens) {
		promptTokens = estimatedPromptTokens
	}

	normalized.InputTokens = max(0, promptTokens-normalized.CacheReadTokens-normalized.CacheWriteTokens)
	return normalized
}

func totalTokensForUsage(usage fantasy.Usage, providerID string) int64 {
	// Use OutputTokens only (not OutputTokens + ReasoningTokens) to avoid
	// double-counting reasoning tokens for OpenAI-style providers where
	// OutputTokens already includes ReasoningTokens.
	return promptTokensForUsage(usage, providerID) + usage.OutputTokens
}

func autoSummarizeReservedTokens(maxOutputTokens int64) int64 {
	if maxOutputTokens <= 0 {
		return autoSummarizeReserveTokens
	}
	return min(autoSummarizeReserveTokens, maxOutputTokens)
}

func estimateStringTokens(s string) int64 {
	if s == "" {
		return 0
	}
	return estimateTextTokens(s, true)
}

func estimateTextTokens(s string, roundUpASCII bool) int64 {
	if s == "" {
		return 0
	}
	asciiBytes, nonASCIIRunes := estimateTextTokenUnits(s)
	return estimateTextTokensFromUnits(asciiBytes, nonASCIIRunes, roundUpASCII)
}

func estimateTextTokenUnits(s string) (asciiBytes int64, nonASCIIRunes int64) {
	for _, r := range s {
		if r < utf8.RuneSelf {
			asciiBytes++
			continue
		}
		nonASCIIRunes++
	}
	return asciiBytes, nonASCIIRunes
}

func estimateTextTokensFromUnits(asciiBytes, nonASCIIRunes int64, roundUpASCII bool) int64 {
	asciiTokens := asciiBytes / 4
	if roundUpASCII && asciiBytes%4 != 0 {
		asciiTokens++
	}
	return asciiTokens + nonASCIIRunes
}

func estimateMessageContentTokens(parts []fantasy.MessagePart) int64 {
	var asciiBytes int64
	var nonASCIIRunes int64
	var imageCount int64

	accumulate := func(s string) {
		partASCII, partNonASCII := estimateTextTokenUnits(s)
		asciiBytes += partASCII
		nonASCIIRunes += partNonASCII
	}

	accumulateData := func(data []byte, mediaType string) {
		if len(data) == 0 {
			return
		}
		if isImageMediaType(mediaType) {
			imageCount++
		} else {
			asciiBytes += int64(len(data))
		}
	}

	for _, part := range parts {
		switch p := part.(type) {
		case fantasy.TextPart:
			accumulate(p.Text)
		case fantasy.ReasoningPart:
			accumulate(p.Text)
		case fantasy.ToolCallPart:
			accumulate(p.Input)
		case fantasy.FilePart:
			accumulateData(p.Data, p.MediaType)
			accumulate(p.Filename)
			accumulate(p.MediaType)
		case fantasy.ToolResultPart:
			if txt, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](p.Output); ok {
				accumulate(txt.Text)
			} else if errOut, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](p.Output); ok && errOut.Error != nil {
				accumulate(errOut.Error.Error())
			} else if mediaOut, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](p.Output); ok {
				accumulateData([]byte(mediaOut.Data), mediaOut.MediaType)
				accumulate(mediaOut.MediaType)
				accumulate(mediaOut.Text)
			}
		}
	}

	textTokens := estimateTextTokensFromUnits(asciiBytes, nonASCIIRunes, false)
	return textTokens + (imageCount * estimatedImageTokens)
}

func isImageMediaType(mediaType string) bool {
	return mediaType == "image/png" ||
		mediaType == "image/jpeg" ||
		mediaType == "image/gif" ||
		mediaType == "image/webp" ||
		mediaType == "image/bmp"
}

func (a *sessionAgent) estimateSessionPromptTokens(history []fantasy.Message, prompt string, attachments []message.Attachment, tools []fantasy.AgentTool, systemPrompt string, promptPrefix string, promptSuffix string) int64 {
	total := estimatePromptTokens(history, tools)
	total += estimateStringTokens(systemPrompt)
	total += estimateStringTokens(promptPrefix)
	total += estimateStringTokens(promptSuffix)
	total += estimateStringTokens(message.PromptWithTextAttachments(prompt, attachments))
	return total
}

func (a *sessionAgent) estimateNextStepPromptTokens(ctx context.Context, sessionID string, tools []fantasy.AgentTool, systemPrompt string, promptPrefix string, model Model, provider plugin.ProviderContext, requestPurpose plugin.ChatTransformPurpose) (int64, bool, error) {
	currentSession, err := a.sessions.Get(ctx, sessionID)
	if err != nil {
		return 0, false, err
	}
	msgs, err := a.getSessionMessages(ctx, currentSession)
	if err != nil {
		return 0, false, err
	}
	state, err := a.buildChatRequestState(ctx, chatRequestStateInput{
		SessionID:      sessionID,
		Agent:          "session",
		Model:          model,
		Provider:       provider,
		Purpose:        plugin.ChatTransformPurposeNextStepEstimate,
		RequestPurpose: requestPurpose,
		Messages:       msgs,
		Message:        message.Message{SessionID: sessionID, Role: message.User},
		SystemPrompt:   systemPrompt,
		PromptPrefix:   promptPrefix,
		PermissionMode: currentSession.PermissionMode,
	})
	if err != nil {
		return 0, false, err
	}
	return a.estimateSessionPromptTokens(state.History, "", nil, tools, state.SystemPrompt, state.PromptPrefix, state.PromptSuffix), state.EstimateReduced, nil
}

func (a *sessionAgent) EstimateSessionPromptTokensForModel(ctx context.Context, sessionID string, model Model) (int64, error) {
	tokens, _, err := a.estimateNextStepPromptTokens(
		ctx,
		sessionID,
		a.tools.Copy(),
		a.systemPrompt.Get(),
		a.systemPromptPrefix.Get(),
		model,
		defaultProviderContext(),
		plugin.ChatTransformPurposeRequest,
	)
	if err != nil {
		return 0, err
	}
	// Cap with the last API-observed token count to avoid character-based
	// over-estimation (can be 2x+ actual) triggering premature summarization
	// on model switch. Falls back to the character estimate when no observed
	// count is available.
	if session, sessErr := a.sessions.Get(ctx, sessionID); sessErr == nil {
		if observed := session.LastInputTokens(); observed > 0 && observed < tokens {
			tokens = observed
		}
	}
	return tokens, nil
}

// estimatePromptTokens estimates the prompt token count from message content
// and tool definitions. This serves as a fallback when providers (e.g., some
// Anthropic-compatible proxies) don't report input tokens in streaming mode.
//
// All message part types are counted:
//   - TextPart / ReasoningPart: plain text bytes
//   - ToolCallPart: Input JSON string bytes
//   - ToolResultPart: text output bytes
//
// Tool definitions include the JSON-encoded parameter schema. ASCII-heavy
// content is estimated at roughly 4 bytes per token, while non-ASCII runes
// count as at least one token each so CJK-heavy prompts are not badly
// under-estimated.
func estimatePromptTokens(messages []fantasy.Message, tools []fantasy.AgentTool) int64 {
	var totalTokens int64
	for _, msg := range messages {
		totalTokens += estimateMessageContentTokens(msg.Content)
	}
	for _, tool := range tools {
		info := tool.Info()
		totalTokens += estimateTextTokens(info.Name, false)
		totalTokens += estimateTextTokens(info.Description, false)
		if schemaJSON, err := json.Marshal(info.Parameters); err == nil {
			totalTokens += estimateTextTokens(string(schemaJSON), false)
		} else {
			totalTokens += 75
		}
	}
	return totalTokens
}

// usageRecordPurpose classifies why an LLM call happened so
// updateSessionUsage can decide which session fields it is allowed to
// touch. This is the single write point for conversation and maintenance
// calls; Summarize is responsible for recomputing the post-compaction
// baseline (PromptTokens / LastPromptTokens / LastCompletionTokens)
// after its usage is recorded.
type usageRecordPurpose int

const (
	// usagePurposeConversation marks a real conversation step (the main
	// agent loop). Its reported prompt tokens are the authoritative "current
	// context length" and update Cost, CompletionTokens, LastPromptTokens,
	// and LastCompletionTokens.
	usagePurposeConversation usageRecordPurpose = iota
	// usagePurposeSummarize marks a compaction/summarize call. The cost and
	// completion tokens it incurred are real and must be recorded, but its
	// own prompt tokens describe the *pre-compaction* history, not the
	// current context, so they must never overwrite LastPromptTokens /
	// LastCompletionTokens. The caller (Summarize) recomputes those fields
	// from the retained post-compaction messages once the summary is
	// committed.
	usagePurposeSummarize
	// usagePurposeMaintenance marks auxiliary calls (title generation,
	// memory extraction, ...). Only Cost and CompletionTokens are recorded;
	// none of the context-length fields are touched.
	usagePurposeMaintenance
)

func (a *sessionAgent) updateSessionUsage(model Model, session *session.Session, usage fantasy.Usage, overrideCost *float64, estimatedPromptTokens int64, estimated bool, purpose usageRecordPurpose) {
	modelConfig := model.CatwalkCfg
	cost := modelConfig.CostPer1MInCached/1e6*float64(usage.CacheCreationTokens) +
		modelConfig.CostPer1MOutCached/1e6*float64(usage.CacheReadTokens) +
		modelConfig.CostPer1MIn/1e6*float64(usage.InputTokens) +
		modelConfig.CostPer1MOut/1e6*float64(usage.OutputTokens)

	if !estimated {
		a.eventTokensUsed(session.ID, model, usage, cost)
	} else {
		session.EstimatedUsage = true
	}

	if overrideCost != nil {
		session.Cost += *overrideCost
	} else {
		session.Cost += cost
	}

	// Cost and cumulative CompletionTokens are real for every purpose: the
	// call happened and consumed output tokens regardless of why it was
	// made.
	session.CompletionTokens += usage.OutputTokens

	if purpose == usagePurposeMaintenance {
		// Maintenance calls (title generation, memory extraction, ...) are
		// billable but must never influence "current context length"
		// bookkeeping used for display and auto-summarize thresholds.
		return
	}

	normalizedUsage := normalizedMessageUsage(usage, usageProvider(model), estimatedPromptTokens)

	if purpose == usagePurposeSummarize {
		// The summarize call's own prompt tokens reflect the
		// pre-compaction history, not the post-compaction context. Do not
		// touch PromptTokens/LastPromptTokens/LastCompletionTokens here;
		// the caller (Summarize) recomputes them from the retained
		// messages once the summary is committed.
		return
	}

	promptTokens := normalizedUsage.PromptTokens()
	// PromptTokens tracks the current context length (the full input sent in
	// the latest exchange), not a sum across exchanges. Provider usage reports
	// the total input for each request, which already includes all history, so
	// accumulating it would double-count earlier turns and produce inflated
	// totals like tens of millions of tokens.
	//
	// Only update when the current step reports a positive prompt token
	// count. Providers sometimes omit input tokens (reporting only output
	// tokens or a total), so preserve the last known context length when the
	// prompt count is unavailable.
	if promptTokens > 0 {
		session.PromptTokens = promptTokens
		session.LastPromptTokens = promptTokens
	}
	// Use OutputTokens (not CompletionTokens which adds ReasoningTokens)
	// to avoid double-counting reasoning tokens for OpenAI-style providers
	// where OutputTokens already includes ReasoningTokens.
	session.LastCompletionTokens = normalizedUsage.OutputTokens
}
