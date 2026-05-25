package tools

import (
	"bufio"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// FilePatch represents the set of changes applied to a single file.
type FilePatch struct {
	OldPath string
	NewPath string
	Hunks   []*PatchHunk
}

// PatchHunk represents a single contiguous block of difference lines.
type PatchHunk struct {
	OldStart int
	OldLen   int
	NewStart int
	NewLen   int
	Lines    []string // Each line prefixed with '+', '-', or ' '
}

var hunkHeaderRegexp = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// ParseUnifiedPatch parses standard unified diff patch strings into structured FilePatches.
func ParseUnifiedPatch(patchText string) ([]*FilePatch, error) {
	scanner := bufio.NewScanner(strings.NewReader(patchText))
	var patches []*FilePatch
	var currentPatch *FilePatch
	var currentHunk *PatchHunk

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "--- ") {
			path := parseDiffPath(line[4:])
			currentPatch = &FilePatch{OldPath: path}
			patches = append(patches, currentPatch)
			currentHunk = nil
			continue
		}

		if strings.HasPrefix(line, "+++ ") && currentPatch != nil {
			path := parseDiffPath(line[4:])
			currentPatch.NewPath = path
			continue
		}

		if strings.HasPrefix(line, "@@ ") {
			if currentPatch == nil {
				return nil, fmt.Errorf("hunk header found before file header")
			}
			matches := hunkHeaderRegexp.FindStringSubmatch(line)
			if len(matches) < 4 {
				return nil, fmt.Errorf("invalid hunk header: %s", line)
			}

			oldStart, _ := strconv.Atoi(matches[1])
			oldLen := 1
			if matches[2] != "" {
				oldLen, _ = strconv.Atoi(matches[2])
			}

			newStart, _ := strconv.Atoi(matches[3])
			newLen := 1
			if matches[4] != "" {
				newLen, _ = strconv.Atoi(matches[4])
			}

			currentHunk = &PatchHunk{
				OldStart: oldStart,
				OldLen:   oldLen,
				NewStart: newStart,
				NewLen:   newLen,
			}
			currentPatch.Hunks = append(currentPatch.Hunks, currentHunk)
			continue
		}

		if currentHunk != nil {
			if strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") || strings.HasPrefix(line, " ") {
				currentHunk.Lines = append(currentHunk.Lines, line)
			} else if line == "" {
				currentHunk.Lines = append(currentHunk.Lines, "")
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error scanning patch: %w", err)
	}

	return patches, nil
}

func parseDiffPath(line string) string {
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return ""
	}
	path := parts[0]
	if strings.HasPrefix(path, "a/") || strings.HasPrefix(path, "b/") {
		return path[2:]
	}
	return path
}

// ApplyPatchToLines applies unified diff hunks sequentially to original file lines.
// Supports fuzz=0 exact line context match with offset alignment: if target line shifted
// due to other changes, searches outward up to 100 lines for the exact matching block.
func ApplyPatchToLines(lines []string, hunks []*PatchHunk) ([]string, error) {
	currentLines := append([]string(nil), lines...)
	offsetShift := 0

	for hunkIdx, hunk := range hunks {
		expectedStart := hunk.OldStart + offsetShift
		trueStart := -1

		// Search outward: expectedStart, expectedStart+1, expectedStart-1 ...
		maxSearchRadius := 100
		for r := 0; r <= maxSearchRadius; r++ {
			candidates := []int{expectedStart + r}
			if r > 0 {
				candidates = append(candidates, expectedStart-r)
			}

			for _, startLine := range candidates {
				if startLine < 1 || startLine > len(currentLines)+1 {
					continue
				}
				if verifyHunkContext(currentLines, startLine, hunk) {
					trueStart = startLine
					break
				}
			}
			if trueStart != -1 {
				break
			}
		}

		if trueStart == -1 {
			return nil, fmt.Errorf("hunk %d context mismatch: could not find matching lines for patch context around expected line %d", hunkIdx+1, expectedStart)
		}

		var updatedLines []string
		updatedLines = append(updatedLines, currentLines[:trueStart-1]...)

		origPtr := trueStart
		for _, hLine := range hunk.Lines {
			if strings.HasPrefix(hLine, "+") {
				updatedLines = append(updatedLines, hLine[1:])
			} else if strings.HasPrefix(hLine, "-") {
				if origPtr > len(currentLines) {
					return nil, fmt.Errorf("hunk %d mismatch: expected deleted line %q, got EOF", hunkIdx+1, hLine[1:])
				}
				if currentLines[origPtr-1] != hLine[1:] {
					return nil, fmt.Errorf("hunk %d mismatch: expected deleted line %q, got %q", hunkIdx+1, hLine[1:], currentLines[origPtr-1])
				}
				origPtr++
			} else {
				content := hLine
				if len(hLine) > 0 {
					content = hLine[1:]
				}
				if origPtr > len(currentLines) {
					return nil, fmt.Errorf("hunk %d mismatch: expected context line %q, got EOF", hunkIdx+1, content)
				}
				if currentLines[origPtr-1] != content {
					return nil, fmt.Errorf("hunk %d mismatch: expected context line %q, got %q", hunkIdx+1, content, currentLines[origPtr-1])
				}
				updatedLines = append(updatedLines, content)
				origPtr++
			}
		}

		if origPtr-1 <= len(currentLines) {
			updatedLines = append(updatedLines, currentLines[origPtr-1:]...)
		}

		linesAdded := 0
		linesRemoved := 0
		for _, hLine := range hunk.Lines {
			if strings.HasPrefix(hLine, "+") {
				linesAdded++
			} else if strings.HasPrefix(hLine, "-") {
				linesRemoved++
			}
		}

		offsetShift = (trueStart - hunk.OldStart) + (linesAdded - linesRemoved)
		currentLines = updatedLines
	}

	return currentLines, nil
}

func verifyHunkContext(lines []string, startLine int, hunk *PatchHunk) bool {
	origPtr := startLine
	for _, hLine := range hunk.Lines {
		if strings.HasPrefix(hLine, "+") {
			continue
		}

		expectedContent := hLine
		if len(hLine) > 0 {
			expectedContent = hLine[1:]
		}

		if origPtr > len(lines) {
			// For creation patch (where lines is empty and we prepend), if it's new file it might match empty
			if len(lines) == 0 && expectedContent == "" {
				continue
			}
			return false
		}

		if lines[origPtr-1] != expectedContent {
			return false
		}
		origPtr++
	}
	return true
}
