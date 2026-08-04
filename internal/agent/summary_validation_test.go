package agent

import (
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/plugin"
	"github.com/stretchr/testify/require"
)

// buildLeakedToolCallText assembles a synthetic "model leaked its native
// tool-call protocol as plain text" fixture out of small fragments. The
// pieces are concatenated at runtime (rather than written as one literal
// blob) purely so the test source never contains a fully-formed nested tag
// sequence in one place; the resulting string is what matters for the
// assertions below.
func buildLeakedToolCallText(pathValue string) string {
	open := "<"
	close_ := "</"
	pipe := "｜" // fullwidth vertical bar (U+FF5C) used by DSML.

	var b strings.Builder
	b.WriteString(open + pipe + "DSML" + pipe + "tool_calls>\n")
	b.WriteString(open + "invoke name=\"read\">\n")
	b.WriteString(open + "parameter name=\"path\" string=\"true\">" + pathValue + close_ + "parameter>\n")
	b.WriteString(close_ + "invoke>\n")
	b.WriteString(open + pipe + "DSML" + pipe + "tool_calls end" + pipe + ">")
	return b.String()
}

func TestSessionSummaryRequiresContinuationSections(t *testing.T) {
	t.Parallel()

	const goal = "## Goal\nwork"
	const currentState = "## Current State\nactive"
	const nextSteps = "## Next Steps\n1. test"

	all := strings.Join([]string{goal, currentState, nextSteps}, "\n\n")
	require.False(t, isInvalidSessionSummaryText(all), "all three required sections present must be valid")

	missingGoal := strings.Join([]string{currentState, nextSteps}, "\n\n")
	require.True(t, isInvalidSessionSummaryText(missingGoal), "missing ## Goal must be invalid")

	missingCurrentState := strings.Join([]string{goal, nextSteps}, "\n\n")
	require.True(t, isInvalidSessionSummaryText(missingCurrentState), "missing ## Current State must be invalid")

	missingNextSteps := strings.Join([]string{goal, currentState}, "\n\n")
	require.True(t, isInvalidSessionSummaryText(missingNextSteps), "missing ## Next Steps must be invalid")

	require.True(t, isInvalidSessionSummaryText("A local implementation detail without any required section"))
}

// TestSummarizeSchemaRetryExhaustionDoesNotOverwriteExistingSummaryPointer
// covers the same content-retry path as
// TestSummarizeExhaustsToolCallMarkupRetriesAndFails (auto_summarize_test.go)
// but for the schema-validation branch (missing required sections) and,
// critically, starts from a session that already has an active summary. Per
// docs/refactor-compaction-context.md P3 / §3.3, a retry-exhausted
// compaction must leave the old SummaryMessageID (and therefore the old
// projection) untouched rather than clearing it -- clearing it would make a
// session that has successfully compacted before regress to appearing
// never-compacted.
func TestSummarizeSchemaRetryExhaustionDoesNotOverwriteExistingSummaryPointer(t *testing.T) {
	plugin.Reset()
	t.Cleanup(plugin.Reset)

	env := testEnv(t)
	testSession, err := env.sessions.Create(t.Context(), "summarize schema retry exhausted keeps old pointer")
	require.NoError(t, err)

	_, err = env.messages.Create(t.Context(), testSession.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: strings.Repeat("x", 4000)}},
	})
	require.NoError(t, err)

	const wellFormedSummary = "## Goal\nDo the thing.\n\n## Current State\nIn progress.\n\n## Next Steps\n1. Keep going."
	firstAgent := &autoSummarizeTestAgent{t: t, summaryTexts: []string{wellFormedSummary}}
	firstSessionAgent := newAutoSummarizeTestSessionAgent(t, env, firstAgent, env.messages, 10000)

	require.NoError(t, firstSessionAgent.Summarize(t.Context(), testSession.ID, nil))

	sessionAfterFirst, err := env.sessions.Get(t.Context(), testSession.ID)
	require.NoError(t, err)
	require.NotEmpty(t, sessionAfterFirst.SummaryMessageID)
	originalSummaryMessageID := sessionAfterFirst.SummaryMessageID

	// New work after the first (successful) compaction, so the second
	// Summarize call has something to summarize.
	_, err = env.messages.Create(t.Context(), testSession.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: strings.Repeat("y", 4000)}},
	})
	require.NoError(t, err)

	const missingSections = "Just some prose with no structured sections at all."
	secondAgent := &autoSummarizeTestAgent{
		t:            t,
		summaryTexts: []string{missingSections, missingSections, missingSections},
	}
	secondSessionAgent := newAutoSummarizeTestSessionAgent(t, env, secondAgent, env.messages, 10000)

	err = secondSessionAgent.Summarize(t.Context(), testSession.ID, nil)
	require.Error(t, err)
	require.Equal(t, maxSummaryContentRetries+1, secondAgent.summaryCalls)

	sessionAfterSecond, err := env.sessions.Get(t.Context(), testSession.ID)
	require.NoError(t, err)
	require.Equal(t, originalSummaryMessageID, sessionAfterSecond.SummaryMessageID,
		"a retry-exhausted compaction must not overwrite the previously active summary pointer")
}

func TestIsInvalidSummaryText(t *testing.T) {
	t.Parallel()

	leaked := buildLeakedToolCallText("internal/agent/agent.go")

	// Two distinct real markers repeated, mirroring an antml-style leak,
	// built from the same marker constants the detector uses so the test
	// stays in sync with looksLikeToolCallMarkup's marker list.
	antmlStyleLeak := strings.Repeat(toolCallMarkupMarkers[5], 2) // "antml:invoke" x2

	tests := []struct {
		name    string
		text    string
		invalid bool
	}{
		{
			name:    "empty text",
			text:    "",
			invalid: true,
		},
		{
			name:    "whitespace only",
			text:    "   \n\t  ",
			invalid: true,
		},
		{
			name:    "dsml tool call markup leaked as plain text",
			text:    leaked,
			invalid: true,
		},
		{
			name:    "bare dsml delimiter is enough on its own",
			text:    "Sure, let me do that. " + "<" + "｜DSML｜tool_calls>",
			invalid: true,
		},
		{
			name:    "repeated antml-style invoke marker",
			text:    "Some preamble text.\n" + antmlStyleLeak + "\nSome trailing text.",
			invalid: true,
		},
		{
			name:    "repeated generic invoke/parameter markers without angle brackets",
			text:    `invoke name="read" ... parameter name="path"`,
			invalid: true,
		},
		{
			name:    "normal summary text",
			text:    "The user asked to refactor the session summarizer and add validation for malformed model output.",
			invalid: false,
		},
		{
			name:    "summary mentioning code and function names",
			text:    "Renamed parseConfig() to loadConfig() and fixed a nil pointer bug in the session summarizer's retry loop.",
			invalid: false,
		},
		{
			name:    "summary with a single incidental mention of invoke",
			text:    "The retry helper will invoke the summarizer once more before giving up.",
			invalid: false,
		},
		{
			name:    "summary with a single incidental mention of parameter",
			text:    "We should document the new parameter for the summarizer's retry budget.",
			invalid: false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.invalid, isInvalidSummaryText(tc.text))
		})
	}
}
