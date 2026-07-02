package planmode

import "strings"

// TOCEntry represents a single entry in a plan's table of contents.
type TOCEntry struct {
	// Level is the heading level (1 for #, 2 for ##, etc.).
	Level int
	// Title is the heading text, trimmed.
	Title string
	// LineIndex is the 0-based line number where the heading appears.
	LineIndex int
}

// ParseTOC extracts a table of contents from a markdown plan. It returns all
// ATX-style headings (# through ######) in document order.
func ParseTOC(content string) []TOCEntry {
	lines := strings.Split(content, "\n")
	var entries []TOCEntry
	inFence := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Track fenced code blocks to skip headings inside them.
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		level := headingLevel(trimmed)
		if level == 0 {
			continue
		}
		title := strings.TrimSpace(trimmed[level:])
		if title == "" {
			continue
		}
		entries = append(entries, TOCEntry{
			Level:     level,
			Title:     title,
			LineIndex: i,
		})
	}
	return entries
}

// SplitSections splits plan content into sections based on headings. Each
// section includes the heading line and all body lines up to (but not
// including) the next heading of equal or higher level.
type Section struct {
	// Heading is the heading text (empty for the preamble before any heading).
	Heading string
	// Level is the heading level (0 for preamble).
	Level int
	// Content is the full section text including the heading line.
	Content string
	// LineStart is the 0-based line number where this section begins.
	LineStart int
	// LineEnd is the 0-based line number of the last line in this section
	// (inclusive).
	LineEnd int
}

// SplitSections splits markdown content into sections at heading boundaries.
// Only headings at level <= maxLevel are treated as section boundaries. If
// maxLevel is 0, level 2 (##) is used as the default.
func SplitSections(content string, maxLevel int) []Section {
	if maxLevel <= 0 {
		maxLevel = 2
	}
	lines := strings.Split(content, "\n")
	entries := ParseTOC(content)

	// Filter to section-boundary headings only.
	var boundaries []TOCEntry
	for _, e := range entries {
		if e.Level <= maxLevel {
			boundaries = append(boundaries, e)
		}
	}

	// If there are no headings, return the whole content as one section.
	if len(boundaries) == 0 {
		return []Section{{
			Content:   content,
			LineStart: 0,
			LineEnd:   len(lines) - 1,
		}}
	}

	var sections []Section

	// Preamble: lines before the first heading.
	if boundaries[0].LineIndex > 0 {
		preamble := strings.Join(lines[:boundaries[0].LineIndex], "\n")
		if strings.TrimSpace(preamble) != "" {
			sections = append(sections, Section{
				Content:   preamble,
				LineStart: 0,
				LineEnd:   boundaries[0].LineIndex - 1,
			})
		}
	}

	// Each heading starts a section that runs until the next boundary heading
	// or end of document.
	for i, b := range boundaries {
		endLine := len(lines) - 1
		if i+1 < len(boundaries) {
			endLine = boundaries[i+1].LineIndex - 1
		}
		sectionLines := lines[b.LineIndex : endLine+1]
		sections = append(sections, Section{
			Heading:   b.Title,
			Level:     b.Level,
			Content:   strings.Join(sectionLines, "\n"),
			LineStart: b.LineIndex,
			LineEnd:   endLine,
		})
	}
	return sections
}

// headingLevel returns the ATX heading level (1-6) of a line, or 0 if the
// line is not a heading.
func headingLevel(line string) int {
	if len(line) < 2 || line[0] != '#' {
		return 0
	}
	level := 0
	for _, ch := range line {
		if ch == '#' {
			level++
			if level > 6 {
				return 0
			}
		} else {
			break
		}
	}
	// Must be followed by a space (or be the entire line for empty headings).
	if level < len(line) && line[level] != ' ' {
		return 0
	}
	return level
}
