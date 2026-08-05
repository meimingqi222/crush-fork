package tools

import (
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/fsext"
)

type parsedHashlineOperation struct {
	Operation    string
	Line         hashlineRef
	Start        hashlineRef
	End          hashlineRef
	ContentLines []string
	Register     string
	PasteBefore  bool
}

func splitHashlineFileLines(content string) ([]string, bool) {
	if content == "" {
		return []string{}, false
	}

	hasTrailingNewline := strings.HasSuffix(content, "\n")
	lines := strings.Split(content, "\n")
	if hasTrailingNewline {
		lines = lines[:len(lines)-1]
	}
	return lines, hasTrailingNewline
}

func joinHashlineFileLines(lines []string, trailingNewline bool) string {
	if len(lines) == 0 {
		return ""
	}

	content := strings.Join(lines, "\n")
	if trailingNewline {
		content += "\n"
	}
	return content
}

func splitHashlineContent(content string) []string {
	normalized, _ := fsext.ToUnixLineEndings(content)
	if normalized == "" {
		return nil
	}
	lines := strings.Split(normalized, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func parseAndValidateHashlineReference(reference string, lines []string) (hashlineRef, error) {
	parsedRef, err := parseHashlineReference(reference)
	if err != nil {
		return hashlineRef{}, err
	}

	currentHash, err := validateHashlineReference(parsedRef, lines)
	if err != nil {
		if currentHash != "" {
			return hashlineRef{}, fmt.Errorf("%w (current hash is %s). Re-read the file with a line selector and retry", err, currentHash)
		}
		return hashlineRef{}, err
	}

	return parsedRef, nil
}

func parseHashlineOperations(operations []HashlineEditOperation, originalLines []string) ([]parsedHashlineOperation, error) {
	parsed := make([]parsedHashlineOperation, 0, len(operations))
	for i, operation := range operations {
		opIndex := i + 1
		contentLines := splitHashlineContent(operation.Content)

		switch operation.Operation {
		case hashlineEditOpReplaceLine:
			lineRef, err := parseAndValidateHashlineReference(operation.Line, originalLines)
			if err != nil {
				return nil, fmt.Errorf("operation %d (%s): %w", opIndex, operation.Operation, err)
			}
			parsed = append(parsed, parsedHashlineOperation{
				Operation:    operation.Operation,
				Line:         lineRef,
				ContentLines: contentLines,
			})
		case hashlineEditOpReplaceRange:
			startRef, err := parseAndValidateHashlineReference(operation.Start, originalLines)
			if err != nil {
				return nil, fmt.Errorf("operation %d (%s): %w", opIndex, operation.Operation, err)
			}
			endRef, err := parseAndValidateHashlineReference(operation.End, originalLines)
			if err != nil {
				return nil, fmt.Errorf("operation %d (%s): %w", opIndex, operation.Operation, err)
			}
			if startRef.Line > endRef.Line {
				return nil, fmt.Errorf("operation %d (%s): start line must be less than or equal to end line", opIndex, operation.Operation)
			}
			parsed = append(parsed, parsedHashlineOperation{
				Operation:    operation.Operation,
				Start:        startRef,
				End:          endRef,
				ContentLines: contentLines,
			})
		case hashlineEditOpPrepend, hashlineEditOpAppend:
			if len(contentLines) == 0 {
				return nil, fmt.Errorf("operation %d (%s): content cannot be empty", opIndex, operation.Operation)
			}
			lineRef, err := parseAndValidateHashlineReference(operation.Line, originalLines)
			if err != nil {
				return nil, fmt.Errorf("operation %d (%s): %w", opIndex, operation.Operation, err)
			}
			parsed = append(parsed, parsedHashlineOperation{
				Operation:    operation.Operation,
				Line:         lineRef,
				ContentLines: contentLines,
			})
		case hashlineEditOpCut:
			startRef, err := parseAndValidateHashlineReference(operation.Start, originalLines)
			if err != nil {
				return nil, fmt.Errorf("operation %d (%s): %w", opIndex, operation.Operation, err)
			}
			endRef, err := parseAndValidateHashlineReference(operation.End, originalLines)
			if err != nil {
				return nil, fmt.Errorf("operation %d (%s): %w", opIndex, operation.Operation, err)
			}
			if startRef.Line > endRef.Line {
				return nil, fmt.Errorf("operation %d (%s): start line must be less than or equal to end line", opIndex, operation.Operation)
			}
			parsed = append(parsed, parsedHashlineOperation{
				Operation: operation.Operation,
				Start:     startRef,
				End:       endRef,
				Register:  operation.Register,
			})
		case hashlineEditOpPaste:
			lineRef, err := parseAndValidateHashlineReference(operation.Line, originalLines)
			if err != nil {
				return nil, fmt.Errorf("operation %d (%s): %w", opIndex, operation.Operation, err)
			}
			parsed = append(parsed, parsedHashlineOperation{
				Operation:    operation.Operation,
				Line:         lineRef,
				ContentLines: contentLines,
				Register:     operation.Register,
				PasteBefore:  operation.PasteBefore,
			})
		default:
			return nil, fmt.Errorf("operation %d: unsupported operation %q. Use replace_line, replace_range, prepend, append, cut, or paste", opIndex, operation.Operation)
		}
	}

	return parsed, nil
}

func applyHashlineOperations(originalLines []string, operations []parsedHashlineOperation) ([]string, error) {
	currentLines := append([]string(nil), originalLines...)
	mapping := make([]int, len(originalLines)+1)
	for line := 1; line <= len(originalLines); line++ {
		mapping[line] = line
	}

	// Track cumulative insert offsets for each original line.
	// prependOffsets[line] = total lines prepended before line
	// appendOffsets[line] = total lines appended after line
	prependOffsets := make([]int, len(originalLines)+1)
	appendOffsets := make([]int, len(originalLines)+1)

	for i, operation := range operations {
		var err error
		switch operation.Operation {
		case hashlineEditOpReplaceLine:
			currentLines, mapping, err = replaceOriginalRange(currentLines, mapping, operation.Line.Line, operation.Line.Line, operation.ContentLines)
		case hashlineEditOpReplaceRange:
			currentLines, mapping, err = replaceOriginalRange(currentLines, mapping, operation.Start.Line, operation.End.Line, operation.ContentLines)
		case hashlineEditOpPrepend:
			currentLines, mapping, prependOffsets, err = insertRelativeToOriginalLine(currentLines, mapping, prependOffsets, appendOffsets, operation.Line.Line, true, operation.ContentLines)
		case hashlineEditOpAppend:
			currentLines, mapping, appendOffsets, err = insertRelativeToOriginalLine(currentLines, mapping, prependOffsets, appendOffsets, operation.Line.Line, false, operation.ContentLines)
		case hashlineEditOpCut:
			// Cut is handled in a pre-pass; here it's a no-op since lines are already deleted
		case hashlineEditOpPaste:
			var pasteLines []string
			if len(operation.ContentLines) > 0 {
				pasteLines = operation.ContentLines
			} else {
				// Retrieve from clipboard register - handled by caller
				pasteLines = nil
			}
			if len(pasteLines) > 0 {
				if operation.PasteBefore {
					currentLines, mapping, prependOffsets, err = insertRelativeToOriginalLine(currentLines, mapping, prependOffsets, appendOffsets, operation.Line.Line, true, pasteLines)
				} else {
					currentLines, mapping, appendOffsets, err = insertRelativeToOriginalLine(currentLines, mapping, prependOffsets, appendOffsets, operation.Line.Line, false, pasteLines)
				}
			}
		default:
			err = fmt.Errorf("unsupported operation %q", operation.Operation)
		}
		if err != nil {
			return nil, fmt.Errorf("operation %d (%s): %w", i+1, operation.Operation, err)
		}
	}

	return currentLines, nil
}

func replaceOriginalRange(lines []string, mapping []int, startOriginal, endOriginal int, contentLines []string) ([]string, []int, error) {
	startCurrent, err := resolveCurrentLine(mapping, startOriginal)
	if err != nil {
		return nil, nil, fmt.Errorf("start line %d: %w", startOriginal, err)
	}
	endCurrent, err := resolveCurrentLine(mapping, endOriginal)
	if err != nil {
		return nil, nil, fmt.Errorf("end line %d: %w", endOriginal, err)
	}
	if startCurrent > endCurrent {
		return nil, nil, fmt.Errorf("resolved range is invalid (%d > %d)", startCurrent, endCurrent)
	}

	replacedLength := endCurrent - startCurrent + 1
	updatedLines := make([]string, 0, len(lines)-replacedLength+len(contentLines))
	updatedLines = append(updatedLines, lines[:startCurrent-1]...)
	updatedLines = append(updatedLines, contentLines...)
	updatedLines = append(updatedLines, lines[endCurrent:]...)

	delta := len(contentLines) - replacedLength
	updatedMapping := append([]int(nil), mapping...)
	for originalLine := 1; originalLine < len(updatedMapping); originalLine++ {
		position := mapping[originalLine]
		if position == 0 {
			continue
		}

		switch {
		case position < startCurrent:
			continue
		case position > endCurrent:
			updatedMapping[originalLine] = position + delta
		default:
			relative := position - startCurrent
			if relative < len(contentLines) {
				updatedMapping[originalLine] = startCurrent + relative
			} else {
				updatedMapping[originalLine] = 0
			}
		}
	}

	return updatedLines, updatedMapping, nil
}

func insertRelativeToOriginalLine(lines []string, mapping []int, prependOffsets []int, appendOffsets []int, originalLine int, before bool, contentLines []string) ([]string, []int, []int, error) {
	if len(contentLines) == 0 {
		if before {
			return lines, mapping, prependOffsets, nil
		}
		return lines, mapping, appendOffsets, nil
	}

	// Check if the anchor line was deleted.
	if originalLine >= len(mapping) || mapping[originalLine] == 0 {
		return nil, nil, nil, fmt.Errorf("line %d no longer exists after previous operations", originalLine)
	}

	// Use mapping[originalLine] as the authoritative current position of the original line.
	// Recalculating from prependOffsets/appendOffsets alone is wrong when prior
	// replace_line/replace_range operations have already shifted positions — those ops
	// update mapping but not the offset arrays, so the offset-based formula diverges and
	// can produce an insertAt that exceeds len(lines), causing a slice-bounds panic.
	lineCurrent := mapping[originalLine]

	// For prepend: insert at the line's current position (after existing prepends, before the line).
	// For append: insert after the line and after any already-appended lines.
	insertAt := lineCurrent
	if !before {
		insertAt = lineCurrent + 1 + appendOffsets[originalLine]
	}

	if insertAt-1 > len(lines) {
		return nil, nil, nil, fmt.Errorf("computed insert position %d exceeds file length %d (line %d is at position %d)", insertAt, len(lines), originalLine, lineCurrent)
	}

	updatedLines := make([]string, 0, len(lines)+len(contentLines))
	updatedLines = append(updatedLines, lines[:insertAt-1]...)
	updatedLines = append(updatedLines, contentLines...)
	updatedLines = append(updatedLines, lines[insertAt-1:]...)

	// Update the appropriate offset counter.
	updatedPrepend := append([]int(nil), prependOffsets...)
	updatedAppend := append([]int(nil), appendOffsets...)
	if before {
		updatedPrepend[originalLine] += len(contentLines)
	} else {
		updatedAppend[originalLine] += len(contentLines)
	}

	// Update mapping with a delta shift: every original line currently at or after
	// insertAt moves down by len(contentLines). This correctly preserves any prior
	// shifts introduced by replace operations instead of recalculating from scratch.
	updatedMapping := append([]int(nil), mapping...)
	for orig := 1; orig < len(mapping); orig++ {
		if mapping[orig] >= insertAt {
			updatedMapping[orig] = mapping[orig] + len(contentLines)
		}
	}

	if before {
		return updatedLines, updatedMapping, updatedPrepend, nil
	}
	return updatedLines, updatedMapping, updatedAppend, nil
}

func resolveCurrentLine(mapping []int, originalLine int) (int, error) {
	if originalLine < 1 || originalLine >= len(mapping) {
		return 0, fmt.Errorf("line %d is outside original file range", originalLine)
	}
	if mapping[originalLine] == 0 {
		return 0, fmt.Errorf("line %d no longer exists after previous operations", originalLine)
	}
	return mapping[originalLine], nil
}

// pendingHashlineCut holds a CUT operation's captured lines before they are
// committed to the global clipboard. Keeping them uncommitted until the edit
// that produced them actually succeeds prevents a denied or failed write from
// leaving a register that claims lines were cut when the source file was
// never modified (a later PASTE from that register would otherwise duplicate
// content that is still present, unchanged, in the original file).
type pendingHashlineCut struct {
	Register string // "" for the anonymous register
	Lines    []string
}

// collectPendingHashlineCuts captures the line ranges targeted by CUT
// operations in opLines without touching the global clipboard. Invalid
// ranges are left for the caller to validate and report.
func collectPendingHashlineCuts(ops []parsedHashlineOperation, opLines []string) []pendingHashlineCut {
	var pending []pendingHashlineCut
	for _, op := range ops {
		if op.Operation != hashlineEditOpCut {
			continue
		}
		startLine, endLine := op.Start.Line, op.End.Line
		if startLine < 1 || endLine > len(opLines) || startLine > endLine {
			continue
		}
		captured := make([]string, endLine-startLine+1)
		copy(captured, opLines[startLine-1:endLine])
		pending = append(pending, pendingHashlineCut{Register: op.Register, Lines: captured})
	}
	return pending
}

// resolvePendingHashlinePastes fills in ContentLines for PASTE operations in
// ops, preferring lines cut earlier in the same batch (pending, not yet
// committed to the clipboard) and falling back to the persisted clipboard for
// cross-call transfers. Returns a non-nil error response if a PASTE's
// register cannot be resolved.
func resolvePendingHashlinePastes(ops []parsedHashlineOperation, pending []pendingHashlineCut, sessionID string) *fantasy.ToolResponse {
	for i, op := range ops {
		if op.Operation != hashlineEditOpPaste || len(op.ContentLines) > 0 {
			continue
		}
		var capturedLines []string
		var found bool
		for _, pc := range pending {
			if pc.Register == op.Register {
				capturedLines, found = pc.Lines, true
			}
		}
		if !found {
			if op.Register != "" {
				capturedLines, found = GlobalClipboard.GetNamed(sessionID, op.Register)
			} else {
				capturedLines, found = GlobalClipboard.GetAnonymous(sessionID)
			}
		}
		if !found || len(capturedLines) == 0 {
			resp := fantasy.NewTextErrorResponse(fmt.Sprintf("operation %d (paste): clipboard register %q is empty or not found. Use a cut operation first.", i+1, op.Register))
			return &resp
		}
		ops[i].ContentLines = capturedLines
	}
	return nil
}

// commitPendingHashlineCuts writes captured CUT lines to the persisted,
// named clipboard registers. Call only after the edit that produced them has
// actually been written -- see pendingHashlineCut.
func commitPendingHashlineCuts(pending []pendingHashlineCut, sessionID string) {
	for _, pc := range pending {
		if pc.Register != "" {
			GlobalClipboard.PutNamed(sessionID, pc.Register, pc.Lines)
		}
	}
}
