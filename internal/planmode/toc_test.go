package planmode

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseTOC(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    []TOCEntry
	}{
		{
			name:    "empty content",
			content: "",
			want:    nil,
		},
		{
			name:    "no headings",
			content: "just some text\nmore text",
			want:    nil,
		},
		{
			name:    "single heading",
			content: "## Approach",
			want: []TOCEntry{
				{Level: 2, Title: "Approach", LineIndex: 0},
			},
		},
		{
			name: "multiple headings",
			content: `# Title

Some preamble text.

## Context

Details about context.

## Approach

### Step 1

Do something.

### Step 2

Do something else.

## Verification

How to verify.`,
			want: []TOCEntry{
				{Level: 1, Title: "Title", LineIndex: 0},
				{Level: 2, Title: "Context", LineIndex: 4},
				{Level: 2, Title: "Approach", LineIndex: 8},
				{Level: 3, Title: "Step 1", LineIndex: 10},
				{Level: 3, Title: "Step 2", LineIndex: 14},
				{Level: 2, Title: "Verification", LineIndex: 18},
			},
		},
		{
			name: "headings in code blocks are skipped",
			content: "## Real Heading\n" +
				"```\n## Not A Heading\n```\n## Another Real",
			want: []TOCEntry{
				{Level: 2, Title: "Real Heading", LineIndex: 0},
				{Level: 2, Title: "Another Real", LineIndex: 4},
			},
		},
		{
			name:    "non-heading hash",
			content: "##NoSpace",
			want:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ParseTOC(tt.content)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestSplitSections(t *testing.T) {
	t.Parallel()

	t.Run("preamble and sections", func(t *testing.T) {
		t.Parallel()
		content := `Some preamble text.

## Context

Context details.

## Approach

Approach details.

### Sub-step

More details.`
		sections := SplitSections(content, 2)
		require.Len(t, sections, 3)

		require.Equal(t, "", sections[0].Heading)
		require.Equal(t, 0, sections[0].Level)
		require.Equal(t, 0, sections[0].LineStart)

		require.Equal(t, "Context", sections[1].Heading)
		require.Equal(t, 2, sections[1].Level)

		require.Equal(t, "Approach", sections[2].Heading)
		require.Equal(t, 2, sections[2].Level)
		// Sub-step should be included in Approach section.
		require.Contains(t, sections[2].Content, "Sub-step")
	})

	t.Run("no headings", func(t *testing.T) {
		t.Parallel()
		content := "just text\nmore text"
		sections := SplitSections(content, 2)
		require.Len(t, sections, 1)
		require.Equal(t, content, sections[0].Content)
	})

	t.Run("default max level", func(t *testing.T) {
		t.Parallel()
		content := "## A\n\n### B\n\n## C"
		sections := SplitSections(content, 0)
		require.Len(t, sections, 2)
		require.Equal(t, "A", sections[0].Heading)
		require.Equal(t, "C", sections[1].Heading)
	})
}
