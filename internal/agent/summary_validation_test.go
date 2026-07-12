package agent

import (
	"strings"
	"testing"

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
