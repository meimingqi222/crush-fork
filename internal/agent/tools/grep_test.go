package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestRegexCache(t *testing.T) {
	cache := newRegexCache()

	// Test basic caching
	pattern := "test.*pattern"
	regex1, err := cache.get(pattern)
	if err != nil {
		t.Fatalf("Failed to compile regex: %v", err)
	}

	regex2, err := cache.get(pattern)
	if err != nil {
		t.Fatalf("Failed to get cached regex: %v", err)
	}

	// Should be the same instance (cached)
	if regex1 != regex2 {
		t.Error("Expected cached regex to be the same instance")
	}

	// Test that it actually works
	if !regex1.MatchString("test123pattern") {
		t.Error("Regex should match test string")
	}
}

func TestGlobIncludePattern(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	// Create test files with different extensions
	for path, content := range map[string]string{
		"file1.go":   "package main",
		"file2.js":   "console.log",
		"file3.ts":   "const x",
		"src/app.go": "package main",
		"src/lib.js": "export default",
	} {
		fullPath := filepath.Join(tempDir, path)
		require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o755))
		require.NoError(t, os.WriteFile(fullPath, []byte(content), 0o644))
	}

	tests := []struct {
		name    string
		include string
		want    int
	}{
		// *.go matches .go files at any depth (ripgrep basename semantics)
		{"single extension", "*.go", 2},
		// *.{js,ts} matches file2.js, file3.ts, and src/lib.js (any depth)
		{"brace expansion", "*.{js,ts}", 3},
		// src/*.go matches files directly in src/
		{"nested pattern", "src/*.go", 1},
		// src/**/*.go matches all .go files under src/
		{"double star subdirectory", "src/**/*.go", 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matches, err := searchFilesWithRegex("package|console|const|export", tempDir, tt.include, 0, 0)
			require.NoError(t, err)
			require.Len(t, matches, tt.want, "include pattern %q should match %d files", tt.include, tt.want)
		})
	}
}

func TestRunGrepSearchFallsBackToLiteralTextOnInvalidRegex(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tempDir, "sample.txt"), []byte("[]fantasy.AgentTool\n"), 0o644))

	result, err := runGrepSearch(t.Context(), GrepParams{Pattern: "[]fantasy.AgentTool"}, tempDir, 100, 0, 0)
	require.NoError(t, err)
	require.Len(t, result.matches, 1)
	require.True(t, result.metadata.LiteralText)
	require.Equal(t, "literal_text_fallback", result.metadata.RecoveredBy)
	require.Equal(t, []string{"literal_text"}, result.metadata.RecoveredParameters)
}

// TestRunGrepSearchFallsBackToLiteralTextOnUnclosedCharacterClass tests that
// patterns with unclosed character classes (like "agents[") are treated
// as literal text when ripgrep reports "unclosed character class" error.
func TestRunGrepSearchFallsBackToLiteralTextOnUnclosedCharacterClass(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tempDir, "sample.go"), []byte("c.agents[config.AgentCoder] = agent\n"), 0o644))

	result, err := runGrepSearch(t.Context(), GrepParams{Pattern: "agents["}, tempDir, 100, 0, 0)
	require.NoError(t, err)
	require.Len(t, result.matches, 1)
	require.True(t, result.metadata.LiteralText)
	require.Equal(t, "literal_text_fallback", result.metadata.RecoveredBy)
}

func TestRunGrepSearchReportsMissingSearchPath(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	missingDir := filepath.Join(tempDir, "missing")

	result, err := runGrepSearch(t.Context(), GrepParams{Pattern: "hello"}, missingDir, 100, 0, 0)
	require.NoError(t, err)
	require.Empty(t, result.matches)
	require.Equal(t, "path_validation", result.metadata.RecoveredBy)
	require.Equal(t, ReadToolName, result.metadata.FallbackTool)
	require.Equal(t, []string{"path"}, result.metadata.RecoveredParameters)
}

func TestGrepWithIgnoreFiles(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	// Create test files
	testFiles := map[string]string{
		"file1.txt":           "hello world",
		"file2.txt":           "hello world",
		"ignored/file3.txt":   "hello world",
		"node_modules/lib.js": "hello world",
		"secret.key":          "hello world",
	}

	for path, content := range testFiles {
		fullPath := filepath.Join(tempDir, path)
		require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o755))
		require.NoError(t, os.WriteFile(fullPath, []byte(content), 0o644))
	}

	// Create .gitignore file
	gitignoreContent := "ignored/\n*.key\n"
	require.NoError(t, os.WriteFile(filepath.Join(tempDir, ".gitignore"), []byte(gitignoreContent), 0o644))

	// Create .crushignore file
	crushignoreContent := "node_modules/\n"
	require.NoError(t, os.WriteFile(filepath.Join(tempDir, ".crushignore"), []byte(crushignoreContent), 0o644))

	// Test both implementations
	for name, fn := range map[string]func(pattern, path, include string, ctxBefore, ctxAfter int) ([]grepMatch, error){
		"regex": searchFilesWithRegex,
		"rg": func(pattern, path, include string, ctxBefore, ctxAfter int) ([]grepMatch, error) {
			return searchWithRipgrep(t.Context(), pattern, path, include, ctxBefore, ctxAfter)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if name == "rg" && getRg() == "" {
				t.Skip("rg is not in $PATH")
			}

			matches, err := fn("hello world", tempDir, "", 0, 0)
			require.NoError(t, err)

			// Convert matches to a set of file paths for easier testing
			foundFiles := make(map[string]bool)
			for _, match := range matches {
				foundFiles[filepath.Base(match.path)] = true
			}

			// Should find file1.txt and file2.txt
			require.True(t, foundFiles["file1.txt"], "Should find file1.txt")
			require.True(t, foundFiles["file2.txt"], "Should find file2.txt")

			// Should NOT find ignored files
			require.False(t, foundFiles["file3.txt"], "Should not find file3.txt (ignored by .gitignore)")
			require.False(t, foundFiles["lib.js"], "Should not find lib.js (ignored by .crushignore)")
			require.False(t, foundFiles["secret.key"], "Should not find secret.key (ignored by .gitignore)")

			// Should find exactly 2 matches
			require.Equal(t, 2, len(matches), "Should find exactly 2 matches")
		})
	}
}

func TestSearchImplementations(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	for path, content := range map[string]string{
		"file1.go":         "package main\nfunc main() {\n\tfmt.Println(\"hello world\")\n}",
		"file2.js":         "console.log('hello world');",
		"file3.txt":        "hello world from text file",
		"binary.exe":       "\x00\x01\x02\x03",
		"empty.txt":        "",
		"subdir/nested.go": "package nested\n// hello world comment",
		".hidden.txt":      "hello world in hidden file",
		"file4.txt":        "hello world from a banana",
		"file5.txt":        "hello world from a grape",
	} {
		fullPath := filepath.Join(tempDir, path)
		require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o755))
		require.NoError(t, os.WriteFile(fullPath, []byte(content), 0o644))
	}

	require.NoError(t, os.WriteFile(filepath.Join(tempDir, ".gitignore"), []byte("file4.txt\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tempDir, ".crushignore"), []byte("file5.txt\n"), 0o644))

	for name, fn := range map[string]func(pattern, path, include string, ctxBefore, ctxAfter int) ([]grepMatch, error){
		"regex": searchFilesWithRegex,
		"rg": func(pattern, path, include string, ctxBefore, ctxAfter int) ([]grepMatch, error) {
			return searchWithRipgrep(t.Context(), pattern, path, include, ctxBefore, ctxAfter)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if name == "rg" && getRg() == "" {
				t.Skip("rg is not in $PATH")
			}

			matches, err := fn("hello world", tempDir, "", 0, 0)
			require.NoError(t, err)

			require.Equal(t, len(matches), 4)
			for _, match := range matches {
				require.NotEmpty(t, match.path)
				require.NotZero(t, match.lineNum)
				require.NotEmpty(t, match.lineText)
				require.NotZero(t, match.modTime)
				require.NotContains(t, match.path, ".hidden.txt")
				require.NotContains(t, match.path, "file4.txt")
				require.NotContains(t, match.path, "file5.txt")
				require.NotContains(t, match.path, "binary.exe")
			}
		})
	}
}

// Benchmark to show performance improvement
func BenchmarkRegexCacheVsCompile(b *testing.B) {
	cache := newRegexCache()
	pattern := "test.*pattern.*[0-9]+"

	b.Run("WithCache", func(b *testing.B) {
		for b.Loop() {
			_, err := cache.get(pattern)
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("WithoutCache", func(b *testing.B) {
		for b.Loop() {
			_, err := regexp.Compile(pattern)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

func TestIsTextFile(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	tests := []struct {
		name     string
		filename string
		content  []byte
		wantText bool
	}{
		{
			name:     "go file",
			filename: "test.go",
			content:  []byte("package main\n\nfunc main() {}\n"),
			wantText: true,
		},
		{
			name:     "yaml file",
			filename: "config.yaml",
			content:  []byte("key: value\nlist:\n  - item1\n  - item2\n"),
			wantText: true,
		},
		{
			name:     "yml file",
			filename: "config.yml",
			content:  []byte("key: value\n"),
			wantText: true,
		},
		{
			name:     "json file",
			filename: "data.json",
			content:  []byte(`{"key": "value"}`),
			wantText: true,
		},
		{
			name:     "javascript file",
			filename: "script.js",
			content:  []byte("console.log('hello');\n"),
			wantText: true,
		},
		{
			name:     "typescript file",
			filename: "script.ts",
			content:  []byte("const x: string = 'hello';\n"),
			wantText: true,
		},
		{
			name:     "markdown file",
			filename: "README.md",
			content:  []byte("# Title\n\nSome content\n"),
			wantText: true,
		},
		{
			name:     "shell script",
			filename: "script.sh",
			content:  []byte("#!/bin/bash\necho 'hello'\n"),
			wantText: true,
		},
		{
			name:     "python file",
			filename: "script.py",
			content:  []byte("print('hello')\n"),
			wantText: true,
		},
		{
			name:     "xml file",
			filename: "data.xml",
			content:  []byte("<?xml version=\"1.0\"?>\n<root></root>\n"),
			wantText: true,
		},
		{
			name:     "plain text",
			filename: "file.txt",
			content:  []byte("plain text content\n"),
			wantText: true,
		},
		{
			name:     "css file",
			filename: "style.css",
			content:  []byte("body { color: red; }\n"),
			wantText: true,
		},
		{
			name:     "scss file",
			filename: "style.scss",
			content:  []byte("$primary: blue;\nbody { color: $primary; }\n"),
			wantText: true,
		},
		{
			name:     "sass file",
			filename: "style.sass",
			content:  []byte("$primary: blue\nbody\n  color: $primary\n"),
			wantText: true,
		},
		{
			name:     "rust file",
			filename: "main.rs",
			content:  []byte("fn main() {\n    println!(\"Hello, world!\");\n}\n"),
			wantText: true,
		},
		{
			name:     "zig file",
			filename: "main.zig",
			content:  []byte("const std = @import(\"std\");\npub fn main() void {}\n"),
			wantText: true,
		},
		{
			name:     "java file",
			filename: "Main.java",
			content:  []byte("public class Main {\n    public static void main(String[] args) {}\n}\n"),
			wantText: true,
		},
		{
			name:     "c file",
			filename: "main.c",
			content:  []byte("#include <stdio.h>\nint main() { return 0; }\n"),
			wantText: true,
		},
		{
			name:     "cpp file",
			filename: "main.cpp",
			content:  []byte("#include <iostream>\nint main() { return 0; }\n"),
			wantText: true,
		},
		{
			name:     "fish shell",
			filename: "script.fish",
			content:  []byte("#!/usr/bin/env fish\necho 'hello'\n"),
			wantText: true,
		},
		{
			name:     "powershell file",
			filename: "script.ps1",
			content:  []byte("Write-Host 'Hello, World!'\n"),
			wantText: true,
		},
		{
			name:     "cmd batch file",
			filename: "script.bat",
			content:  []byte("@echo off\necho Hello, World!\n"),
			wantText: true,
		},
		{
			name:     "cmd file",
			filename: "script.cmd",
			content:  []byte("@echo off\necho Hello, World!\n"),
			wantText: true,
		},
		{
			name:     "binary exe",
			filename: "binary.exe",
			content:  []byte{0x4D, 0x5A, 0x90, 0x00, 0x03, 0x00, 0x00, 0x00},
			wantText: false,
		},
		{
			name:     "png image",
			filename: "image.png",
			content:  []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A},
			wantText: false,
		},
		{
			name:     "jpeg image",
			filename: "image.jpg",
			content:  []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 0x4A, 0x46},
			wantText: false,
		},
		{
			name:     "zip archive",
			filename: "archive.zip",
			content:  []byte{0x50, 0x4B, 0x03, 0x04, 0x14, 0x00, 0x00, 0x00},
			wantText: false,
		},
		{
			name:     "pdf file",
			filename: "document.pdf",
			content:  []byte("%PDF-1.4\n%âãÏÓ\n"),
			wantText: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			filePath := filepath.Join(tempDir, tt.filename)
			require.NoError(t, os.WriteFile(filePath, tt.content, 0o644))

			got := isTextFile(filePath)
			require.Equal(t, tt.wantText, got, "isTextFile(%s) = %v, want %v", tt.filename, got, tt.wantText)
		})
	}
}

func TestColumnMatch(t *testing.T) {
	t.Parallel()

	// Test both implementations
	for name, fn := range map[string]func(pattern, path, include string, ctxBefore, ctxAfter int) ([]grepMatch, error){
		"regex": searchFilesWithRegex,
		"rg": func(pattern, path, include string, ctxBefore, ctxAfter int) ([]grepMatch, error) {
			return searchWithRipgrep(t.Context(), pattern, path, include, ctxBefore, ctxAfter)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if name == "rg" && getRg() == "" {
				t.Skip("rg is not in $PATH")
			}

			matches, err := fn("THIS", "./testdata/", "", 0, 0)
			require.NoError(t, err)
			require.Len(t, matches, 1)
			match := matches[0]
			require.Equal(t, 2, match.lineNum)
			require.Equal(t, 14, match.charNum)
			require.Equal(t, "I wanna grep THIS particular word", match.lineText)
			require.Equal(t, "testdata/grep.txt", filepath.ToSlash(filepath.Clean(match.path)))
		})
	}
}

// TestGrepWithDifferentWorkingDirectory verifies that ripgrep-based search
// works correctly when the process CWD differs from the search directory.
// This is a regression test for the cmd.Dir bug where searchWithRipgrep
// would return "No files found" because ripgrep ran from the wrong directory.
func TestGrepWithDifferentWorkingDirectory(t *testing.T) {
	// Cannot use t.Parallel() because os.Chdir affects the entire process.
	searchDir := t.TempDir()
	unrelatedDir := t.TempDir()

	// Create test files in the search directory.
	for path, content := range map[string]string{
		"file1.go":     "package main\nfunc main() {}",
		"sub/file2.go": "package sub\nfunc helper() {}",
	} {
		fullPath := filepath.Join(searchDir, path)
		require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o755))
		require.NoError(t, os.WriteFile(fullPath, []byte(content), 0o644))
	}

	// Change process CWD to an unrelated directory.
	origDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(unrelatedDir))
	t.Cleanup(func() { require.NoError(t, os.Chdir(origDir)) })

	// Both implementations should find matches using the absolute search path.
	for name, fn := range map[string]func(pattern, path, include string, ctxBefore, ctxAfter int) ([]grepMatch, error){
		"regex": searchFilesWithRegex,
		"rg": func(pattern, path, include string, ctxBefore, ctxAfter int) ([]grepMatch, error) {
			return searchWithRipgrep(t.Context(), pattern, path, include, ctxBefore, ctxAfter)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if name == "rg" && getRg() == "" {
				t.Skip("rg is not in $PATH")
			}

			matches, err := fn("package", searchDir, "", 0, 0)
			require.NoError(t, err)
			require.Len(t, matches, 2, "should find 'package' in both files regardless of CWD")
		})
	}
}

func TestGrepToolUsesSessionWorkingDirForRelativeSearchPath(t *testing.T) {
	t.Parallel()

	sessionDir := t.TempDir()
	fallbackDir := t.TempDir()
	relativeDir := filepath.Join(sessionDir, "src")
	require.NoError(t, os.Mkdir(relativeDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(relativeDir, "main.go"), []byte("package session\n"), 0o644))

	tool := NewGrepTool(fallbackDir, config.ToolGrep{})
	ctx := context.WithValue(t.Context(), WorkingDirContextKey, sessionDir)
	input, err := json.Marshal(GrepParams{Pattern: "package", Path: "src"})
	require.NoError(t, err)
	response, err := tool.Run(ctx, fantasy.ToolCall{ID: "grep-contract", Name: GrepToolName, Input: string(input)})
	require.NoError(t, err)
	require.False(t, response.IsError)

	var metadata GrepResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(response.Metadata), &metadata))
	require.Equal(t, sessionDir, metadata.WorkingDirectory)
	require.Equal(t, filepath.Join(sessionDir, "src"), metadata.ResolvedPath)
	require.Equal(t, "src", metadata.DisplayPath)
	require.Contains(t, response.Content, "src/main.go")
}
func TestGrepSortingByModTime(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	// Create files with a small delay to ensure different modification times
	file1 := filepath.Join(tempDir, "older.txt")
	file2 := filepath.Join(tempDir, "newer.txt")

	require.NoError(t, os.WriteFile(file1, []byte("test content"), 0o644))
	// Small delay to ensure different mod time
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, os.WriteFile(file2, []byte("test content"), 0o644))

	// Test regex implementation
	matches, err := searchFilesWithRegex("test", tempDir, "", 0, 0)
	require.NoError(t, err)
	require.Len(t, matches, 2)

	// Newer file should come first (sorted by mod time descending)
	require.Equal(t, file2, matches[0].path, "newer file should be first")
	require.Equal(t, file1, matches[1].path, "older file should be second")
}
