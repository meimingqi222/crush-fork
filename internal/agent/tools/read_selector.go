package tools

import (
	"os"
	"regexp"
	"strconv"
	"strings"
)

// pathSelector holds the result of parsing a path string for embedded
// selectors (line ranges, raw mode).
type pathSelector struct {
	filePath    string // Clean file path with selector stripped.
	offset      int    // 0-based line offset for internal use.
	limit       int    // Number of lines to read; 0 means use default.
	raw         bool   // Verbatim output mode (no line numbers or anchors).
	hasSelector bool   // Whether any selector was present.
	hasLineSel  bool   // Whether a line-range selector was present (drives auto-hashline).
}

// lineRangePattern matches a single line-range token such as "50", "50-100",
// "50+10", "50-", "L50", "L50-L100".
var lineRangePattern = regexp.MustCompile(`^(?i)L?(\d+)(?:([-+])L?(\d+)?|-)?$`)

// parsePathSelector extracts an optional selector suffix from a path string.
//
// Selector syntax (appended after the last colon(s) in the path):
//
//	:50          — read from line 50 onward
//	:50-100      — lines 50–100 inclusive
//	:50+10       — 10 lines starting at line 50
//	:50-         — from line 50 to end of file
//	:raw         — verbatim output, no line numbers or anchors
//	:50-100:raw  — combined (order-independent)
//
// URLs (http:// or https://) are never parsed for selectors.
//
// When a token after a colon does not match any known selector pattern, the
// entire input is returned as a plain path (no split).  If the split produces
// a path that does not exist on disk but the full input does, the full input
// is used instead (handles filenames containing colons).
func parsePathSelector(input string) pathSelector {
	// URL guard: never parse selectors out of URLs.
	if strings.HasPrefix(input, "http://") || strings.HasPrefix(input, "https://") {
		return pathSelector{filePath: input}
	}

	// No colon at all — plain path.
	lastColon := strings.LastIndex(input, ":")
	if lastColon < 0 {
		return pathSelector{filePath: input}
	}

	// Greedily collect valid selector tokens from right to left.
	// For "file.ts:50-100:raw" this collects ["50-100", "raw"] and leaves
	// "file.ts" as the base path.
	var selTokens []string
	searchEnd := len(input)

	for {
		colonIdx := strings.LastIndex(input[:searchEnd], ":")
		if colonIdx < 0 {
			break
		}

		candidate := input[colonIdx+1 : searchEnd]
		if candidate == "" || !isValidSelectorToken(candidate) {
			break
		}

		selTokens = append([]string{candidate}, selTokens...)
		searchEnd = colonIdx
		if colonIdx == 0 {
			break
		}
	}

	// No valid selector tokens found.
	if len(selTokens) == 0 {
		return pathSelector{filePath: input}
	}

	basePath := input[:searchEnd]
	if basePath == "" {
		// Edge case: input starts with ":" — treat as plain path.
		return pathSelector{filePath: input}
	}

	sel := buildSelectorFromTokens(basePath, selTokens)

	// Existence fallback: if the split path doesn't exist but the full input
	// does, treat the whole string as a file path (handles filenames that
	// contain colons followed by selector-like text).
	if _, err := os.Stat(sel.filePath); err != nil {
		if _, fullErr := os.Stat(input); fullErr == nil {
			return pathSelector{filePath: input}
		}
	}

	return sel
}

// isValidSelectorToken returns true when a single token is a recognised
// selector (line range or "raw").
func isValidSelectorToken(t string) bool {
	if t == "" {
		return false
	}
	if strings.EqualFold(t, "raw") {
		return true
	}
	if !lineRangePattern.MatchString(t) {
		return false
	}
	// Reject line 0 — line numbers are 1-indexed.
	m := lineRangePattern.FindStringSubmatch(t)
	if m != nil {
		line, _ := strconv.Atoi(m[1])
		if line == 0 {
			return false
		}
	}
	return true
}

// buildSelectorFromTokens converts validated tokens into a pathSelector.
func buildSelectorFromTokens(filePath string, tokens []string) pathSelector {
	sel := pathSelector{
		filePath:    filePath,
		hasSelector: true,
	}

	for _, t := range tokens {
		if strings.EqualFold(t, "raw") {
			sel.raw = true
			continue
		}

		m := lineRangePattern.FindStringSubmatch(t)
		if m == nil {
			continue
		}

		sel.hasLineSel = true
		startLine, _ := strconv.Atoi(m[1])
		op := m[2]
		endOrCount := m[3]

		switch op {
		case "-":
			if endOrCount == "" {
				// :50- (open-ended)
				sel.offset = startLine - 1
			} else {
				// :50-100 (inclusive range)
				endLine, _ := strconv.Atoi(endOrCount)
				sel.offset = startLine - 1
				sel.limit = endLine - startLine + 1
			}
		case "+":
			// :50+10 (count-based)
			count, _ := strconv.Atoi(endOrCount)
			sel.offset = startLine - 1
			sel.limit = count
		default:
			// :50 (single line number → from that line onward)
			sel.offset = startLine - 1
		}
	}

	return sel
}
