package agent

import (
	"strings"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// modelWithContextWindow builds a Model with just enough CatwalkCfg set for
// retainBudgetTokens/EffectiveContextWindow and promptTokenBudgetForModel to
// produce a deterministic budget in tests.
func modelWithContextWindow(contextWindow int64) Model {
	return Model{CatwalkCfg: catwalk.Model{ContextWindow: contextWindow}}
}

// generousModel has a large enough context window that retainBudgetTokens
// hits its 20k flat cap and the headroom guard never binds, so tests using
// it are only exercising segment-selection/ordering logic, not the budget
// ladder.
func generousModel() Model {
	return modelWithContextWindow(10_000_000)
}

func TestProjectSessionMessagesNoSummaryReturnsRawHistoryUnchanged(t *testing.T) {
	t.Parallel()

	raw := []message.Message{
		textMsg(message.User, "request"),
		textMsg(message.Assistant, "answer"),
	}

	for _, mode := range []sessionProjectionMode{summaryOnly, summaryWithRecentWork} {
		got := projectSessionMessages(sessionProjectionParams{
			RawMessages:      raw,
			SummaryMessageID: "",
			Mode:             mode,
			Model:            generousModel(),
		})
		require.Equal(t, raw, got)
	}

	// A SummaryMessageID that doesn't resolve against RawMessages (e.g. the
	// summary message was deleted, or this is a stale/foreign ID) must also
	// fall back to unchanged raw history rather than panic or return empty.
	got := projectSessionMessages(sessionProjectionParams{
		RawMessages:      raw,
		SummaryMessageID: "does-not-exist",
		Mode:             summaryWithRecentWork,
		Model:            generousModel(),
	})
	require.Equal(t, raw, got)
}

// TestProjectSessionMessagesSummaryOnlyMatchesPreChangeBehavior is the
// regression floor for handoff, prompt enhancement, and recap: summaryOnly
// must reproduce the old inline SummaryMessageID slice exactly -- same
// messages, same order, and critically no role rewrite, since none of the
// three pre-existing call sites rewrote Role.
func TestProjectSessionMessagesSummaryOnlyMatchesPreChangeBehavior(t *testing.T) {
	t.Parallel()

	summaryMsg := message.Message{
		ID:               "sum1",
		Role:             message.Assistant,
		IsSummaryMessage: true,
		Parts:            []message.ContentPart{message.TextContent{Text: "## Goal\nDo the thing.\n"}},
	}
	raw := []message.Message{
		textMsg(message.User, "old request"),
		textMsg(message.Assistant, "old answer"),
		summaryMsg,
		textMsg(message.User, "new request"),
		textMsg(message.Assistant, "new answer"),
	}

	got := projectSessionMessages(sessionProjectionParams{
		RawMessages:      raw,
		SummaryMessageID: "sum1",
		Mode:             summaryOnly,
	})

	// Old behavior: msgs[summaryMsgIndex:], verbatim, no role rewrite.
	require.Equal(t, raw[2:], got)
	require.Equal(t, message.Assistant, got[0].Role, "summaryOnly must not rewrite role")
}

// TestProjectSessionMessagesRecentWorkOrdersSummaryRetainedTail covers the
// first compaction: no preceding summary exists, so the retained segment's
// lower bound is 0 and the whole pre-summary history is eligible.
func TestProjectSessionMessagesRecentWorkOrdersSummaryRetainedTail(t *testing.T) {
	t.Parallel()

	summaryMsg := message.Message{
		ID:               "sum1",
		Role:             message.Assistant,
		IsSummaryMessage: true,
		Parts:            []message.ContentPart{message.TextContent{Text: "## Goal\n..."}},
	}
	preSummary := []message.Message{
		textMsg(message.User, "epoch0 request"),
		textMsg(message.Assistant, "epoch0 answer"),
	}
	tail := []message.Message{
		textMsg(message.User, "tail request"),
		textMsg(message.Assistant, "tail answer"),
	}

	raw := append(append(append([]message.Message{}, preSummary...), summaryMsg), tail...)

	got := projectSessionMessages(sessionProjectionParams{
		RawMessages:      raw,
		SummaryMessageID: "sum1",
		Mode:             summaryWithRecentWork,
		Model:            generousModel(),
	})

	require.Len(t, got, 1+len(preSummary)+len(tail))
	// The active summary comes first, copied and re-roled to user.
	require.True(t, got[0].IsSummaryMessage)
	require.Equal(t, message.User, got[0].Role)
	require.Equal(t, "## Goal\n...", got[0].Content().Text)
	// The original summary message in raw history must be untouched (copy,
	// never mutate -- P2.5).
	require.Equal(t, message.Assistant, summaryMsg.Role)
	require.Equal(t, message.Assistant, raw[2].Role)

	// Then the retained segment (all of preSummary here, the only turn).
	require.Equal(t, "epoch0 request", got[1].Content().Text)
	require.Equal(t, "epoch0 answer", got[2].Content().Text)

	// Then the post-summary tail, in order.
	require.Equal(t, "tail request", got[3].Content().Text)
	require.Equal(t, "tail answer", got[4].Content().Text)
}

// TestProjectSessionMessagesRecentWorkSecondCompactionPicksNewestSegment
// models the state right after a first compaction and a second one on top
// of it: raw history is [epoch0, summary1 (now superseded), epoch1 work,
// summary2 (active)]. The retained segment for summary2's projection must
// be epoch1's work only -- never epoch0, never summary1 itself.
func TestProjectSessionMessagesRecentWorkSecondCompactionPicksNewestSegment(t *testing.T) {
	t.Parallel()

	epoch0 := []message.Message{
		textMsg(message.User, "epoch0 request"),
		textMsg(message.Assistant, "epoch0 answer"),
	}
	summary1 := message.Message{
		ID:               "sum1",
		Role:             message.Assistant,
		IsSummaryMessage: true,
		Parts:            []message.ContentPart{message.TextContent{Text: "## Goal\nepoch0 summary"}},
	}
	epoch1 := []message.Message{
		textMsg(message.User, "epoch1 request"),
		textMsg(message.Assistant, "epoch1 answer"),
	}
	summary2 := message.Message{
		ID:               "sum2",
		Role:             message.Assistant,
		IsSummaryMessage: true,
		Parts:            []message.ContentPart{message.TextContent{Text: "## Goal\nepoch1 summary"}},
	}

	raw := append(append(append(append([]message.Message{}, epoch0...), summary1), epoch1...), summary2)

	got := projectSessionMessages(sessionProjectionParams{
		RawMessages:      raw,
		SummaryMessageID: "sum2",
		Mode:             summaryWithRecentWork,
		Model:            generousModel(),
	})

	require.Len(t, got, 1+len(epoch1))
	require.Equal(t, "## Goal\nepoch1 summary", got[0].Content().Text)
	require.Equal(t, "epoch1 request", got[1].Content().Text)
	require.Equal(t, "epoch1 answer", got[2].Content().Text)

	for _, msg := range got {
		require.False(t, msg.IsSummaryMessage && msg.ID == "sum1", "old summary must never appear as a retained message")
		require.NotEqual(t, "epoch0 request", msg.Content().Text)
		require.NotEqual(t, "epoch0 answer", msg.Content().Text)
	}
}

// TestProjectSessionMessagesRecentWorkThirdCompactionPicksNewestSegment
// extends the second-compaction scenario one epoch further: [epoch0,
// summary1, epoch1 (folded into summary2 last time), summary2, epoch2,
// summary3 (active)]. The retained segment must be epoch2 only.
func TestProjectSessionMessagesRecentWorkThirdCompactionPicksNewestSegment(t *testing.T) {
	t.Parallel()

	epoch0 := []message.Message{textMsg(message.User, "epoch0 request")}
	summary1 := message.Message{ID: "sum1", Role: message.Assistant, IsSummaryMessage: true, Parts: []message.ContentPart{message.TextContent{Text: "s1"}}}
	epoch1 := []message.Message{textMsg(message.User, "epoch1 request")}
	summary2 := message.Message{ID: "sum2", Role: message.Assistant, IsSummaryMessage: true, Parts: []message.ContentPart{message.TextContent{Text: "s2"}}}
	epoch2 := []message.Message{
		textMsg(message.User, "epoch2 request"),
		textMsg(message.Assistant, "epoch2 answer"),
	}
	summary3 := message.Message{ID: "sum3", Role: message.Assistant, IsSummaryMessage: true, Parts: []message.ContentPart{message.TextContent{Text: "s3"}}}

	var raw []message.Message
	raw = append(raw, epoch0...)
	raw = append(raw, summary1)
	raw = append(raw, epoch1...)
	raw = append(raw, summary2)
	raw = append(raw, epoch2...)
	raw = append(raw, summary3)

	got := projectSessionMessages(sessionProjectionParams{
		RawMessages:      raw,
		SummaryMessageID: "sum3",
		Mode:             summaryWithRecentWork,
		Model:            generousModel(),
	})

	require.Len(t, got, 1+len(epoch2))
	require.Equal(t, "epoch2 request", got[1].Content().Text)
	require.Equal(t, "epoch2 answer", got[2].Content().Text)
	for _, msg := range got {
		require.NotContains(t, []string{"epoch0 request", "epoch1 request"}, msg.Content().Text)
	}
}

// TestProjectSessionMessagesRecentWorkExcludesProvisionalSummaries covers a
// failed/provisional summary attempt sitting in raw history between the
// preceding adopted summary and the active one: resetSummaryMessage and the
// retry-exhausted path persist these with messages.Update (never Delete),
// so they remain in DB history without ever being adopted into
// SummaryMessageID. It must never surface as retained content.
func TestProjectSessionMessagesRecentWorkExcludesProvisionalSummaries(t *testing.T) {
	t.Parallel()

	failedSummary := message.Message{
		ID:               "failed1",
		Role:             message.Assistant,
		IsSummaryMessage: true,
		Parts:            nil, // Failed/reset summaries persist with empty Parts.
	}
	epoch1 := []message.Message{
		textMsg(message.User, "current request"),
		failedSummary,
		textMsg(message.Assistant, "current answer"),
	}
	activeSummary := message.Message{
		ID:               "sum-active",
		Role:             message.Assistant,
		IsSummaryMessage: true,
		Parts:            []message.ContentPart{message.TextContent{Text: "## Goal\n..."}},
	}

	raw := append(append([]message.Message{}, epoch1...), activeSummary)

	got := projectSessionMessages(sessionProjectionParams{
		RawMessages:      raw,
		SummaryMessageID: "sum-active",
		Mode:             summaryWithRecentWork,
		Model:            generousModel(),
	})

	for _, msg := range got {
		if msg.ID == "sum-active" {
			continue
		}
		require.False(t, msg.IsSummaryMessage, "no provisional/failed summary may appear in the projection except the active one")
	}
	// The failed summary itself also acts as a P1.2 lower bound ("adopted
	// or failed, doesn't matter"): everything before it (here, "current
	// request") is excluded from the candidate pool too, not just filtered
	// out of the final segment. Only "current answer", which comes after
	// the failed summary, survives as retained content.
	require.Len(t, got, 2)
	require.Equal(t, "current answer", got[1].Content().Text)
	for _, msg := range got {
		require.NotEqual(t, "current request", msg.Content().Text)
	}
}

// TestProjectSessionMessagesRecentWorkNeverSplitsToolPair mirrors
// TestSelectRecentWorkSegmentExtendsBackwardForOrphanedResult at the
// projection level: the naive "most recent user message" cut would orphan
// a tool result whose call precedes it, so the retained segment must
// extend backward to include the call.
func TestProjectSessionMessagesRecentWorkNeverSplitsToolPair(t *testing.T) {
	t.Parallel()

	summaryMsg := message.Message{ID: "sum1", Role: message.Assistant, IsSummaryMessage: true, Parts: []message.ContentPart{message.TextContent{Text: "s"}}}
	preSummary := []message.Message{
		textMsg(message.User, "first request"),
		callMsg("call-1", "bash", `{"cmd":"ls"}`),
		resultMsg("call-1", "file listing", false),
		textMsg(message.User, "second request, no new tool call"),
	}

	raw := append(append([]message.Message{}, preSummary...), summaryMsg)

	got := projectSessionMessages(sessionProjectionParams{
		RawMessages:      raw,
		SummaryMessageID: "sum1",
		Mode:             summaryWithRecentWork,
		Model:            generousModel(),
	})

	// Every tool result in the projection must have its call present too.
	seenCalls := map[string]bool{}
	for _, msg := range got {
		for _, tc := range msg.ToolCalls() {
			seenCalls[tc.ID] = true
		}
	}
	for _, msg := range got {
		for _, tr := range msg.ToolResults() {
			require.True(t, seenCalls[tr.ToolCallID], "tool result %s must not be orphaned from its call", tr.ToolCallID)
		}
	}
}

// TestProjectSessionMessagesRecentWorkOversizedSegmentUsesBoundedRepresentation
// forces the safe segment alone to exceed retainBudgetTokens by making its
// content far larger than the bounded representation's fixed caps
// (boundedUserRequestMaxRunes etc, P1.4), while leaving headroom generous
// so budget -- not headroom -- is what's under test.
func TestProjectSessionMessagesRecentWorkOversizedSegmentUsesBoundedRepresentation(t *testing.T) {
	t.Parallel()

	summaryMsg := message.Message{ID: "sum1", Role: message.Assistant, IsSummaryMessage: true, Parts: []message.ContentPart{message.TextContent{Text: "s"}}}
	hugeText := strings.Repeat("recent work filler content ", 2000) // Far beyond any bounded cap.
	preSummary := []message.Message{
		textMsg(message.User, "request: "+hugeText),
		textMsg(message.Assistant, "answer: "+hugeText),
	}
	raw := append(append([]message.Message{}, preSummary...), summaryMsg)

	segFloor := selectRecentWorkSegment(preSummary, 0)
	segTokens := estimateMessagesTokensForFitting(segFloor.Messages)
	bounded := buildBoundedSegmentRepresentation(segFloor)
	boundedTokens := estimateMessagesTokensForFitting(bounded)
	require.Less(t, boundedTokens, segTokens, "bounded representation must be meaningfully smaller than the raw oversized segment")

	// Pick a context window whose 12% retainBudget sits strictly between
	// boundedTokens and segTokens, and whose headroom (80% of a generously
	// large usable budget) has no trouble fitting the bounded form.
	retainBudget := boundedTokens + (segTokens-boundedTokens)/2
	contextWindow := retainBudget * 100 / 12
	model := modelWithContextWindow(contextWindow)
	require.Greater(t, retainBudgetTokens(model), boundedTokens)
	require.Less(t, retainBudgetTokens(model), segTokens)

	got := projectSessionMessages(sessionProjectionParams{
		RawMessages:      raw,
		SummaryMessageID: "sum1",
		Mode:             summaryWithRecentWork,
		Model:            model,
	})

	require.Len(t, got, 2) // summary + one bounded-representation message.
	require.Contains(t, got[1].Content().Text, "bounded representation")
	require.NotContains(t, got[1].Content().Text, hugeText)
}

// TestProjectSessionMessagesRecentWorkFallsBackToSummaryOnlyWhenHeadroomInsufficient
// constructs a model budget where the retained segment (in either its full
// or bounded form) comfortably clears retainBudgetTokens on its own, but
// adding it to summary+tail would leave less than the P2.3 20% headroom
// margin before the next auto-summarize trigger. The retained segment must
// be dropped entirely rather than risk an immediate re-compaction.
func TestProjectSessionMessagesRecentWorkFallsBackToSummaryOnlyWhenHeadroomInsufficient(t *testing.T) {
	t.Parallel()

	filler := strings.Repeat("summary and tail filler content ", 400)
	summaryMsg := message.Message{
		ID:               "sum1",
		Role:             message.Assistant,
		IsSummaryMessage: true,
		Parts:            []message.ContentPart{message.TextContent{Text: "## Goal\n" + filler}},
	}
	tail := []message.Message{
		textMsg(message.User, "tail request "+filler),
		textMsg(message.Assistant, "tail answer"),
	}
	preSummary := []message.Message{
		textMsg(message.User, "small request"),
		textMsg(message.Assistant, "small answer"),
	}
	raw := append(append(append([]message.Message{}, preSummary...), summaryMsg), tail...)

	sTokens := estimateMessagesTokensForFitting(append([]message.Message{summaryMsg}, tail...))
	segFloor := selectRecentWorkSegment(preSummary, 0)
	rTokens := estimateMessagesTokensForFitting(segFloor.Messages)
	boundedTokens := estimateMessagesTokensForFitting(buildBoundedSegmentRepresentation(segFloor))
	minSeg := min(rTokens, boundedTokens)
	require.Positive(t, minSeg)

	// usable = sTokens + minSeg exactly, so threshold = 0.8*usable is
	// strictly less than sTokens+minSeg <= sTokens+{rTokens,boundedTokens}:
	// both the full and bounded retained candidates fail headroom.
	maxOutputTokens := int64(1)
	usable := sTokens + minSeg
	contextWindow := usable + maxOutputTokens
	model := modelWithContextWindow(contextWindow)

	require.GreaterOrEqual(t, retainBudgetTokens(model), rTokens, "test setup: budget must not be the reason retained is dropped")
	require.GreaterOrEqual(t, retainBudgetTokens(model), boundedTokens, "test setup: budget must not be the reason retained is dropped")

	got := projectSessionMessages(sessionProjectionParams{
		RawMessages:      raw,
		SummaryMessageID: "sum1",
		Mode:             summaryWithRecentWork,
		Model:            model,
		MaxOutputTokens:  maxOutputTokens,
	})

	require.Len(t, got, 1+len(tail), "retained segment must be dropped; projection is summary + tail only")
	require.True(t, got[0].IsSummaryMessage)
	require.Equal(t, message.User, got[0].Role)
	require.Equal(t, tail[0].Content().Text, got[1].Content().Text)
	require.Equal(t, tail[1].Content().Text, got[2].Content().Text)
}

// TestProjectSessionMessagesRecentWorkStablePrefixAcrossGrowingTail is the
// prompt-cache stability property from P2.4: messages[:summaryIdx] is
// frozen for the whole epoch once a compaction lands, so the retained
// selection must not drift as new tail messages arrive within that epoch --
// otherwise the [summary + retained] prefix would change every turn and
// break prompt caching. Only the tail may grow between calls.
func TestProjectSessionMessagesRecentWorkStablePrefixAcrossGrowingTail(t *testing.T) {
	t.Parallel()

	summaryMsg := message.Message{ID: "sum1", Role: message.Assistant, IsSummaryMessage: true, Parts: []message.ContentPart{message.TextContent{Text: "## Goal\n..."}}}
	preSummary := []message.Message{
		textMsg(message.User, "epoch request"),
		textMsg(message.Assistant, "epoch answer"),
	}
	baseRaw := append(append([]message.Message{}, preSummary...), summaryMsg)

	firstCall := projectSessionMessages(sessionProjectionParams{
		RawMessages:      append(append([]message.Message{}, baseRaw...), textMsg(message.User, "turn 1")),
		SummaryMessageID: "sum1",
		Mode:             summaryWithRecentWork,
		Model:            generousModel(),
	})

	growingTail := []message.Message{
		textMsg(message.User, "turn 1"),
		textMsg(message.Assistant, "turn 1 reply"),
		textMsg(message.User, "turn 2"),
		textMsg(message.Assistant, "turn 2 reply"),
	}
	secondCall := projectSessionMessages(sessionProjectionParams{
		RawMessages:      append(append([]message.Message{}, baseRaw...), growingTail...),
		SummaryMessageID: "sum1",
		Mode:             summaryWithRecentWork,
		Model:            generousModel(),
	})

	stablePrefixLen := 1 + len(preSummary) // summary + retained segment.
	require.GreaterOrEqual(t, len(firstCall), stablePrefixLen)
	require.GreaterOrEqual(t, len(secondCall), stablePrefixLen)
	require.Equal(t, firstCall[:stablePrefixLen], secondCall[:stablePrefixLen],
		"the [summary + retained] prefix must stay byte-identical across calls within the same epoch as the tail grows, or prompt caching breaks every turn")
}
