package agent

import (
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestHasRepetitiveLines(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		text         string
		wantRepeated bool
		wantLine     string
		wantMinCount int
	}{
		{
			name:         "empty text",
			text:         "",
			wantRepeated: false,
		},
		{
			name:         "normal text without repetition",
			text:         "Hello world\nThis is a test\nEverything looks good",
			wantRepeated: false,
		},
		{
			name: "7 identical import lines - screenshot scenario",
			text: strings.Join([]string{
				`import type { Context } from "hono"`,
				`import type { Context } from "hono"`,
				`import type { Context } from "hono"`,
				`import type { Context } from "hono"`,
				`import type { Context } from "hono"`,
				`import type { Context } from "hono"`,
				`import type { Context } from "hono"`,
			}, "\n"),
			wantRepeated: true,
			wantLine:     `import type { Context } from "hono"`,
			wantMinCount: 7,
		},
		{
			name: "repeated lines below ratio threshold",
			text: strings.Join([]string{
				"line one",
				"line two",
				"line three",
				"line four",
				"line five",
				"line six",
				"line seven",
				"line eight",
				"line nine",
				"line one",
				"line one",
			}, "\n"),
			wantRepeated: false, // 3/11 = 0.27 < 0.5
		},
		{
			name: "repeated lines above ratio threshold",
			text: strings.Join([]string{
				"line one",
				"line one",
				"line one",
				"line two",
			}, "\n"),
			wantRepeated: true, // 3/4 = 0.75 > 0.5
			wantLine:     "line one",
			wantMinCount: 3,
		},
		{
			name:         "short lines ignored",
			text:         "ok\nok\nok\nok\nok",
			wantRepeated: false, // "ok" is 2 runes < 4 minimum
		},
		{
			name:         "blank lines ignored",
			text:         "\n\n\n\n\n",
			wantRepeated: false,
		},
		{
			name: "mixed with blank lines and short lines",
			text: strings.Join([]string{
				"",
				"ok",
				`console.log("hello")`,
				"",
				`console.log("hello")`,
				"}",
				`console.log("hello")`,
				"",
			}, "\n"),
			wantRepeated: true, // 3/3 = 1.0 (only qualifying lines)
			wantLine:     `console.log("hello")`,
			wantMinCount: 3,
		},
		{
			name: "legitimate code with some repeated patterns",
			text: strings.Join([]string{
				"import React from 'react'",
				"import { useState } from 'react'",
				"import { useEffect } from 'react'",
				"",
				"function App() {",
				"  const [count, setCount] = useState(0)",
				"  useEffect(() => {",
				"    document.title = `Count: ${count}`",
				"  }, [count])",
				"  return <div>{count}</div>",
				"}",
			}, "\n"),
			wantRepeated: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repeated, line, count, _ := hasRepetitiveLines(tc.text)
			require.Equal(t, tc.wantRepeated, repeated, "repeated flag")
			if tc.wantRepeated {
				require.Equal(t, tc.wantLine, line, "repeated line")
				require.GreaterOrEqual(t, count, tc.wantMinCount, "repeat count")
			}
		})
	}
}

func TestIsDegenerateOutput(t *testing.T) {
	t.Parallel()

	makeMsg := func(text string, reason message.FinishReason, toolCalls []message.ToolCall, thinking string) *message.Message {
		parts := []message.ContentPart{
			message.TextContent{Text: text},
			message.Finish{Reason: reason},
		}
		if thinking != "" {
			parts = append(parts, message.ReasoningContent{Thinking: thinking})
		}
		for _, tc := range toolCalls {
			parts = append(parts, tc)
		}
		return &message.Message{
			ID:    "test",
			Role:  message.Assistant,
			Parts: parts,
		}
	}

	t.Run("nil assistant", func(t *testing.T) {
		t.Parallel()
		require.False(t, isDegenerateOutput(nil))
	})

	t.Run("empty text", func(t *testing.T) {
		t.Parallel()
		msg := makeMsg("", message.FinishReasonEndTurn, nil, "")
		require.False(t, isDegenerateOutput(msg))
	})

	t.Run("3 char text triggers", func(t *testing.T) {
		t.Parallel()
		msg := makeMsg("...", message.FinishReasonEndTurn, nil, "")
		require.True(t, isDegenerateOutput(msg))
	})

	t.Run("5 char text triggers", func(t *testing.T) {
		t.Parallel()
		msg := makeMsg("hello", message.FinishReasonEndTurn, nil, "")
		require.True(t, isDegenerateOutput(msg))
	})

	t.Run("6 char text does not trigger", func(t *testing.T) {
		t.Parallel()
		msg := makeMsg("hello!", message.FinishReasonEndTurn, nil, "")
		require.False(t, isDegenerateOutput(msg))
	})

	t.Run("short text with tool calls", func(t *testing.T) {
		t.Parallel()
		msg := makeMsg("ok!", message.FinishReasonEndTurn, []message.ToolCall{
			{ID: "tc-1", Name: "bash", Input: "{}", Finished: true},
		}, "")
		require.False(t, isDegenerateOutput(msg))
	})

	t.Run("short text with reasoning", func(t *testing.T) {
		t.Parallel()
		msg := makeMsg("ok!", message.FinishReasonEndTurn, nil, "thinking about this")
		require.False(t, isDegenerateOutput(msg))
	})

	t.Run("short text with error finish", func(t *testing.T) {
		t.Parallel()
		msg := makeMsg("ok!", message.FinishReasonError, nil, "")
		require.False(t, isDegenerateOutput(msg))
	})

	t.Run("short text with unknown finish", func(t *testing.T) {
		t.Parallel()
		msg := makeMsg("ok!", message.FinishReasonUnknown, nil, "")
		require.True(t, isDegenerateOutput(msg))
	})
}

func TestDetectContentAnomalies(t *testing.T) {
	t.Parallel()

	makeMsg := func(text string, reason message.FinishReason, toolCalls []message.ToolCall) *message.Message {
		parts := []message.ContentPart{
			message.TextContent{Text: text},
			message.Finish{Reason: reason},
		}
		for _, tc := range toolCalls {
			parts = append(parts, tc)
		}
		return &message.Message{
			ID:    "test",
			Role:  message.Assistant,
			Parts: parts,
		}
	}

	t.Run("nil assistant", func(t *testing.T) {
		t.Parallel()
		require.Nil(t, detectContentAnomalies(nil))
	})

	t.Run("normal response - no anomalies", func(t *testing.T) {
		t.Parallel()
		msg := makeMsg(
			"I've updated the configuration file and all tests pass.",
			message.FinishReasonEndTurn, nil,
		)
		require.Empty(t, detectContentAnomalies(msg))
	})

	t.Run("repetitive text detected", func(t *testing.T) {
		t.Parallel()
		text := strings.Join([]string{
			`import type { Context } from "hono"`,
			`import type { Context } from "hono"`,
			`import type { Context } from "hono"`,
			`import type { Context } from "hono"`,
		}, "\n")
		msg := makeMsg(text, message.FinishReasonEndTurn, nil)
		anomalies := detectContentAnomalies(msg)
		require.Len(t, anomalies, 1)
		require.Equal(t, anomalyRepetitiveText, anomalies[0].Kind)
		require.Equal(t, 4, anomalies[0].RepeatCount)
	})

	t.Run("degenerate output detected", func(t *testing.T) {
		t.Parallel()
		msg := makeMsg("...", message.FinishReasonEndTurn, nil)
		anomalies := detectContentAnomalies(msg)
		require.Len(t, anomalies, 1)
		require.Equal(t, anomalyDegenerateOutput, anomalies[0].Kind)
	})

	t.Run("tool use response - no anomalies", func(t *testing.T) {
		t.Parallel()
		msg := makeMsg("ok!", message.FinishReasonToolUse, []message.ToolCall{
			{ID: "tc-1", Name: "bash", Input: "{}", Finished: true},
		})
		require.Empty(t, detectContentAnomalies(msg))
	})
}
