package agent

import "github.com/charmbracelet/crush/internal/message"

// recentWorkSegment is the most recent safe tail interval of a message
// history: a run of messages that can be cut from the rest of history
// without splitting a tool call from its result. See
// docs/refactor-compaction-context.md P1.2.
type recentWorkSegment struct {
	Messages []message.Message
	// HasPendingCall reports whether the segment ends with a tool call that
	// has not yet received a result (e.g. compaction triggered mid tool
	// loop). The segment still includes the call message as-is; callers
	// must never fabricate a completed result for it.
	HasPendingCall bool
}

// selectRecentWorkSegment picks the most recent safe work segment from
// messages. It:
//
//   - excludes all IsSummaryMessage messages;
//   - starts at the most recent non-summary user message;
//   - never selects anything before lowerBound, even if that means the
//     segment has no user message at all (e.g. new work created after the
//     active summary but before the next user turn, such as compaction
//     triggered mid tool-loop);
//   - extends backward past that starting point, but never past
//     lowerBound, when the initial cut would otherwise orphan a tool
//     result whose call lies before the cut;
//   - keeps every result for a parallel tool call group together, since
//     extending back to the assistant message that issued the calls pulls
//     in all of that message's results, which always follow it.
//
// lowerBound is the index in messages that the segment must not cross --
// typically one past the active compaction summary, so a second
// compaction epoch can never reach back into work already folded into the
// previous summary (docs/refactor-compaction-context.md P1.2, "关于
// groupCompactionTurns 的 openCalls 重置").
func selectRecentWorkSegment(messages []message.Message, lowerBound int) recentWorkSegment {
	if lowerBound < 0 {
		lowerBound = 0
	}
	if lowerBound >= len(messages) {
		return recentWorkSegment{}
	}
	candidates := messages[lowerBound:]

	start := mostRecentUserMessageIndex(candidates)
	if start < 0 {
		// No user message in the candidate range at all -- do not reach
		// back past lowerBound looking for one; take everything from the
		// bound forward instead.
		start = 0
	}
	start = extendPastOrphanedResults(candidates, start)

	segment := filterSummaryMessages(candidates[start:])
	return recentWorkSegment{
		Messages:       segment,
		HasPendingCall: hasOpenToolCall(segment),
	}
}

// selectFittingSegment extends selectRecentWorkSegment's minimal "most
// recent safe segment" backward, turn by turn, while the running estimate
// of prefix (messages[:lowerBound]) plus the growing segment stays within
// maxTokens. This is the "从最老处删除" tier of
// docs/refactor-compaction-context.md P1.3: history that doesn't fit in
// full is served as much recent, safe history as the budget allows,
// dropping only the oldest turns once the budget is exhausted, rather than
// jumping straight from "everything" to "one turn".
//
// Turn boundaries are the same non-summary user-message positions
// selectRecentWorkSegment anchors on; the newest turn boundary this
// function tries is exactly selectRecentWorkSegment's own starting point,
// so the result here is always a superset of (never smaller than) what
// selectRecentWorkSegment alone would return. If even that minimal segment
// exceeds maxTokens, it is returned unchanged (still safe, just over
// budget) so the caller can fall through to a bounded representation of
// it.
func selectFittingSegment(messages []message.Message, lowerBound int, maxTokens int64) recentWorkSegment {
	floor := selectRecentWorkSegment(messages, lowerBound)
	if lowerBound < 0 {
		lowerBound = 0
	}
	if lowerBound >= len(messages) {
		return floor
	}
	candidates := messages[lowerBound:]
	prefixTokens := estimateMessagesTokensForFitting(messages[:lowerBound])

	starts := turnStartBoundaries(candidates)
	best := floor
	// Walk turn boundaries from newest to oldest, growing the segment one
	// turn at a time. The first (newest) boundary reproduces the floor
	// exactly; each subsequent, older boundary adds another whole turn.
	// Stop -- keeping the last boundary that fit -- as soon as one doesn't,
	// since every older boundary only adds more messages/tokens.
	for i := len(starts) - 1; i >= 0; i-- {
		start := extendPastOrphanedResults(candidates, starts[i])
		segment := filterSummaryMessages(candidates[start:])
		total := prefixTokens + estimateMessagesTokensForFitting(segment)
		if total > maxTokens {
			break
		}
		best = recentWorkSegment{
			Messages:       segment,
			HasPendingCall: hasOpenToolCall(segment),
		}
	}
	return best
}

// turnStartBoundaries returns the ascending indices in messages where each
// "turn" begins: index 0, plus the index of every subsequent non-summary
// user message. It never looks past messages -- callers scope messages to
// the region after any lower bound before calling this.
func turnStartBoundaries(messages []message.Message) []int {
	if len(messages) == 0 {
		return nil
	}
	starts := []int{0}
	for i := 1; i < len(messages); i++ {
		if messages[i].Role == message.User && !messages[i].IsSummaryMessage {
			starts = append(starts, i)
		}
	}
	return starts
}

// filterSummaryMessages drops IsSummaryMessage entries. Defensive: a
// failed/provisional summary can be physically interleaved in history
// without ever being adopted as the active summary (see
// previousSummaryText). It carries no real work and must never appear
// inside a work segment.
func filterSummaryMessages(messages []message.Message) []message.Message {
	out := make([]message.Message, 0, len(messages))
	for _, msg := range messages {
		if msg.IsSummaryMessage {
			continue
		}
		out = append(out, msg)
	}
	return out
}

// mostRecentUserMessageIndex returns the index of the last non-summary
// user message in messages, or -1 if there is none.
func mostRecentUserMessageIndex(messages []message.Message) int {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == message.User && !messages[i].IsSummaryMessage {
			return i
		}
	}
	return -1
}

// extendPastOrphanedResults walks start backward, as many times as
// necessary, so that no tool result in messages[start:] refers to a call
// issued before start. It never moves start below 0. Each step finds the
// assistant message that issued the orphaning call and moves start to
// that message's index -- which also pulls in any sibling results from a
// parallel tool call group at the same message, since the segment's tail
// end never changes.
func extendPastOrphanedResults(messages []message.Message, start int) int {
	for start > 0 {
		orphanCallID := firstOrphanedToolCallID(messages[start:])
		if orphanCallID == "" {
			return start
		}
		callIdx := lastToolCallMessageIndex(messages[:start], orphanCallID)
		if callIdx < 0 {
			// The call itself isn't reachable without crossing below index
			// 0 (or it was already excluded upstream). Nothing more can be
			// done without violating the lower bound.
			return start
		}
		start = callIdx
	}
	return start
}

// firstOrphanedToolCallID returns the tool_call_id of the first tool
// result in messages whose corresponding call does not appear earlier in
// messages, or "" if every result is matched.
func firstOrphanedToolCallID(messages []message.Message) string {
	open := make(map[string]struct{})
	for _, msg := range messages {
		for _, call := range msg.ToolCalls() {
			if call.ID != "" {
				open[call.ID] = struct{}{}
			}
		}
		for _, result := range msg.ToolResults() {
			if _, ok := open[result.ToolCallID]; ok {
				delete(open, result.ToolCallID)
				continue
			}
			return result.ToolCallID
		}
	}
	return ""
}

// lastToolCallMessageIndex returns the index of the last message in
// messages that issued a tool call with the given ID, or -1 if none did.
func lastToolCallMessageIndex(messages []message.Message, callID string) int {
	for i := len(messages) - 1; i >= 0; i-- {
		for _, call := range messages[i].ToolCalls() {
			if call.ID == callID {
				return i
			}
		}
	}
	return -1
}

// hasOpenToolCall reports whether messages contains a tool call with no
// matching result within messages.
func hasOpenToolCall(messages []message.Message) bool {
	open := make(map[string]struct{})
	for _, msg := range messages {
		for _, call := range msg.ToolCalls() {
			if call.ID != "" {
				open[call.ID] = struct{}{}
			}
		}
		for _, result := range msg.ToolResults() {
			delete(open, result.ToolCallID)
		}
	}
	return len(open) > 0
}
