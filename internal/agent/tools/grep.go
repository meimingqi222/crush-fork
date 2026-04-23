package tools

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/bmatcuk/doublestar/v4"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/fsext"
)

// regexCache provides thread-safe caching of compiled regex patterns
type regexCache struct {
	*csync.Map[string, *regexp.Regexp]
}

// newRegexCache creates a new regex cache
func newRegexCache() *regexCache {
	return &regexCache{
		Map: csync.NewMap[string, *regexp.Regexp](),
	}
}

// get retrieves a compiled regex from cache or compiles and caches it
func (rc *regexCache) get(pattern string) (*regexp.Regexp, error) {
	var rerr error
	return rc.GetOrSet(pattern, func() *regexp.Regexp {
		regex, err := regexp.Compile(pattern)
		if err != nil {
			rerr = err
		}
		return regex
	}), rerr
}

// ResetCache clears compiled regex caches to prevent unbounded growth across sessions.
func ResetCache() {
	searchRegexCache.Reset(map[string]*regexp.Regexp{})
}

// Global regex cache instances
var (
	searchRegexCache = newRegexCache()
)

type GrepParams struct {
	Pattern     string `json:"pattern" description:"The regex pattern to search for in file contents"`
	Path        string `json:"path,omitempty" description:"The directory to search in. Defaults to the current working directory."`
	Include     string `json:"include,omitempty" description:"File pattern to include in the search (e.g. \"*.js\", \"*.{ts,tsx}\")"`
	LiteralText bool   `json:"literal_text,omitempty" description:"If true, the pattern will be treated as literal text with special regex characters escaped. Default is false."`
}

type grepMatch struct {
	path     string
	modTime  time.Time
	lineNum  int
	charNum  int
	lineText string
}

type GrepResponseMetadata struct {
	NumberOfMatches     int      `json:"number_of_matches"`
	Truncated           bool     `json:"truncated"`
	Pattern             string   `json:"pattern,omitempty"`
	LiteralText         bool     `json:"literal_text,omitempty"`
	RecoveredBy         string   `json:"recovered_by,omitempty"`
	RecoveryAction      string   `json:"recovery_action,omitempty"`
	FallbackTool        string   `json:"fallback_tool,omitempty"`
	FallbackToolQuery   string   `json:"fallback_tool_query,omitempty"`
	RecoveredParameters []string `json:"recovered_parameters,omitempty"`
}

type grepExecutionResult struct {
	matches   []grepMatch
	truncated bool
	metadata  GrepResponseMetadata
}

const (
	GrepToolName        = "grep"
	maxGrepContentWidth = 500
)

//go:embed grep.md
var grepDescription []byte

// escapeRegexPattern escapes special regex characters so they're treated as literal characters
func escapeRegexPattern(pattern string) string {
	specialChars := []string{"\\", ".", "+", "*", "?", "(", ")", "[", "]", "{", "}", "^", "$", "|"}
	escaped := pattern

	for _, char := range specialChars {
		escaped = strings.ReplaceAll(escaped, char, "\\"+char)
	}

	return escaped
}

func NewGrepTool(workingDir string, config config.ToolGrep) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		GrepToolName,
		string(grepDescription),
		func(ctx context.Context, params GrepParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Pattern == "" {
				return fantasy.NewTextErrorResponse("pattern is required"), nil
			}

			// Use session-specific working directory from context if available.
			effectiveWorkingDir := cmp.Or(GetWorkingDirFromContext(ctx), workingDir)
			searchPath := cmp.Or(params.Path, effectiveWorkingDir)

			searchCtx, cancel := context.WithTimeout(ctx, config.GetTimeout())
			defer cancel()

			result, err := runGrepSearch(searchCtx, params, searchPath, 100)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("error searching files: %v", err)), nil
			}

			var output strings.Builder
			if len(result.matches) == 0 {
				output.WriteString("No files found")
			} else {
				fmt.Fprintf(&output, "Found %d matches\n", len(result.matches))

				currentFile := ""
				for _, match := range result.matches {
					if currentFile != match.path {
						if currentFile != "" {
							output.WriteString("\n")
						}
						currentFile = match.path
						fmt.Fprintf(&output, "%s:\n", filepath.ToSlash(match.path))
					}
					if match.lineNum > 0 {
						lineText := match.lineText
						if len(lineText) > maxGrepContentWidth {
							lineText = lineText[:maxGrepContentWidth] + "..."
						}
						if match.charNum > 0 {
							fmt.Fprintf(&output, "  Line %d, Char %d: %s\n", match.lineNum, match.charNum, lineText)
						} else {
							fmt.Fprintf(&output, "  Line %d: %s\n", match.lineNum, lineText)
						}
					} else {
						fmt.Fprintf(&output, "  %s\n", match.path)
					}
				}

				if result.truncated {
					output.WriteString("\n(Results are truncated. Consider using a more specific path or pattern.)")
				}
			}

			result.metadata.NumberOfMatches = len(result.matches)
			result.metadata.Truncated = result.truncated
			return fantasy.WithResponseMetadata(
				fantasy.NewTextResponse(output.String()),
				result.metadata,
			), nil
		})
}

func runGrepSearch(ctx context.Context, params GrepParams, rootPath string, limit int) (grepExecutionResult, error) {
	metadata := GrepResponseMetadata{
		Pattern:     params.Pattern,
		LiteralText: params.LiteralText,
	}
	resolvedRootPath, err := validateGrepPath(rootPath)
	if err != nil {
		metadata.RecoveredBy = "path_validation"
		metadata.RecoveryAction = err.Error()
		metadata.FallbackTool = ViewToolName
		metadata.FallbackToolQuery = cmp.Or(params.Path, rootPath)
		metadata.RecoveredParameters = []string{"path"}
		return grepExecutionResult{metadata: metadata}, nil
	}

	searchPattern := params.Pattern
	if params.LiteralText {
		searchPattern = escapeRegexPattern(params.Pattern)
	}

	matches, err := searchWithRipgrep(ctx, searchPattern, resolvedRootPath, params.Include)
	if err != nil {
		matches, metadata, err = recoverGrepMatches(ctx, params, resolvedRootPath, metadata)
		if err != nil {
			return grepExecutionResult{}, err
		}
	}

	sort.Slice(matches, func(i, j int) bool {
		return matches[i].modTime.After(matches[j].modTime)
	})

	truncated := len(matches) > limit
	if truncated {
		matches = matches[:limit]
	}

	return grepExecutionResult{matches: matches, truncated: truncated, metadata: metadata}, nil
}

func validateGrepPath(rootPath string) (string, error) {
	resolved := filepath.Clean(rootPath)
	info, err := os.Stat(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("Search path does not exist: %s. Verify the path first or use glob/view to inspect nearby files.", resolved)
		}
		return "", fmt.Errorf("error accessing search path: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("Search path is not a directory: %s. Use view for files or provide a directory path.", resolved)
	}
	return resolved, nil
}

func recoverGrepMatches(ctx context.Context, params GrepParams, rootPath string, metadata GrepResponseMetadata) ([]grepMatch, GrepResponseMetadata, error) {
	matches, regexErr := searchFilesWithRegex(params.Pattern, rootPath, params.Include)
	if regexErr == nil {
		return matches, metadata, nil
	}
	if params.LiteralText || !looksLikeRegexSyntaxError(regexErr) {
		return nil, metadata, regexErr
	}

	literalPattern := escapeRegexPattern(params.Pattern)
	matches, err := searchWithRipgrep(ctx, literalPattern, rootPath, params.Include)
	if err != nil {
		matches, err = searchFilesWithRegex(literalPattern, rootPath, params.Include)
		if err != nil {
			return nil, metadata, regexErr
		}
	}
	metadata.LiteralText = true
	metadata.RecoveredBy = "literal_text_fallback"
	metadata.RecoveryAction = fmt.Sprintf("Pattern %q was not valid regex syntax. Treated it as literal text instead.", params.Pattern)
	metadata.FallbackTool = GrepToolName
	metadata.FallbackToolQuery = params.Pattern
	metadata.RecoveredParameters = []string{"literal_text"}
	return matches, metadata, nil
}

func looksLikeRegexSyntaxError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "invalid regex pattern") || strings.Contains(msg, "error parsing regexp") || strings.Contains(msg, "missing closing") || strings.Contains(msg, "unexpected")
}

func searchWithRipgrep(ctx context.Context, pattern, rootPath, include string) ([]grepMatch, error) {
	cmd := getRgSearchCmd(ctx, pattern, rootPath, include)
	if cmd == nil {
		return nil, fmt.Errorf("ripgrep not found in $PATH")
	}

	// Only add ignore files if they exist
	for _, ignoreFile := range []string{".gitignore", ".crushignore"} {
		ignorePath := filepath.Join(rootPath, ignoreFile)
		if _, err := os.Stat(ignorePath); err == nil {
			cmd.Args = append(cmd.Args, "--ignore-file", ignorePath)
		}
	}

	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return []grepMatch{}, nil
		}
		return nil, err
	}

	var matches []grepMatch
	for line := range bytes.SplitSeq(bytes.TrimSpace(output), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var match ripgrepMatch
		if err := json.Unmarshal(line, &match); err != nil {
			continue
		}
		if match.Type != "match" {
			continue
		}
		for _, m := range match.Data.Submatches {
			fi, err := os.Stat(match.Data.Path.Text)
			if err != nil {
				continue // Skip files we can't access
			}
			matches = append(matches, grepMatch{
				path:     match.Data.Path.Text,
				modTime:  fi.ModTime(),
				lineNum:  match.Data.LineNumber,
				charNum:  m.Start + 1, // ensure 1-based
				lineText: strings.TrimSpace(match.Data.Lines.Text),
			})
			// only get the first match of each line
			break
		}
	}
	return matches, nil
}

type ripgrepMatch struct {
	Type string `json:"type"`
	Data struct {
		Path struct {
			Text string `json:"text"`
		} `json:"path"`
		Lines struct {
			Text string `json:"text"`
		} `json:"lines"`
		LineNumber int `json:"line_number"`
		Submatches []struct {
			Start int `json:"start"`
		} `json:"submatches"`
	} `json:"data"`
}

func searchFilesWithRegex(pattern, rootPath, include string) ([]grepMatch, error) {
	matches := []grepMatch{}

	// Use cached regex compilation
	regex, err := searchRegexCache.get(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid regex pattern: %w", err)
	}

	// Create walker with gitignore and crushignore support
	walker := fsext.NewFastGlobWalker(rootPath)

	err = filepath.Walk(rootPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors
		}

		if info.IsDir() {
			// Check if directory should be skipped
			if walker.ShouldSkip(path) {
				return filepath.SkipDir
			}
			return nil // Continue into directory
		}

		// Use walker's shouldSkip method for files
		if walker.ShouldSkip(path) {
			return nil
		}

		// Skip hidden files (starting with a dot) to match ripgrep's default behavior
		base := filepath.Base(path)
		if base != "." && strings.HasPrefix(base, ".") {
			return nil
		}

		// Use doublestar for glob matching (supports **, character classes, etc.).
		if include != "" {
			relPath, relErr := filepath.Rel(rootPath, path)
			if relErr != nil {
				return nil
			}
			relPath = filepath.ToSlash(relPath)
			// Ripgrep treats patterns without path separator as basename matches at any depth.
			// Mirror this by prepending **/ for patterns without /.
			includePattern := filepath.ToSlash(include)
			if !strings.Contains(includePattern, "/") {
				includePattern = "**/" + includePattern
			}
			matched, matchErr := doublestar.Match(includePattern, relPath)
			if matchErr != nil || !matched {
				return nil
			}
		}

		match, lineNum, charNum, lineText, err := fileContainsPattern(path, regex)
		if err != nil {
			return nil // Skip files we can't read
		}

		if match {
			matches = append(matches, grepMatch{
				path:     path,
				modTime:  info.ModTime(),
				lineNum:  lineNum,
				charNum:  charNum,
				lineText: lineText,
			})

			if len(matches) >= 200 {
				return filepath.SkipAll
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return matches, nil
}

func fileContainsPattern(filePath string, pattern *regexp.Regexp) (bool, int, int, string, error) {
	// Only search text files.
	if !isTextFile(filePath) {
		return false, 0, 0, "", nil
	}

	file, err := os.Open(filePath)
	if err != nil {
		return false, 0, 0, "", err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		if loc := pattern.FindStringIndex(line); loc != nil {
			charNum := loc[0] + 1
			return true, lineNum, charNum, line, nil
		}
	}

	return false, 0, 0, "", scanner.Err()
}

// isTextFile checks if a file is a text file by examining its MIME type.
func isTextFile(filePath string) bool {
	file, err := os.Open(filePath)
	if err != nil {
		return false
	}
	defer file.Close()

	// Read first 512 bytes for MIME type detection.
	buffer := make([]byte, 512)
	n, err := file.Read(buffer)
	if err != nil && err != io.EOF {
		return false
	}

	// Detect content type.
	contentType := http.DetectContentType(buffer[:n])

	// Check if it's a text MIME type.
	return strings.HasPrefix(contentType, "text/") ||
		contentType == "application/json" ||
		contentType == "application/xml" ||
		contentType == "application/javascript" ||
		contentType == "application/x-sh"
}
