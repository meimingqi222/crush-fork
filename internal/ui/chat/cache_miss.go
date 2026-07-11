package chat

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
)

// CacheMissDividerItem is a slim divider shown above an assistant turn that
// lost a warm Anthropic-style prompt cache.
type CacheMissDividerItem struct {
	sty               *styles.Styles
	assistantID       string
	reprocessedTokens int64
}

func NewCacheMissDividerItem(sty *styles.Styles, assistantID string, reprocessedTokens int64) MessageItem {
	return &CacheMissDividerItem{sty: sty, assistantID: assistantID, reprocessedTokens: reprocessedTokens}
}

func (c *CacheMissDividerItem) ID() string {
	return "cache-miss-" + c.assistantID
}

func (c *CacheMissDividerItem) RawRender(width int) string {
	return c.Render(width)
}

func (c *CacheMissDividerItem) Render(width int) string {
	label := fmt.Sprintf("cache miss · %s tokens", strings.ToLower(common.FormatTokenCount(c.reprocessedTokens)))
	rule := c.sty.Chat.Message.SummaryHeaderLine.Render("──────────")
	return rule + " " + c.sty.Subtle.Render(label)
}

// AssistantUsageBefore returns the usage of the last assistant message strictly
// before index idx in msgs.
func AssistantUsageBefore(msgs []message.Message, idx int) (message.Usage, bool) {
	for i := idx - 1; i >= 0; i-- {
		if msgs[i].Role != message.Assistant {
			continue
		}
		if msgs[i].Usage.PromptTokens()+msgs[i].Usage.OutputTokens <= 0 {
			continue
		}
		return msgs[i].Usage, true
	}
	return message.Usage{}, false
}