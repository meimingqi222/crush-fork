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

type chatRequestState struct {
	Messages         []message.Message
	History          []fantasy.Message
	Files            []fantasy.FilePart
	SystemPrompt     string
	PromptPrefix     string
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
		snap.CompletionTokens = m.Usage.CompletionTokens()
		snap.ReasoningTokens = m.Usage.ReasoningTokens
		snap.CacheReadTokens = m.Usage.CacheReadTokens
		snap.CacheWriteTokens = m.Usage.CacheWriteTokens
		snap.TotalTokens = m.Usage.TotalTokens()
		break
	}
	// ContextUsed is max(last_total_tokens, estimated_prompt_tokens) to give
	// plugins a single number to compare to EffectiveContextWindow.
	ctxUsed := snap.TotalTokens
	if estimatedPromptTokens > ctxUsed {
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
	systemPrompt, promptPrefix, err := a.transformSystemPrompt(ctx, input)
	if err != nil {
		return chatRequestState{}, err
	}
	slog.Debug("[PERF] buildChatRequestState: transformSystemPrompt done", "duration", time.Since(start), "session_id", input.SessionID)
	if autoModePrompt, ok := pendingAutoModePromptText(transformedMessages, input.PermissionMode); ok {
		systemPrompt = joinSystemSections([]string{systemPrompt, autoModePrompt})
	}
	history, files := a.preparePrompt(transformedMessages, input.Attachments...)
	transformedEstimate := estimatePromptStateTokens(history, systemPrompt, promptPrefix)
	slog.Debug("[PERF] buildChatRequestState: preparePrompt done", "duration", time.Since(start), "session_id", input.SessionID)
	return chatRequestState{
		Messages:     transformedMessages,
		History:      history,
		Files:        files,
		SystemPrompt: systemPrompt,
		PromptPrefix: promptPrefix,
		TransformChanged: !reflect.DeepEqual(transformedMessages, input.Messages) ||
			systemPrompt != input.SystemPrompt ||
			promptPrefix != input.PromptPrefix,
		EstimateReduced: transformedEstimate < originalEstimate,
	}, nil
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
