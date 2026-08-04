package agent

import (
	"log/slog"

	"github.com/charmbracelet/crush/internal/message"
)

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
	// summaryForCompaction returns the active summary, re-roled to user,
	// followed only by messages physically created after it. Unlike the
	// main request projection, it deliberately excludes retained pre-summary
	// work: that work is already represented by the active summary and must
	// not be summarized again on every compaction epoch.
	summaryForCompaction
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
		if p.Mode == summaryForCompaction {
			// A failed first compaction can leave an unadopted summary in
			// raw history even though the session has no active summary ID.
			// It is UI/error state, not conversation evidence for the next
			// summarizer.
			return filterSummaryMessages(p.RawMessages)
		}
		return p.RawMessages
	}
	summaryIdx := activeSummaryIndex(p.RawMessages, p.SummaryMessageID)
	if summaryIdx < 0 {
		return p.RawMessages
	}

	if p.Mode == summaryWithRecentWork {
		return buildRecentWorkProjection(p.RawMessages, summaryIdx, p.Model, p.MaxOutputTokens)
	}
	if p.Mode == summaryForCompaction {
		projected := make([]message.Message, 0, len(p.RawMessages)-summaryIdx)
		projected = append(projected, copyMessageAsUserRole(p.RawMessages[summaryIdx]))
		projected = append(projected, filterSummaryMessages(p.RawMessages[summaryIdx+1:])...)
		return projected
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

	retained := selectRetainedSegment(raw[:summaryIdx], model, maxOutputTokens, activeSummary)

	// retained_tokens is the only signal that says whether this refactor is
	// actually doing anything in production: 0 means the projection has
	// silently degraded to the pre-refactor summary-only behaviour (budget
	// too tight, headroom guard tripped, or no safe segment before the
	// summary). Logged at Debug because getSessionMessages runs on every
	// request (docs/refactor-compaction-context.md §7).
	slog.Debug("Built recent-work session projection",
		"retained_tokens", estimateMessagesTokensForFitting(retained),
		"retained_messages", len(retained),
		"pre_summary_messages", summaryIdx,
		"tail_messages", len(tail),
	)

	projected := make([]message.Message, 0, 1+len(retained)+len(tail))
	projected = append(projected, activeSummary)
	projected = append(projected, retained...)
	projected = append(projected, tail...)
	return projected
}

// selectRetainedSegment picks the retained work segment for the main-path
// projection, applying the P2.3 budget ladder: the full safe segment if it
// fits both retainBudgetTokens and the post-projection headroom guard,
// else its bounded representation (P1.4) if that fits, else no retained
// segment at all -- summary-only degrades gracefully rather than blocking
// the projection or growing a retry state machine.
func selectRetainedSegment(preSummary []message.Message, model Model, maxOutputTokens int64, activeSummary message.Message) []message.Message {
	if len(preSummary) == 0 {
		return nil
	}

	budget := retainBudgetTokens(model)
	lowerBound := precedingAdoptedSummaryLowerBound(preSummary)
	// Scope the selector to the current epoch. Passing a sliced candidate
	// list avoids charging the retain budget for older messages that cannot
	// appear in the projection.
	seg := selectFittingSegment(preSummary[lowerBound:], 0, budget)

	if fitsRetainedTiers(seg.Messages, budget, maxOutputTokens, activeSummary, model) {
		return seg.Messages
	}
	bounded := buildBoundedSegmentRepresentation(seg)
	if fitsRetainedTiers(bounded, budget, maxOutputTokens, activeSummary, model) {
		return bounded
	}
	return nil
}

// precedingAdoptedSummaryLowerBound returns one past the latest successful
// summary in preSummary. Successful summaries delimit compaction epochs;
// failed or interrupted provisional summaries do not, even though they also
// have IsSummaryMessage set.
func precedingAdoptedSummaryLowerBound(preSummary []message.Message) int {
	for i := len(preSummary) - 1; i >= 0; i-- {
		if preSummary[i].IsSummaryMessage && preSummary[i].FinishReason() == message.FinishReasonEndTurn {
			return i + 1
		}
	}
	return 0
}

func fitsRetainedTiers(candidate []message.Message, budget, maxOutputTokens int64, activeSummary message.Message, model Model) bool {
	if estimateMessagesTokensForFitting(candidate) > budget {
		return false
	}
	return projectionFitsHeadroom(activeSummary, candidate, model, maxOutputTokens)
}

// projectionFitsHeadroom implements the P2.3 headroom guard: the assembled
// post-compaction projection must not exceed retainHeadroomPercent of the
// auto-summarize trigger threshold (the same UsableInputTokens
// shouldAutoSummarize compares against), or the very next turn would
// immediately re-trigger compaction.
//
// The live tail is deliberately NOT counted. P2.3 scopes this guard to the
// *post-compaction* projection -- the moment compaction finishes, when the
// tail is empty -- and P2.4 requires the retained prefix to be decided by
// the frozen pre-summary messages so it stays byte-stable for the whole
// epoch. Including the tail broke both: the tail grows monotonically every
// turn, so the gate would flip mid-epoch, silently dropping the retained
// segment and breaking the [system][summary][retained] prompt-cache prefix
// exactly once per epoch, after which the retained-work benefit was lost
// until the next compaction. Later tail growth pushing the session back over
// the trigger is what auto-compaction is for; retainBudgetTokens already
// caps this segment's contribution.
func projectionFitsHeadroom(activeSummary message.Message, retained []message.Message, model Model, maxOutputTokens int64) bool {
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
		estimateMessagesTokensForFitting(retained)
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
