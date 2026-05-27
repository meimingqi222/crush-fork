package tools

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParsePathSelectorNoSelector(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
	}{
		{"plain file", "src/main.go"},
		{"relative path", "internal/agent/tools/read.go"},
		{"absolute path", "/home/user/file.go"},
		{"with extension", "file.ts"},
		{"no extension", "Makefile"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sel := parsePathSelector(tc.input)
			require.Equal(t, tc.input, sel.filePath)
			require.False(t, sel.hasSelector)
			require.False(t, sel.raw)
			require.False(t, sel.hasLineSel)
			require.Equal(t, 0, sel.offset)
			require.Equal(t, 0, sel.limit)
		})
	}
}

func TestParsePathSelectorURLGuard(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
	}{
		{"https with port", "https://example.com:8080"},
		{"https with path", "https://example.com:443/path/to/page"},
		{"http localhost", "http://localhost:3000/api"},
		{"https no port", "https://example.com/path"},
		{"http plain", "http://example.com"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sel := parsePathSelector(tc.input)
			require.Equal(t, tc.input, sel.filePath)
			require.False(t, sel.hasSelector, "URLs should never have selectors")
		})
	}
}

func TestParsePathSelectorLineRanges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		input       string
		wantPath    string
		wantOffset  int
		wantLimit   int
		wantLineSel bool
	}{
		{
			name:        "single line number",
			input:       "file.ts:50",
			wantPath:    "file.ts",
			wantOffset:  49,
			wantLimit:   0,
			wantLineSel: true,
		},
		{
			name:        "inclusive range",
			input:       "file.ts:50-100",
			wantPath:    "file.ts",
			wantOffset:  49,
			wantLimit:   51,
			wantLineSel: true,
		},
		{
			name:        "count-based",
			input:       "file.ts:50+10",
			wantPath:    "file.ts",
			wantOffset:  49,
			wantLimit:   10,
			wantLineSel: true,
		},
		{
			name:        "open-ended",
			input:       "file.ts:50-",
			wantPath:    "file.ts",
			wantOffset:  49,
			wantLimit:   0,
			wantLineSel: true,
		},
		{
			name:        "line 1",
			input:       "file.ts:1",
			wantPath:    "file.ts",
			wantOffset:  0,
			wantLimit:   0,
			wantLineSel: true,
		},
		{
			name:        "L prefix",
			input:       "file.ts:L50",
			wantPath:    "file.ts",
			wantOffset:  49,
			wantLimit:   0,
			wantLineSel: true,
		},
		{
			name:        "L prefix range",
			input:       "file.ts:L50-L100",
			wantPath:    "file.ts",
			wantOffset:  49,
			wantLimit:   51,
			wantLineSel: true,
		},
		{
			name:        "absolute path with selector",
			input:       "/home/user/file.go:100",
			wantPath:    "/home/user/file.go",
			wantOffset:  99,
			wantLimit:   0,
			wantLineSel: true,
		},
		{
			name:        "relative path with selector",
			input:       "internal/agent/read.go:20-50",
			wantPath:    "internal/agent/read.go",
			wantOffset:  19,
			wantLimit:   31,
			wantLineSel: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sel := parsePathSelector(tc.input)
			require.Equal(t, tc.wantPath, sel.filePath)
			require.True(t, sel.hasSelector)
			require.Equal(t, tc.wantLineSel, sel.hasLineSel)
			require.Equal(t, tc.wantOffset, sel.offset)
			require.Equal(t, tc.wantLimit, sel.limit)
			require.False(t, sel.raw)
		})
	}
}

func TestParsePathSelectorRawMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		input       string
		wantPath    string
		wantOffset  int
		wantLimit   int
		wantLineSel bool
	}{
		{
			name:     "raw only",
			input:    "file.ts:raw",
			wantPath: "file.ts",
		},
		{
			name:        "range then raw",
			input:       "file.ts:50-100:raw",
			wantPath:    "file.ts",
			wantOffset:  49,
			wantLimit:   51,
			wantLineSel: true,
		},
		{
			name:        "raw then range",
			input:       "file.ts:raw:50-100",
			wantPath:    "file.ts",
			wantOffset:  49,
			wantLimit:   51,
			wantLineSel: true,
		},
		{
			name:        "single line then raw",
			input:       "file.ts:50:raw",
			wantPath:    "file.ts",
			wantOffset:  49,
			wantLineSel: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sel := parsePathSelector(tc.input)
			require.Equal(t, tc.wantPath, sel.filePath)
			require.True(t, sel.hasSelector)
			require.True(t, sel.raw)
			require.Equal(t, tc.wantLineSel, sel.hasLineSel)
			require.Equal(t, tc.wantOffset, sel.offset)
			require.Equal(t, tc.wantLimit, sel.limit)
		})
	}
}

func TestParsePathSelectorInvalidTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
	}{
		{"non-numeric token", "file.ts:abc"},
		{"line zero", "file.ts:0"},
		{"empty after colon", "file.ts:"},
		{"mixed valid and invalid", "file.ts:50:abc"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sel := parsePathSelector(tc.input)
			// Invalid selector tokens → entire input is the path.
			require.Equal(t, tc.input, sel.filePath)
			require.False(t, sel.hasSelector)
		})
	}
}

func TestParsePathSelectorExistenceFallback(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Create a file whose name contains a colon-like suffix.
	weirdName := "data:50"
	weirdPath := filepath.Join(dir, weirdName)
	require.NoError(t, os.WriteFile(weirdPath, []byte("content"), 0o644))

	// When we're in the directory and the weird file exists, the parser
	// should prefer the full path over splitting on the colon.
	input := weirdPath
	sel := parsePathSelector(input)
	require.Equal(t, weirdPath, sel.filePath)
	require.False(t, sel.hasSelector,
		"existence fallback should treat the full path as a file")
}

func TestParsePathSelectorColonInFilenameWithRealSelector(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	weirdName := "my:file.ts"
	weirdPath := filepath.Join(dir, weirdName)
	require.NoError(t, os.WriteFile(weirdPath, []byte("content"), 0o644))

	// "my:file.ts:50-100" — the parser should find "50-100" as a valid
	// selector and "my:file.ts" as the base path (which exists).
	input := weirdPath + ":50-100"
	sel := parsePathSelector(input)
	require.Equal(t, weirdPath, sel.filePath)
	require.True(t, sel.hasSelector)
	require.True(t, sel.hasLineSel)
	require.Equal(t, 49, sel.offset)
	require.Equal(t, 51, sel.limit)
}
