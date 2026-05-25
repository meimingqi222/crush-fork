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

	// Guard against large files to avoid memory pressure of O(M*N) matrix.
	if m*n > maxLCSMatrixSize {
		return baseToOurs
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

// tryRecoverHashline attempts to apply the hashline operations to current disk lines (ours)
// using an LCS-aligned mapping against the memory read cache (base) of the current session.
// Fuzz factor is strictly 0: any mismatch on modified/deleted target lines will fail and throw a conflict.
func tryRecoverHashline(sessionID, filePath string, oursLines []string, operations []HashlineEditOperation) ([]string, error) {
	baseLines, ok := GlobalFileCache.Get(sessionID, filePath)
	if !ok {
		return nil, fmt.Errorf("read snapshot not found in memory cache")
	}

	// 1. Dry run parse operations against the original Base content.
	// Since operations are constructed targeting Base, this parse must succeed if the input is valid.
	parsedOps, err := parseHashlineOperations(operations, baseLines)
	if err != nil {
		return nil, fmt.Errorf("failed to parse operations against base snapshot: %w", err)
	}

	// 2. Perform LCS alignment between Base and Ours to find shifts.
	baseToOurs := alignLinesLCS(baseLines, oursLines)

	// 3. Map Base target lines to current Ours target lines.
	mappedOps := make([]parsedHashlineOperation, len(parsedOps))
	for idx, op := range parsedOps {
		mappedOp := parsedHashlineOperation{
			Operation:    op.Operation,
			ContentLines: op.ContentLines,
		}

		switch op.Operation {
		case hashlineEditOpReplaceLine:
			if op.Line.Line < 0 || op.Line.Line >= len(baseToOurs) {
				return nil, fmt.Errorf("target line %d is out of bounds (hash mismatch)", op.Line.Line)
			}
			mappedLine := baseToOurs[op.Line.Line]
			if mappedLine == 0 {
				return nil, fmt.Errorf("target line %d has been modified or deleted by another change (hash mismatch)", op.Line.Line)
			}
			mappedOp.Line = hashlineRef{Line: mappedLine, Hash: op.Line.Hash}

		case hashlineEditOpReplaceRange:
			if op.Start.Line < 0 || op.Start.Line >= len(baseToOurs) || op.End.Line < 0 || op.End.Line >= len(baseToOurs) {
				return nil, fmt.Errorf("range boundary lines %d-%d are out of bounds (hash mismatch)", op.Start.Line, op.End.Line)
			}
			startMapped := baseToOurs[op.Start.Line]
			endMapped := baseToOurs[op.End.Line]
			if startMapped == 0 || endMapped == 0 {
				return nil, fmt.Errorf("range boundary lines %d-%d have been modified or deleted by another change (hash mismatch)", op.Start.Line, op.End.Line)
			}
			// Strict continuity: mapped range must not have changed its size
			if endMapped-startMapped != op.End.Line-op.Start.Line {
				return nil, fmt.Errorf("range %d-%d has split or mutated structurally in current disk content (hash mismatch)", op.Start.Line, op.End.Line)
			}
			// Verify that every single line inside the range remains unchanged and continuous
			for line := op.Start.Line; line <= op.End.Line; line++ {
				if line < 0 || line >= len(baseToOurs) {
					return nil, fmt.Errorf("range %d-%d contains out of bounds line %d (hash mismatch)", op.Start.Line, op.End.Line, line)
				}
				expected := startMapped + (line - op.Start.Line)
				if baseToOurs[line] != expected {
					return nil, fmt.Errorf("range %d-%d contains lines mutated by another change (hash mismatch)", op.Start.Line, op.End.Line)
				}
			}
			mappedOp.Start = hashlineRef{Line: startMapped, Hash: op.Start.Hash}
			mappedOp.End = hashlineRef{Line: endMapped, Hash: op.End.Hash}

		case hashlineEditOpPrepend, hashlineEditOpAppend:
			if op.Line.Line < 0 || op.Line.Line >= len(baseToOurs) {
				return nil, fmt.Errorf("target line %d is out of bounds (hash mismatch)", op.Line.Line)
			}
			mappedLine := baseToOurs[op.Line.Line]
			if mappedLine == 0 {
				return nil, fmt.Errorf("anchor line %d for %s has been modified or deleted by another change (hash mismatch)", op.Line.Line, op.Operation)
			}
			mappedOp.Line = hashlineRef{Line: mappedLine, Hash: op.Line.Hash}
		}
		mappedOps[idx] = mappedOp
	}

	// 4. Apply the mapped operations to Ours.
	newLines, err := applyHashlineOperations(oursLines, mappedOps)
	if err != nil {
		return nil, fmt.Errorf("conflict applying recovered operations: %w", err)
	}

	return newLines, nil
}
