package agent

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/crush/internal/message"
)

// Bounded representation limits for an oversized recent work segment.
// docs/refactor-compaction-context.md P1.4.
const (
	boundedUserRequestMaxRunes   = 4000
	boundedAssistantTextMaxRunes = 4000
	boundedAssistantHeadRunes    = boundedAssistantTextMaxRunes / 2
	boundedAssistantTailRunes    = boundedAssistantTextMaxRunes / 2
	boundedMaxToolActivities     = 12
	boundedToolInputMaxRunes     = 500
	boundedToolOutputMaxRunes    = 2000
	boundedToolOutputHeadRunes   = boundedToolOutputMaxRunes / 2
	boundedToolOutputTailRunes   = boundedToolOutputMaxRunes / 2
)

// fitCompactionResult is the outcome of fitCompactionHistory.
type fitCompactionResult struct {
	// Messages is the fitted trimmable history to send to the summarizer,
	// in original order. Empty when EnvelopeOnly is true.
	Messages []message.Message
	// Bounded reports that the safe recent work segment alone exceeded
	// budget and was replaced with a deterministic bounded representation
	// (P1.4) instead of the original messages.
	Bounded bool
	// EnvelopeOnly reports that even the bounded representation exceeded
	// budget. Messages is empty; the caller must summarize from the fixed
	// envelope alone (Session Anchor + Previous Summary sections inlined +
	// memory rescue + summary instructions) rather than fail outright --
	// returning failure here would mean compaction can never succeed again
	// for this session (P1.4, last tier).
	EnvelopeOnly bool
}

// fitCompactionHistory fits messages into maxTokens while message
// structure -- tool call/result pairing, IsSummaryMessage -- is still
// intact, i.e. before preparePrompt/flattenToolCallsForSummary have
// converted the history to fantasy messages and destroyed that structure.
// It never summarizes or infers anything; it only selects which existing
// messages to keep, in four tiers:
//
//  1. everything fits: keep messages unchanged;
//  2. it doesn't: keep as many whole, safe turns as fit within maxTokens,
//     walking backward from the newest turn and dropping older history
//     from the oldest end once the budget is exhausted (see
//     selectFittingSegment), plus the fixed prefix before lowerBound
//     (normally just the previous active summary message, if any) --
//     this always keeps at least selectRecentWorkSegment's single newest
//     turn, and more when the budget allows;
//  3. even the single newest safe segment alone doesn't fit: replace it
//     with a deterministic bounded representation
//     (buildBoundedSegmentRepresentation) and re-estimate;
//  4. that still doesn't fit: drop everything (EnvelopeOnly).
//
// lowerBound is the index in messages the safe-segment selector must not
// cross -- see selectRecentWorkSegment.
func fitCompactionHistory(messages []message.Message, lowerBound int, maxTokens int64) fitCompactionResult {
	if len(messages) == 0 {
		return fitCompactionResult{Messages: messages}
	}
	if maxTokens <= 0 {
		return fitCompactionResult{EnvelopeOnly: true}
	}
	if lowerBound < 0 {
		lowerBound = 0
	}
	if lowerBound > len(messages) {
		lowerBound = len(messages)
	}

	if estimateMessagesTokensForFitting(messages) <= maxTokens {
		return fitCompactionResult{Messages: messages}
	}

	prefix := messages[:lowerBound]
	segment := selectFittingSegment(messages, lowerBound, maxTokens)

	candidate := make([]message.Message, 0, len(prefix)+len(segment.Messages))
	candidate = append(candidate, prefix...)
	candidate = append(candidate, segment.Messages...)
	if estimateMessagesTokensForFitting(candidate) <= maxTokens {
		return fitCompactionResult{Messages: candidate}
	}

	// selectFittingSegment already guarantees segment is at least
	// selectRecentWorkSegment's single newest turn; reaching here means
	// even that minimal, safe segment alone exceeds maxTokens.
	bounded := buildBoundedSegmentRepresentation(segment)
	boundedCandidate := make([]message.Message, 0, len(prefix)+len(bounded))
	boundedCandidate = append(boundedCandidate, prefix...)
	boundedCandidate = append(boundedCandidate, bounded...)
	if estimateMessagesTokensForFitting(boundedCandidate) <= maxTokens {
		return fitCompactionResult{Messages: boundedCandidate, Bounded: true}
	}

	return fitCompactionResult{Bounded: true, EnvelopeOnly: true}
}

// estimateMessagesTokensForFitting estimates the token cost of a
// []message.Message slice using the same per-part accounting as
// estimatePromptTokens, by converting each message through its normal
// ToAIMessage path. It is a pure function (no *sessionAgent dependency) so
// fitCompactionHistory stays independently testable; the exact figure will
// differ slightly from the estimate computed on the final, flattened
// aiMsgs sent to the provider, since flattening and preparePrompt's
// tool-result de-duplication happen after fitting. Both are heuristics, so
// this is an acceptable approximation for deciding what to keep.
func estimateMessagesTokensForFitting(msgs []message.Message) int64 {
	var total int64
	for _, m := range msgs {
		for _, fm := range m.ToAIMessage() {
			total += estimateMessageContentTokens(fm.Content)
		}
	}
	return total
}

// capRunes truncates s to at most max runes, appending a truncation marker
// when content was dropped.
func capRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "… (truncated)"
}

// headTailRunes keeps the first head and last tail runes of s, replacing
// the middle with a marker, when s exceeds head+tail runes.
func headTailRunes(s string, head, tail int) string {
	r := []rune(s)
	if len(r) <= head+tail {
		return s
	}
	return string(r[:head]) + "\n… (truncated) …\n" + string(r[len(r)-tail:])
}

// boundedToolActivity is one rendered "Tool activity" entry in a bounded
// segment representation.
type boundedToolActivity struct {
	Name   string
	CallID string
	Status string // "completed" | "error" | "pending"
	Input  string
	Output string
}

// buildBoundedSegmentRepresentation produces the deterministic, bounded
// stand-in for an oversized recent work segment described in
// docs/refactor-compaction-context.md P1.4: the most recent user request,
// the most recent visible assistant text, and up to
// boundedMaxToolActivities recent tool activities. It never infers
// decisions, completion status, or anything not already present in the
// segment's messages, and it never calls a model.
func buildBoundedSegmentRepresentation(segment recentWorkSegment) []message.Message {
	userRequest := boundedUserRequestText(segment.Messages)
	assistantText := boundedAssistantText(segment.Messages)
	activities := boundedToolActivities(segment.Messages)

	var b strings.Builder
	b.WriteString("[The most recent work segment exceeded the summarization budget. ")
	b.WriteString("This is a bounded representation of it, not full history: ")
	b.WriteString("the request, most recent visible progress, and recent tool activity, each truncated to a fixed limit.]\n")

	b.WriteString("\nUser request:\n")
	if userRequest != "" {
		b.WriteString(userRequest)
	} else {
		b.WriteString("[no user message in this segment]")
	}

	b.WriteString("\n\nAssistant progress:\n")
	if assistantText != "" {
		b.WriteString(assistantText)
	} else {
		b.WriteString("[no assistant text in this segment]")
	}

	b.WriteString("\n\nTool activity:")
	if len(activities) == 0 {
		b.WriteString(" [none in this segment]")
	}
	for _, act := range activities {
		fmt.Fprintf(&b, "\n\n- %s (%s): %s", act.Name, act.CallID, act.Status)
		if act.Input != "" {
			fmt.Fprintf(&b, "\n  input: %s", act.Input)
		}
		if act.Output != "" {
			fmt.Fprintf(&b, "\n  output: %s", act.Output)
		}
	}

	rendered := message.Message{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: b.String()}},
	}
	rendered.AddFinish(message.FinishReasonEndTurn, "", "")
	return []message.Message{rendered}
}

// boundedUserRequestText returns the most recent non-summary user message
// text in messages, capped to boundedUserRequestMaxRunes.
func boundedUserRequestText(messages []message.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != message.User || messages[i].IsSummaryMessage {
			continue
		}
		if text := messageTextForCompaction(messages[i]); text != "" {
			return capRunes(text, boundedUserRequestMaxRunes)
		}
	}
	return ""
}

// boundedAssistantText returns the most recent assistant visible text
// (TextContent only -- reasoning is never included) in messages, as
// boundedAssistantHeadRunes head + boundedAssistantTailRunes tail.
func boundedAssistantText(messages []message.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != message.Assistant {
			continue
		}
		if text := messageTextForCompaction(messages[i]); text != "" {
			return headTailRunes(text, boundedAssistantHeadRunes, boundedAssistantTailRunes)
		}
	}
	return ""
}

// boundedToolActivities collects tool call/result pairs from messages, in
// call order, keeping every result for a parallel tool call group
// together and capping the total to roughly boundedMaxToolActivities by
// dropping whole groups from the oldest end. Already-archived/truncated
// results (the builtinPruneToolResults placeholder) are passed through
// verbatim rather than re-truncated, and don't count against the output
// rune cap -- they are already bounded.
func boundedToolActivities(messages []message.Message) []boundedToolActivity {
	type activity struct {
		name   string
		callID string
		status string
		input  string
		output string
	}

	var groups [][]*activity
	byCallID := make(map[string]*activity)

	for _, msg := range messages {
		switch msg.Role {
		case message.Assistant:
			calls := msg.ToolCalls()
			if len(calls) == 0 {
				continue
			}
			group := make([]*activity, 0, len(calls))
			for _, tc := range calls {
				if tc.ID == "" {
					continue
				}
				act := &activity{
					name:   tc.Name,
					callID: tc.ID,
					status: "pending",
					input:  capRunes(strings.TrimSpace(tc.Input), boundedToolInputMaxRunes),
				}
				byCallID[tc.ID] = act
				group = append(group, act)
			}
			if len(group) > 0 {
				groups = append(groups, group)
			}
		case message.Tool:
			for _, tr := range msg.ToolResults() {
				act, ok := byCallID[tr.ToolCallID]
				if !ok {
					// The segment selector extends backward to avoid
					// orphaned results, so this shouldn't happen; skip
					// defensively rather than fabricate a call.
					continue
				}
				if tr.IsError {
					act.status = "error"
				} else {
					act.status = "completed"
				}
				if tr.Content == "" {
					continue
				}
				if strings.HasPrefix(tr.Content, builtinPruneCompactedNoticePrefix) {
					act.output = tr.Content
				} else {
					act.output = headTailRunes(tr.Content, boundedToolOutputHeadRunes, boundedToolOutputTailRunes)
				}
			}
		}
	}

	startGroup := len(groups)
	count := 0
	for i := len(groups) - 1; i >= 0; i-- {
		if count > 0 && count+len(groups[i]) > boundedMaxToolActivities {
			break
		}
		startGroup = i
		count += len(groups[i])
	}

	result := make([]boundedToolActivity, 0, count)
	for _, group := range groups[startGroup:] {
		for _, act := range group {
			result = append(result, boundedToolActivity{
				Name:   act.name,
				CallID: act.callID,
				Status: act.status,
				Input:  act.input,
				Output: act.output,
			})
		}
	}
	return result
}
