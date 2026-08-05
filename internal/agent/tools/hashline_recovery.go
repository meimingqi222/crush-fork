package tools

import (
	"fmt"
)

// maxLCSMatrixSize limits the product of Base and Ours lengths to prevent memory explosion.
// 2,500,000 corresponds to e.g. 1500 lines * 1500 lines or 5000 lines * 500 lines.
const maxLCSMatrixSize = 2500000

// alignLinesLCS finds the longest common subsequence alignment between base and ours lines.
// Returns a 1-indexed slice baseToOurs where baseToOurs[i] = corresponding index in ours (1-indexed),
// or 0 if the line was modified, deleted, or skipped in the alignment.
func alignLinesLCS(base, ours []string) []int {
	m := len(base)
	n := len(ours)

	baseToOurs := make([]int, m+1)
	if m == 0 || n == 0 {
		return baseToOurs
	}

	if m*n > maxLCSMatrixSize {
		return alignLinesGreedy(base, ours)
	}

	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}

	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if base[i-1] == ours[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else {
				dp[i][j] = max(dp[i-1][j], dp[i][j-1])
			}
		}
	}

	i, j := m, n
	for i > 0 && j > 0 {
		if base[i-1] == ours[j-1] {
			baseToOurs[i] = j
			i--
			j--
		} else if dp[i-1][j] >= dp[i][j-1] {
			i--
		} else {
			j--
		}
	}
	return baseToOurs
}

func alignLinesGreedy(base, ours []string) []int {
	mapping := make([]int, len(base)+1)
	positions := make(map[string][]int)
	for index, line := range ours {
		positions[line] = append(positions[line], index+1)
	}
	searchStart := 1
	for baseIndex, baseLine := range base {
		candidates := positions[baseLine]
		for _, candidate := range candidates {
			if candidate < searchStart {
				continue
			}
			mapping[baseIndex+1] = candidate
			searchStart = candidate + 1
			break
		}
	}
	return mapping
}

// tryRecoverHashline attempts to apply the hashline operations to current disk lines (ours)
// using an LCS-aligned mapping against the memory read cache (base) of the current session.
// Fuzz factor is strictly 0: any mismatch on modified/deleted target lines will fail and throw a conflict.
func tryRecoverHashline(sessionID, filePath string, oursLines []string, operations []HashlineEditOperation) ([]string, error) {
	snapshots, ok := GlobalFileCache.GetHistory(sessionID, filePath)
	if !ok {
		return nil, fmt.Errorf("read snapshot not found in memory cache")
	}

	var lastErr error
	for index := len(snapshots) - 1; index >= 0; index-- {
		newLines, err := recoverHashlineFromSnapshot(snapshots[index], oursLines, operations)
		if err == nil {
			return newLines, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func recoverHashlineFromSnapshot(baseLines, oursLines []string, operations []HashlineEditOperation) ([]string, error) {
	parsedOps, err := parseHashlineOperations(operations, baseLines)
	if err != nil {
		return nil, fmt.Errorf("failed to parse operations against base snapshot: %w", err)
	}

	baseToOurs := alignLinesLCS(baseLines, oursLines)
	mappedOps := make([]parsedHashlineOperation, len(parsedOps))
	for idx, op := range parsedOps {
		mappedOp := parsedHashlineOperation{Operation: op.Operation, ContentLines: op.ContentLines}
		switch op.Operation {
		case hashlineEditOpReplaceLine, hashlineEditOpPrepend, hashlineEditOpAppend:
			if op.Line.Line < 1 || op.Line.Line >= len(baseToOurs) {
				return nil, fmt.Errorf("target line %d is out of bounds (hash mismatch)", op.Line.Line)
			}
			mappedLine := baseToOurs[op.Line.Line]
			if mappedLine == 0 {
				return nil, fmt.Errorf("target line %d has been modified or deleted by another change (hash mismatch)", op.Line.Line)
			}
			mappedOp.Line = hashlineRef{Line: mappedLine, Hash: op.Line.Hash}
		case hashlineEditOpReplaceRange:
			if op.Start.Line < 1 || op.Start.Line >= len(baseToOurs) || op.End.Line < 1 || op.End.Line >= len(baseToOurs) {
				return nil, fmt.Errorf("range boundary lines %d-%d are out of bounds (hash mismatch)", op.Start.Line, op.End.Line)
			}
			startMapped, endMapped := baseToOurs[op.Start.Line], baseToOurs[op.End.Line]
			if startMapped == 0 || endMapped == 0 || endMapped-startMapped != op.End.Line-op.Start.Line {
				return nil, fmt.Errorf("range %d-%d has been modified or split (hash mismatch)", op.Start.Line, op.End.Line)
			}
			for line := op.Start.Line; line <= op.End.Line; line++ {
				if baseToOurs[line] != startMapped+line-op.Start.Line {
					return nil, fmt.Errorf("range %d-%d contains modified lines (hash mismatch)", op.Start.Line, op.End.Line)
				}
			}
			mappedOp.Start = hashlineRef{Line: startMapped, Hash: op.Start.Hash}
			mappedOp.End = hashlineRef{Line: endMapped, Hash: op.End.Hash}
		case hashlineEditOpCut, hashlineEditOpPaste:
			// CUT/PASTE are not safe to 3-way-merge here: this function has no
			// session/clipboard access (recovery re-parses operations against a
			// past snapshot, independent of the caller's clipboard pre-pass), so
			// a mapped CUT would silently fail to populate any register and a
			// mapped PASTE would silently insert nothing -- both would report
			// "recovered" while actually doing nothing. Fail loudly instead so
			// the caller re-reads the file and retries with fresh line refs.
			return nil, fmt.Errorf("%s operation cannot be safely recovered via 3-way merge after a concurrent change; re-read the file with a line selector and retry", op.Operation)
		}
		mappedOps[idx] = mappedOp
	}

	newLines, err := applyHashlineOperations(oursLines, mappedOps)
	if err != nil {
		return nil, fmt.Errorf("conflict applying recovered operations: %w", err)
	}
	return newLines, nil
}
