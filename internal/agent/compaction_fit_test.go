package agent

import (
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestFitCompactionHistoryKeepsEverythingWhenItFits(t *testing.T) {
	t.Parallel()

	messages := []message.Message{
		textMsg(message.User, "request"),
		textMsg(message.Assistant, "answer"),
	}

	result := fitCompactionHistory(messages, 0, 10_000)
	require.False(t, result.Bounded)
	require.False(t, result.EnvelopeOnly)
	require.Equal(t, messages, result.Messages)
}

func TestFitCompactionHistoryDropsOlderHistoryFromOldestEnd(t *testing.T) {
	t.Parallel()

	old := strings.Repeat("old filler content ", 500)
	recent := strings.Repeat("recent filler content ", 50)

	messages := []message.Message{
		textMsg(message.User, "old request "+old),
		textMsg(message.Assistant, "old answer "+old),
		textMsg(message.User, "recent request "+recent),
		textMsg(message.Assistant, "recent answer "+recent),
	}

	// A budget that fits only the most recent segment, not everything.
	segmentTokens := estimateMessagesTokensForFitting(messages[2:])
	result := fitCompactionHistory(messages, 0, segmentTokens+50)

	require.False(t, result.Bounded)
	require.False(t, result.EnvelopeOnly)
	require.Len(t, result.Messages, 2)
	require.Contains(t, result.Messages[0].Content().Text, "recent request")
	require.Contains(t, result.Messages[1].Content().Text, "recent answer")
}

// TestFitCompactionHistoryTierTwoKeepsMultipleRecentSegmentsWhenBudgetAllows
// covers the middle tier between "keep everything" and "keep only the
// single newest safe segment": when several recent turns fit within
// budget, fitCompactionHistory must retain all of them, walking backward
// from the newest and dropping only the oldest turn once the budget is
// exhausted -- not collapse straight to selectRecentWorkSegment's single
// newest turn (docs/refactor-compaction-context.md P1.3, "更早历史从最老处删除").
func TestFitCompactionHistoryTierTwoKeepsMultipleRecentSegmentsWhenBudgetAllows(t *testing.T) {
	t.Parallel()

	old := strings.Repeat("old filler content ", 500)
	mid := strings.Repeat("middle filler content ", 200)
	recent := strings.Repeat("recent filler content ", 50)

	messages := []message.Message{
		textMsg(message.User, "old request "+old),
		textMsg(message.Assistant, "old answer "+old),
		textMsg(message.User, "middle request "+mid),
		textMsg(message.Assistant, "middle answer "+mid),
		textMsg(message.User, "recent request "+recent),
		textMsg(message.Assistant, "recent answer "+recent),
	}

	twoTurnTokens := estimateMessagesTokensForFitting(messages[2:])
	allTokens := estimateMessagesTokensForFitting(messages)
	require.Less(t, twoTurnTokens, allTokens, "test fixture sanity: the old turn must add tokens")

	// Enough budget for the middle + recent turns together, but not for
	// all three turns.
	budget := twoTurnTokens + 50

	result := fitCompactionHistory(messages, 0, budget)
	require.False(t, result.Bounded)
	require.False(t, result.EnvelopeOnly)

	// The old tier-2 behaviour (selectRecentWorkSegment alone) would return
	// only the recent turn (2 messages); the new middle tier must do
	// strictly better when the budget allows it.
	floor := selectRecentWorkSegment(messages, 0)
	require.Greater(t, len(result.Messages), len(floor.Messages))
	require.Len(t, result.Messages, 4)
	require.Contains(t, result.Messages[0].Content().Text, "middle request")
	require.Contains(t, result.Messages[1].Content().Text, "middle answer")
	require.Contains(t, result.Messages[2].Content().Text, "recent request")
	require.Contains(t, result.Messages[3].Content().Text, "recent answer")

	for _, msg := range result.Messages {
		require.NotContains(t, msg.Content().Text, "old request")
		require.NotContains(t, msg.Content().Text, "old answer")
	}
}

func TestFitCompactionHistoryProducesBoundedRepresentationForOversizedSegment(t *testing.T) {
	t.Parallel()

	hugeOutput := strings.Repeat("x", 20_000)
	messages := []message.Message{
		textMsg(message.User, "please read the huge file"),
		callMsg("call-1", "read", `{"path":"huge.txt"}`),
		resultMsg("call-1", hugeOutput, false),
		textMsg(message.Assistant, "here is what I found: it is huge"),
	}

	// A budget too small for the raw segment (~5,000 tokens of tool output)
	// but big enough for the bounded representation (well under 1,000
	// tokens once capped).
	result := fitCompactionHistory(messages, 0, 3000)

	require.True(t, result.Bounded)
	require.False(t, result.EnvelopeOnly)
	require.Len(t, result.Messages, 1)
	text := result.Messages[0].Content().Text
	require.Contains(t, text, "User request:")
	require.Contains(t, text, "please read the huge file")
	require.Contains(t, text, "Assistant progress:")
	require.Contains(t, text, "here is what I found")
	require.Contains(t, text, "Tool activity:")
	require.Contains(t, text, "call-1")
	require.Contains(t, text, "completed")
	// The raw 20,000-char output must not survive unbounded.
	require.Less(t, len(text), len(hugeOutput))
}

func TestFitCompactionHistoryFallsBackToEnvelopeOnlyWhenBoundedStillExceedsBudget(t *testing.T) {
	t.Parallel()

	messages := []message.Message{
		textMsg(message.User, strings.Repeat("request ", 2000)),
		callMsg("call-1", "read", `{"path":"huge.txt"}`),
		resultMsg("call-1", strings.Repeat("y", 20_000), false),
		textMsg(message.Assistant, strings.Repeat("progress ", 2000)),
	}

	// A budget too small even for the bounded representation's fixed caps.
	result := fitCompactionHistory(messages, 0, 10)

	require.True(t, result.Bounded)
	require.True(t, result.EnvelopeOnly)
	require.Empty(t, result.Messages)
}

func TestFitCompactionHistoryZeroBudgetUsesEnvelopeOnly(t *testing.T) {
	t.Parallel()

	messages := []message.Message{textMsg(message.User, "hello")}
	result := fitCompactionHistory(messages, 0, 0)
	require.Empty(t, result.Messages)
	require.False(t, result.Bounded)
	require.True(t, result.EnvelopeOnly)
}

func TestBoundedSegmentRepresentationPrefersArchiveReferenceOverRetruncating(t *testing.T) {
	t.Parallel()

	archived := builtinPruneCompactedNoticePrefix + " to reduce context size. 500 estimated tokens omitted, 2000 characters omitted. The full output was archived to `archive/foo.txt`; use the read tool to inspect it.]"
	segment := recentWorkSegment{
		Messages: []message.Message{
			textMsg(message.User, "look at the archived result"),
			callMsg("call-1", "read", `{"path":"foo.txt"}`),
			resultMsg("call-1", archived, false),
		},
	}

	rendered := buildBoundedSegmentRepresentation(segment)
	require.Len(t, rendered, 1)
	text := rendered[0].Content().Text
	// The archive placeholder must survive verbatim, not be re-truncated
	// head/tail.
	require.Contains(t, text, archived)
}

func TestBoundedSegmentRepresentationPreservesErrorHeadTail(t *testing.T) {
	t.Parallel()

	errText := strings.Repeat("E", 1500) + "-MIDDLE-" + strings.Repeat("F", 1500)
	segment := recentWorkSegment{
		Messages: []message.Message{
			textMsg(message.User, "run the failing command"),
			callMsg("call-1", "bash", `{"cmd":"false"}`),
			resultMsg("call-1", errText, true),
		},
	}

	rendered := buildBoundedSegmentRepresentation(segment)
	text := rendered[0].Content().Text
	require.Contains(t, text, "error")
	require.Contains(t, text, strings.Repeat("E", 100))
	require.Contains(t, text, strings.Repeat("F", 100))
	require.NotContains(t, text, "MIDDLE")
}

func TestBoundedSegmentRepresentationPreservesAssistantVisibleText(t *testing.T) {
	t.Parallel()

	segment := recentWorkSegment{
		Messages: []message.Message{
			textMsg(message.User, "what's the status"),
			textMsg(message.Assistant, "all tests pass and the build is green"),
		},
	}

	rendered := buildBoundedSegmentRepresentation(segment)
	text := rendered[0].Content().Text
	require.Contains(t, text, "all tests pass and the build is green")
}

func TestBoundedSegmentRepresentationMarksPendingCallWithoutFakingCompletion(t *testing.T) {
	t.Parallel()

	segment := recentWorkSegment{
		Messages: []message.Message{
			textMsg(message.User, "start the long build"),
			callMsg("call-1", "bash", `{"cmd":"go build ./..."}`),
		},
		HasPendingCall: true,
	}

	rendered := buildBoundedSegmentRepresentation(segment)
	text := rendered[0].Content().Text
	require.Contains(t, text, "call-1")
	require.Contains(t, text, "pending")
	require.NotContains(t, text, "completed")
}

func TestBoundedSegmentRepresentationCapsToolActivitiesToMostRecent(t *testing.T) {
	t.Parallel()

	var messages []message.Message
	messages = append(messages, textMsg(message.User, "run many tools"))
	for i := range 20 {
		id := "call-" + string(rune('a'+i))
		messages = append(messages, callMsg(id, "bash", `{"n":`+string(rune('0'+i%10))+`}`))
		messages = append(messages, resultMsg(id, "ok", false))
	}
	segment := recentWorkSegment{Messages: messages}

	activities := boundedToolActivities(segment.Messages)
	require.Len(t, activities, boundedMaxToolActivities)
	// The most recent call must be present; the earliest must be dropped.
	require.Equal(t, "call-"+string(rune('a'+19)), activities[len(activities)-1].CallID)
	for _, act := range activities {
		require.NotEqual(t, "call-a", act.CallID)
	}
}
