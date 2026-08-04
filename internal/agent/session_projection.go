package agent

import "github.com/charmbracelet/crush/internal/message"

// sessionProjectionMode selects which active-summary-relative view of
// session history projectSessionMessages returns. See
// docs/refactor-compaction-context.md P2.1.
type sessionProjectionMode int

const (
	// summaryOnly slices raw history at the active summary boundary and
	// returns it unchanged: no role rewrite, no retained segment. This is
	// the pre-existing SummaryMessageID-slicing behavior used by handoff,
	// prompt enhancement, and recap, preserved byte-for-byte as the
	// regression floor for those three call sites.
	summaryOnly sessionProjectionMode = iota
	// summaryWithRecentWork additionally reinserts the most recent safe
	// work segment from before the active summary -- the part that
	// disappeared entirely in the pre-refactor getSessionMessages, since
	// SummaryMessageID slicing kept only the summary and what came after
	// it -- and rewrites the summary's role to user so the projection
	// still opens with a user-role message. Used for the main request path
	// (docs/refactor-compaction-context.md §3.2).
	summaryWithRecentWork
)

// sessionProjectionParams bundles the raw session history and, for
// summaryWithRecentWork, the model whose context budget governs the
// retained-segment and headroom-guard calculations. summaryOnly ignores
// Model and MaxOutputTokens entirely.
type sessionProjectionParams struct {
	// RawMessages is the full, unsliced session history, e.g. straight from
	// messages.List. It is never mutated.
	RawMessages []message.Message
	// SummaryMessageID is session.SummaryMessageID. Empty means no active
	// summary; RawMessages is returned unchanged regardless of Mode.
	SummaryMessageID string
	Mode             sessionProjectionMode
	// Model and MaxOutputTokens are only consulted by summaryWithRecentWork,
	// to size the retained segment (P2.3's retainBudget) and to guard that
	// the assembled projection leaves headroom before the next
	// auto-summarize trigger.
	Model           Model
	MaxOutputTokens int64
}

// projectSessionMessages is the single place every call site that reads
// session history relative to the active compaction summary goes through
// (docs/refactor-compaction-context.md P2.1, P2.4). Before this,
// getSessionMessages, GenerateHandoff, loadEnhancePromptHistory, and
// loadRecapHistory each reimplemented the SummaryMessageID boundary lookup
// independently -- and getSessionMessages's copy discarded everything
// before the active summary, including the recent work that summary was
// supposed to be a compressed *record* of, not a replacement for. That is
// the bug this refactor fixes; summaryWithRecentWork below is the fix, and
// summaryOnly is the preserved old behavior for the other three call sites.
func projectSessionMessages(p sessionProjectionParams) []message.Message {
	if p.SummaryMessageID == "" {
		return p.RawMessages
	}
	summaryIdx := activeSummaryIndex(p.RawMessages, p.SummaryMessageID)
	if summaryIdx < 0 {
		return p.RawMessages
	}

	if p.Mode == summaryWithRecentWork {
		return buildRecentWorkProjection(p.RawMessages, summaryIdx, p.Model, p.MaxOutputTokens)
	}
	return p.RawMessages[summaryIdx:]
}

// activeSummaryIndex returns the index of the message in msgs whose ID
// matches summaryMessageID, or -1 if not found. This is exact-ID matching
// only -- never "the last IsSummaryMessage" -- for the same reason as
// previousSummaryText: failed/provisional summaries persist in the DB
// (resetSummaryMessage and the retry-exhausted path use messages.Update,
// not Delete) without ever being adopted into SummaryMessageID
// (docs/refactor-compaction-context.md P0.1).
func activeSummaryIndex(msgs []message.Message, summaryMessageID string) int {
	for i, msg := range msgs {
		if msg.ID == summaryMessageID {
			return i
		}
	}
	return -1
}

// retainBudget* implement docs/refactor-compaction-context.md P2.3's
// retainBudget = min(20_000, EffectiveContextWindow * 12 / 100). The
// percentage term only binds below an effective window of roughly 167k
// tokens; above that this is a flat 20k-token cap.
const (
	retainBudgetTokenCap  int64 = 20_000
	retainBudgetPercent   int64 = 12
	retainHeadroomPercent int64 = 80
)

func retainBudgetTokens(model Model) int64 {
	pct := EffectiveContextWindow(model.CatwalkCfg) * retainBudgetPercent / 100
	return min(retainBudgetTokenCap, pct)
}

// buildRecentWorkProjection assembles the main-path projection:
//
//	active summary (copied, role rewritten to user)
//	+ newest safe work segment retained from before it
//	+ messages created after it
//
// docs/refactor-compaction-context.md §3.2 -- summary first, not
// recent-then-summary as in grok-build's assemble_compacted_history -- is a
// deliberate deviation for two Crush-specific reasons: the projection must
// open with a user-role message, and [system prompt][summary] then stays a
// stable, and longer than today's, prompt-cache prefix across the whole
// compaction epoch.
func buildRecentWorkProjection(raw []message.Message, summaryIdx int, model Model, maxOutputTokens int64) []message.Message {
	activeSummary := copyMessageAsUserRole(raw[summaryIdx])
	// Tail messages are defensively filtered for IsSummaryMessage too: a
	// compaction attempt that failed after this active summary was already
	// adopted persists its provisional summary message (Update, not
	// Delete) without ever updating SummaryMessageID, so it can appear
	// physically after the active summary in raw history.
	tail := filterSummaryMessages(raw[summaryIdx+1:])

	retained := selectRetainedSegment(raw[:summaryIdx], model, maxOutputTokens, activeSummary, tail)

	projected := make([]message.Message, 0, 1+len(retained)+len(tail))
	projected = append(projected, activeSummary)
	projected = append(projected, retained...)
	projected = append(projected, tail...)
	return projected
}

// precedingSummaryLowerBound returns the P1.2 "边界下限" (lower bound) for
// retained-segment selection within preSummary (raw[:summaryIdx], i.e.
// everything before the active summary): one past the nearest
// IsSummaryMessage in preSummary -- adopted or failed, it doesn't matter --
// or 0 if there is none.
//
// This is a bounding use only, per P2.2: content is never read from that
// message, and it is never treated as "the previous summary" or used to
// reconstruct a summary chain. Without this bound, once summary messages
// are filtered out of the candidate pool, the previous epoch's retained
// work becomes physically adjacent to this epoch's new work, and a
// selector that walks back to "the most recent user message" can cross
// into the older, already-summarized epoch when the new work contains no
// user message of its own (e.g. compaction triggered mid tool-loop).
func precedingSummaryLowerBound(preSummary []message.Message) int {
	for i := len(preSummary) - 1; i >= 0; i-- {
		if preSummary[i].IsSummaryMessage {
			return i + 1
		}
	}
	return 0
}

// selectRetainedSegment picks the retained work segment for the main-path
// projection, applying the P2.3 budget ladder: the full safe segment if it
// fits both retainBudgetTokens and the post-projection headroom guard,
// else its bounded representation (P1.4) if that fits, else no retained
// segment at all -- summary-only degrades gracefully rather than blocking
// the projection or growing a retry state machine.
func selectRetainedSegment(preSummary []message.Message, model Model, maxOutputTokens int64, activeSummary message.Message, tail []message.Message) []message.Message {
	lowerBound := precedingSummaryLowerBound(preSummary)
	if lowerBound >= len(preSummary) {
		return nil
	}
	candidates := preSummary[lowerBound:]

	budget := retainBudgetTokens(model)
	// selectFittingSegment is called with lowerBound 0 against the
	// already-bounded candidates slice (rather than passing preSummary and
	// lowerBound directly) so its internal prefixTokens accounting -- meant
	// for a prefix that is itself part of the output, as in
	// fitCompactionHistory -- doesn't charge the budget for
	// preSummary[:lowerBound], which is excluded from the projection
	// entirely, not retained alongside the segment.
	seg := selectFittingSegment(candidates, 0, budget)

	if fitsRetainedTiers(seg.Messages, budget, maxOutputTokens, activeSummary, tail, model) {
		return seg.Messages
	}
	bounded := buildBoundedSegmentRepresentation(seg)
	if fitsRetainedTiers(bounded, budget, maxOutputTokens, activeSummary, tail, model) {
		return bounded
	}
	return nil
}

func fitsRetainedTiers(candidate []message.Message, budget, maxOutputTokens int64, activeSummary message.Message, tail []message.Message, model Model) bool {
	if estimateMessagesTokensForFitting(candidate) > budget {
		return false
	}
	return projectionFitsHeadroom(activeSummary, candidate, tail, model, maxOutputTokens)
}

// projectionFitsHeadroom implements the P2.3 headroom guard: the assembled
// post-compaction projection must not exceed retainHeadroomPercent of the
// auto-summarize trigger threshold (the same UsableInputTokens
// shouldAutoSummarize compares against), or the very next turn would
// immediately re-trigger compaction.
func projectionFitsHeadroom(activeSummary message.Message, retained, tail []message.Message, model Model, maxOutputTokens int64) bool {
	trigger := promptTokenBudgetForModel(model, maxOutputTokens).UsableInputTokens
	if trigger <= 0 {
		// Unknown or already-negative usable budget (e.g. a misconfigured
		// model whose context window is smaller than the reserved output
		// tokens): there is no meaningful headroom fraction to compare
		// against, and the model is already in an unrecoverable overflow
		// state this guard cannot fix, so don't block on it here.
		return true
	}
	total := estimateMessagesTokensForFitting([]message.Message{activeSummary}) +
		estimateMessagesTokensForFitting(retained) +
		estimateMessagesTokensForFitting(tail)
	return total <= trigger*retainHeadroomPercent/100
}

// copyMessageAsUserRole returns a copy of msg with Role rewritten to User,
// leaving msg itself untouched. Used for the active summary at the head of
// the projection so it always opens with a user-role message
// (docs/refactor-compaction-context.md §3.2), without mutating the message
// read from the DB in place. Callers must never write the result back to
// storage (P2.5: doing so would cause UI duplication, tool-ID collisions,
// and duplicate memory extraction).
func copyMessageAsUserRole(msg message.Message) message.Message {
	cp := msg
	cp.Role = message.User
	if msg.Parts != nil {
		cp.Parts = append([]message.ContentPart(nil), msg.Parts...)
	}
	return cp
}
