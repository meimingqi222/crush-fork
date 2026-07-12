package agent

import (
	"errors"
	"strings"
)

// maxSummaryContentRetries caps how many times Summarize will retry after
// the model produces invalid summary text (tool-call markup or an empty
// response), independent of the transport-level retry budget
// (maxRetriableAttempts) used for network/provider errors.
const maxSummaryContentRetries = 2

// errSummaryToolCallMarkup is returned by Summarize when, after
// maxSummaryContentRetries attempts, the model still produced tool-call
// markup instead of a written summary. It is marked non-retriable so the
// existing transient-error retry loop in Summarize gives up immediately
// instead of spending its own retry budget on a problem retries can't fix.
var errSummaryToolCallMarkup = markNonRetriableError(
	errors.New("summarization produced tool-call markup instead of a summary"),
)

// summaryContentRetryGuard is appended to the summarization prompt when
// retrying after invalid summary text, to make the "no tool calls, plain
// text only" instruction impossible to miss for models that otherwise ignore
// ToolChoiceNone and echo their native tool-call protocol.
const summaryContentRetryGuard = "\n\nIMPORTANT: Output only a plain-text summary. Do not emit any tool call, function call, or markup syntax of any kind (no <invoke>, <function_calls>, DSML tokens, or similar tags) -- write prose only."

// toolCallMarkupMarkers are literal substrings that only appear when a model
// leaks its native tool-calling protocol (DSML, antml, or similar XML-ish
// tool-call syntax) into plain text instead of producing written prose. A
// single occurrence of one of these could plausibly appear in a legitimate
// summary that happens to discuss tool-call syntax (e.g. documenting a bug),
// so callers should only treat the text as garbage once markers repeat -- see
// looksLikeToolCallMarkup.
var toolCallMarkupMarkers = []string{
	"tool_calls>",
	`invoke name="`,
	`parameter name="`,
	"</invoke>",
	"<function_calls",
	"antml:invoke",
}

// looksLikeToolCallMarkup reports whether text appears to be raw tool-call
// markup leaked into a summarization response instead of a written summary.
// This is observed with weaker models (e.g. DeepSeek V4 Flash) that ignore
// the ToolChoiceNone instruction during summarization and instead stream
// their native tool-call protocol as plain text, such as
// "<｜DSML｜tool_calls>" followed by "invoke name=..." / "parameter name=..."
// blocks.
func looksLikeToolCallMarkup(text string) bool {
	// DeepSeek's DSML protocol delimits special tokens with fullwidth
	// vertical bars (U+FF5C), e.g. <｜DSML｜tool_calls>. These delimiters
	// never appear in ordinary prose, so a single occurrence is enough to
	// flag the text as protocol leakage rather than a real summary.
	if strings.Contains(text, "<｜") || strings.Contains(text, "｜>") || strings.Contains(text, "｜DSML｜") {
		return true
	}

	// Generic tool-call markup (DSML "invoke"/"parameter" tags, Claude-style
	// antml invoke blocks, etc.) is only treated as a signal once it repeats,
	// since a single incidental mention of one of these words is plausible
	// in normal prose.
	matches := 0
	for _, marker := range toolCallMarkupMarkers {
		matches += strings.Count(text, marker)
		if matches >= 2 {
			return true
		}
	}
	return false
}

// isInvalidSummaryText reports whether text is unusable as a persisted
// session summary: either empty (the model produced nothing usable) or
// tool-call markup leaked from a model that ignored the summarization
// instructions (see looksLikeToolCallMarkup). Callers should pass the final,
// already-destreamed summary text (e.g. after stripping <think> blocks), not
// raw provider deltas.
func isInvalidSummaryText(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return true
	}
	return looksLikeToolCallMarkup(trimmed)
}
