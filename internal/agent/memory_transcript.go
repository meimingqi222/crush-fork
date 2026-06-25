package agent

import (
	"regexp"
	"strings"

	"github.com/charmbracelet/crush/internal/message"
)

const (
	// transcriptWindowMaxRunes limits transcript windows by Unicode code points.
	transcriptWindowMaxRunes = 12_000
)

// memoryTagPatterns are regex patterns for memory-related XML blocks that must
// be stripped from transcript content before retention. Without stripping,
// previously injected memory blocks (recall results, mental models) would be
// retained back into Hindsight, creating a positive feedback loop where
// memories are repeatedly re-transcribed, amplified, and self-referenced.
//
// Mirrors oh-my-pi's stripMemoryTags approach, extended with crush's
// <system-reminder> wrapper that frames auto-recall injections.
var memoryTagPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`),
	regexp.MustCompile(`(?s)<hindsight_memories>.*?</hindsight_memories>`),
	regexp.MustCompile(`(?s)<mental_models>.*?</mental_models>`),
	regexp.MustCompile(`(?s)<relevant_memories>.*?</relevant_memories>`),
	regexp.MustCompile(`(?s)<memories>.*?</memories>`),
}

// stripMemoryTags removes memory-related XML blocks from content. This prevents
// previously injected recall/mental-model blocks from being retained back into
// the memory backend, which would create a feedback loop.
func stripMemoryTags(content string) string {
	for _, re := range memoryTagPatterns {
		content = re.ReplaceAllString(content, "")
	}
	return content
}

// buildTranscriptWindow constructs a transcript window from recent messages.
// It truncates to transcriptWindowMaxRunes to keep the retained window bounded.
// Memory injection tags are stripped to prevent feedback loops on retain.
func buildTranscriptWindow(msgs []message.Message) string {
	var lines []string
	totalRunes := 0

	// Walk backwards from most recent messages.
	for i := len(msgs) - 1; i >= 0; i-- {
		msg := msgs[i]
		var line string
		switch msg.Role {
		case message.User:
			if text := strings.TrimSpace(stripMemoryTags(msg.Content().Text)); text != "" {
				line = "USER: " + text
			}
		case message.Assistant:
			if text := strings.TrimSpace(stripMemoryTags(msg.Content().Text)); text != "" {
				line = "ASSISTANT: " + text
			}
		}
		if line == "" {
			continue
		}
		lineRunes := []rune(line)
		lineLen := len(lineRunes)
		if len(lines) > 0 {
			lineLen++
		}
		if totalRunes+lineLen > transcriptWindowMaxRunes {
			if len(lines) == 0 {
				line = string(lineRunes[:transcriptWindowMaxRunes])
				lineLen = transcriptWindowMaxRunes
			} else {
				break
			}
		}
		// Prepend to maintain order.
		lines = append([]string{line}, lines...)
		totalRunes += lineLen
	}

	return strings.Join(lines, "\n")
}
