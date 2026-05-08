package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"charm.land/fantasy"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/message"
)

var (
	textualToolCallProtocolRegex      = regexp.MustCompile(`(?is)<\|tool_calls_section_begin\|>.*?<\|tool_call_begin\|>.*?functions\.[a-zA-Z0-9_]+(?::\d+)?`)
	textualToolCallProtocolBlockRegex = regexp.MustCompile(`(?is)<\|tool_calls_section_begin\|>.*?(?:<\|tool_calls_section_end\|>|$)`)
	textualToolCallProtocolCallRegex  = regexp.MustCompile(`(?is)<\|tool_call_begin\|>\s*functions\.([a-zA-Z0-9_]+)(?::([0-9]+))?\s*<\|tool_call_argument_begin\|>\s*(.*?)\s*<\|tool_call_end\|>`)
)

const (
	maxTextualToolProtocolRetries            = 2
	maxTextualToolProtocolRecoveries         = 64
	maxRepeatedTextualToolProtocolRecoveries = 3
)

func previewText(text string, maxLen int) string {
	if len(text) <= maxLen {
		return text
	}
	return text[:maxLen] + "..."
}

func textualToolCallSource(assistant *message.Message) string {
	if assistant == nil {
		return ""
	}
	return strings.Join([]string{
		assistant.Content().Text,
		assistant.ReasoningContent().Thinking,
	}, "\n")
}

func parseTextualToolCallsFromAssistant(assistant *message.Message) []message.ToolCall {
	source := textualToolCallSource(assistant)
	if !hasTextualToolCallProtocol(source) {
		return nil
	}
	matches := textualToolCallProtocolCallRegex.FindAllStringSubmatch(source, -1)
	if len(matches) == 0 {
		slog.Warn("Textual tool-call protocol was detected but no complete tool call blocks could be parsed",
			"message_id", assistant.ID,
			"model", assistant.Model,
			"provider", assistant.Provider,
			"text_preview", previewText(assistant.Content().Text, 500),
			"reasoning_preview", previewText(assistant.ReasoningContent().Thinking, 500),
		)
		return nil
	}

	seen := make(map[string]struct{}, len(matches))
	toolCalls := make([]message.ToolCall, 0, len(matches))
	for _, match := range matches {
		name := strings.TrimSpace(match[1])
		index := strings.TrimSpace(match[2])
		input := strings.TrimSpace(match[3])
		if name == "" || input == "" || !json.Valid([]byte(input)) {
			slog.Warn("Skipping malformed textual tool-call block",
				"message_id", assistant.ID,
				"model", assistant.Model,
				"provider", assistant.Provider,
				"tool_name", name,
				"has_input", input != "",
				"input_preview", previewText(input, 500),
			)
			continue
		}
		id := "functions." + name
		if index != "" {
			id += ":" + index
		}
		id = sanitizeAnthropicToolCallID(id)
		key := id + "\x00" + name + "\x00" + input
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		toolCalls = append(toolCalls, message.ToolCall{
			ID:               id,
			Name:             name,
			Input:            input,
			ProviderExecuted: false,
			Finished:         true,
		})
	}
	return toolCalls
}

func textualToolCallRecoveryKey(toolCall message.ToolCall) string {
	var normalizedInput any
	input := strings.TrimSpace(toolCall.Input)
	if err := json.Unmarshal([]byte(input), &normalizedInput); err == nil {
		if encoded, marshalErr := json.Marshal(normalizedInput); marshalErr == nil {
			input = string(encoded)
		}
	}
	return toolCall.Name + "\x00" + input
}

func recordTextualToolCallRecoveries(toolCalls []message.ToolCall, counts map[string]int) (string, int, bool) {
	for _, toolCall := range toolCalls {
		key := textualToolCallRecoveryKey(toolCall)
		counts[key]++
		if counts[key] > maxRepeatedTextualToolProtocolRecoveries {
			return key, counts[key], true
		}
	}
	return "", 0, false
}

func toolResponseToToolResultContent(toolCall message.ToolCall, response fantasy.ToolResponse, runErr error) fantasy.ToolResultContent {
	result := fantasy.ToolResultContent{
		ToolCallID:       toolCall.ID,
		ToolName:         toolCall.Name,
		ClientMetadata:   response.Metadata,
		ProviderExecuted: false,
	}
	if runErr != nil {
		result.Result = fantasy.ToolResultOutputContentError{Error: runErr}
		return result
	}
	if response.IsError {
		result.Result = fantasy.ToolResultOutputContentError{Error: errors.New(response.Content)}
		return result
	}
	if response.Type == "image" || response.Type == "media" {
		result.Result = fantasy.ToolResultOutputContentMedia{
			Data:      base64.StdEncoding.EncodeToString(response.Data),
			MediaType: response.MediaType,
			Text:      response.Content,
		}
		return result
	}
	result.Result = fantasy.ToolResultOutputContentText{Text: response.Content}
	return result
}

func (a *sessionAgent) persistRecoveredToolResult(ctx context.Context, genCtx context.Context, assistant *message.Message, result fantasy.ToolResultContent, runtimeConfig *sessionAgentRuntimeConfig, currentStepToolResultChars *int) (string, error) {
	toolResult := a.convertToToolResult(result)
	if toolResult.Name == tools.ToolSearchToolName {
		if state, ok := deferredToolStateFromToolSearchResult(toolResult.Content); ok {
			toolResult = toolResult.WithDeferredToolState(state)
		}
	}
	toolResult, additionalMedia := a.extractAdditionalMCPMedia(toolResult)
	if runtimeConfig != nil {
		toolResult = a.applyToolResultReview(genCtx, assistant.SessionID, toolResult, runtimeConfig.PermissionMode)
	}
	if currentStepToolResultChars != nil {
		toolResult = a.enforceStepToolResultBudget(assistant.SessionID, toolResult, currentStepToolResultChars)
	}
	if truncatedResult, truncated := a.truncateToolResult(assistant.SessionID, toolResult); truncated {
		toolResult = truncatedResult
	}
	toolMsg, createMsgErr := a.messages.Create(ctx, assistant.SessionID, message.CreateMessageParams{
		Role:                   message.Tool,
		Parts:                  []message.ContentPart{toolResult},
		ActivatedDeferredTools: a.currentActivatedDeferredTools(assistant.SessionID),
	})
	if createMsgErr != nil {
		return "", createMsgErr
	}

	if len(additionalMedia) > 0 {
		parts := make([]message.ContentPart, 0, len(additionalMedia)+1)
		parts = append(parts, message.TextContent{Text: "Additional media content from the tool result:"})
		for _, mediaPart := range additionalMedia {
			parts = append(parts, mediaPart)
		}
		if _, additionalErr := a.messages.Create(ctx, assistant.SessionID, message.CreateMessageParams{
			Role:                   message.User,
			Parts:                  parts,
			ActivatedDeferredTools: a.currentActivatedDeferredTools(assistant.SessionID),
		}); additionalErr != nil {
			return "", additionalErr
		}
	}
	return toolMsg.ID, nil
}

func (a *sessionAgent) recoverTextualToolCallProtocol(ctx context.Context, genCtx context.Context, assistant *message.Message, toolCalls []message.ToolCall, agentTools []fantasy.AgentTool, runtimeConfig *sessionAgentRuntimeConfig, model Model, currentStepToolResultChars *int) ([]string, int, string, bool, error) {
	if assistant == nil || len(assistant.ToolCalls()) > 0 {
		return nil, 0, "", false, nil
	}
	if len(toolCalls) == 0 {
		return nil, 0, "", false, nil
	}

	toolMap := make(map[string]fantasy.AgentTool, len(agentTools))
	for _, tool := range agentTools {
		toolMap[tool.Info().Name] = tool
	}

	assistant.FinishThinking()
	for _, toolCall := range toolCalls {
		assistant.AddToolCall(toolCall)
	}
	if stripTextualToolCallProtocolFromAssistant(assistant) {
		slog.Warn("Recovered structured tool calls from textual tool-call protocol",
			"session_id", assistant.SessionID,
			"message_id", assistant.ID,
			"model", assistant.Model,
			"provider", assistant.Provider,
			"tool_calls_count", len(toolCalls),
		)
	}
	assistant.AddFinish(message.FinishReasonToolUse, "", "")
	if err := a.messages.Update(ctx, *assistant); err != nil {
		return nil, 0, "", false, err
	}

	toolCtx := context.WithValue(genCtx, tools.MessageIDContextKey, assistant.ID)
	toolCtx = context.WithValue(toolCtx, tools.SupportsImagesContextKey, model.CatwalkCfg.SupportsImages)
	toolCtx = context.WithValue(toolCtx, tools.ModelNameContextKey, model.CatwalkCfg.Name)
	toolCtx = context.WithValue(toolCtx, tools.SessionServiceContextKey, a.sessions)

	var toolMessageIDs []string
	var lastTool string
	for _, toolCall := range toolCalls {
		tool := toolMap[toolCall.Name]
		var response fantasy.ToolResponse
		var runErr error
		if tool == nil {
			runErr = fmt.Errorf("tool not found: %s", toolCall.Name)
		} else {
			response, runErr = tool.Run(toolCtx, fantasy.ToolCall{
				ID:    toolCall.ID,
				Name:  toolCall.Name,
				Input: toolCall.Input,
			})
		}
		result := toolResponseToToolResultContent(toolCall, response, runErr)
		toolMessageID, persistErr := a.persistRecoveredToolResult(ctx, genCtx, assistant, result, runtimeConfig, currentStepToolResultChars)
		if persistErr != nil {
			return nil, 0, "", false, persistErr
		}
		toolMessageIDs = append(toolMessageIDs, toolMessageID)
		lastTool = toolCall.Name
	}
	return toolMessageIDs, len(toolCalls), lastTool, true, nil
}

func hasTextualToolCallProtocol(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	hasSectionBegin := strings.Contains(text, "tool_calls_section_begin")
	hasCallBegin := strings.Contains(text, "tool_call_begin")
	if !hasSectionBegin || !hasCallBegin {
		slog.Debug("Textual tool-call protocol detection failed: missing required tags",
			"has_section_begin", hasSectionBegin,
			"has_call_begin", hasCallBegin,
			"text_preview", previewText(text, 200),
		)
		return false
	}
	if !strings.Contains(text, "functions.") {
		slog.Debug("Textual tool-call protocol detection failed: missing functions. prefix",
			"text_preview", previewText(text, 200),
		)
		return false
	}
	matched := textualToolCallProtocolRegex.MatchString(text)
	if !matched {
		slog.Warn("Textual tool-call protocol detected by substring but regex did not match; possible format mismatch",
			"text_preview", previewText(text, 500),
		)
	}
	return matched
}

func stripTextualToolCallProtocol(text string) (string, bool) {
	if !hasTextualToolCallProtocol(text) {
		return text, false
	}
	cleaned := textualToolCallProtocolBlockRegex.ReplaceAllString(text, "")
	cleaned = strings.TrimSpace(cleaned)
	return cleaned, cleaned != text
}

func stripTextualToolCallProtocolFromAssistant(assistant *message.Message) bool {
	if assistant == nil {
		return false
	}
	var changed bool
	if cleaned, textChanged := stripTextualToolCallProtocol(assistant.Content().Text); textChanged {
		assistant.SetContent(cleaned)
		changed = true
	}
	if cleaned, reasoningChanged := stripTextualToolCallProtocol(assistant.ReasoningContent().Thinking); reasoningChanged {
		assistant.SetReasoningThinking(cleaned)
		changed = true
	}
	return changed
}

func shouldRetryForTextualToolCallProtocol(assistant *message.Message) bool {
	if assistant == nil {
		slog.Debug("Should not retry for textual tool-call protocol: assistant message is nil")
		return false
	}
	if len(assistant.ToolCalls()) != 0 {
		text := assistant.Content().Text
		reasoning := assistant.ReasoningContent().Thinking
		textHasProtocol := hasTextualToolCallProtocol(text)
		reasoningHasProtocol := hasTextualToolCallProtocol(reasoning)
		if textHasProtocol || reasoningHasProtocol {
			slog.Warn("Assistant contains textual tool-call protocol but already has structured tool calls; will strip text instead of retrying",
				"tool_calls_count", len(assistant.ToolCalls()),
				"message_id", assistant.ID,
				"model", assistant.Model,
				"provider", assistant.Provider,
				"finish_reason", assistant.FinishReason(),
				"text_has_protocol", textHasProtocol,
				"reasoning_has_protocol", reasoningHasProtocol,
				"text_preview", previewText(text, 500),
				"reasoning_preview", previewText(reasoning, 500),
			)
		} else {
			slog.Debug("Should not retry for textual tool-call protocol: assistant already has structured tool calls",
				"tool_calls_count", len(assistant.ToolCalls()),
				"message_id", assistant.ID,
			)
		}
		return false
	}
	reason := assistant.FinishReason()
	if reason != message.FinishReasonEndTurn &&
		reason != message.FinishReasonUnknown &&
		reason != message.FinishReasonToolUse {
		slog.Debug("Should not retry for textual tool-call protocol: finish reason indicates non-tool state",
			"finish_reason", reason,
			"message_id", assistant.ID,
		)
		return false
	}
	text := assistant.Content().Text
	reasoning := assistant.ReasoningContent().Thinking
	textNeedsRetry := hasTextualToolCallProtocol(text)
	reasoningNeedsRetry := hasTextualToolCallProtocol(reasoning)
	needsRetry := textNeedsRetry || reasoningNeedsRetry
	if needsRetry {
		slog.Warn("Assistant emitted textual tool-call protocol without structured tool calls; will retry request",
			"message_id", assistant.ID,
			"model", assistant.Model,
			"provider", assistant.Provider,
			"finish_reason", reason,
			"text_has_protocol", textNeedsRetry,
			"reasoning_has_protocol", reasoningNeedsRetry,
			"text_preview", previewText(text, 500),
			"reasoning_preview", previewText(reasoning, 500),
		)
	} else {
		slog.Debug("Checked textual tool-call protocol for retry",
			"needs_retry", needsRetry,
			"message_id", assistant.ID,
			"finish_reason", reason,
			"text_preview", previewText(text, 200),
			"reasoning_preview", previewText(reasoning, 200),
		)
	}
	return needsRetry
}

func (a *sessionAgent) cleanupFailedAttempt(ctx context.Context, assistant *message.Message, toolMessageIDs []string) error {
	for _, toolMessageID := range toolMessageIDs {
		if err := a.messages.Delete(ctx, toolMessageID); err != nil {
			return err
		}
	}
	if assistant == nil {
		return nil
	}
	return a.messages.Delete(ctx, assistant.ID)
}

// shouldRetryForEmptyStreamResponse detects the case where the upstream API
// returned HTTP 200 but the SSE stream contained no content — just a [DONE]
// sentinel or an immediately-terminated stream that maps to
// finish_reason=unknown with zero output. This is distinct from a legitimate
// end_turn or tool_use completion: the response has no text, no reasoning, and
// no tool calls. Retrying is safe because no tool side-effects occurred.
func shouldRetryForEmptyStreamResponse(assistant *message.Message) bool {
	if assistant == nil {
		return false
	}
	if assistant.FinishReason() != message.FinishReasonUnknown {
		return false
	}
	if assistant.Content().Text != "" {
		return false
	}
	if assistant.ReasoningContent().Thinking != "" {
		return false
	}
	if len(assistant.ToolCalls()) > 0 {
		return false
	}
	slog.Warn("Detected empty response stream (finish_reason=unknown, no content); will retry",
		"message_id", assistant.ID,
		"model", assistant.Model,
		"provider", assistant.Provider,
	)
	return true
}
