package agent

import (
	"fmt"
	"log/slog"

	"github.com/charmbracelet/crush/internal/message"
)

const (
	// builtinMicroCompactRecentWindow is the number of recent assistant turns
	// whose reasoning content and binary attachments are preserved during
	// micro-compaction. Older turns have these stripped to reduce token usage.
	builtinMicroCompactRecentWindow = 5

	// builtinAutoCompactRecentWindow is the number of recent assistant turns
	// to keep at full fidelity during auto-compaction. Tool results in older
	// turns are truncated to builtinAutoCompactToolResultMaxChars.
	builtinAutoCompactRecentWindow = 10

	// builtinAutoCompactToolResultMaxChars is the maximum number of characters
	// kept per tool-result during auto-compaction of older turns.
	builtinAutoCompactToolResultMaxChars = 1_000

	// builtinPruneProtectChars is the character budget for recent tool results
	// that are protected from pruning. Tool results within this budget
	// (counting backwards from newest) are kept intact.
	builtinPruneProtectChars = 120_000

	// builtinPruneMinChars is the minimum number of characters that must be
	// prunable before we bother running the prune pass. Avoids churning
	// through messages for negligible savings.
	builtinPruneMinChars = 60_000

	// builtinPruneRecentTurns is the number of recent assistant turns whose
	// tool results are unconditionally protected from pruning.
	builtinPruneRecentTurns = 2
)

// builtinMicroCompactMessages strips reasoning content and binary attachments
// from older assistant messages to reduce token usage before summarization.
// Messages within the most recent builtinMicroCompactRecentWindow assistant
// turns are left untouched.
func builtinMicroCompactMessages(msgs []message.Message) []message.Message {
	cutoff := assistantTurnCutoff(msgs, builtinMicroCompactRecentWindow)
	if cutoff == 0 {
		return msgs
	}
	changed := false
	result := make([]message.Message, len(msgs))
	copy(result, msgs)
	for i := 0; i < cutoff; i++ {
		newParts := make([]message.ContentPart, 0, len(msgs[i].Parts))
		stripped := false
		for _, part := range msgs[i].Parts {
			switch part.(type) {
			case message.ReasoningContent, message.BinaryContent, message.ImageURLContent:
				stripped = true
				changed = true
			default:
				newParts = append(newParts, part)
			}
		}
		if stripped {
			cloned := msgs[i].Clone()
			cloned.Parts = newParts
			result[i] = cloned
		}
	}
	if !changed {
		return msgs
	}
	return result
}

// builtinAutoCompactMessages truncates oversized tool results in older turns to
// a compact representation suitable for summarization context. Messages within
// the most recent builtinAutoCompactRecentWindow assistant turns are kept at
// full fidelity.
func builtinAutoCompactMessages(msgs []message.Message) []message.Message {
	cutoff := assistantTurnCutoff(msgs, builtinAutoCompactRecentWindow)
	if cutoff == 0 {
		return msgs
	}
	changed := false
	result := make([]message.Message, len(msgs))
	copy(result, msgs)
	for i := 0; i < cutoff; i++ {
		if msgs[i].Role != message.Tool {
			continue
		}
		cloned := msgs[i].Clone()
		modified := false
		for j, part := range cloned.Parts {
			tr, ok := part.(message.ToolResult)
			if !ok || tr.IsError || tr.Data != "" || tr.MIMEType != "" {
				continue
			}
			runes := []rune(tr.Content)
			if len(runes) <= builtinAutoCompactToolResultMaxChars {
				continue
			}
			omitted := len(runes) - builtinAutoCompactToolResultMaxChars
			tr.Content = string(runes[:builtinAutoCompactToolResultMaxChars]) +
				fmt.Sprintf("\n\n[%d characters omitted during context compaction]", omitted)
			cloned.Parts[j] = tr
			modified = true
			changed = true
		}
		if modified {
			result[i] = cloned
		}
	}
	if !changed {
		return msgs
	}
	return result
}

// assistantTurnCutoff returns the message index at which the n-th most recent
// assistant turn begins. All messages before this index are candidates for
// compaction. Returns 0 when there are fewer than n assistant turns, meaning
// no compaction should occur.
func assistantTurnCutoff(msgs []message.Message, n int) int {
	count := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == message.Assistant {
			count++
			if count >= n {
				return i
			}
		}
	}
	return 0
}

// builtinPruneToolResults clears oversized tool result content from older
// messages to reduce the payload sent to plugins and the LLM. It works
// similarly to OpenCode's prune mechanism:
//
//  1. Walk messages backwards, skip the most recent builtinPruneRecentTurns
//     assistant turns unconditionally.
//  2. Accumulate tool result character counts. While the running total is
//     within builtinPruneProtectChars, keep the results intact.
//  3. Once the protect budget is exhausted, replace remaining (older) tool
//     result content with a short placeholder.
//  4. Only apply the prune if the total prunable content exceeds
//     builtinPruneMinChars.
func builtinPruneToolResults(msgs []message.Message) []message.Message {
	if len(msgs) == 0 {
		return msgs
	}

	// Phase 1: Identify the protect boundary — skip recent turns entirely.
	protectCutoff := assistantTurnCutoff(msgs, builtinPruneRecentTurns)

	// Phase 2: Walk backwards from the protect boundary, accumulate tool
	// result chars. Once the protect budget is exhausted, mark older
	// tool results for pruning.
	type pruneTarget struct {
		msgIdx  int
		partIdx int
		chars   int
	}

	var (
		protectedChars int64
		targets        []pruneTarget
		totalPrunable  int64
	)

	for i := protectCutoff - 1; i >= 0; i-- {
		if msgs[i].Role != message.Tool {
			continue
		}
		for j, part := range msgs[i].Parts {
			tr, ok := part.(message.ToolResult)
			if !ok || tr.IsError || tr.Content == "" {
				continue
			}
			charCount := len([]rune(tr.Content))
			if protectedChars < int64(builtinPruneProtectChars) {
				protectedChars += int64(charCount)
				continue
			}
			// Beyond protect budget — candidate for pruning.
			targets = append(targets, pruneTarget{msgIdx: i, partIdx: j, chars: charCount})
			totalPrunable += int64(charCount)
		}
	}

	if totalPrunable < builtinPruneMinChars || len(targets) == 0 {
		return msgs
	}

	// Phase 3: Apply pruning.
	result := make([]message.Message, len(msgs))
	copy(result, msgs)
	modifiedMsgs := make(map[int]bool)

	for _, t := range targets {
		if !modifiedMsgs[t.msgIdx] {
			result[t.msgIdx] = msgs[t.msgIdx].Clone()
			modifiedMsgs[t.msgIdx] = true
		}
		tr := result[t.msgIdx].Parts[t.partIdx].(message.ToolResult)
		tr.Content = fmt.Sprintf("[Old tool result content cleared to reduce context size. %d characters omitted.]", t.chars)
		result[t.msgIdx].Parts[t.partIdx] = tr
	}

	slog.Info("Pruned old tool results before plugin transform",
		"pruned_results", len(targets),
		"pruned_chars", totalPrunable,
		"protected_chars", protectedChars)
	return result
}
