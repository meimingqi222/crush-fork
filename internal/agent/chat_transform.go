package agent

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/plugin"
	"github.com/charmbracelet/crush/internal/session"
)

type sessionCompactingPurposeContextKey struct{}

// compactionRescueContextKey carries a Markdown-formatted "memory rescue"
// block that must be injected into the compaction prompt so durable memories
// survive session summarization. The payload is produced by
// engine.PrepareCompactionRescue via MemoryEngineHooks.OnBeforeCompaction.
type compactionRescueContextKey struct{}

// withCompactionRescue attaches a non-empty memory-rescue payload to ctx.
// Empty payloads are dropped to keep the context clean.
func withCompactionRescue(ctx context.Context, payload string) context.Context {
	if strings.TrimSpace(payload) == "" {
		return ctx
	}
	return context.WithValue(ctx, compactionRescueContextKey{}, payload)
}

// compactionRescueFromContext returns the rescue payload previously attached
// via withCompactionRescue, or an empty string when no payload was set.
func compactionRescueFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	payload, _ := ctx.Value(compactionRescueContextKey{}).(string)
	return payload
}

type chatRequestState struct {
	Messages         []message.Message
	History          []fantasy.Message
	Files            []fantasy.FilePart
	SystemPrompt     string
	PromptPrefix     string
	PromptSuffix     string
	TransformChanged bool
	EstimateReduced  bool
}

type chatRequestStateInput struct {
	SessionID             string
	Agent                 string
	Model                 Model
	Provider              plugin.ProviderContext
	Purpose               plugin.ChatTransformPurpose
	RequestPurpose        plugin.ChatTransformPurpose
	Messages              []message.Message
	Message               message.Message
	Attachments           []message.Attachment
	SystemPrompt          string
	PromptPrefix          string
	PermissionMode        session.PermissionMode
	EstimatedPromptTokens int64
	// Tools, when non-nil, are included in the per-step token segment
	// breakdown (tool_schema_tokens). Callers that have the prepared tool
	// list (e.g. estimateNextStepPromptTokens) pass it so the diagnostic
	// reflects the real wire cost of tool definitions.
	Tools []fantasy.AgentTool
}

func withSessionCompactingPurpose(ctx context.Context, purpose plugin.ChatTransformPurpose) context.Context {
	if purpose == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionCompactingPurposeContextKey{}, purpose)
}

func sessionCompactingPurposeFromContext(ctx context.Context) plugin.ChatTransformPurpose {
	purpose, ok := ctx.Value(sessionCompactingPurposeContextKey{}).(plugin.ChatTransformPurpose)
	if !ok || purpose == "" {
		return plugin.ChatTransformPurposeSummarize
	}
	return purpose
}

func cloneMessages(msgs []message.Message) []message.Message {
	cloned := make([]message.Message, len(msgs))
	for i := range msgs {
		cloned[i] = msgs[i].Clone()
	}
	return cloned
}

func agentModelInfo(model Model) plugin.ModelInfo {
	limits := ContextWindowLimitsFor(model.CatwalkCfg)
	return plugin.ModelInfo{
		ProviderID:             model.ModelCfg.Provider,
		ModelID:                model.ModelCfg.Model,
		ContextWindow:          limits.ContextWindow,
		MaxPromptTokens:        limits.MaxPromptTokens,
		EffectiveContextWindow: limits.EffectiveContextWindow,
	}
}

// usageSnapshotFromMessages builds a UsageSnapshot from the most recent
// assistant message that has reported usage, augmented with estimatedPromptTokens.
func usageSnapshotFromMessages(msgs []message.Message, estimatedPromptTokens int64) plugin.UsageSnapshot {
	snap := plugin.UsageSnapshot{EstimatedPromptTokens: estimatedPromptTokens}
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role != message.Assistant {
			continue
		}
		if !m.Usage.HasDisplayOutput() {
			continue
		}
		snap.PromptTokens = m.Usage.PromptTokens()
		snap.CompletionTokens = m.Usage.OutputTokens
		snap.ReasoningTokens = m.Usage.ReasoningTokens
		snap.CacheReadTokens = m.Usage.CacheReadTokens
		snap.CacheWriteTokens = m.Usage.CacheWriteTokens
		// TotalTokens = PromptTokens + OutputTokens (not CompletionTokens).
		// OutputTokens already includes ReasoningTokens for OpenAI-style
		// providers (and Anthropic output_tokens includes thinking tokens),
		// so adding ReasoningTokens again would double-count.
		snap.TotalTokens = m.Usage.PromptTokens() + m.Usage.OutputTokens
		break
	}
	// ContextUsed tracks the latest observed context length. Only fall back to
	// the character estimate when no assistant usage is available yet.
	ctxUsed := snap.TotalTokens
	if ctxUsed <= 0 && estimatedPromptTokens > 0 {
		ctxUsed = estimatedPromptTokens
	}
	snap.ContextUsed = ctxUsed
	return snap
}

func defaultProviderContext() plugin.ProviderContext {
	return plugin.ProviderContext{Source: "config", Options: map[string]any{}}
}

func transientUserMessage(sessionID, prompt string, attachments []message.Attachment) message.Message {
	parts := []message.ContentPart{message.TextContent{Text: prompt}}
	for _, attachment := range attachments {
		parts = append(parts, message.BinaryContent{
			Path:     attachment.FilePath,
			MIMEType: attachment.MimeType,
			Data:     attachment.Content,
		})
	}
	return message.Message{
		SessionID: sessionID,
		Role:      message.User,
		Parts:     parts,
	}
}

func joinSystemSections(sections []string) string {
	filtered := make([]string, 0, len(sections))
	for _, section := range sections {
		if strings.TrimSpace(section) == "" {
			continue
		}
		filtered = append(filtered, section)
	}
	return strings.Join(filtered, "\n")
}

func estimatePromptStateTokens(history []fantasy.Message, systemPrompt, promptPrefix string) int64 {
	return estimatePromptTokens(history, nil) +
		estimateStringTokens(systemPrompt) +
		estimateStringTokens(promptPrefix)
}

// estimatePromptForMessages is a lightweight estimate used at call sites that
// only have session messages (not a full fantasy history). It mirrors the
// heuristic used by buildChatRequestState without tools/system prompt.
func (a *sessionAgent) estimatePromptForMessages(msgs []message.Message) int64 {
	prepared, _ := a.preparePrompt(msgs)
	return estimatePromptTokens(prepared, nil)
}

func (a *sessionAgent) transformSessionMessages(ctx context.Context, input chatRequestStateInput) ([]message.Message, error) {
	transformed, err := a.plugins().TriggerChatMessagesTransform(ctx, plugin.ChatMessagesTransformInput{
		SessionID:      input.SessionID,
		Agent:          input.Agent,
		Model:          agentModelInfo(input.Model),
		Provider:       input.Provider,
		Purpose:        input.Purpose,
		RequestPurpose: input.RequestPurpose,
		Message:        input.Message,
		Usage:          usageSnapshotFromMessages(input.Messages, input.EstimatedPromptTokens),
	}, plugin.ChatMessagesTransformOutput{Messages: cloneMessages(input.Messages)})
	if err != nil {
		return nil, err
	}
	return transformed.Messages, nil
}

func (a *sessionAgent) transformSystemPrompt(ctx context.Context, input chatRequestStateInput) (string, string, error) {
	transformed, err := a.plugins().TriggerChatSystemTransform(ctx, plugin.ChatSystemTransformInput{
		SessionID:      input.SessionID,
		Agent:          input.Agent,
		Model:          agentModelInfo(input.Model),
		Provider:       input.Provider,
		Purpose:        input.Purpose,
		RequestPurpose: input.RequestPurpose,
		Message:        input.Message,
		Usage:          usageSnapshotFromMessages(input.Messages, input.EstimatedPromptTokens),
	}, plugin.ChatSystemTransformOutput{System: []string{input.SystemPrompt}, Prefix: input.PromptPrefix})
	if err != nil {
		return "", "", err
	}
	return joinSystemSections(transformed.System), transformed.Prefix, nil
}

func (a *sessionAgent) buildChatRequestState(ctx context.Context, input chatRequestStateInput) (chatRequestState, error) {
	start := time.Now()
	originalHistory, _ := a.preparePrompt(input.Messages)
	originalEstimate := estimatePromptStateTokens(originalHistory, input.SystemPrompt, input.PromptPrefix)
	if input.EstimatedPromptTokens <= 0 {
		input.EstimatedPromptTokens = originalEstimate
	}
	transformedMessages, err := a.transformSessionMessages(ctx, input)
	if err != nil {
		return chatRequestState{}, err
	}
	slog.Debug("[PERF] buildChatRequestState: transformSessionMessages done", "duration", time.Since(start), "session_id", input.SessionID, "msg_count", len(transformedMessages))

	// Inject dynamic, idempotent caveat warning ONLY to the LATEST (last) User message in memory
	// when session is in Orchestrate (coordinator) mode. This avoids mutating historical user messages,
	// ensuring that the established prompt cache (KV cache) on the LLM side remains 100% hit and intact.
	if sess, err := a.sessions.Get(ctx, input.SessionID); err == nil && sess.CollaborationMode == session.CollaborationModeOrchestrate {
		caveat := "<system_intent_gate_caveat>Notice: You are in Orchestrate (coordinator) mode. You decompose, dispatch, verify, and iterate. You do **not** edit code yourself. Every file mutation MUST go through a specialized subagent (e.g. by using the \"agent\" tool). Do not attempt to call \"edit\" or \"write\" directly in this session.</system_intent_gate_caveat>\n\n"
		for i := len(transformedMessages) - 1; i >= 0; i-- {
			if transformedMessages[i].Role == message.User {
				text := transformedMessages[i].Content().Text
				if text != "" && !strings.HasPrefix(text, caveat) {
					transformedMessages[i].SetContent(caveat + text)
				}
				break
			}
		}
	}

	systemPrompt, promptPrefix, err := a.transformSystemPrompt(ctx, input)
	if err != nil {
		return chatRequestState{}, err
	}
	slog.Debug("[PERF] buildChatRequestState: transformSystemPrompt done", "duration", time.Since(start), "session_id", input.SessionID)
	var promptSuffix string
	if autoModePrompt, ok := pendingAutoModePromptText(transformedMessages, input.PermissionMode); ok {
		// Keep the main system prompt stable for prompt caching; inject the
		// auto-mode reminder as a trailing system message instead.
		promptSuffix = autoModePrompt
	}
	history, files := a.preparePrompt(transformedMessages, input.Attachments...)
	files = filterImageFilesForModel(files, input.Model)
	transformedEstimate := estimatePromptStateTokens(history, systemPrompt, promptPrefix) + estimateStringTokens(promptSuffix)
	// Per-segment breakdown so context-growth optimizations can be measured
	// and compared against a fixed baseline. tool_schema_tokens is only
	// non-zero when the caller passes the prepared tool list.
	systemPromptTokens := estimateStringTokens(systemPrompt)
	promptPrefixTokens := estimateStringTokens(promptPrefix)
	promptSuffixTokens := estimateStringTokens(promptSuffix)
	toolSchemaTokens := estimatePromptTokens(nil, input.Tools)
	historyTokens := estimatePromptTokens(history, nil)
	userTokens := estimateStringTokens(message.PromptWithTextAttachments(input.Message.Content().Text, input.Attachments))
	segmentAttrs := []any{
		"session_id", input.SessionID,
		"agent", input.Agent,
		"purpose", input.Purpose,
		"request_purpose", input.RequestPurpose,
		"model", input.Model.ModelCfg.Model,
		"provider", input.Model.ModelCfg.Provider,
		"original_messages", len(input.Messages),
		"transformed_messages", len(transformedMessages),
		"history_messages", len(history),
		"original_estimate_tokens", originalEstimate,
		"transformed_estimate_tokens", transformedEstimate,
		"estimate_reduced", transformedEstimate < originalEstimate,
		"system_prompt_tokens", systemPromptTokens,
		"tool_schema_tokens", toolSchemaTokens,
		"tool_count", len(input.Tools),
		"prior_history_tokens", historyTokens,
		"current_user_tokens", userTokens,
		"prompt_prefix_tokens", promptPrefixTokens,
		"prompt_suffix_tokens", promptSuffixTokens,
	}
	slog.Debug("Built chat request token estimate", segmentAttrs...)
	if contextUsageDiagEnabled() {
		slog.Info("Context usage segments",
			"session_id", input.SessionID,
			"model", input.Model.ModelCfg.Model,
			"purpose", input.Purpose,
			"system_prompt_tokens", systemPromptTokens,
			"tool_schema_tokens", toolSchemaTokens,
			"tool_count", len(input.Tools),
			"prior_history_tokens", historyTokens,
			"current_user_tokens", userTokens,
			"prompt_prefix_tokens", promptPrefixTokens,
			"prompt_suffix_tokens", promptSuffixTokens,
			"total_estimated_tokens", transformedEstimate,
		)
	}
	return chatRequestState{
		Messages:     transformedMessages,
		History:      history,
		Files:        files,
		SystemPrompt: systemPrompt,
		PromptPrefix: promptPrefix,
		PromptSuffix: promptSuffix,
		TransformChanged: !reflect.DeepEqual(transformedMessages, input.Messages) ||
			systemPrompt != input.SystemPrompt ||
			promptPrefix != input.PromptPrefix,
		EstimateReduced: transformedEstimate < originalEstimate,
	}, nil
}

// filterImageFilesForModel removes image FileParts from the request when the
// primary model does not support image inputs. This prevents providers from
// rejecting requests with unsupported media; the model still sees a text
// placeholder produced by stripImagePartsFromFantasyMessages(WithVision).
func filterImageFilesForModel(files []fantasy.FilePart, model Model) []fantasy.FilePart {
	if model.CatwalkCfg.SupportsImages {
		return files
	}
	filtered := make([]fantasy.FilePart, 0, len(files))
	for _, f := range files {
		if strings.HasPrefix(f.MediaType, "image/") {
			continue
		}
		filtered = append(filtered, f)
	}
	return filtered
}

func buildSessionCompactingPrompt(todos []session.Todo, extraContext []string, promptOverride string) string {
	base := buildSummaryPrompt(todos)
	if promptOverride != "" {
		base = promptOverride
	}
	if len(extraContext) == 0 {
		return base
	}

	var sb strings.Builder
	sb.WriteString(base)
	sb.WriteString("\n\n## Additional Context\n\n")
	for _, item := range extraContext {
		fmt.Fprintf(&sb, "- %s\n", item)
	}
	return sb.String()
}
