package agent

import "strings"

const thinkTagOpen = "<think>"

// stripStreamingThinkTags removes complete <think>…</think> blocks and any
// trailing unclosed <think> segment still being streamed.
func stripStreamingThinkTags(s string) string {
	s = thinkTagRegex.ReplaceAllString(s, "")
	if idx := strings.LastIndex(s, thinkTagOpen); idx >= 0 {
		if strings.Index(s[idx:], "</think>") < 0 {
			s = s[:idx]
		}
	}
	return s
}
