package agent

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
)

// Goal source labels for the "Goal source" anchor line. See
// compactionGoalAndSource for the priority order these are assigned in.
const (
	goalSourceGoalTool        = "goal-tool"
	goalSourceEarliestUserMsg = "earliest-user-message"
	goalSourcePreviousSummary = "previous-summary"
	goalSourceUnavailable     = "unavailable"
)

// unavailableGoalPlaceholder is emitted instead of a guessed goal when none
// of the goal sources below produced anything usable. Never replaced with an
// inferred value -- see docs/refactor-compaction-context.md P0.1.
const unavailableGoalPlaceholder = "[unavailable in retained history]"

func messageTextForCompaction(msg message.Message) string {
	return strings.TrimSpace(msg.Content().Text)
}

// earliestUserGoalText returns the text of the earliest non-summary user
// message in messages, or "" if none exists. Callers must pass raw, unsliced
// session history (e.g. from messages.List), not the active summarization
// projection -- getSessionMessages slices history to start at the active
// summary boundary, so the original request may not be reachable from that
// projection at all.
func earliestUserGoalText(messages []message.Message) string {
	for _, msg := range messages {
		if msg.Role != message.User || msg.IsSummaryMessage {
			continue
		}
		if text := messageTextForCompaction(msg); text != "" {
			return text
		}
	}
	return ""
}

// previousSummaryText returns the text of the previously active compaction
// summary by matching summaryMessageID exactly against msgs, never by
// scanning for "the last IsSummaryMessage". Failed compaction attempts
// persist as IsSummaryMessage messages too (resetSummaryMessage and the
// retry-exhausted path in Summarize use messages.Update, not Delete) but are
// never adopted into session.SummaryMessageID, so scanning for the last
// match can silently pick up an orphaned, empty failed summary instead of
// the real active one. See docs/refactor-compaction-context.md P0.1.
func previousSummaryText(summaryMessageID string, msgs []message.Message) string {
	if summaryMessageID != "" {
		for _, msg := range msgs {
			if msg.ID == summaryMessageID {
				return messageTextForCompaction(msg)
			}
		}
	}
	// Fallback safety net: getSessionMessages already places the active
	// summary at index 0 when SummaryMessageID resolves against the full
	// history. Use it directly if the exact-ID lookup above missed, e.g.
	// because an upstream transform changed message identity between
	// getSessionMessages and this call.
	if len(msgs) > 0 && msgs[0].IsSummaryMessage {
		return messageTextForCompaction(msgs[0])
	}
	return ""
}

// lastUserMessageText returns the most recent genuine user request. Summary
// messages are skipped for the same reason as in earliestUserGoalText:
// getSessionMessages re-roles the active summary to message.User, so a
// projection whose only user-role message is that summary would otherwise
// report the entire previous summary as the current user request.
func lastUserMessageText(messages []message.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != message.User || messages[i].IsSummaryMessage {
			continue
		}
		if text := messageTextForCompaction(messages[i]); text != "" {
			return text
		}
	}
	return ""
}

// compactionGoalAndSource resolves the original user goal and reports how it
// was derived, following the fixed priority order from
// docs/refactor-compaction-context.md P0.1:
//
//  1. an active or paused session.Goal.Text -- the user-confirmed objective,
//     which always wins over inferring intent from history;
//  2. the earliest non-summary user message in the raw (unsliced) session
//     history, for sessions that never used the goal tool;
//  3. the "## Goal" section of the previous compaction summary, when even
//     raw history does not reach back far enough (e.g. it was itself
//     compacted away in an earlier epoch);
//  4. otherwise the literal "unavailable" marker -- never a guess.
func compactionGoalAndSource(sess session.Session, rawMessages []message.Message, previousSummary string) (goalText, source string) {
	if goal := strings.TrimSpace(sess.Goal.Text); goal != "" {
		if sess.Goal.IsActive() || sess.Goal.Status == session.GoalStatusPaused {
			return goal, goalSourceGoalTool
		}
	}
	if text := earliestUserGoalText(rawMessages); text != "" {
		return text, goalSourceEarliestUserMsg
	}
	if previousSummary != "" {
		if text := extractSummarySection(previousSummary, "Goal"); text != "" {
			return text, goalSourcePreviousSummary
		}
	}
	return "", goalSourceUnavailable
}

// buildCompactionAnchor renders the fixed "Session Anchor" block that opens
// every summarization prompt. Unlike the rest of the summarization input,
// this section is derived from durable session state (session.Goal,
// PlanFilePath, SummaryMessageID) instead of from history that gets dropped
// or rewritten every compaction epoch, so it survives compaction even when
// the retained message window does not reach back far enough to contain the
// original request.
//
// rawMessages must be the raw, unsliced session history (e.g. from
// messages.List); it is used only to recover the original user goal when no
// session.Goal is set (see compactionGoalAndSource) and is not otherwise
// part of what gets sent to the summarization model. projectionMessages is
// the active summarization input -- the same, already summary-boundary-
// sliced history Summarize sends to the model -- and is used here only for
// the current/latest user request line, matching prior behavior.
//
// inlinePreviousSummary requests that previousSummary's Goal/Current
// State/Next Steps sections be copied into the anchor instead of the
// one-line "present" marker. It must only be set true when
// fitCompactionHistory has actually dropped the previous summary message
// from the fitted history under budget pressure (the EnvelopeOnly tier) --
// normally the summary survives as a plain message in the fitted history
// and inlining it here would just duplicate it
// (docs/refactor-compaction-context.md P0.2, P1.4).
func buildCompactionAnchor(sess session.Session, rawMessages, projectionMessages []message.Message, previousSummary string, inlinePreviousSummary bool) string {
	originalGoal, goalSource := compactionGoalAndSource(sess, rawMessages, previousSummary)
	currentRequest := lastUserMessageText(projectionMessages)

	var b strings.Builder
	b.WriteString("## Session Anchor\n")
	if originalGoal != "" {
		fmt.Fprintf(&b, "- Original user goal: %s\n", compactAnchorText(originalGoal))
	} else {
		fmt.Fprintf(&b, "- Original user goal: %s\n", unavailableGoalPlaceholder)
	}
	fmt.Fprintf(&b, "- Goal source: %s\n", goalSource)
	if sess.Goal.Status != "" {
		fmt.Fprintf(&b, "- Goal status: %s\n", sess.Goal.Status)
		if sess.Goal.HasBudget() {
			fmt.Fprintf(&b, "- Goal budget: %d/%d\n", sess.Goal.TokensUsed, sess.Goal.TokenBudget)
		}
	}
	if currentRequest != "" {
		fmt.Fprintf(&b, "- Current/latest user request: %s\n", compactAnchorText(currentRequest))
	}
	if sess.WorkspaceCWD != "" {
		fmt.Fprintf(&b, "- Workspace: %s\n", sess.WorkspaceCWD)
	}
	if sess.PlanFilePath != "" {
		fmt.Fprintf(&b, "- Active plan file: %s (authoritative -- re-read it after compaction before continuing plan work)\n", sess.PlanFilePath)
	}
	// session.Todos is subagent-bridge runtime state (see the stale-todos
	// clear in sessionAgent.Run and subagentBridge.syncTodos in
	// mailbox_bridge.go), not a general task source. It is surfaced here,
	// labeled accordingly, and never given precedence over the summary.
	if len(sess.Todos) > 0 {
		b.WriteString("- Running subagents (not a task list):\n")
		for _, todo := range sess.Todos {
			fmt.Fprintf(&b, "  - [%s] %s\n", todo.Status, todo.Content)
		}
	}
	switch {
	case previousSummary != "" && inlinePreviousSummary:
		b.WriteString("- Previous summary: dropped from history under budget pressure; key sections inlined below:\n")
		for _, section := range []string{"Goal", "Current State", "Next Steps"} {
			if text := extractSummarySection(previousSummary, section); text != "" {
				fmt.Fprintf(&b, "  ## %s\n  %s\n", section, compactAnchorText(text))
			}
		}
	case previousSummary != "":
		b.WriteString("- Previous summary: present\n")
	default:
		b.WriteString("- Previous summary: absent\n")
	}
	return strings.TrimSpace(b.String())
}

// extractSummarySection returns the trimmed body of a top-level "## <section>"
// Markdown section within summary, or "" if the section is absent.
func extractSummarySection(summary, section string) string {
	marker := "## " + section
	start := strings.Index(summary, marker)
	if start < 0 {
		return ""
	}
	start += len(marker)
	end := strings.Index(summary[start:], "\n## ")
	if end < 0 {
		end = len(summary) - start
	}
	return strings.TrimSpace(summary[start : start+end])
}

func compactAnchorText(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if len([]rune(text)) > 4000 {
		text = string([]rune(text)[:4000]) + "…"
	}
	return text
}
