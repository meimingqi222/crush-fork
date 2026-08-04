package agent

import (
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func textMessage(role message.MessageRole, text string) message.Message {
	return message.Message{Role: role, Parts: []message.ContentPart{message.TextContent{Text: text}}}
}

func TestBuildCompactionAnchorPreservesGoalStateAndCurrentRequest(t *testing.T) {
	t.Parallel()

	sess := session.Session{
		WorkspaceCWD: "/workspace/project",
		Todos:        []session.Todo{{Status: "in_progress", Content: "Fix the delayed rendering bug"}},
	}
	raw := []message.Message{
		textMessage(message.User, "Fix the rendering pipeline and add regression tests."),
		textMessage(message.Assistant, "Investigated the renderer."),
		textMessage(message.User, "Now inspect the delayed update path."),
	}

	anchor := buildCompactionAnchor(sess, raw, raw, "", false)
	require.Contains(t, anchor, "Original user goal: Fix the rendering pipeline and add regression tests.")
	require.Contains(t, anchor, "Goal source: earliest-user-message")
	require.Contains(t, anchor, "Current/latest user request: Now inspect the delayed update path.")
	require.Contains(t, anchor, "Workspace: /workspace/project")
	require.Contains(t, anchor, "Fix the delayed rendering bug")
}

// --- Goal source priority levels (docs/refactor-compaction-context.md P0.1) ---

func TestBuildCompactionAnchorGoalSourceGoalTool(t *testing.T) {
	t.Parallel()

	sess := session.Session{
		Goal: session.Goal{
			Text:        "Ship the compaction refactor",
			Status:      session.GoalStatusActive,
			TokenBudget: 100_000,
			TokensUsed:  25_000,
		},
	}
	raw := []message.Message{textMessage(message.User, "Some earlier, unrelated request")}

	// Pass empty projectionMessages so "Current/latest user request" (which
	// legitimately reads from the projection, independent of goal source)
	// doesn't get confused with goal derivation in this assertion.
	anchor := buildCompactionAnchor(sess, raw, nil, "", false)
	require.Contains(t, anchor, "Original user goal: Ship the compaction refactor")
	require.Contains(t, anchor, "Goal source: goal-tool")
	require.Contains(t, anchor, "Goal status: active")
	require.Contains(t, anchor, "Goal budget: 25000/100000")
	// The active goal must win over raw history, not just supplement it.
	require.NotContains(t, anchor, "Some earlier, unrelated request")
}

func TestBuildCompactionAnchorGoalSourcePausedGoalStillWins(t *testing.T) {
	t.Parallel()

	sess := session.Session{
		Goal: session.Goal{Text: "Paused objective", Status: session.GoalStatusPaused},
	}
	anchor := buildCompactionAnchor(sess, nil, nil, "", false)
	require.Contains(t, anchor, "Original user goal: Paused objective")
	require.Contains(t, anchor, "Goal source: goal-tool")
	require.Contains(t, anchor, "Goal status: paused")
	require.NotContains(t, anchor, "Goal budget:", "no budget was set, so the budget line must be omitted")
}

func TestBuildCompactionAnchorGoalSourceDroppedGoalDoesNotWin(t *testing.T) {
	t.Parallel()

	// A dropped/completed goal must not be treated as the current objective
	// -- it falls through to the next source instead.
	sess := session.Session{
		Goal: session.Goal{Text: "Old, no longer active objective", Status: session.GoalStatusDropped},
	}
	raw := []message.Message{textMessage(message.User, "The actual earliest request")}

	anchor := buildCompactionAnchor(sess, raw, raw, "", false)
	require.Contains(t, anchor, "Original user goal: The actual earliest request")
	require.Contains(t, anchor, "Goal source: earliest-user-message")
}

func TestBuildCompactionAnchorGoalSourceEarliestUserMessage(t *testing.T) {
	t.Parallel()

	raw := []message.Message{
		textMessage(message.Assistant, "System-ish preamble, not a user message"),
		textMessage(message.User, "This is the original ask"),
		textMessage(message.User, "A later follow-up"),
	}

	anchor := buildCompactionAnchor(session.Session{}, raw, raw, "", false)
	require.Contains(t, anchor, "Original user goal: This is the original ask")
	require.Contains(t, anchor, "Goal source: earliest-user-message")
}

func TestBuildCompactionAnchorGoalSourcePreviousSummary(t *testing.T) {
	t.Parallel()

	// No session.Goal and no user message in raw history (e.g. it was
	// itself compacted away in an earlier epoch) -- fall back to the
	// previous summary's own Goal section.
	previousSummary := "## Goal\nPreserve the project state across compaction.\n\n## Next Steps\n- Continue testing"

	anchor := buildCompactionAnchor(session.Session{}, nil, nil, previousSummary, false)
	require.Contains(t, anchor, "Original user goal: Preserve the project state across compaction.")
	require.Contains(t, anchor, "Goal source: previous-summary")
}

func TestBuildCompactionAnchorGoalSourceUnavailable(t *testing.T) {
	t.Parallel()

	anchor := buildCompactionAnchor(session.Session{}, nil, nil, "", false)
	require.Contains(t, anchor, "Original user goal: [unavailable in retained history]")
	require.Contains(t, anchor, "Goal source: unavailable")
}

// TestBuildCompactionAnchorSecondCompactionDoesNotTreatOldSummaryAsGoal
// guards against a second-epoch regression: a message list that contains a
// projected-active-summary entry (Role rewritten to User by
// getSessionMessages, IsSummaryMessage still true) ahead of the real
// earliest user request must not have that summary text picked up as the
// "original user goal".
func TestBuildCompactionAnchorSecondCompactionDoesNotTreatOldSummaryAsGoal(t *testing.T) {
	t.Parallel()

	raw := []message.Message{
		{ID: "summary-1", Role: message.User, IsSummaryMessage: true, Parts: []message.ContentPart{message.TextContent{Text: "## Goal\nStale summary text that must not win"}}},
		textMessage(message.User, "The real earliest request"),
	}

	anchor := buildCompactionAnchor(session.Session{}, raw, raw, "", false)
	require.Contains(t, anchor, "Original user goal: The real earliest request")
	require.Contains(t, anchor, "Goal source: earliest-user-message")
	require.NotContains(t, anchor, "Stale summary text")
}

// --- Plan file, todos/subagents, previous-summary presence ---

func TestBuildCompactionAnchorInjectsActivePlanFile(t *testing.T) {
	t.Parallel()

	sess := session.Session{PlanFilePath: "local://feature-plan.md"}
	anchor := buildCompactionAnchor(sess, nil, nil, "", false)
	require.Contains(t, anchor, "Active plan file: local://feature-plan.md")
	require.Contains(t, anchor, "re-read it after compaction")
}

func TestBuildCompactionAnchorOmitsActivePlanFileWhenUnset(t *testing.T) {
	t.Parallel()

	anchor := buildCompactionAnchor(session.Session{}, nil, nil, "", false)
	require.NotContains(t, anchor, "Active plan file:")
}

func TestBuildCompactionAnchorTodosRenderOnlyAsRunningSubagents(t *testing.T) {
	t.Parallel()

	sess := session.Session{
		Todos: []session.Todo{
			{Status: session.TodoStatusInProgress, Content: "reviewer subagent working"},
		},
	}
	anchor := buildCompactionAnchor(sess, nil, nil, "", false)
	require.Contains(t, anchor, "Running subagents (not a task list):")
	require.Contains(t, anchor, "reviewer subagent working")
	require.NotContains(t, anchor, "Live tracked tasks")
	require.NotContains(t, anchor, "Tracked Tasks")
}

func TestBuildCompactionAnchorOmitsRunningSubagentsWhenNoTodos(t *testing.T) {
	t.Parallel()

	anchor := buildCompactionAnchor(session.Session{}, nil, nil, "", false)
	require.NotContains(t, anchor, "Running subagents")
}

func TestBuildCompactionAnchorPreviousSummaryNotInlined(t *testing.T) {
	t.Parallel()

	const previousSummary = "## Goal\nDo the thing.\n\n## Current State\nA very long previous summary body that must not be duplicated verbatim into the anchor because it is already present as a message in the summarization history.\n\n## Next Steps\n1. Keep going."

	anchor := buildCompactionAnchor(session.Session{}, nil, nil, previousSummary, false)
	require.Contains(t, anchor, "Previous summary: present")
	require.NotContains(t, anchor, "A very long previous summary body")
}

// TestBuildCompactionAnchorPreviousSummaryInlinedWhenDropped covers the
// fitCompactionHistory EnvelopeOnly tier (P1.4 last tier): when fitting
// drops the previous summary message from history under budget pressure,
// buildCompactionAnchor must fall back to inlining its Goal/Current
// State/Next Steps sections instead of the bare "present" marker, so that
// state is not lost entirely.
func TestBuildCompactionAnchorPreviousSummaryInlinedWhenDropped(t *testing.T) {
	t.Parallel()

	const previousSummary = "## Goal\nDo the thing.\n\n## Current State\nHalfway done with the refactor.\n\n## Next Steps\n1. Keep going."

	anchor := buildCompactionAnchor(session.Session{}, nil, nil, previousSummary, true)
	require.NotContains(t, anchor, "Previous summary: present")
	require.Contains(t, anchor, "Previous summary: dropped from history under budget pressure")
	require.Contains(t, anchor, "## Goal")
	require.Contains(t, anchor, "Do the thing.")
	require.Contains(t, anchor, "## Current State")
	require.Contains(t, anchor, "Halfway done with the refactor.")
	require.Contains(t, anchor, "## Next Steps")
	require.Contains(t, anchor, "Keep going.")
}

// TestBuildCompactionAnchorInlineRequestedButNoPreviousSummary covers the
// first-compaction case: inlinePreviousSummary is requested but there is no
// previous summary to inline (previousSummary == ""). The anchor must fall
// back to "absent", never fabricate sections.
func TestBuildCompactionAnchorInlineRequestedButNoPreviousSummary(t *testing.T) {
	t.Parallel()

	anchor := buildCompactionAnchor(session.Session{}, nil, nil, "", true)
	require.Contains(t, anchor, "Previous summary: absent")
}

func TestBuildCompactionAnchorPreviousSummaryAbsent(t *testing.T) {
	t.Parallel()

	anchor := buildCompactionAnchor(session.Session{}, nil, nil, "", false)
	require.Contains(t, anchor, "Previous summary: absent")
}

// --- previousSummaryText (P0.1 regression: SummaryMessageID exact match) ---

// TestPreviousSummaryTextRegressionFailedSummaryDoesNotShadowActiveSummary
// is the P0.1 regression test: a failed compaction attempt persists as an
// IsSummaryMessage message (FinishReasonError, empty Parts) but is never
// adopted into SummaryMessageID (resetSummaryMessage/the retry-exhausted
// path in Summarize use messages.Update, not Delete). Scanning for "the last
// IsSummaryMessage" would pick up that empty failed message instead of the
// real active summary; matching by ID must not.
func TestPreviousSummaryTextRegressionFailedSummaryDoesNotShadowActiveSummary(t *testing.T) {
	t.Parallel()

	msgs := []message.Message{
		{ID: "active-summary", Role: message.User, IsSummaryMessage: true, Parts: []message.ContentPart{message.TextContent{Text: "## Goal\nreal summary\n\n## Current State\nworking\n\n## Next Steps\n1. go"}}},
		textMessage(message.User, "new work after the active summary"),
		textMessage(message.Assistant, "did some work"),
		// Orphaned failed summary: IsSummaryMessage true, empty Parts,
		// never adopted as SummaryMessageID, but chronologically after the
		// active summary so it is still within the sliced msgs range.
		{ID: "failed-summary", Role: message.Assistant, IsSummaryMessage: true, Parts: nil},
	}

	got := previousSummaryText("active-summary", msgs)
	require.Contains(t, got, "real summary")
}

func TestPreviousSummaryTextEmptyIDFallsBackToLeadingSummaryMessage(t *testing.T) {
	t.Parallel()

	msgs := []message.Message{
		{ID: "some-id", Role: message.User, IsSummaryMessage: true, Parts: []message.ContentPart{message.TextContent{Text: "leading summary"}}},
	}
	// SummaryMessageID does not match anything in msgs (e.g. an upstream
	// transform changed message identity); fall back to msgs[0] since
	// getSessionMessages already positions the active summary there.
	got := previousSummaryText("does-not-match-anything", msgs)
	require.Equal(t, "leading summary", got)
}

func TestPreviousSummaryTextNoSummaryReturnsEmpty(t *testing.T) {
	t.Parallel()

	msgs := []message.Message{textMessage(message.User, "no summary here")}
	require.Empty(t, previousSummaryText("", msgs))
	require.Empty(t, previousSummaryText("missing-id", msgs))
}
