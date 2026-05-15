package agent

import (
	"strings"

	"github.com/charmbracelet/crush/internal/message"
)

const (
	// transcriptWindowMaxRunes limits transcript windows by Unicode code points.
	transcriptWindowMaxRunes = 12_000
)

// buildTranscriptWindow constructs a transcript window from recent messages.
// It truncates to transcriptWindowMaxRunes to keep the retained window bounded.
func buildTranscriptWindow(msgs []message.Message) string {
	var lines []string
	totalRunes := 0

	// Walk backwards from most recent messages.
	for i := len(msgs) - 1; i >= 0; i-- {
		msg := msgs[i]
		var line string
		switch msg.Role {
		case message.User:
			if text := strings.TrimSpace(msg.Content().Text); text != "" {
				line = "USER: " + text
			}
		case message.Assistant:
			if text := strings.TrimSpace(msg.Content().Text); text != "" {
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
