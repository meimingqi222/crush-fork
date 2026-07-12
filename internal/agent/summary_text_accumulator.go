package agent

import (
	"strings"

	"github.com/charmbracelet/crush/internal/message"
)

// summaryTextAccumulator strips streaming <think> blocks from summarize output.
type summaryTextAccumulator struct {
	raw strings.Builder
}

func (a *summaryTextAccumulator) reset() {
	a.raw.Reset()
}

func (a *summaryTextAccumulator) appendDelta(msg *message.Message, delta string) {
	a.raw.WriteString(delta)
	stripped := stripStreamingThinkTags(a.raw.String())
	msg.SetContent(stripped)
}
