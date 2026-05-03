package agent

import (
	"errors"
	"strings"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
)

func disableAnthropicThinking(opts fantasy.ProviderOptions) (fantasy.ProviderOptions, bool) {
	anthropicOpts, ok := opts[anthropic.Name].(*anthropic.ProviderOptions)
	if !ok || anthropicOpts == nil || anthropicOpts.Thinking == nil {
		return opts, false
	}

	cloned := make(fantasy.ProviderOptions, len(opts))
	for k, v := range opts {
		cloned[k] = v
	}
	sanitized := *anthropicOpts
	sanitized.Thinking = nil
	cloned[anthropic.Name] = &sanitized
	return cloned, true
}

func isAnthropicStyleProtocolProvider(model Model) bool {
	return isAnthropicStyleUsageProvider(model.ModelCfg.Provider) || isAnthropicStyleUsageProvider(usageProvider(model))
}

func sanitizeAnthropicToolCallID(id string) string {
	if id == "" {
		return id
	}
	var sb strings.Builder
	sb.Grow(len(id))
	for _, r := range id {
		if (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '_' ||
			r == '-' {
			sb.WriteRune(r)
			continue
		}
		sb.WriteByte('_')
	}
	return sb.String()
}

func sanitizeAnthropicToolCallIDsInMessages(messages []fantasy.Message) ([]fantasy.Message, bool, int) {
	var changed bool
	var count int
	result := make([]fantasy.Message, len(messages))
	copy(result, messages)
	for i := range result {
		if len(result[i].Content) == 0 {
			continue
		}
		var clonedParts []fantasy.MessagePart
		for j, part := range result[i].Content {
			switch p := part.(type) {
			case fantasy.ToolCallPart:
				sanitized := sanitizeAnthropicToolCallID(p.ToolCallID)
				if sanitized == p.ToolCallID {
					continue
				}
				if clonedParts == nil {
					clonedParts = append([]fantasy.MessagePart(nil), result[i].Content...)
				}
				p.ToolCallID = sanitized
				clonedParts[j] = p
				changed = true
				count++
			case *fantasy.ToolCallPart:
				if p == nil {
					continue
				}
				sanitized := sanitizeAnthropicToolCallID(p.ToolCallID)
				if sanitized == p.ToolCallID {
					continue
				}
				if clonedParts == nil {
					clonedParts = append([]fantasy.MessagePart(nil), result[i].Content...)
				}
				cloned := *p
				cloned.ToolCallID = sanitized
				clonedParts[j] = &cloned
				changed = true
				count++
			case fantasy.ToolResultPart:
				sanitized := sanitizeAnthropicToolCallID(p.ToolCallID)
				if sanitized == p.ToolCallID {
					continue
				}
				if clonedParts == nil {
					clonedParts = append([]fantasy.MessagePart(nil), result[i].Content...)
				}
				p.ToolCallID = sanitized
				clonedParts[j] = p
				changed = true
				count++
			case *fantasy.ToolResultPart:
				if p == nil {
					continue
				}
				sanitized := sanitizeAnthropicToolCallID(p.ToolCallID)
				if sanitized == p.ToolCallID {
					continue
				}
				if clonedParts == nil {
					clonedParts = append([]fantasy.MessagePart(nil), result[i].Content...)
				}
				cloned := *p
				cloned.ToolCallID = sanitized
				clonedParts[j] = &cloned
				changed = true
				count++
			}
		}
		if clonedParts != nil {
			result[i].Content = clonedParts
		}
	}
	if !changed {
		return messages, false, 0
	}
	return result, true, count
}

// stripRedactedThinkingParts removes Anthropic redacted_thinking reasoning
// blocks from assistant messages. Some Anthropic-compatible proxies (e.g.
// third-party OpenAI/Claude bridges) do not implement the
// `redacted_thinking` content-block type and reject requests that contain
// it with a 422 Unprocessable Entity. Stripping these blocks lets the
// conversation continue; the signed/plaintext thinking blocks and all
// other content are preserved.
func stripRedactedThinkingParts(messages []fantasy.Message) ([]fantasy.Message, bool) {
	changed := false
	for i := range messages {
		if messages[i].Role != fantasy.MessageRoleAssistant {
			continue
		}
		src := messages[i].Content
		if len(src) == 0 {
			continue
		}
		filtered := make([]fantasy.MessagePart, 0, len(src))
		stripped := false
		for _, p := range src {
			if rp, ok := p.(fantasy.ReasoningPart); ok && isAnthropicRedactedReasoning(rp) {
				stripped = true
				continue
			}
			filtered = append(filtered, p)
		}
		if stripped {
			messages[i].Content = filtered
			changed = true
		}
	}
	return messages, changed
}

func isAnthropicRedactedReasoning(rp fantasy.ReasoningPart) bool {
	if rp.ProviderOptions == nil {
		return false
	}
	meta, ok := rp.ProviderOptions[anthropic.Name]
	if !ok {
		return false
	}
	m, ok := meta.(*anthropic.ReasoningOptionMetadata)
	if !ok || m == nil {
		return false
	}
	// A redacted block is one with opaque data and no signature. Signed
	// thinking blocks are legitimate across all Anthropic-compatible
	// endpoints and must not be stripped.
	return m.Signature == "" && m.RedactedData != ""
}

// shouldRetryWithoutRedactedThinking detects proxies that reject the
// Anthropic `redacted_thinking` content block. Typical signatures:
//   - 422 Unprocessable Entity with Pydantic union validation listing
//     only ClaudeContentBlockText/Image/ToolUse/ToolResult/Thinking.
//   - Any error message explicitly mentioning "redacted_thinking".
func shouldRetryWithoutRedactedThinking(err error) bool {
	var providerErr *fantasy.ProviderError
	if !errors.As(err, &providerErr) || providerErr == nil {
		return false
	}
	if providerErr.StatusCode != 422 && providerErr.StatusCode != 400 {
		return false
	}
	msg := strings.ToLower(providerErr.Message)
	if msg == "" {
		return false
	}
	if strings.Contains(msg, "redacted_thinking") {
		return true
	}
	// Pydantic union-mismatch rejection: the proxy lists accepted Claude
	// content block types (without a redacted variant) and complains the
	// input is not one of them.
	if strings.Contains(msg, "claudecontentblock") &&
		strings.Contains(msg, "unprocessable entity") {
		return true
	}
	if strings.Contains(msg, "claudecontentblock") &&
		(strings.Contains(msg, "input should be") || strings.Contains(msg, "union")) &&
		strings.Contains(msg, "thinking") {
		return true
	}
	return false
}

func shouldRetryWithoutAnthropicThinking(err error, opts fantasy.ProviderOptions) bool {
	anthropicOpts, ok := opts[anthropic.Name].(*anthropic.ProviderOptions)
	if !ok || anthropicOpts == nil || anthropicOpts.Thinking == nil {
		return false
	}
	var providerErr *fantasy.ProviderError
	if !errors.As(err, &providerErr) || providerErr == nil {
		return false
	}
	if providerErr.StatusCode != 400 {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(providerErr.Message))
	if msg == "" {
		return false
	}
	if strings.Contains(msg, "thinking is enabled but reasoning_content is missing") {
		return true
	}
	// Some Anthropic-compatible proxies enforce that every assistant turn
	// in history carries its original `thinking` block when thinking mode
	// is on. After an ESC interruption, an assistant turn may be persisted
	// without a signed thinking block, causing errors like:
	//   "The content[].thinking in the thinking mode must be passed back
	//    to the API."
	// Treat these as retryable by disabling thinking for the retry.
	if strings.Contains(msg, "thinking") &&
		(strings.Contains(msg, "must be passed back") ||
			strings.Contains(msg, "passed back to the api")) {
		return true
	}
	hasThinking := strings.Contains(msg, "thinking")
	hasReasoning := strings.Contains(msg, "reasoning_content") || strings.Contains(msg, "reasoning content")
	hasMissing := strings.Contains(msg, "missing") || strings.Contains(msg, "required")
	isToolContext := strings.Contains(msg, "tool call") || strings.Contains(msg, "tool_use") || strings.Contains(msg, "tool use")
	return hasThinking && hasReasoning && hasMissing && isToolContext
}
