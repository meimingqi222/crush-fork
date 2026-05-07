package agent

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/charmbracelet/crush/internal/message"
)

const (
	// builtinPruneProtectTokens mirrors opencode's PRUNE_PROTECT. When walking
	// backwards, this many estimated tool-output tokens are protected before
	// older tool results become pruning candidates.
	builtinPruneProtectTokens int64 = 40_000

	// builtinPruneMinTokens mirrors opencode's PRUNE_MINIMUM. The prune pass
	// is only applied when candidate tool output exceeds this estimate.
	builtinPruneMinTokens int64 = 20_000

	// builtinPruneRecentUserTurns is the number of recent user turns whose
	// tool results are unconditionally protected from pruning.
	builtinPruneRecentUserTurns = 2

	builtinPruneCompactedNoticePrefix = "[Old tool result content cleared"
	builtinPruneProtectedToolName     = "skill"
)

// builtinPruneToolResults clears oversized tool result content from older
// messages to reduce the payload sent to plugins and the LLM. It works
// similarly to OpenCode's prune mechanism:
//
//  1. Walk messages backwards, skip the most recent builtinPruneRecentUserTurns
//     user turns unconditionally.
//  2. Stop at the previous summary boundary.
//  3. Accumulate estimated tool result tokens. While the running total is
//     within builtinPruneProtectTokens, keep the results intact.
//  4. Once the protect budget is exhausted, replace remaining (older) tool
//     result content with a short placeholder.
//  5. Only apply the prune if the total prunable content exceeds
//     builtinPruneMinTokens.
func builtinPruneToolResults(msgs []message.Message) []message.Message {
	if len(msgs) == 0 {
		return msgs
	}

	type pruneTarget struct {
		msgIdx  int
		partIdx int
		tokens  int64
		chars   int
	}

	var (
		protectedTokens int64
		targets         []pruneTarget
		totalPrunable   int64
		userTurns       int
	)

loop:
	for i := len(msgs) - 1; i >= 0; i-- {
		switch msgs[i].Role {
		case message.User:
			userTurns++
			if userTurns < builtinPruneRecentUserTurns {
				continue
			}
		case message.Assistant:
			if msgs[i].IsSummaryMessage {
				break loop
			}
		}
		if userTurns < builtinPruneRecentUserTurns {
			continue
		}
		if msgs[i].Role != message.Tool {
			continue
		}
		for j, part := range msgs[i].Parts {
			tr, ok := part.(message.ToolResult)
			if !ok || tr.IsError || tr.Content == "" {
				continue
			}
			if tr.Name == builtinPruneProtectedToolName {
				continue
			}
			if strings.HasPrefix(tr.Content, builtinPruneCompactedNoticePrefix) {
				break loop
			}
			tokenEstimate := estimateStringTokens(tr.Content)
			if protectedTokens < builtinPruneProtectTokens {
				protectedTokens += tokenEstimate
				continue
			}
			targets = append(targets, pruneTarget{
				msgIdx:  i,
				partIdx: j,
				tokens:  tokenEstimate,
				chars:   len([]rune(tr.Content)),
			})
			totalPrunable += tokenEstimate
		}
	}

	if totalPrunable <= builtinPruneMinTokens || len(targets) == 0 {
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
		tr.Content = fmt.Sprintf("[Old tool result content cleared to reduce context size. %d estimated tokens omitted, %d characters omitted.]", t.tokens, t.chars)
		result[t.msgIdx].Parts[t.partIdx] = tr
	}

	slog.Info("Pruned old tool results before plugin transform",
		"pruned_results", len(targets),
		"pruned_tokens", totalPrunable,
		"protected_tokens", protectedTokens)
	return result
}
