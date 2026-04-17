package agent

import (
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"github.com/stretchr/testify/require"
)

func TestStripRedactedThinkingParts(t *testing.T) {
	t.Parallel()

	messages := []fantasy.Message{
		{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "hi"}}},
		{
			Role: fantasy.MessageRoleAssistant,
			Content: []fantasy.MessagePart{
				fantasy.ReasoningPart{
					Text: "",
					ProviderOptions: fantasy.ProviderOptions{
						anthropic.Name: &anthropic.ReasoningOptionMetadata{
							RedactedData: "ZGF0YQ==",
						},
					},
				},
				fantasy.ReasoningPart{
					Text: "signed reasoning",
					ProviderOptions: fantasy.ProviderOptions{
						anthropic.Name: &anthropic.ReasoningOptionMetadata{
							Signature: "sig-abc",
						},
					},
				},
				fantasy.TextPart{Text: "answer"},
				fantasy.ToolCallPart{ToolCallID: "t1", ToolName: "x"},
			},
		},
	}

	out, changed := stripRedactedThinkingParts(messages)
	require.True(t, changed)
	require.Len(t, out, 2)

	assistant := out[1]
	require.Len(t, assistant.Content, 3, "only the redacted reasoning block should be removed")

	reasoning, ok := assistant.Content[0].(fantasy.ReasoningPart)
	require.True(t, ok)
	require.Equal(t, "signed reasoning", reasoning.Text)
	meta, ok := reasoning.ProviderOptions[anthropic.Name].(*anthropic.ReasoningOptionMetadata)
	require.True(t, ok)
	require.Equal(t, "sig-abc", meta.Signature)

	_, ok = assistant.Content[1].(fantasy.TextPart)
	require.True(t, ok)
	_, ok = assistant.Content[2].(fantasy.ToolCallPart)
	require.True(t, ok)
}

func TestStripRedactedThinkingParts_NoOp(t *testing.T) {
	t.Parallel()

	messages := []fantasy.Message{{
		Role: fantasy.MessageRoleAssistant,
		Content: []fantasy.MessagePart{
			fantasy.TextPart{Text: "answer"},
			fantasy.ReasoningPart{
				Text: "signed",
				ProviderOptions: fantasy.ProviderOptions{
					anthropic.Name: &anthropic.ReasoningOptionMetadata{Signature: "sig"},
				},
			},
		},
	}}

	_, changed := stripRedactedThinkingParts(messages)
	require.False(t, changed)
}

func TestShouldRetryWithoutRedactedThinking(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "explicit redacted_thinking mention",
			err: &fantasy.ProviderError{
				StatusCode: 422,
				Message:    "'redacted_thinking' is not a valid content block type",
			},
			want: true,
		},
		{
			name: "pydantic union rejection 422",
			err: &fantasy.ProviderError{
				StatusCode: 422,
				Message: `422 Unprocessable Entity {"detail":[{"type":"string_type",` +
					`"loc":["body","messages",1,"content","list[union[ClaudeContentBlockText,` +
					`ClaudeContentBlockImage,ClaudeContentBlockToolUse,ClaudeContentBlockToolResult,` +
					`ClaudeContentBlockThinking]]",0,"ClaudeContentBlockText","text"]}]}`,
			},
			want: true,
		},
		{
			name: "different 422 unrelated",
			err: &fantasy.ProviderError{
				StatusCode: 422,
				Message:    "invalid model parameter",
			},
			want: false,
		},
		{
			name: "wrong status",
			err: &fantasy.ProviderError{
				StatusCode: 500,
				Message:    "redacted_thinking rejected",
			},
			want: false,
		},
		{
			name: "not a provider error",
			err:  assertiveError("redacted_thinking rejected"),
			want: false,
		},
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, shouldRetryWithoutRedactedThinking(tt.err))
		})
	}
}
