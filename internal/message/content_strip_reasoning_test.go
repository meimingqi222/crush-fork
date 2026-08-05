package message

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStripReasoningParts(t *testing.T) {
	t.Parallel()

	msg := Message{
		Parts: []ContentPart{
			ReasoningContent{Thinking: "hidden"},
			TextContent{Text: "summary body"},
		},
	}
	msg.StripReasoningParts()
	require.Equal(t, "summary body", msg.Content().Text)
	require.Empty(t, msg.ReasoningContent().Thinking)
}
