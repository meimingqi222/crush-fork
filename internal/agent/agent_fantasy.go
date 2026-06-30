package agent

import (
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
)

func mergeConsecutiveSameRoleFantasyMessages(msgs []fantasy.Message) []fantasy.Message {
	if len(msgs) < 2 {
		return msgs
	}
	out := make([]fantasy.Message, 0, len(msgs))
	for _, m := range msgs {
		if len(out) == 0 {
			out = append(out, m)
			continue
		}
		prev := &out[len(out)-1]
		if prev.Role != m.Role || prev.Role == fantasy.MessageRoleSystem {
			out = append(out, m)
			continue
		}
		// Same role, non-system: concatenate content with adjacent
		// duplicate text-part suppression.
		for _, part := range m.Content {
			if isDuplicateAdjacentTextPart(prev.Content, part) {
				continue
			}
			prev.Content = append(prev.Content, part)
		}
		// Preserve cache_control / provider hints from the latest
		// message in the merged group. Anthropic's cache_control marker
		// must sit on the tail of the user turn to be effective.
		if m.ProviderOptions != nil {
			prev.ProviderOptions = m.ProviderOptions
		}
	}
	return out
}

// isDuplicateAdjacentTextPart returns true if appending `part` to `existing`
// would produce two adjacent text parts with identical text. This drops the
// typical "继续 继续 继续" repetition without affecting tool_result, file,
// source, or interleaved text+other-content patterns.
func isDuplicateAdjacentTextPart(existing []fantasy.MessagePart, part fantasy.MessagePart) bool {
	if len(existing) == 0 {
		return false
	}
	newText, ok := fantasy.AsMessagePart[fantasy.TextPart](part)
	if !ok {
		return false
	}
	prevText, ok := fantasy.AsMessagePart[fantasy.TextPart](existing[len(existing)-1])
	if !ok {
		return false
	}
	return strings.TrimSpace(newText.Text) == strings.TrimSpace(prevText.Text) && strings.TrimSpace(newText.Text) != ""
}

// stripImagePartsFromFantasyMessages removes all image content from fantasy
// messages for models that do not support image inputs. This prevents
// "invalid content type" errors when conversation history contains images
// recorded during a previous session with a vision-capable model.
// FilePart entries in user messages are replaced with a text placeholder that
// preserves the filename and MIME type. Media tool results are replaced with a
// text placeholder.
func stripImagePartsFromFantasyMessages(messages []fantasy.Message) []fantasy.Message {
	result := make([]fantasy.Message, 0, len(messages))
	for _, msg := range messages {
		switch msg.Role {
		case fantasy.MessageRoleUser:
			filtered := make([]fantasy.MessagePart, 0, len(msg.Content))
			for _, part := range msg.Content {
				if filePart := filePartFromMessagePart(part); filePart != nil {
					filtered = append(filtered, fantasy.TextPart{Text: imageAttachmentPlaceholder(filePart.Filename, filePart.MediaType, false)})
					continue
				}
				filtered = append(filtered, part)
			}
			if len(filtered) == 0 {
				continue
			}
			msg.Content = filtered
			result = append(result, msg)
		case fantasy.MessageRoleTool:
			filtered := make([]fantasy.MessagePart, 0, len(msg.Content))
			for _, part := range msg.Content {
				tr, ok := part.(fantasy.ToolResultPart)
				if !ok {
					filtered = append(filtered, part)
					continue
				}
				if _, isMedia := tr.Output.(fantasy.ToolResultOutputContentMedia); isMedia {
					tr.Output = fantasy.ToolResultOutputContentText{
						Text: "[Image/media content not supported by current model]",
					}
				}
				filtered = append(filtered, tr)
			}
			msg.Content = filtered
			result = append(result, msg)
		default:
			result = append(result, msg)
		}
	}
	return result
}

// stripImagePartsFromFantasyMessagesWithVision works like
// stripImagePartsFromFantasyMessages but, when a VisionDescriber is provided,
// replaces FilePart entries in user messages with a placeholder that tells the
// model it can call the describe_image tool to obtain a text description of the
// image instead of preprocessing the image eagerly.
func stripImagePartsFromFantasyMessagesWithVision(messages []fantasy.Message, vision tools.VisionDescriber) []fantasy.Message {
	if vision == nil || !vision.IsAvailable() {
		return stripImagePartsFromFantasyMessages(messages)
	}
	result := make([]fantasy.Message, 0, len(messages))
	for _, msg := range messages {
		switch msg.Role {
		case fantasy.MessageRoleUser:
			filtered := make([]fantasy.MessagePart, 0, len(msg.Content))
			for _, part := range msg.Content {
				if filePart := filePartFromMessagePart(part); filePart != nil {
					filtered = append(filtered, fantasy.TextPart{Text: imageAttachmentPlaceholder(filePart.Filename, filePart.MediaType, true)})
					continue
				}
				filtered = append(filtered, part)
			}
			if len(filtered) == 0 {
				continue
			}
			msg.Content = filtered
			result = append(result, msg)
		case fantasy.MessageRoleTool:
			filtered := make([]fantasy.MessagePart, 0, len(msg.Content))
			for _, part := range msg.Content {
				tr, ok := part.(fantasy.ToolResultPart)
				if !ok {
					filtered = append(filtered, part)
					continue
				}
				if _, isMedia := tr.Output.(fantasy.ToolResultOutputContentMedia); isMedia {
					tr.Output = fantasy.ToolResultOutputContentText{
						Text: "[Image/media content not supported by current model]",
					}
				}
				filtered = append(filtered, tr)
			}
			msg.Content = filtered
			result = append(result, msg)
		default:
			result = append(result, msg)
		}
	}
	return result
}

func filePartFromMessagePart(part fantasy.MessagePart) *fantasy.FilePart {
	if fp, ok := part.(fantasy.FilePart); ok {
		return &fp
	}
	if fp, ok := part.(*fantasy.FilePart); ok && fp != nil {
		return fp
	}
	return nil
}

func imageAttachmentPlaceholder(filename, mimeType string, hasVision bool) string {
	name := filename
	if name == "" {
		name = "image"
	}
	if hasVision {
		return fmt.Sprintf("[Image attachment: %s (%s). Use the describe_image tool if you need a description.]", name, mimeType)
	}
	return fmt.Sprintf("[Image attachment: %s (%s). The current model does not support images and no vision helper is configured.]", name, mimeType)
}

func imageAttachmentPlaceholderForMessage(filename, mimeType, messageID string, imageIndex int, hasVision bool) string {
	name := filename
	if name == "" {
		name = "image"
	}
	if hasVision {
		if messageID != "" && imageIndex > 0 {
			return fmt.Sprintf("[Image attachment: %s (%s). Use the describe_image tool with message_id=%q and image_index=%d if you need a description.]", name, mimeType, messageID, imageIndex)
		}
		return imageAttachmentPlaceholder(name, mimeType, true)
	}
	return fmt.Sprintf("[Image attachment: %s (%s). The current model does not support images and no vision helper is configured.]", name, mimeType)
}

// promptWithImageAttachmentPlaceholders returns a user prompt that includes
// text attachments inline and appends placeholder notes for any image
// attachments. This is used for the initial request to non-vision models,
// where the image is sent via Files/Prompt rather than as a FilePart in the
// message history (which stripImagePartsFromFantasyMessages would handle).
func promptWithImageAttachmentPlaceholders(prompt string, attachments []message.Attachment, hasVision bool) string {
	return promptWithImageAttachmentPlaceholdersForMessage(prompt, attachments, "", hasVision)
}

func promptWithImageAttachmentPlaceholdersForMessage(prompt string, attachments []message.Attachment, messageID string, hasVision bool) string {
	prompt = message.PromptWithTextAttachments(prompt, attachments)
	var imagePlaceholders []string
	imageIndex := 0
	for _, att := range attachments {
		if !att.IsImage() {
			continue
		}
		imageIndex++
		name := att.FileName
		if name == "" {
			name = att.FilePath
		}
		imagePlaceholders = append(imagePlaceholders, imageAttachmentPlaceholderForMessage(name, att.MimeType, messageID, imageIndex, hasVision))
	}
	if len(imagePlaceholders) == 0 {
		return prompt
	}
	if prompt == "" {
		return strings.Join(imagePlaceholders, "\n\n")
	}
	return prompt + "\n\n" + strings.Join(imagePlaceholders, "\n\n")
}

// describeImageToolSystemPromptNote returns a short system prompt note that
// reminds the model it can call describe_image for any image attachment when
// the primary model does not support vision inputs.
func describeImageToolSystemPromptNote() string {
	return `You do not have direct vision capabilities. When the user includes image attachments, use the describe_image tool with the attachment's message_id and image_index to obtain a text description. Use the filename path only when no message_id is shown. You may call it multiple times if there are several images.`
}

// stripToolCallPartsFromFantasyMessages removes tool-call parts from
// assistant messages and drops tool-result messages entirely. This is used
// for auxiliary flows (such as prompt enhancement) that send sanitized chat
// history without the corresponding tool execution results: strict
// OpenAI-compatible providers reject assistant messages whose tool_calls
// have no matching tool response, so leaving the parts in place produces
// HTTP 400 errors.
//
// Assistant messages that become empty after stripping are dropped so that
// providers do not see an assistant turn with no content.
func stripToolCallPartsFromFantasyMessages(messages []fantasy.Message) []fantasy.Message {
	result := make([]fantasy.Message, 0, len(messages))
	for _, msg := range messages {
		switch msg.Role {
		case fantasy.MessageRoleAssistant:
			filtered := make([]fantasy.MessagePart, 0, len(msg.Content))
			hasMeaningful := false
			for _, part := range msg.Content {
				if _, ok := part.(fantasy.ToolCallPart); ok {
					continue
				}
				if _, ok := part.(*fantasy.ToolCallPart); ok {
					continue
				}
				filtered = append(filtered, part)
				switch p := part.(type) {
				case fantasy.TextPart:
					if strings.TrimSpace(p.Text) != "" {
						hasMeaningful = true
					}
				case *fantasy.TextPart:
					if p != nil && strings.TrimSpace(p.Text) != "" {
						hasMeaningful = true
					}
				case fantasy.ReasoningPart:
					if strings.TrimSpace(p.Text) != "" {
						hasMeaningful = true
					}
				case *fantasy.ReasoningPart:
					if p != nil && strings.TrimSpace(p.Text) != "" {
						hasMeaningful = true
					}
				}
			}
			if !hasMeaningful {
				continue
			}
			msg.Content = filtered
			result = append(result, msg)
		case fantasy.MessageRoleTool:
			// Drop tool result messages entirely: their matching tool_calls
			// have already been stripped from the preceding assistant turn.
			continue
		default:
			result = append(result, msg)
		}
	}
	return result
}

// buildSummaryPrompt constructs the prompt text for session summarization.
func buildSummaryPrompt(todos []session.Todo) string {
	var sb strings.Builder
	sb.WriteString("Provide a detailed summary of our conversation above.")
	if len(todos) > 0 {
		sb.WriteString("\n\n## Tracked Tasks\n\n")
		for _, todo := range todos {
			fmt.Fprintf(&sb, "- [%s] %s\n", todo.Status, todo.Content)
		}
		sb.WriteString("\nInclude these tasks and their statuses in your summary.")
	}
	return sb.String()
}

func hasAutoRecallInMessages(messages []fantasy.Message) bool {
	for _, msg := range messages {
		if msg.Role != fantasy.MessageRoleUser && msg.Role != fantasy.MessageRoleSystem {
			continue
		}
		for _, part := range msg.Content {
			var text string
			if textPart, ok := part.(fantasy.TextPart); ok {
				text = textPart.Text
			} else if textPartPtr, ok := part.(*fantasy.TextPart); ok && textPartPtr != nil {
				text = textPartPtr.Text
			}
			if text != "" {
				if strings.Contains(text, "<system-reminder>") {
					return true
				}
			}
		}
	}
	return false
}
