package message

import (
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
)

type MessageRole string

const (
	Assistant MessageRole = "assistant"
	User      MessageRole = "user"
	System    MessageRole = "system"
	Tool      MessageRole = "tool"
)

type FinishReason string

const (
	FinishReasonEndTurn          FinishReason = "end_turn"
	FinishReasonMaxTokens        FinishReason = "max_tokens"
	FinishReasonToolUse          FinishReason = "tool_use"
	FinishReasonCanceled         FinishReason = "canceled"
	FinishReasonError            FinishReason = "error"
	FinishReasonPermissionDenied FinishReason = "permission_denied"
	// FinishReasonBudgetExceeded marks a run that was force-aborted by the
	// soft/hard request-step budget. Distinct from generic error so callers
	// can skip fruitless yield retries — the model already had its steer
	// chance and ignored it.
	FinishReasonBudgetExceeded FinishReason = "budget_exceeded"

	// Should never happen
	FinishReasonUnknown FinishReason = "unknown"
)

type ContentPart interface {
	isPart()
}

var textualToolCallProtocolBlockRegexes = []*regexp.Regexp{
	regexp.MustCompile(`(?is)<\|tool_calls_section_begin\|>.*?(?:<\|tool_calls_section_end\|>|$)`),
	regexp.MustCompile(`(?is)<[|｜]DSML[|｜]tool_calls\s*>.*?(?:</[|｜]DSML[|｜]tool_calls\s*>|$)`),
}

func StripTextualToolCallProtocol(text string) (string, bool) {
	cleaned := text
	for _, blockRegex := range textualToolCallProtocolBlockRegexes {
		cleaned = blockRegex.ReplaceAllString(cleaned, "")
	}
	cleaned = strings.TrimSpace(cleaned)
	return cleaned, cleaned != text
}

var internalDisplayBlockRegexes = []*regexp.Regexp{
	regexp.MustCompile(`(?is)<think>.*?(?:</think>|$)`),
	regexp.MustCompile(`(?is)<system-reminder>.*?</system-reminder>`),
	regexp.MustCompile(`(?is)<system_intent_gate_caveat>.*?</system_intent_gate_caveat>`),
	regexp.MustCompile(`(?is)<hindsight_memories>.*?</hindsight_memories>`),
	regexp.MustCompile(`(?is)<mental_models>.*?</mental_models>`),
	regexp.MustCompile(`(?is)<relevant_memories>.*?</relevant_memories>`),
	regexp.MustCompile(`(?is)<memories>.*?</memories>`),
}

var (
	internalTaskNotificationBlockRegex = regexp.MustCompile(`(?is)<task-notification>.*?</task-notification>`)
	internalTaskNotificationTagRegex   = regexp.MustCompile(`(?i)</?(?:task-notification|task-id|status|summary|result)>`)
)

// DisplayText returns the display-safe form of a user or assistant text part.
// It removes only known internal protocols; ordinary user-authored XML or
// Markdown is left unchanged. Tool messages do not use this function because
// their renderers own the display of tool calls and results.
func DisplayText(text string) (string, bool) {
	cleaned := text
	if guided, ok := displayGuidedGoalText(cleaned); ok {
		cleaned = guided
	}
	for _, blockRegex := range internalDisplayBlockRegexes {
		cleaned = blockRegex.ReplaceAllString(cleaned, "")
	}
	cleaned, _ = StripTextualToolCallProtocol(cleaned)
	cleaned = internalTaskNotificationBlockRegex.ReplaceAllStringFunc(cleaned, func(block string) string {
		return internalTaskNotificationTagRegex.ReplaceAllString(block, "")
	})
	cleaned = strings.TrimSpace(cleaned)
	return cleaned, cleaned != text
}

func displayGuidedGoalText(text string) (string, bool) {
	trimmed := strings.TrimSpace(text)
	const (
		openTag    = "<guided_goal>"
		closeTag   = "</guided_goal>"
		roughLabel = "\nRough goal from the user:\n"
		rulesLabel = "\n\nRules:\n"
	)
	if !strings.HasPrefix(trimmed, openTag) || !strings.HasSuffix(trimmed, closeTag) {
		return text, false
	}

	body := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trimmed, openTag), closeTag))
	roughStart := strings.Index(body, roughLabel)
	if roughStart < 0 {
		return text, false
	}
	roughStart += len(roughLabel)
	rulesStart := strings.LastIndex(body, rulesLabel)
	if rulesStart < roughStart {
		return text, false
	}
	rough := strings.TrimSpace(body[roughStart:rulesStart])
	if rough == "" {
		return text, false
	}

	return "Guided goal:\n" + rough, true
}

type ReasoningContent struct {
	Thinking         string                             `json:"thinking"`
	Signature        string                             `json:"signature"`
	ThoughtSignature string                             `json:"thought_signature"` // Used for google
	ToolID           string                             `json:"tool_id"`           // Used for openrouter google models
	ResponsesData    *openai.ResponsesReasoningMetadata `json:"responses_data"`
	StartedAt        int64                              `json:"started_at,omitempty"`
	FinishedAt       int64                              `json:"finished_at,omitempty"`
}

func (tc ReasoningContent) String() string {
	return tc.Thinking
}
func (ReasoningContent) isPart() {}

type TextContent struct {
	Text string `json:"text"`
}

func (tc TextContent) String() string {
	return tc.Text
}

func (TextContent) isPart() {}

type ImageURLContent struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

func (iuc ImageURLContent) String() string {
	return iuc.URL
}

func (ImageURLContent) isPart() {}

type BinaryContent struct {
	Path     string
	MIMEType string
	Data     []byte
}

func (bc BinaryContent) String(p catwalk.InferenceProvider) string {
	base64Encoded := base64.StdEncoding.EncodeToString(bc.Data)
	if p == catwalk.InferenceProviderOpenAI {
		return "data:" + bc.MIMEType + ";base64," + base64Encoded
	}
	return base64Encoded
}

func (BinaryContent) isPart() {}

type ToolCall struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Input            string `json:"input"`
	ProviderExecuted bool   `json:"provider_executed"`
	Finished         bool   `json:"finished"`
}

func (ToolCall) isPart() {}

type ToolResult struct {
	ToolCallID string `json:"tool_call_id"`
	Name       string `json:"name"`
	Content    string `json:"content"`
	Data       string `json:"data"`
	MIMEType   string `json:"mime_type"`
	Metadata   string `json:"metadata"`
	IsError    bool   `json:"is_error"`

	AutoReviewMeta     ToolResultAutoReview     `json:"-"`
	SubtaskResultMeta  ToolResultSubtaskResult  `json:"-"`
	SubagentFinishMeta ToolResultSubagentFinish `json:"-"`
	YieldMeta          ToolResultYield          `json:"-"`
	ReducerMeta        ToolResultReducer        `json:"-"`
}

func (ToolResult) isPart() {}

type Finish struct {
	Reason  FinishReason `json:"reason"`
	Time    int64        `json:"time"`
	Message string       `json:"message,omitempty"`
	Details string       `json:"details,omitempty"`
}

func (Finish) isPart() {}

type Usage struct {
	InputTokens      int64 `json:"input_tokens,omitempty"`
	OutputTokens     int64 `json:"output_tokens,omitempty"`
	ReasoningTokens  int64 `json:"reasoning_tokens,omitempty"`
	CacheReadTokens  int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
}

func UsageFromFantasy(usage fantasy.Usage) Usage {
	return Usage{
		InputTokens:      usage.InputTokens,
		OutputTokens:     usage.OutputTokens,
		ReasoningTokens:  usage.ReasoningTokens,
		CacheReadTokens:  usage.CacheReadTokens,
		CacheWriteTokens: usage.CacheCreationTokens,
	}
}

func (u Usage) PromptTokens() int64 {
	return u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

func (u Usage) CompletionTokens() int64 {
	return u.OutputTokens + u.ReasoningTokens
}

func (u Usage) TotalTokens() int64 {
	return u.PromptTokens() + u.CompletionTokens()
}

func (u Usage) HasDisplayOutput() bool {
	return u.OutputTokens > 0
}

type Message struct {
	ID                     string
	Role                   MessageRole
	SessionID              string
	Parts                  []ContentPart
	Usage                  Usage
	Model                  string
	Provider               string
	CreatedAt              int64
	UpdatedAt              int64
	IsSummaryMessage       bool
	ActivatedDeferredTools []string
}

func (m *Message) Content() TextContent {
	for _, part := range m.Parts {
		if c, ok := part.(TextContent); ok {
			return c
		}
	}
	return TextContent{}
}

func (m *Message) ReasoningContent() ReasoningContent {
	for _, part := range m.Parts {
		if c, ok := part.(ReasoningContent); ok {
			return c
		}
	}
	return ReasoningContent{}
}

func (m *Message) ImageURLContent() []ImageURLContent {
	imageURLContents := make([]ImageURLContent, 0)
	for _, part := range m.Parts {
		if c, ok := part.(ImageURLContent); ok {
			imageURLContents = append(imageURLContents, c)
		}
	}
	return imageURLContents
}

func (m *Message) BinaryContent() []BinaryContent {
	binaryContents := make([]BinaryContent, 0)
	for _, part := range m.Parts {
		if c, ok := part.(BinaryContent); ok {
			binaryContents = append(binaryContents, c)
		}
	}
	return binaryContents
}

func (m *Message) ToolCalls() []ToolCall {
	toolCalls := make([]ToolCall, 0)
	for _, part := range m.Parts {
		if c, ok := part.(ToolCall); ok {
			toolCalls = append(toolCalls, c)
		}
	}
	return toolCalls
}

func (m *Message) ToolResults() []ToolResult {
	toolResults := make([]ToolResult, 0)
	for _, part := range m.Parts {
		if c, ok := part.(ToolResult); ok {
			toolResults = append(toolResults, c)
		}
	}
	return toolResults
}

func (m *Message) IsFinished() bool {
	for _, part := range m.Parts {
		if _, ok := part.(Finish); ok {
			return true
		}
	}
	return false
}

func (m *Message) FinishPart() *Finish {
	for _, part := range m.Parts {
		if c, ok := part.(Finish); ok {
			return &c
		}
	}
	return nil
}

func (m *Message) FinishReason() FinishReason {
	for _, part := range m.Parts {
		if c, ok := part.(Finish); ok {
			return c.Reason
		}
	}
	return ""
}

func (m *Message) IsThinking() bool {
	if m.ReasoningContent().Thinking != "" &&
		m.Content().Text == "" &&
		len(m.ToolCalls()) == 0 &&
		!m.IsFinished() {
		return true
	}
	return false
}

func (m *Message) AppendContent(delta string) {
	for i, part := range m.Parts {
		if c, ok := part.(TextContent); ok {
			m.Parts[i] = TextContent{Text: c.Text + delta}
			return
		}
	}
	m.Parts = append(m.Parts, TextContent{Text: delta})
}

func (m *Message) SetContent(text string) {
	for i, part := range m.Parts {
		if _, ok := part.(TextContent); ok {
			if text == "" {
				m.Parts = slices.Delete(m.Parts, i, i+1)
			} else {
				m.Parts[i] = TextContent{Text: text}
			}
			return
		}
	}
	if text != "" {
		m.Parts = append(m.Parts, TextContent{Text: text})
	}
}

func (m *Message) AppendReasoningContent(delta string) {
	found := false
	for i, part := range m.Parts {
		if c, ok := part.(ReasoningContent); ok {
			m.Parts[i] = ReasoningContent{
				Thinking:         c.Thinking + delta,
				Signature:        c.Signature,
				ThoughtSignature: c.ThoughtSignature,
				ToolID:           c.ToolID,
				ResponsesData:    c.ResponsesData,
				StartedAt:        c.StartedAt,
				FinishedAt:       c.FinishedAt,
			}
			found = true
		}
	}
	if !found {
		m.Parts = append(m.Parts, ReasoningContent{
			Thinking:  delta,
			StartedAt: time.Now().Unix(),
		})
	}
}

func (m *Message) SetReasoningThinking(thinking string) {
	for i, part := range m.Parts {
		if c, ok := part.(ReasoningContent); ok {
			m.Parts[i] = ReasoningContent{
				Thinking:         thinking,
				Signature:        c.Signature,
				ThoughtSignature: c.ThoughtSignature,
				ToolID:           c.ToolID,
				ResponsesData:    c.ResponsesData,
				StartedAt:        c.StartedAt,
				FinishedAt:       c.FinishedAt,
			}
			return
		}
	}
	if thinking != "" {
		m.Parts = append(m.Parts, ReasoningContent{
			Thinking:  thinking,
			StartedAt: time.Now().Unix(),
		})
	}
}

func (m *Message) AppendThoughtSignature(signature string, toolCallID string) {
	for i, part := range m.Parts {
		if c, ok := part.(ReasoningContent); ok {
			m.Parts[i] = ReasoningContent{
				Thinking:         c.Thinking,
				ThoughtSignature: c.ThoughtSignature + signature,
				ToolID:           toolCallID,
				Signature:        c.Signature,
				StartedAt:        c.StartedAt,
				FinishedAt:       c.FinishedAt,
			}
			return
		}
	}
	m.Parts = append(m.Parts, ReasoningContent{ThoughtSignature: signature})
}

func (m *Message) AppendReasoningSignature(signature string) {
	for i, part := range m.Parts {
		if c, ok := part.(ReasoningContent); ok {
			m.Parts[i] = ReasoningContent{
				Thinking:         c.Thinking,
				Signature:        c.Signature + signature,
				ThoughtSignature: c.ThoughtSignature,
				ToolID:           c.ToolID,
				ResponsesData:    c.ResponsesData,
				StartedAt:        c.StartedAt,
				FinishedAt:       c.FinishedAt,
			}
			return
		}
	}
	m.Parts = append(m.Parts, ReasoningContent{Signature: signature})
}

func (m *Message) SetReasoningResponsesData(data *openai.ResponsesReasoningMetadata) {
	for i, part := range m.Parts {
		if c, ok := part.(ReasoningContent); ok {
			m.Parts[i] = ReasoningContent{
				Thinking:         c.Thinking,
				Signature:        c.Signature,
				ThoughtSignature: c.ThoughtSignature,
				ToolID:           c.ToolID,
				ResponsesData:    data,
				StartedAt:        c.StartedAt,
				FinishedAt:       c.FinishedAt,
			}
			return
		}
	}
}

func (m *Message) FinishThinking() {
	for i, part := range m.Parts {
		if c, ok := part.(ReasoningContent); ok {
			if c.FinishedAt == 0 {
				m.Parts[i] = ReasoningContent{
					Thinking:         c.Thinking,
					Signature:        c.Signature,
					ThoughtSignature: c.ThoughtSignature,
					ToolID:           c.ToolID,
					ResponsesData:    c.ResponsesData,
					StartedAt:        c.StartedAt,
					FinishedAt:       time.Now().Unix(),
				}
			}
			return
		}
	}
}

// StripReasoningParts removes all reasoning parts from the message. Used for
// context-summary messages so summarizer reasoning is not persisted or sent as
// conversation history.
func (m *Message) StripReasoningParts() {
	if len(m.Parts) == 0 {
		return
	}
	filtered := m.Parts[:0]
	for _, part := range m.Parts {
		if _, ok := part.(ReasoningContent); ok {
			continue
		}
		filtered = append(filtered, part)
	}
	m.Parts = filtered
}

func (m *Message) ThinkingDuration() time.Duration {
	reasoning := m.ReasoningContent()
	if reasoning.StartedAt == 0 {
		return 0
	}

	endTime := reasoning.FinishedAt
	if endTime == 0 {
		endTime = time.Now().Unix()
	}

	return time.Duration(endTime-reasoning.StartedAt) * time.Second
}

func (m *Message) FinishToolCall(toolCallID string) {
	for i, part := range m.Parts {
		if c, ok := part.(ToolCall); ok {
			if c.ID == toolCallID {
				m.Parts[i] = ToolCall{
					ID:       c.ID,
					Name:     c.Name,
					Input:    c.Input,
					Finished: true,
				}
				return
			}
		}
	}
}

func (m *Message) AppendToolCallInput(toolCallID string, inputDelta string) {
	for i, part := range m.Parts {
		if c, ok := part.(ToolCall); ok {
			if c.ID == toolCallID {
				m.Parts[i] = ToolCall{
					ID:       c.ID,
					Name:     c.Name,
					Input:    c.Input + inputDelta,
					Finished: c.Finished,
				}
				return
			}
		}
	}
}

func (m *Message) AddToolCall(tc ToolCall) {
	for i, part := range m.Parts {
		if c, ok := part.(ToolCall); ok {
			if c.ID == tc.ID {
				m.Parts[i] = tc
				return
			}
		}
	}
	m.Parts = append(m.Parts, tc)
}

func (m *Message) SetToolCalls(tc []ToolCall) {
	// remove any existing tool call part it could have multiple
	parts := make([]ContentPart, 0)
	for _, part := range m.Parts {
		if _, ok := part.(ToolCall); ok {
			continue
		}
		parts = append(parts, part)
	}
	m.Parts = parts
	for _, toolCall := range tc {
		m.Parts = append(m.Parts, toolCall)
	}
}

func (m *Message) AddToolResult(tr ToolResult) {
	m.Parts = append(m.Parts, tr)
}

func (m *Message) SetUsage(usage Usage) {
	m.Usage = usage
}

func (m *Message) SetToolResults(tr []ToolResult) {
	for _, toolResult := range tr {
		m.Parts = append(m.Parts, toolResult)
	}
}

// Clone returns a deep copy of the message with an independent Parts slice.
// This prevents race conditions when the message is modified concurrently.
func (m *Message) Clone() Message {
	clone := *m
	clone.Parts = make([]ContentPart, len(m.Parts))
	copy(clone.Parts, m.Parts)
	clone.ActivatedDeferredTools = append([]string(nil), m.ActivatedDeferredTools...)
	return clone
}

func (m *Message) AddFinish(reason FinishReason, message, details string) {
	// remove any existing finish part
	for i, part := range m.Parts {
		if _, ok := part.(Finish); ok {
			m.Parts = slices.Delete(m.Parts, i, i+1)
			break
		}
	}
	m.Parts = append(m.Parts, Finish{Reason: reason, Time: time.Now().Unix(), Message: message, Details: details})
}

func (m *Message) AddImageURL(url, detail string) {
	m.Parts = append(m.Parts, ImageURLContent{URL: url, Detail: detail})
}

func (m *Message) AddBinary(mimeType string, data []byte) {
	// Copy data to prevent external modifications from affecting the message
	copiedData := make([]byte, len(data))
	copy(copiedData, data)
	m.Parts = append(m.Parts, BinaryContent{MIMEType: mimeType, Data: copiedData})
}

func PromptWithTextAttachments(prompt string, attachments []Attachment) string {
	var sb strings.Builder
	sb.WriteString(prompt)
	addedAttachments := false
	for _, content := range attachments {
		if !content.IsText() {
			continue
		}
		if !addedAttachments {
			sb.WriteString("\n<system_info>The files below have been attached by the user, consider them in your response</system_info>\n")
			addedAttachments = true
		}
		if content.FilePath != "" {
			fmt.Fprintf(&sb, "<file path='%s'>\n", content.FilePath)
		} else {
			sb.WriteString("<file>\n")
		}
		sb.WriteString("\n")
		sb.Write(content.Content)
		sb.WriteString("\n</file>\n")
	}
	return sb.String()
}

func (m *Message) ToAIMessage() []fantasy.Message {
	var messages []fantasy.Message
	metadata := newFantasyMessageMetadata(*m)
	switch m.Role {
	case System:
		if _, ok := ParseAutoModePrompt(*m); ok {
			return nil
		}
		text := strings.TrimSpace(m.Content().Text)
		if text == "" {
			return nil
		}
		messages = append(messages, fantasy.Message{
			Role: fantasy.MessageRoleSystem,
			Content: attachFantasyMessageMetadata([]fantasy.MessagePart{
				fantasy.TextPart{Text: text},
			}, metadata),
		})
	case User:
		var parts []fantasy.MessagePart
		text := strings.TrimSpace(m.Content().Text)
		var textAttachments []Attachment
		for _, content := range m.BinaryContent() {
			if !strings.HasPrefix(content.MIMEType, "text/") {
				continue
			}
			textAttachments = append(textAttachments, Attachment{
				FilePath: content.Path,
				MimeType: content.MIMEType,
				Content:  content.Data,
			})
		}
		text = PromptWithTextAttachments(text, textAttachments)
		if text != "" {
			parts = append(parts, fantasy.TextPart{Text: text})
		}
		for _, content := range m.BinaryContent() {
			// skip text attachements
			if strings.HasPrefix(content.MIMEType, "text/") {
				continue
			}
			parts = append(parts, fantasy.FilePart{
				Filename:  content.Path,
				Data:      content.Data,
				MediaType: content.MIMEType,
			})
		}
		parts = attachFantasyMessageMetadata(parts, metadata)
		messages = append(messages, fantasy.Message{
			Role:    fantasy.MessageRoleUser,
			Content: parts,
		})
	case Assistant:
		var parts []fantasy.MessagePart
		text := strings.TrimSpace(m.Content().Text)
		if strings.HasPrefix(text, "Service temporarily unavailable. Retrying") {
			return nil
		}
		reasoning := m.ReasoningContent()
		text, _ = StripTextualToolCallProtocol(text)
		thinking, _ := StripTextualToolCallProtocol(reasoning.Thinking)
		hasToolCalls := len(m.ToolCalls()) > 0
		if thinking != "" {
			reasoningPart := fantasy.ReasoningPart{Text: thinking, ProviderOptions: fantasy.ProviderOptions{}}
			if reasoning.Signature != "" {
				reasoningPart.ProviderOptions[anthropic.Name] = &anthropic.ReasoningOptionMetadata{
					Signature: reasoning.Signature,
				}
			}
			if reasoning.ResponsesData != nil {
				reasoningPart.ProviderOptions[openai.Name] = reasoning.ResponsesData
			}
			if reasoning.ThoughtSignature != "" {
				reasoningPart.ProviderOptions[google.Name] = &google.ReasoningMetadata{
					Signature: reasoning.ThoughtSignature,
					ToolID:    reasoning.ToolID,
				}
			}
			if len(reasoningPart.ProviderOptions) > 0 {
				parts = append(parts, reasoningPart)
			} else if text == "" && !hasToolCalls {
				// Reasoning has no provider-specific signature/metadata and there is
				// no text content or tool call to carry the response. Without this
				// fallback, the assistant turn would be sent as empty content,
				// causing the model to lose its own prior reply from history (seen
				// with DeepSeek's Anthropic-compatible proxy, which can return
				// reasoning blocks without signatures and no separate text block).
				text = thinking
				parts = append(parts, reasoningPart)
			} else {
				// No provider-specific metadata (not Anthropic signed, Google, or
				// OpenAI Responses) but there is text content and/or tool calls
				// alongside the reasoning. Include as a plain ReasoningPart so
				// that OpenAI-compatible providers (e.g. DeepSeek, MiMo, or other
				// reasoning models via the native OpenAI-compat API) receive the
				// required reasoning_content field when conversation history is
				// passed back to the API. Providers that do not use this field
				// (e.g. Anthropic without metadata) will emit a soft warning and
				// skip it.
				parts = append(parts, reasoningPart)
			}
		}
		if text != "" {
			parts = append(parts, fantasy.TextPart{Text: text})
		}
		for _, call := range m.ToolCalls() {
			parts = append(parts, fantasy.ToolCallPart{
				ToolCallID:       call.ID,
				ToolName:         call.Name,
				Input:            call.Input,
				ProviderExecuted: call.ProviderExecuted,
			})
		}
		parts = attachFantasyMessageMetadata(parts, metadata)
		if len(parts) == 0 {
			return nil
		}
		messages = append(messages, fantasy.Message{
			Role:    fantasy.MessageRoleAssistant,
			Content: parts,
		})
	case Tool:
		var parts []fantasy.MessagePart
		for _, result := range m.ToolResults() {
			var content fantasy.ToolResultOutputContent
			if result.IsError {
				content = fantasy.ToolResultOutputContentError{
					Error: errors.New(result.ModelSafeContent()),
				}
			} else if result.Data != "" {
				content = fantasy.ToolResultOutputContentMedia{
					Data:      result.Data,
					MediaType: result.MIMEType,
				}
			} else {
				content = fantasy.ToolResultOutputContentText{
					Text: result.ModelSafeContent(),
				}
			}
			parts = append(parts, fantasy.ToolResultPart{
				ToolCallID: result.ToolCallID,
				Output:     content,
			})
		}
		parts = attachFantasyMessageMetadata(parts, metadata)
		messages = append(messages, fantasy.Message{
			Role:    fantasy.MessageRoleTool,
			Content: parts,
		})
	}
	return messages
}

// FromFantasyMessages converts fantasy.Messages to message.Messages.
// This is used when we need to pass messages to plugins that expect the internal message format.
func FromFantasyMessages(msgs []fantasy.Message) []Message {
	result := make([]Message, 0, len(msgs))
	for _, msg := range msgs {
		m := Message{
			Parts: make([]ContentPart, 0),
		}
		if metadata, ok := fantasyMessageMetadataFromMessage(msg); ok {
			applyFantasyMessageMetadata(&m, metadata)
		}
		switch msg.Role {
		case fantasy.MessageRoleSystem:
			m.Role = System
			for _, part := range msg.Content {
				if tp, ok := part.(fantasy.TextPart); ok {
					m.Parts = append(m.Parts, TextContent{Text: tp.Text})
				}
			}
		case fantasy.MessageRoleUser:
			m.Role = User
			for _, part := range msg.Content {
				switch p := part.(type) {
				case fantasy.TextPart:
					m.Parts = append(m.Parts, TextContent{Text: p.Text})
				case fantasy.FilePart:
					m.Parts = append(m.Parts, BinaryContent{
						Path:     p.Filename,
						MIMEType: p.MediaType,
						Data:     p.Data,
					})
				}
			}
		case fantasy.MessageRoleAssistant:
			m.Role = Assistant
			hasToolCall := false
			for _, part := range msg.Content {
				switch p := part.(type) {
				case fantasy.TextPart:
					m.Parts = append(m.Parts, TextContent{Text: p.Text})
				case fantasy.ReasoningPart:
					rc := ReasoningContent{Thinking: p.Text}
					if p.ProviderOptions != nil {
						if anthMeta, ok := p.ProviderOptions[anthropic.Name].(*anthropic.ReasoningOptionMetadata); ok {
							rc.Signature = anthMeta.Signature
						}
						if respData, ok := p.ProviderOptions[openai.Name].(*openai.ResponsesReasoningMetadata); ok {
							rc.ResponsesData = respData
						}
						if googleMeta, ok := p.ProviderOptions[google.Name].(*google.ReasoningMetadata); ok {
							rc.ThoughtSignature = googleMeta.Signature
							rc.ToolID = googleMeta.ToolID
						}
					}
					m.Parts = append(m.Parts, rc)
				case fantasy.ToolCallPart:
					hasToolCall = true
					m.Parts = append(m.Parts, ToolCall{
						ID:               p.ToolCallID,
						Name:             p.ToolName,
						Input:            p.Input,
						ProviderExecuted: p.ProviderExecuted,
					})
				}
			}
			// Synthesize a Finish part for assistant turns reconstructed from
			// fantasy messages. Any assistant turn that has already been
			// converted into fantasy form represents a completed turn (either
			// a final reply or a step that produced tool calls). Without this,
			// downstream consumers (e.g. trimCanceledPromptBranches) would see
			// FinishReason() == "" and incorrectly classify the message as an
			// orphaned/interrupted turn, dropping the entire prior conversation
			// turn before the next user message.
			if len(m.Parts) > 0 && !m.IsFinished() {
				reason := FinishReasonEndTurn
				if hasToolCall {
					reason = FinishReasonToolUse
				}
				m.Parts = append(m.Parts, Finish{Reason: reason})
			}
		case fantasy.MessageRoleTool:
			m.Role = Tool
			for _, part := range msg.Content {
				if tr, ok := part.(fantasy.ToolResultPart); ok {
					result := ToolResult{
						ToolCallID: tr.ToolCallID,
					}
					switch out := tr.Output.(type) {
					case fantasy.ToolResultOutputContentText:
						result.Content = out.Text
					case fantasy.ToolResultOutputContentError:
						result.Content = out.Error.Error()
						result.IsError = true
					case fantasy.ToolResultOutputContentMedia:
						result.Data = out.Data
						result.MIMEType = out.MediaType
					}
					m.Parts = append(m.Parts, result)
				}
			}
		}
		if len(m.Parts) > 0 || m.Role == System {
			result = append(result, m)
		}
	}
	return result
}

// FilterNonTextContent filters out non-text content from messages for non-multimodal models.
// This removes ImageURLContent and BinaryContent parts from all messages.
func FilterNonTextContent(msgs []Message) []Message {
	filtered := make([]Message, 0, len(msgs))
	for _, msg := range msgs {
		filteredMsg := msg
		filteredMsg.Parts = make([]ContentPart, 0, len(msg.Parts))
		for _, part := range msg.Parts {
			// Skip non-text content types (images, binary files, etc.)
			if _, ok := part.(ImageURLContent); ok {
				continue
			}
			if _, ok := part.(BinaryContent); ok {
				continue
			}
			filteredMsg.Parts = append(filteredMsg.Parts, part)
		}
		filtered = append(filtered, filteredMsg)
	}
	return filtered
}

// CountNonTextContent counts the number of non-text content parts in messages.
func CountNonTextContent(msgs []Message) int {
	count := 0
	for _, msg := range msgs {
		for _, part := range msg.Parts {
			if _, ok := part.(ImageURLContent); ok {
				count++
			}
			if _, ok := part.(BinaryContent); ok {
				count++
			}
		}
	}
	return count
}
