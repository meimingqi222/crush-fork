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

// get retrieves a compiled regex from cache or compiles and caches it.
// Returns an error if the pattern is invalid (not stored in cache).
func (rc *regexCache) get(pattern string) (*regexp.Regexp, error) {
	// Try cache first
	if cached, ok := rc.Map.Get(pattern); ok {
		return cached, nil
	}
	// Compile and cache only on success
	regex, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	rc.Map.Set(pattern, regex)
	return regex, nil
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
	Pattern       string `json:"pattern" description:"The regex pattern to search for in file contents"`
	Path          string `json:"path,omitempty" description:"The directory to search in. Defaults to the current working directory."`
	Include       string `json:"include,omitempty" description:"File pattern to include in the search (e.g. \"*.js\", \"*.{ts,tsx}\")"`
	LiteralText   bool   `json:"literal_text,omitempty" description:"If true, the pattern will be treated as literal text with special regex characters escaped. Default is false."`
	ContextBefore int    `json:"context_before,omitempty" description:"Number of context lines to show before each match (0-5). Default is 0."`
	ContextAfter  int    `json:"context_after,omitempty" description:"Number of context lines to show after each match (0-5). Default is 0."`
}

type grepContextLine struct {
	lineNum  int
	lineText string
}

type grepMatch struct {
	path          string
	modTime       time.Time
	lineNum       int
	charNum       int
	lineText      string
	contextBefore []grepContextLine
	contextAfter  []grepContextLine
}

type GrepResponseMetadata struct {
	ToolPathMetadata
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
	GrepToolName             = "grep"
	maxGrepContentWidth      = 500
	defaultPerFileMatchLimit = 10
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
	return NewGrepToolWithArchiveDir(workingDir, config, "")
}

// NewGrepToolWithArchiveDir creates a grep tool that can also search
// archive:// URIs (tool-result archives written by read). When params.Path
// starts with archive://, the URI is resolved to the underlying archive file
// and searched as a single file; matches are reported under the archive URI so
// the model can follow up with read.
func NewGrepToolWithArchiveDir(workingDir string, config config.ToolGrep, archiveDir string) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		GrepToolName,
		string(grepDescription),
		func(ctx context.Context, params GrepParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Pattern == "" {
				return fantasy.NewTextErrorResponse("pattern is required"), nil
			}

			effectiveWorkingDir := EffectiveWorkingDir(ctx, workingDir)
			pathInput := cmp.Or(params.Path, ".")
			toolPath := ResolveToolPath(ctx, effectiveWorkingDir, pathInput)

			searchCtx, cancel := context.WithTimeout(ctx, config.GetTimeout())
			defer cancel()

			ctxBefore := params.ContextBefore
			if ctxBefore < 0 {
				ctxBefore = 0
			}
			if ctxBefore > 5 {
				ctxBefore = 5
			}
			ctxAfter := params.ContextAfter
			if ctxAfter < 0 {
				ctxAfter = 0
			}
			if ctxAfter > 5 {
				ctxAfter = 5
			}
			var result grepExecutionResult
			var err error
			archiveSearch := strings.HasPrefix(params.Path, "archive://")
			if archiveSearch {
				result, err = runArchiveGrepSearch(searchCtx, params, archiveDir, 100, ctxBefore, ctxAfter)
			} else {
				result, err = runGrepSearch(searchCtx, params, toolPath.AbsolutePath, 100, ctxBefore, ctxAfter)
			}
			archiveURI := pathInput
			if archiveSearch {
				// Strip any selector suffix (e.g. :raw or :100) so metadata and
				// follow-up read calls reference the clean archive URI.
				archiveURI = parsePathSelector(pathInput).filePath
			}
			if err != nil {
				// For archive searches the target is a tool-result archive, not
				// a filesystem path, so report the URI as-is (SmartJoin would
				// otherwise invent a bogus `workingDir/archive:/...` join).
				pathMeta := toolPath
				recovery := "Verify the search path exists. Use glob to list files before retrying."
				if archiveSearch {
					pathMeta = archiveURIToolPath(archiveURI, effectiveWorkingDir)
					recovery = "Verify the archive reference is correct and still present. Use the read tool to re-inspect the archived output."
				}
				return fantasy.WithResponseMetadata(
					fantasy.NewTextErrorResponse(fmt.Sprintf("error searching files: %v", err)),
					NewToolPathErrorMetadata(pathMeta, "search_failed", recovery),
				), nil
			}

			if archiveSearch {
				result.metadata.ToolPathMetadata = NewToolPathMetadata(archiveURIToolPath(archiveURI, effectiveWorkingDir))
			} else {
				result.metadata.ToolPathMetadata = NewToolPathMetadata(toolPath)
			}

			var output strings.Builder
			if len(result.matches) == 0 {
				output.WriteString("No files found")
			} else {
				fmt.Fprintf(&output, "Found %d matches\n", len(result.matches))

				lastFilePrinted := ""
				for _, match := range result.matches {
					if lastFilePrinted != match.path {
						if lastFilePrinted != "" {
							output.WriteString("\n")
						}
						lastFilePrinted = match.path
						fmt.Fprintf(&output, "%s:\n", FormatToolPath(match.path, effectiveWorkingDir))
					}
					lastPrintedLine := 0
					for _, ctx := range match.contextBefore {
						lineText := ctx.lineText
						if len(lineText) > maxGrepContentWidth {
							lineText = lineText[:maxGrepContentWidth] + "..."
						}
						fmt.Fprintf(&output, "  Line %d: %s\n", ctx.lineNum, lineText)
						lastPrintedLine = ctx.lineNum
					}
					if match.lineNum > 0 {
						if lastPrintedLine > 0 && match.lineNum > lastPrintedLine+1 {
							output.WriteString("  ...\n")
						}
						lineText := match.lineText
						if len(lineText) > maxGrepContentWidth {
							lineText = lineText[:maxGrepContentWidth] + "..."
						}
						if match.charNum > 0 {
							fmt.Fprintf(&output, "  Line %d, Char %d: %s\n", match.lineNum, match.charNum, lineText)
						} else {
							fmt.Fprintf(&output, "  Line %d: %s\n", match.lineNum, lineText)
						}
						lastPrintedLine = match.lineNum
					}
					for _, ctx := range match.contextAfter {
						if lastPrintedLine > 0 && ctx.lineNum > lastPrintedLine+1 {
							output.WriteString("  ...\n")
						}
						lineText := ctx.lineText
						if len(lineText) > maxGrepContentWidth {
							lineText = lineText[:maxGrepContentWidth] + "..."
						}
						fmt.Fprintf(&output, "  Line %d: %s\n", ctx.lineNum, lineText)
						lastPrintedLine = ctx.lineNum
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

func runGrepSearch(ctx context.Context, params GrepParams, rootPath string, limit int, contextBefore, contextAfter int) (grepExecutionResult, error) {
	metadata := GrepResponseMetadata{
		Pattern:     params.Pattern,
		LiteralText: params.LiteralText,
	}
	resolvedRootPath, err := validateGrepPath(rootPath)
	if err != nil {
		metadata.RecoveredBy = "path_validation"
		metadata.RecoveryAction = err.Error()
		metadata.FallbackTool = ReadToolName
		metadata.FallbackToolQuery = cmp.Or(params.Path, rootPath)
		metadata.RecoveredParameters = []string{"path"}
		return grepExecutionResult{metadata: metadata}, nil
	}

	searchPattern := params.Pattern
	if params.LiteralText {
		searchPattern = escapeRegexPattern(params.Pattern)
	}

	matches, err := searchWithRipgrep(ctx, searchPattern, resolvedRootPath, params.Include, contextBefore, contextAfter)
	if err != nil {
		matches, metadata, err = recoverGrepMatches(ctx, params, resolvedRootPath, metadata, contextBefore, contextAfter)
		if err != nil {
			return grepExecutionResult{}, err
		}
	}

	sort.Slice(matches, func(i, j int) bool {
		return matches[i].modTime.After(matches[j].modTime)
	})

	// Apply per-file match limit to ensure file diversity in results.
	if len(matches) > 0 {
		perFileCounts := make(map[string]int)
		limited := make([]grepMatch, 0, len(matches))
		for _, m := range matches {
			if perFileCounts[m.path] >= defaultPerFileMatchLimit {
				continue
			}
			perFileCounts[m.path]++
			limited = append(limited, m)
		}
		matches = limited
	}

	truncated := len(matches) > limit
	if truncated {
		matches = matches[:limit]
	}

	return grepExecutionResult{matches: matches, truncated: truncated, metadata: metadata}, nil
}

// archiveURIToolPath builds a ToolPath representing an archive:// URI rather
// than a filesystem path. It is used so observability/error metadata for
// archive searches reports the URI itself instead of a bogus
// `workingDir/archive://...` join produced by ResolveToolPath.
func archiveURIToolPath(uri, workingDir string) ToolPath {
	return ToolPath{
		InputPath:        uri,
		WorkingDir:       workingDir,
		AbsolutePath:     uri,
		DisplayPath:      uri,
		IsOutsideSession: false,
	}
}

// runArchiveGrepSearch searches a single tool-result archive (resolved from an
// archive:// URI) for a regex pattern. Matches are reported under the archive
// URI so the model can follow up with read for more context. Unlike
// runGrepSearch it never walks a directory, so include filters and ignore
// files do not apply.
func runArchiveGrepSearch(ctx context.Context, params GrepParams, archiveDir string, limit int, contextBefore, contextAfter int) (grepExecutionResult, error) {
	metadata := GrepResponseMetadata{
		Pattern:     params.Pattern,
		LiteralText: params.LiteralText,
	}
	if archiveDir == "" {
		return grepExecutionResult{}, fmt.Errorf("archive directory is not configured")
	}
	// Strip any selector suffix (e.g. :raw or :100) so grep can be pointed at
	// the same archive:// URI that read surfaces in its continuation hint.
	archiveURI := parsePathSelector(params.Path).filePath
	filePath, err := resolveArchiveFilePath(archiveURI, archiveDir)
	if err != nil {
		return grepExecutionResult{}, err
	}

	searchPattern := params.Pattern
	if params.LiteralText {
		searchPattern = escapeRegexPattern(params.Pattern)
	}
	regex, err := searchRegexCache.get(searchPattern)
	if err != nil {
		return grepExecutionResult{}, fmt.Errorf("invalid regex pattern: %w", err)
	}

	fileMatches, err := fileContainsPattern(filePath, regex, contextBefore, contextAfter)
	if err != nil {
		return grepExecutionResult{}, err
	}

	// Report matches under the archive URI so the model can follow up with
	// read for more context.
	for i := range fileMatches {
		fileMatches[i].path = archiveURI
	}

	truncated := len(fileMatches) > limit
	if truncated {
		fileMatches = fileMatches[:limit]
	}

	return grepExecutionResult{matches: fileMatches, truncated: truncated, metadata: metadata}, nil
}

func validateGrepPath(rootPath string) (string, error) {
	resolved := filepath.Clean(rootPath)
	info, err := os.Stat(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("Search path does not exist: %s. Verify the path first or use glob/read to inspect nearby files.", resolved)
		}
		return "", fmt.Errorf("error accessing search path: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("Search path is not a directory: %s. Use read for files or provide a directory path.", resolved)
	}
	return resolved, nil
}

func recoverGrepMatches(ctx context.Context, params GrepParams, rootPath string, metadata GrepResponseMetadata, contextBefore, contextAfter int) ([]grepMatch, GrepResponseMetadata, error) {
	matches, regexErr := searchFilesWithRegex(params.Pattern, rootPath, params.Include, contextBefore, contextAfter)
	if regexErr == nil {
		return matches, metadata, nil
	}
	if params.LiteralText || !looksLikeRegexSyntaxError(regexErr) {
		return nil, metadata, regexErr
	}

	literalPattern := escapeRegexPattern(params.Pattern)
	matches, err := searchWithRipgrep(ctx, literalPattern, rootPath, params.Include, contextBefore, contextAfter)
	if err != nil {
		matches, err = searchFilesWithRegex(literalPattern, rootPath, params.Include, contextBefore, contextAfter)
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
	return strings.Contains(msg, "invalid regex pattern") ||
		strings.Contains(msg, "error parsing regexp") ||
		strings.Contains(msg, "missing closing") ||
		strings.Contains(msg, "unexpected") ||
		strings.Contains(msg, "unclosed character class") ||
		strings.Contains(msg, "regex parse error")
}

func searchWithRipgrep(ctx context.Context, pattern, rootPath, include string, contextBefore, contextAfter int) ([]grepMatch, error) {
	// Convert rootPath to absolute so cmd.Dir and ripgrep's search
	// target always resolve correctly, regardless of process CWD.
	absRoot, err := filepath.Abs(rootPath)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve search path: %w", err)
	}
	rootPathIsAbs := filepath.IsAbs(rootPath)

	cmd := getRgSearchCmd(ctx, pattern, absRoot, include, contextBefore, contextAfter)
	if cmd == nil {
		return nil, fmt.Errorf("ripgrep not found in $PATH")
	}

	// Set working directory so ripgrep discovers .gitignore and
	// .crushignore files correctly, matching the Glob tool's behavior.
	cmd.Dir = absRoot

	// Pass ignore files relative to cmd.Dir. Both .gitignore and
	// .crushignore need explicit --ignore-file because ripgrep only
	// auto-discovers VCS ignore files inside a git repository.
	for _, ignoreFile := range []string{".gitignore", ".crushignore"} {
		ignorePath := filepath.Join(absRoot, ignoreFile)
		if _, err := os.Stat(ignorePath); err == nil {
			cmd.Args = append(cmd.Args, "--ignore-file", ignoreFile)
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
	type pendingMatch struct {
		storePath string
		modTime   time.Time
		lineNum   int
		charNum   int
		lineText  string
		before    []grepContextLine
		after     []grepContextLine
	}
	var pending *pendingMatch
	currentFilePath := ""

	flushPending := func() {
		if pending == nil {
			return
		}
		matches = append(matches, grepMatch{
			path:          pending.storePath,
			modTime:       pending.modTime,
			lineNum:       pending.lineNum,
			charNum:       pending.charNum,
			lineText:      pending.lineText,
			contextBefore: pending.before,
			contextAfter:  pending.after,
		})
		pending = nil
	}

	for line := range bytes.SplitSeq(bytes.TrimSpace(output), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var raw ripgrepRawLine
		if err := json.Unmarshal(line, &raw); err != nil {
			continue
		}
		switch raw.Type {
		case "match":
			flushPending()
			if len(raw.Data.Submatches) == 0 {
				continue
			}
			rawPath := raw.Data.Path.Text
			absPath := rawPath
			if !filepath.IsAbs(absPath) {
				absPath = filepath.Join(absRoot, rawPath)
			}
			fi, err := os.Stat(absPath)
			if err != nil {
				continue
			}
			storePath := absPath
			if !rootPathIsAbs {
				if rel, relErr := filepath.Rel(absRoot, absPath); relErr == nil {
					storePath = filepath.Join(filepath.Clean(rootPath), rel)
				}
			}
			pending = &pendingMatch{
				storePath: storePath,
				modTime:   fi.ModTime(),
				lineNum:   raw.Data.LineNumber,
				charNum:   raw.Data.Submatches[0].Start + 1,
				lineText:  strings.TrimSpace(raw.Data.Lines.Text),
			}
			currentFilePath = rawPath
		case "context":
			if pending == nil {
				break
			}
			// If the context line's path differs from the current match,
			// it belongs to a different file; flush and skip.
			if raw.Data.Path.Text != currentFilePath {
				flushPending()
				break
			}
			ctxLine := grepContextLine{
				lineNum:  raw.Data.LineNumber,
				lineText: strings.TrimSpace(raw.Data.Lines.Text),
			}
			if pending != nil && ctxLine.lineNum < pending.lineNum {
				pending.before = append(pending.before, ctxLine)
			} else if pending != nil {
				pending.after = append(pending.after, ctxLine)
			}
		case "end":
			flushPending()
		}
	}
	flushPending()
	return matches, nil
}

type ripgrepRawLine struct {
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

func searchFilesWithRegex(pattern, rootPath, include string, contextBefore, contextAfter int) ([]grepMatch, error) {
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
			if walker.ShouldSkip(path) {
				return filepath.SkipDir
			}
			return nil
		}

		if walker.ShouldSkip(path) {
			return nil
		}

		base := filepath.Base(path)
		if base != "." && strings.HasPrefix(base, ".") {
			return nil
		}

		if include != "" {
			relPath, relErr := filepath.Rel(rootPath, path)
			if relErr != nil {
				return nil
			}
			relPath = filepath.ToSlash(relPath)
			includePattern := filepath.ToSlash(include)
			if !strings.Contains(includePattern, "/") {
				includePattern = "**/" + includePattern
			}
			matched, matchErr := doublestar.Match(includePattern, relPath)
			if matchErr != nil || !matched {
				return nil
			}
		}

		fileMatches, err := fileContainsPattern(path, regex, contextBefore, contextAfter)
		if err != nil {
			return nil
		}

		if len(fileMatches) > 0 {
			matches = append(matches, fileMatches...)
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

func fileContainsPattern(filePath string, pattern *regexp.Regexp, contextBefore, contextAfter int) ([]grepMatch, error) {
	if !isTextFile(filePath) {
		return nil, nil
	}

	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var allLines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		allLines = append(allLines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	info, err := os.Stat(filePath)
	if err != nil {
		return nil, err
	}

	// First pass: find all match positions.
	type matchPos struct {
		lineNum int
		charNum int
	}
	var matchPositions []matchPos
	for i, line := range allLines {
		if loc := pattern.FindStringIndex(line); loc != nil {
			matchPositions = append(matchPositions, matchPos{
				lineNum: i + 1,
				charNum: loc[0] + 1,
			})
		}
	}
	if len(matchPositions) == 0 {
		return nil, nil
	}

	// Second pass: build grepMatch entries with context.
	var matches []grepMatch
	lastContextEnd := 0
	for _, mp := range matchPositions {
		idx := mp.lineNum - 1

		// Compute context-before range.
		beforeStart := idx - contextBefore
		if beforeStart < 0 {
			beforeStart = 0
		}
		if beforeStart < lastContextEnd {
			beforeStart = lastContextEnd
		}

		var before []grepContextLine
		for j := beforeStart; j < idx; j++ {
			before = append(before, grepContextLine{
				lineNum:  j + 1,
				lineText: strings.TrimSpace(allLines[j]),
			})
		}

		// Compute context-after range.
		afterEnd := idx + contextAfter
		if afterEnd >= len(allLines) {
			afterEnd = len(allLines) - 1
		}

		var after []grepContextLine
		for j := idx + 1; j <= afterEnd; j++ {
			after = append(after, grepContextLine{
				lineNum:  j + 1,
				lineText: strings.TrimSpace(allLines[j]),
			})
		}

		matches = append(matches, grepMatch{
			path:          filePath,
			modTime:       info.ModTime(),
			lineNum:       mp.lineNum,
			charNum:       mp.charNum,
			lineText:      strings.TrimSpace(allLines[idx]),
			contextBefore: before,
			contextAfter:  after,
		})
		lastContextEnd = afterEnd + 1
	}

	return matches, nil
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
