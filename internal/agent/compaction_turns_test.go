package agent

import (
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

func textMsg(role message.MessageRole, text string) message.Message {
	return message.Message{Role: role, Parts: []message.ContentPart{message.TextContent{Text: text}}}
}

func callMsg(id, name, input string) message.Message {
	return message.Message{Role: message.Assistant, Parts: []message.ContentPart{message.ToolCall{ID: id, Name: name, Input: input}}}
}

func resultMsg(callID, content string, isError bool) message.Message {
	return message.Message{Role: message.Tool, Parts: []message.ContentPart{message.ToolResult{ToolCallID: callID, Content: content, IsError: isError}}}
}

func TestSelectRecentWorkSegmentOrdinaryUserAssistant(t *testing.T) {
	t.Parallel()

	messages := []message.Message{
		textMsg(message.User, "old request"),
		textMsg(message.Assistant, "old answer"),
		textMsg(message.User, "current request"),
		textMsg(message.Assistant, "current answer"),
	}

	segment := selectRecentWorkSegment(messages, 0)
	require.Len(t, segment.Messages, 2)
	require.Equal(t, "current request", segment.Messages[0].Content().Text)
	require.Equal(t, "current answer", segment.Messages[1].Content().Text)
	require.False(t, segment.HasPendingCall)
}

func TestSelectRecentWorkSegmentKeepsParallelCallsAndResultsTogether(t *testing.T) {
	t.Parallel()

	messages := []message.Message{
		textMsg(message.User, "do two things"),
		{
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.ToolCall{ID: "call-1", Name: "read"},
				message.ToolCall{ID: "call-2", Name: "grep"},
			},
		},
		resultMsg("call-1", "result-1", false),
		resultMsg("call-2", "result-2", false),
		textMsg(message.Assistant, "done"),
	}

	segment := selectRecentWorkSegment(messages, 0)
	require.Len(t, segment.Messages, 5)
	require.False(t, segment.HasPendingCall)

	// Both results for the parallel call group must be present.
	var seenCallIDs []string
	for _, msg := range segment.Messages {
		for _, tr := range msg.ToolResults() {
			seenCallIDs = append(seenCallIDs, tr.ToolCallID)
		}
	}
	require.ElementsMatch(t, []string{"call-1", "call-2"}, seenCallIDs)
}

func TestSelectRecentWorkSegmentExtendsBackwardForOrphanedResult(t *testing.T) {
	t.Parallel()

	// The most recent "user message" anchor would normally start the
	// segment at the second user turn (index 2), which would orphan the
	// tool result at index 3 (its call is at index 1, before the cut).
	messages := []message.Message{
		textMsg(message.User, "first request"),
		callMsg("call-1", "bash", `{"cmd":"ls"}`),
		resultMsg("call-1", "file listing", false),
		textMsg(message.Assistant, "here are the files"),
	}
	// Simulate a naive cut that would start right at the tool result by
	// using a lower bound that still allows walking back to the call: the
	// user message precedes the call, so the naive "most recent user
	// message" start (index 0) already includes everything here. Instead,
	// exercise the orphan-extension directly against a segment that starts
	// exactly at the tool result.
	start := extendPastOrphanedResults(messages, 2)
	require.Equal(t, 1, start, "must extend back to the assistant message that issued call-1")
}

func TestSelectRecentWorkSegmentDoesNotCrossActiveSummaryLowerBound(t *testing.T) {
	t.Parallel()

	// Epoch 0 (already folded into the active summary): a user message
	// followed by an assistant reply.
	epoch0 := []message.Message{
		textMsg(message.User, "epoch 0 request"),
		textMsg(message.Assistant, "epoch 0 answer"),
	}
	// Active summary message sits at index len(epoch0), immediately after
	// epoch 0.
	summary := message.Message{
		Role:             message.User, // getSessionMessages re-roles the active summary to User.
		IsSummaryMessage: true,
		Parts:            []message.ContentPart{message.TextContent{Text: "## Goal\n...\n"}},
	}
	// New work created after the summary contains NO user message at all
	// (e.g. compaction triggered mid tool-loop continuation).
	newWork := []message.Message{
		callMsg("call-9", "bash", `{"cmd":"go test ./..."}`),
		resultMsg("call-9", "PASS", false),
	}

	messages := append(append(append([]message.Message{}, epoch0...), summary), newWork...)
	lowerBound := len(epoch0) + 1 // right after the summary message.

	segment := selectRecentWorkSegment(messages, lowerBound)

	// The segment must be exactly the new work -- it must never reach back
	// into epoch 0 looking for a user message to anchor on.
	require.Len(t, segment.Messages, len(newWork))
	for _, msg := range segment.Messages {
		require.NotEqual(t, "epoch 0 request", msg.Content().Text)
		require.NotEqual(t, "epoch 0 answer", msg.Content().Text)
		require.False(t, msg.IsSummaryMessage)
	}
}

func TestSelectRecentWorkSegmentPreservesPendingCall(t *testing.T) {
	t.Parallel()

	messages := []message.Message{
		textMsg(message.User, "run the build"),
		callMsg("call-1", "bash", `{"cmd":"go build ./..."}`),
	}

	segment := selectRecentWorkSegment(messages, 0)
	require.Len(t, segment.Messages, 2)
	require.True(t, segment.HasPendingCall)
	// The call itself must be retained unmodified, not faked as completed.
	require.Len(t, segment.Messages[1].ToolCalls(), 1)
	require.Empty(t, segment.Messages[1].ToolResults())
}

func TestSelectRecentWorkSegmentExcludesSummaryMessages(t *testing.T) {
	t.Parallel()

	failedSummary := message.Message{
		Role:             message.Assistant,
		IsSummaryMessage: true,
		Parts:            nil, // Failed/reset summaries persist with empty Parts.
	}
	messages := []message.Message{
		textMsg(message.User, "current request"),
		failedSummary,
		textMsg(message.Assistant, "current answer"),
	}

	segment := selectRecentWorkSegment(messages, 0)
	for _, msg := range segment.Messages {
		require.False(t, msg.IsSummaryMessage)
	}
	require.Len(t, segment.Messages, 2)
}
