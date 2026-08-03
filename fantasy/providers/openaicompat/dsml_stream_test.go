package openaicompat

import (
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

func TestTransformDSMLStreamRecoversSplitTextToolCall(t *testing.T) {
	t.Parallel()

	parts := collectDSMLStreamParts(TransformDSMLStream(streamParts(
		fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "text"},
		fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: "before<｜DS"},
		fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: "ML｜tool_calls><｜DSML｜invoke name=\"read\">"},
		fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: "<｜DSML｜parameter name=\"path\" string=\"true\">docs/a&amp;b.md</｜DSML｜parameter>"},
		fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: "</｜DSML｜invoke></｜DSML｜tool_calls>after"},
		fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "text"},
		fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop},
	)))

	require.Equal(t, "beforeafter", streamText(parts))
	require.NotContains(t, streamText(parts), "DSML")
	toolCalls := streamPartsOfType(parts, fantasy.StreamPartTypeToolCall)
	require.Len(t, toolCalls, 1)
	require.True(t, strings.HasPrefix(toolCalls[0].ID, "call_dsml_"))
	require.Equal(t, "read", toolCalls[0].ToolCallName)
	require.JSONEq(t, `{"path":"docs/a&b.md"}`, toolCalls[0].ToolCallInput)
	require.Equal(t, fantasy.FinishReasonToolCalls, parts[len(parts)-1].FinishReason)
}

func TestTransformDSMLStreamRecoversReasoningAndMultipleCalls(t *testing.T) {
	t.Parallel()

	parts := collectDSMLStreamParts(TransformDSMLStream(streamParts(
		fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningStart, ID: "reasoning"},
		fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningDelta, ID: "reasoning", Delta: `<|DSML|tool_calls><|DSML|invoke name="read"><|DSML|parameter name="offset">12</|DSML|parameter></|DSML|invoke>`},
		fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningDelta, ID: "reasoning", Delta: `<|DSML|invoke name="status"></|DSML|invoke></|DSML|tool_calls>`},
		fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop},
	)))

	require.Empty(t, streamText(parts))
	require.Empty(t, streamReasoning(parts))
	toolCalls := streamPartsOfType(parts, fantasy.StreamPartTypeToolCall)
	require.Len(t, toolCalls, 2)
	require.Equal(t, "read", toolCalls[0].ToolCallName)
	require.JSONEq(t, `{"offset":12}`, toolCalls[0].ToolCallInput)
	require.Equal(t, "status", toolCalls[1].ToolCallName)
	require.JSONEq(t, `{}`, toolCalls[1].ToolCallInput)
	require.Equal(t, fantasy.FinishReasonToolCalls, parts[len(parts)-1].FinishReason)
}

func TestTransformDSMLStreamPreservesMalformedAndPartialText(t *testing.T) {
	t.Parallel()

	malformed := `<｜DSML｜tool_calls><｜DSML｜invoke name="read"><｜DSML｜parameter name="path">main.go</｜DSML｜invoke></｜DSML｜tool_calls>`
	parts := collectDSMLStreamParts(TransformDSMLStream(streamParts(
		fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "text"},
		fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: malformed},
		fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: " trailing <｜DSM"},
		fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "text"},
		fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop},
	)))

	require.Equal(t, malformed+" trailing <｜DSM", streamText(parts))
	require.Empty(t, streamPartsOfType(parts, fantasy.StreamPartTypeToolCall))
	require.Equal(t, fantasy.FinishReasonStop, parts[len(parts)-1].FinishReason)
}

func TestTransformDSMLStreamPrefersNativeToolCalls(t *testing.T) {
	t.Parallel()

	dsml := `<｜DSML｜tool_calls><｜DSML｜invoke name="read"><｜DSML｜parameter name="path" string="true">main.go</｜DSML｜parameter></｜DSML｜invoke></｜DSML｜tool_calls>`
	parts := collectDSMLStreamParts(TransformDSMLStream(streamParts(
		fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "text"},
		fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: dsml},
		fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "text"},
		fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputStart, ID: "native", ToolCallName: "read"},
		fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputDelta, ID: "native", Delta: `{"path":"main.go"}`},
		fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputEnd, ID: "native"},
		fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "native", ToolCallName: "read", ToolCallInput: `{"path":"main.go"}`},
		fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls},
	)))

	require.Empty(t, streamText(parts))
	toolCalls := streamPartsOfType(parts, fantasy.StreamPartTypeToolCall)
	require.Len(t, toolCalls, 1)
	require.Equal(t, "native", toolCalls[0].ID)
	require.Equal(t, fantasy.FinishReasonToolCalls, parts[len(parts)-1].FinishReason)
}

func TestTransformDSMLStreamUsesUniqueIDsAcrossStreams(t *testing.T) {
	t.Parallel()

	dsml := `<|DSML|tool_calls><|DSML|invoke name="status"></|DSML|invoke></|DSML|tool_calls>`
	transform := func() fantasy.StreamPart {
		parts := collectDSMLStreamParts(TransformDSMLStream(streamParts(
			fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "text"},
			fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: dsml},
			fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop},
		)))
		calls := streamPartsOfType(parts, fantasy.StreamPartTypeToolCall)
		require.Len(t, calls, 1)
		return calls[0]
	}

	require.NotEqual(t, transform().ID, transform().ID)
}

func TestTransformDSMLStreamStopsWhenConsumerStops(t *testing.T) {
	t.Parallel()

	stream := TransformDSMLStream(streamParts(
		fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "text"},
		fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: "visible"},
		fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "text"},
	))
	count := 0
	stream(func(fantasy.StreamPart) bool {
		count++
		return false
	})
	require.Equal(t, 1, count)
}

func streamParts(parts ...fantasy.StreamPart) fantasy.StreamResponse {
	return func(yield func(fantasy.StreamPart) bool) {
		for _, part := range parts {
			if !yield(part) {
				return
			}
		}
	}
}

func collectDSMLStreamParts(stream fantasy.StreamResponse) []fantasy.StreamPart {
	var parts []fantasy.StreamPart
	stream(func(part fantasy.StreamPart) bool {
		parts = append(parts, part)
		return true
	})
	return parts
}

func streamPartsOfType(parts []fantasy.StreamPart, partType fantasy.StreamPartType) []fantasy.StreamPart {
	var matches []fantasy.StreamPart
	for _, part := range parts {
		if part.Type == partType {
			matches = append(matches, part)
		}
	}
	return matches
}

func streamText(parts []fantasy.StreamPart) string {
	var text strings.Builder
	for _, part := range parts {
		if part.Type == fantasy.StreamPartTypeTextDelta {
			text.WriteString(part.Delta)
		}
	}
	return text.String()
}

func streamReasoning(parts []fantasy.StreamPart) string {
	var text strings.Builder
	for _, part := range parts {
		if part.Type == fantasy.StreamPartTypeReasoningDelta {
			text.WriteString(part.Delta)
		}
	}
	return text.String()
}
