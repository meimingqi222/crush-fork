package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
)

// deferredToolStateFromToolSearchResult extracts deferred tool state from a tool_search result.
func deferredToolStateFromToolSearchResult(content string) (message.ToolResultDeferredToolState, bool) {
	var payload tools.ToolSearchResponse
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		return message.ToolResultDeferredToolState{}, false
	}
	state := message.ToolResultDeferredToolState{
		ActivatedTools: payload.ActivatedTools,
		RecoveryAction: strings.TrimSpace(payload.ActivationHint),
	}
	if state.ActivatedTools == nil && strings.TrimSpace(state.RecoveryAction) == "" {
		return message.ToolResultDeferredToolState{}, false
	}
	return state, true
}

// deferredToolStateFromToolError extracts deferred tool state from an error response.
func deferredToolStateFromToolError(content string, metadata string) (message.ToolResultDeferredToolState, bool) {
	for _, candidate := range []string{metadata, content} {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		var payload struct {
			Error               string   `json:"error"`
			RecoveredBy         string   `json:"recovered_by"`
			Tool                string   `json:"tool"`
			RecoveryAction      string   `json:"recovery_action"`
			SuggestedToolSearch string   `json:"suggested_tool_search"`
			FallbackTool        string   `json:"fallback_tool"`
			FallbackToolQuery   string   `json:"fallback_tool_query"`
			RecoveredParameters []string `json:"recovered_parameters"`
		}
		if err := json.Unmarshal([]byte(candidate), &payload); err != nil {
			continue
		}
		if payload.Error != "deferred_tool_not_activated" && payload.RecoveredBy != "deferred_tool_not_activated" {
			continue
		}
		state := message.ToolResultDeferredToolState{
			RecoveredTool:       strings.TrimSpace(payload.Tool),
			RecoveryAction:      strings.TrimSpace(payload.RecoveryAction),
			SuggestedTool:       strings.TrimSpace(payload.Tool),
			SuggestedToolQuery:  strings.TrimSpace(payload.SuggestedToolSearch),
			FallbackTool:        strings.TrimSpace(payload.FallbackTool),
			FallbackToolQuery:   strings.TrimSpace(payload.FallbackToolQuery),
			RecoveredParameters: payload.RecoveredParameters,
		}
		if state.SuggestedToolQuery == "" {
			state.SuggestedToolQuery = state.FallbackToolQuery
		}
		if state.SuggestedTool == "" {
			state.SuggestedTool = state.FallbackTool
		}
		if state.RecoveredTool == "" && state.RecoveryAction == "" && state.FallbackTool == "" {
			continue
		}
		return state, true
	}
	return message.ToolResultDeferredToolState{}, false
}

func (a *sessionAgent) convertToToolResult(ctx context.Context, result fantasy.ToolResultContent) message.ToolResult {
	baseResult := message.ToolResult{
		ToolCallID: result.ToolCallID,
		Name:       result.ToolName,
		Metadata:   result.ClientMetadata,
	}

	switch result.Result.GetType() {
	case fantasy.ToolResultContentTypeText:
		if r, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](result.Result); ok {
			baseResult.Content = r.Text
			if state := protocolStateFromRecoveryMetadata("", baseResult.Metadata); state.RecoveryAction != "" || state.FallbackTool != "" || state.FallbackToolQuery != "" || len(state.RecoveredParameters) > 0 {
				baseResult = baseResult.WithDeferredToolState(state)
			}
		}
	case fantasy.ToolResultContentTypeError:
		if r, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](result.Result); ok {
			baseResult.Content = r.Error.Error()
			baseResult.IsError = true

			// Smart semantic error interceptor for Orchestrate (coordinator) mode.
			// Replaces tech jargon ("tool not found") with actionable high-agency role reminders.
			if strings.Contains(baseResult.Content, "tool not found: edit") ||
				strings.Contains(baseResult.Content, "tool not found: write") ||
				strings.Contains(baseResult.Content, "tool not found: bash") {
				sessionID := tools.GetSessionFromContext(ctx)
				if sessionID != "" {
					if sess, err := a.sessions.Get(ctx, sessionID); err == nil && sess.CollaborationMode == session.CollaborationModeOrchestrate {
						baseResult.Content = fmt.Sprintf(
							"Failed: The %q tool is not available in Orchestrate (coordinator) mode. "+
								"You decompose, dispatch, verify, and iterate. You do NOT edit code or run mutating commands directly. "+
								"Every file mutation and code implementation MUST go through a specialized subagent (e.g. by using the \"agent\" tool). "+
								"Do not attempt to call %q directly in this session.",
							baseResult.Name, baseResult.Name,
						)
					}
				}
			}

			if state, ok := deferredToolStateFromToolError(baseResult.Content, baseResult.Metadata); ok {
				baseResult = baseResult.WithDeferredToolState(state)
			}
		}
	case fantasy.ToolResultContentTypeMedia:
		if r, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](result.Result); ok {
			content := r.Text
			if content == "" {
				content = fmt.Sprintf("Loaded %s content", r.MediaType)
			}
			baseResult.Content = content
			baseResult.Data = r.Data
			baseResult.MIMEType = r.MediaType
		}
	}

	return baseResult
}

const (
	mcpAdditionalMediaMetadataKey = "mcp_additional_media"
)

type additionalMediaItem struct {
	Type      string `json:"type"`
	Data      string `json:"data"`
	MediaType string `json:"media_type"`
}

func (a *sessionAgent) extractAdditionalMCPMedia(toolResult message.ToolResult) (message.ToolResult, []message.BinaryContent) {
	if toolResult.Metadata == "" {
		return toolResult, nil
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(toolResult.Metadata), &payload); err != nil {
		return toolResult, nil
	}

	rawAdditional, ok := payload[mcpAdditionalMediaMetadataKey]
	if !ok {
		return toolResult, nil
	}

	var additional []additionalMediaItem
	if err := json.Unmarshal(rawAdditional, &additional); err != nil {
		slog.Warn("Failed to decode MCP additional media metadata", "error", err, "tool_name", toolResult.Name, "tool_call_id", toolResult.ToolCallID)
		return toolResult, nil
	}

	delete(payload, mcpAdditionalMediaMetadataKey)
	if len(payload) == 0 {
		toolResult.Metadata = ""
	} else if cleaned, err := json.Marshal(payload); err != nil {
		slog.Warn("Failed to re-encode tool metadata after removing additional media", "error", err, "tool_name", toolResult.Name, "tool_call_id", toolResult.ToolCallID)
	} else {
		toolResult.Metadata = string(cleaned)
	}

	media := make([]message.BinaryContent, 0, len(additional))
	for index, item := range additional {
		if strings.TrimSpace(item.Data) == "" {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(item.Data)
		if err != nil {
			slog.Warn("Failed to decode additional MCP media payload", "error", err, "tool_name", toolResult.Name, "tool_call_id", toolResult.ToolCallID)
			continue
		}
		mediaType := strings.TrimSpace(item.MediaType)
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		media = append(media, message.BinaryContent{
			Path:     fmt.Sprintf("tool-result-%s-extra-%d", toolResult.ToolCallID, index+1),
			MIMEType: mediaType,
			Data:     decoded,
		})
	}

	if len(media) == 0 {
		return toolResult, nil
	}
	return toolResult, media
}

// workaroundProviderMediaLimitations converts media content in tool results to
// user messages for providers that don't natively support images in tool results.
//
// Problem: OpenAI, Google, OpenRouter, and other OpenAI-compatible providers
// don't support sending images/media in tool result messages - they only accept
// text in tool results. However, they DO support images in user messages.
//
// If we send media in tool results to these providers, the API returns an error.
//
// Solution: For these providers, we:
//  1. Replace the media in the tool result with a text placeholder
//  2. Inject a user message immediately after with the image as a file attachment
//  3. This maintains the tool execution flow while working around API limitations
//
// Anthropic and Bedrock support images natively in tool results, so we skip
// this workaround for them.
//
// Example transformation:
//
//	BEFORE: [tool result: image data]
//	AFTER:  [tool result: "Image loaded - see attached"], [user: image attachment]
func (a *sessionAgent) workaroundProviderMediaLimitations(messages []fantasy.Message, largeModel Model) []fantasy.Message {
	providerSupportsMedia := largeModel.ModelCfg.Provider == string(catwalk.InferenceProviderAnthropic) ||
		largeModel.ModelCfg.Provider == string(catwalk.InferenceProviderBedrock)

	if providerSupportsMedia {
		return messages
	}

	convertedMessages := make([]fantasy.Message, 0, len(messages))

	for _, msg := range messages {
		if msg.Role != fantasy.MessageRoleTool {
			convertedMessages = append(convertedMessages, msg)
			continue
		}

		textParts := make([]fantasy.MessagePart, 0, len(msg.Content))
		var mediaFiles []fantasy.FilePart

		for _, part := range msg.Content {
			toolResult, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
			if !ok {
				textParts = append(textParts, part)
				continue
			}

			if media, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](toolResult.Output); ok {
				decoded, err := base64.StdEncoding.DecodeString(media.Data)
				if err != nil {
					slog.Warn("Failed to decode media data", "error", err)
					textParts = append(textParts, part)
					continue
				}

				mediaFiles = append(mediaFiles, fantasy.FilePart{
					Data:      decoded,
					MediaType: media.MediaType,
					Filename:  fmt.Sprintf("tool-result-%s", toolResult.ToolCallID),
				})

				textParts = append(textParts, fantasy.ToolResultPart{
					ToolCallID: toolResult.ToolCallID,
					Output: fantasy.ToolResultOutputContentText{
						Text: "[Image/media content loaded - see attached file]",
					},
					ProviderOptions: toolResult.ProviderOptions,
				})
			} else {
				textParts = append(textParts, part)
			}
		}

		convertedMessages = append(convertedMessages, fantasy.Message{
			Role:    fantasy.MessageRoleTool,
			Content: textParts,
		})

		if len(mediaFiles) > 0 {
			convertedMessages = append(convertedMessages, fantasy.NewUserMessage(
				"Here is the media content from the tool result:",
				mediaFiles...,
			))
		}
	}

	return convertedMessages
}
