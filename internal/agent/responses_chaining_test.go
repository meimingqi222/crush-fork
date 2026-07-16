package agent

import (
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/stretchr/testify/require"
)

func toolResultMsg(id, text string) fantasy.Message {
	return fantasy.Message{
		Role: fantasy.MessageRoleTool,
		Content: []fantasy.MessagePart{
			fantasy.ToolResultPart{
				ToolCallID: id,
				Output:     fantasy.ToolResultOutputContentText{Text: text},
			},
		},
	}
}

func fantasyAssistantMsg(text string) fantasy.Message {
	return fantasy.Message{
		Role:    fantasy.MessageRoleAssistant,
		Content: []fantasy.MessagePart{fantasy.TextPart{Text: text}},
	}
}

func toolCallStep(ids ...string) fantasy.StepResult {
	content := make(fantasy.ResponseContent, 0, len(ids))
	for _, id := range ids {
		content = append(content, fantasy.ToolCallContent{ToolCallID: id, ToolName: "bash"})
	}
	return fantasy.StepResult{Response: fantasy.Response{Content: content}}
}

func responsesBaseOptions() fantasy.ProviderOptions {
	return fantasy.ProviderOptions{openai.Name: &openai.ResponsesProviderOptions{}}
}

func newChainingAgent() *sessionAgent {
	return &sessionAgent{
		responsesChaining: true,
		lastResponseID:    csync.NewMap[string, string](),
	}
}

func TestResponsesChainingOptions(t *testing.T) {
	t.Parallel()

	_, ok := responsesChainingOptions(nil)
	require.False(t, ok)

	_, ok = responsesChainingOptions(fantasy.ProviderOptions{})
	require.False(t, ok)

	opts, ok := responsesChainingOptions(responsesBaseOptions())
	require.True(t, ok)
	require.NotNil(t, opts)
}

func TestEnableResponsesStore(t *testing.T) {
	t.Parallel()

	// No Responses options: returned unchanged.
	plain := fantasy.ProviderOptions{}
	require.Equal(t, plain, enableResponsesStore(plain))

	base := responsesBaseOptions()
	out := enableResponsesStore(base)
	stored, ok := responsesChainingOptions(out)
	require.True(t, ok)
	require.NotNil(t, stored.Store)
	require.True(t, *stored.Store)

	// The original must not be mutated (clone semantics).
	orig, _ := responsesChainingOptions(base)
	require.Nil(t, orig.Store)
}

func TestResponseIDFromMetadata(t *testing.T) {
	t.Parallel()

	require.Equal(t, "", responseIDFromMetadata(nil))
	require.Equal(t, "", responseIDFromMetadata(fantasy.ProviderMetadata{}))

	meta := fantasy.ProviderMetadata{openai.Name: &openai.ResponsesProviderMetadata{ResponseID: "resp_123"}}
	require.Equal(t, "resp_123", responseIDFromMetadata(meta))
}

func TestToolResultMessagesForIDs(t *testing.T) {
	t.Parallel()

	msgs := []fantasy.Message{
		fantasy.NewSystemMessage("sys"),
		fantasyAssistantMsg("calling tools"),
		toolResultMsg("call_1", "a"),
		toolResultMsg("call_2", "b"),
	}
	ids := map[string]struct{}{"call_1": {}, "call_2": {}}
	out, matched := toolResultMessagesForIDs(msgs, ids)
	require.Len(t, out, 2)
	require.Equal(t, 2, matched)

	// Missing one result.
	partial := []fantasy.Message{toolResultMsg("call_1", "a")}
	out, matched = toolResultMessagesForIDs(partial, ids)
	require.Len(t, out, 1)
	require.Equal(t, 1, matched)
}

func TestNewUserTurnTail(t *testing.T) {
	t.Parallel()

	// Clean new-user turn after the last assistant, with a trailing system
	// (prompt suffix) that must be skipped.
	msgs := []fantasy.Message{
		fantasy.NewSystemMessage("sys"),
		fantasy.NewUserMessage("old"),
		fantasyAssistantMsg("answer"),
		fantasy.NewUserMessage("new question"),
		fantasy.NewSystemMessage("suffix"),
	}
	tail := newUserTurnTail(msgs)
	require.Len(t, tail, 1)
	require.Equal(t, fantasy.MessageRoleUser, tail[0].Role)

	// No assistant yet: bail.
	require.Nil(t, newUserTurnTail([]fantasy.Message{fantasy.NewUserMessage("hi")}))

	// Tool result after the last assistant: not a clean user turn, bail.
	dirty := []fantasy.Message{
		fantasyAssistantMsg("answer"),
		toolResultMsg("call_1", "x"),
	}
	require.Nil(t, newUserTurnTail(dirty))
}

func TestApplyResponsesChaining_WithinRun(t *testing.T) {
	t.Parallel()

	a := newChainingAgent()
	a.lastResponseID.Set("s1", "resp_prev")

	prepared := &fantasy.PrepareStepResult{
		Messages: []fantasy.Message{
			fantasy.NewSystemMessage("sys"),
			fantasy.NewUserMessage("do it"),
			fantasyAssistantMsg("calling"),
			toolResultMsg("call_1", "output"),
		},
	}
	applied := a.applyResponsesChaining(prepared, responsesChainingInput{
		sessionID:   "s1",
		baseOptions: enableResponsesStore(responsesBaseOptions()),
		stepNumber:  1,
		steps:       []fantasy.StepResult{toolCallStep("call_1")},
	})
	require.True(t, applied)
	// Only the tool result survives as the incremental turn.
	require.Len(t, prepared.Messages, 1)
	require.Equal(t, fantasy.MessageRoleTool, prepared.Messages[0].Role)

	opts, ok := responsesChainingOptions(prepared.ProviderOptions)
	require.True(t, ok)
	require.NotNil(t, opts.PreviousResponseID)
	require.Equal(t, "resp_prev", *opts.PreviousResponseID)
	require.NotNil(t, opts.Store)
	require.True(t, *opts.Store)
}

func TestApplyResponsesChaining_CrossTurn(t *testing.T) {
	t.Parallel()

	a := newChainingAgent()
	a.lastResponseID.Set("s1", "resp_prev")

	prepared := &fantasy.PrepareStepResult{
		Messages: []fantasy.Message{
			fantasy.NewSystemMessage("sys"),
			fantasy.NewUserMessage("old"),
			fantasyAssistantMsg("answer"),
			fantasy.NewUserMessage("new turn"),
		},
	}
	applied := a.applyResponsesChaining(prepared, responsesChainingInput{
		sessionID:   "s1",
		baseOptions: enableResponsesStore(responsesBaseOptions()),
		stepNumber:  0,
	})
	require.True(t, applied)
	require.Len(t, prepared.Messages, 1)
	require.Equal(t, fantasy.MessageRoleUser, prepared.Messages[0].Role)
}

func TestApplyResponsesChaining_Bails(t *testing.T) {
	t.Parallel()

	base := enableResponsesStore(responsesBaseOptions())
	msgs := []fantasy.Message{
		fantasy.NewUserMessage("do it"),
		fantasyAssistantMsg("calling"),
		toolResultMsg("call_1", "output"),
	}

	t.Run("no stored response id", func(t *testing.T) {
		t.Parallel()
		a := newChainingAgent()
		prepared := &fantasy.PrepareStepResult{Messages: append([]fantasy.Message(nil), msgs...)}
		applied := a.applyResponsesChaining(prepared, responsesChainingInput{
			sessionID:   "s1",
			baseOptions: base,
			stepNumber:  1,
			steps:       []fantasy.StepResult{toolCallStep("call_1")},
		})
		require.False(t, applied)
		require.Nil(t, prepared.ProviderOptions)
	})

	t.Run("context injected", func(t *testing.T) {
		t.Parallel()
		a := newChainingAgent()
		a.lastResponseID.Set("s1", "resp_prev")
		prepared := &fantasy.PrepareStepResult{Messages: append([]fantasy.Message(nil), msgs...)}
		applied := a.applyResponsesChaining(prepared, responsesChainingInput{
			sessionID:       "s1",
			baseOptions:     base,
			stepNumber:      1,
			steps:           []fantasy.StepResult{toolCallStep("call_1")},
			contextInjected: true,
		})
		require.False(t, applied)
	})

	t.Run("incomplete tool results", func(t *testing.T) {
		t.Parallel()
		a := newChainingAgent()
		a.lastResponseID.Set("s1", "resp_prev")
		prepared := &fantasy.PrepareStepResult{Messages: append([]fantasy.Message(nil), msgs...)}
		applied := a.applyResponsesChaining(prepared, responsesChainingInput{
			sessionID:   "s1",
			baseOptions: base,
			stepNumber:  1,
			// Two calls but only one result present in messages.
			steps: []fantasy.StepResult{toolCallStep("call_1", "call_2")},
		})
		require.False(t, applied)
	})

	t.Run("no responses options", func(t *testing.T) {
		t.Parallel()
		a := newChainingAgent()
		a.lastResponseID.Set("s1", "resp_prev")
		prepared := &fantasy.PrepareStepResult{Messages: append([]fantasy.Message(nil), msgs...)}
		applied := a.applyResponsesChaining(prepared, responsesChainingInput{
			sessionID:   "s1",
			baseOptions: fantasy.ProviderOptions{},
			stepNumber:  1,
			steps:       []fantasy.StepResult{toolCallStep("call_1")},
		})
		require.False(t, applied)
	})
}
