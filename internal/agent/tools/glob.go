package tools

import (
	"bytes"
	"cmp"
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/fsext"
)

const GlobToolName = "glob"

//go:embed glob.md
var globDescription []byte

const (
	globDefaultLimit = 200
	globMaxLimit     = 200
)

type GlobParams struct {
	Path      string `json:"path,omitempty" description:"Glob, file, or directory to search. A single path or a semicolon-delimited list (e.g. \"src/**/*.ts; test/**/*.ts\"). Defaults to the current working directory."`
	Hidden    *bool  `json:"hidden,omitempty" description:"If true, hidden files and directories are included. Default is true."`
	Gitignore *bool  `json:"gitignore,omitempty" description:"If true, .gitignore and .crushignore rules are respected. Default is true."`
	Limit     *int   `json:"limit,omitempty" description:"Maximum number of results to return. Default is 200, maximum is 200."`
}

type GlobResponseMetadata struct {
	ToolPathMetadata
	NumberOfFiles int  `json:"number_of_files"`
	Truncated     bool `json:"truncated"`
}

func NewGlobTool(workingDir string) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		GlobToolName,
		string(globDescription),
		func(ctx context.Context, params GlobParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			pathInput := strings.TrimSpace(params.Path)
			if pathInput == "" {
				pathInput = "."
			}

			effectiveWorkingDir := cmp.Or(GetWorkingDirFromContext(ctx), workingDir)

			hidden := true
			if params.Hidden != nil {
				hidden = *params.Hidden
			}

			gitignore := true
			if params.Gitignore != nil {
				gitignore = *params.Gitignore
			}

			limit := globDefaultLimit
			if params.Limit != nil {
				limit = *params.Limit
			}
			if limit <= 0 {
				limit = globDefaultLimit
			}
			if limit > globMaxLimit {
				limit = globMaxLimit
			}

			paths := splitGlobPathInput(pathInput)
			if len(paths) == 0 {
				paths = []string{"."}
			}

			var allFiles []string
			seen := make(map[string]struct{})
			truncated := false
			var skippedPaths []string

			for _, p := range paths {
				files, tr, err := globForPath(ctx, p, effectiveWorkingDir, limit, hidden, gitignore)
				if err != nil {
					slog.Warn("Glob search skipped missing path", "error", err, "path", p)
					skippedPaths = append(skippedPaths, p)
					continue
				}
				if tr {
					truncated = true
				}
				for _, f := range files {
					if _, ok := seen[f]; ok {
						continue
					}
					seen[f] = struct{}{}
					allFiles = append(allFiles, f)
					if len(allFiles) >= limit {
						allFiles = allFiles[:limit]
						truncated = true
						break
					}
				}
				if len(allFiles) >= limit {
					break
				}
			}

			// If every path failed, return an error so the agent knows nothing
			// was found and can adjust.
			if len(allFiles) == 0 && len(skippedPaths) == len(paths) {
				pathMeta := ResolveToolPath(ctx, workingDir, skippedPaths[0])
				detail := skippedPaths[0]
				if len(skippedPaths) > 1 {
					detail = fmt.Sprintf("%d paths (%s)", len(skippedPaths), strings.Join(skippedPaths, "; "))
				}
				return fantasy.WithResponseMetadata(
					fantasy.NewTextErrorResponse(fmt.Sprintf("Glob search failed: path not found: %s", detail)),
					NewToolPathErrorMetadata(pathMeta, "path_not_found",
						"Verify the base directory exists. Use a broader path or list the parent directory with read before retrying."),
				), nil
			}

			var output string
			if len(allFiles) == 0 {
				output = "No files found"
			} else {
				normalizeFilePaths(allFiles)
				output = strings.Join(allFiles, "\n")
				if truncated {
					output += "\n\n(Results are truncated. Consider using a more specific path or pattern.)"
				}
			}
			if len(skippedPaths) > 0 {
				output += "\n\nSkipped missing paths: " + strings.Join(skippedPaths, "; ")
			}

			return fantasy.WithResponseMetadata(
				fantasy.NewTextResponse(output),
				GlobResponseMetadata{
					ToolPathMetadata: NewToolPathMetadata(ResolveToolPath(ctx, workingDir, pathInput)),
					NumberOfFiles:    len(allFiles),
					Truncated:        truncated,
				},
			), nil
		})
}

// splitGlobPathInput splits a semicolon-delimited path list while respecting
// brace groups, matching oh-my-pi's glob tool behavior.
func splitGlobPathInput(input string) []string {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil
	}

	var parts []string
	var current strings.Builder
	braceDepth := 0
	escaped := false

	for _, ch := range input {
		if escaped {
			current.WriteRune(ch)
			escaped = false
			continue
		}
		if ch == '\\' {
			current.WriteRune(ch)
			escaped = true
			continue
		}
		if ch == '{' {
			braceDepth++
			current.WriteRune(ch)
			continue
		}
		if ch == '}' {
			if braceDepth > 0 {
				braceDepth--
			}
			current.WriteRune(ch)
			continue
		}
		if braceDepth == 0 && ch == ';' {
			part := strings.TrimSpace(current.String())
			if part != "" {
				parts = append(parts, part)
			}
			current.Reset()
			continue
		}
		current.WriteRune(ch)
	}

	if current.Len() > 0 {
		part := strings.TrimSpace(current.String())
		if part != "" {
			parts = append(parts, part)
		}
	}

	return parts
}

// globForPath resolves a single path entry into matching files.
func globForPath(ctx context.Context, pathInput, workingDir string, limit int, hidden, gitignore bool) ([]string, bool, error) {
	pathInput = strings.ReplaceAll(pathInput, "\\", "/")
	basePath, globPattern, hasGlob := parseFindPattern(pathInput)

	searchPath := filepath.FromSlash(basePath)
	if !filepath.IsAbs(searchPath) {
		searchPath = filepath.Join(workingDir, searchPath)
	}
	searchPath = filepath.Clean(searchPath)

	info, err := os.Stat(searchPath)
	if err != nil {
		return nil, false, fmt.Errorf("path not found: %s", FormatToolPath(searchPath, workingDir))
	}

	if !hasGlob && info.IsDir() {
		globPattern = "**/*"
		hasGlob = true
	}

	if !hasGlob {
		// Exact file path.
		if info.IsDir() {
			// Should not happen because directories are converted to **/* above.
			globPattern = "**/*"
			hasGlob = true
		} else {
			rel, err := filepath.Rel(workingDir, searchPath)
			if err != nil {
				rel = searchPath
			}
			return []string{rel}, false, nil
		}
	}

	globPattern = fixUnclosedBraces(globPattern)

	files, truncated, err := globFiles(ctx, globPattern, searchPath, limit, hidden, gitignore)
	if err != nil {
		return nil, false, err
	}

	// Convert absolute results to paths relative to the working directory.
	relFiles := make([]string, 0, len(files))
	for _, f := range files {
		rel, err := filepath.Rel(workingDir, f)
		if err != nil {
			rel = f
		}
		relFiles = append(relFiles, rel)
	}

	return relFiles, truncated, nil
}

// parseFindPattern splits a path into a base directory and a glob pattern,
// matching oh-my-pi's semantics.
func parseFindPattern(pattern string) (basePath, globPattern string, hasGlob bool) {
	pattern = strings.ReplaceAll(pattern, "\\", "/")
	segments := strings.Split(pattern, "/")

	firstGlobIndex := -1
	for i, seg := range segments {
		if hasGlobPathChars(seg) {
			firstGlobIndex = i
			break
		}
	}

	if firstGlobIndex == -1 {
		return pattern, "**/*", false
	}

	if firstGlobIndex == 0 {
		if strings.HasPrefix(pattern, "**/") {
			return ".", pattern, true
		}
		return ".", "**/" + pattern, true
	}

	basePath = strings.Join(segments[:firstGlobIndex], "/")
	globPattern = strings.Join(segments[firstGlobIndex:], "/")
	return basePath, globPattern, true
}

// hasGlobPathChars reports whether s contains glob metacharacters.
func hasGlobPathChars(s string) bool {
	return strings.ContainsAny(s, "*?[{|")
}

// fixUnclosedBraces appends missing closing braces to keep the glob valid.
func fixUnclosedBraces(pattern string) string {
	opens := strings.Count(pattern, "{")
	closes := strings.Count(pattern, "}")
	if opens > closes {
		pattern += strings.Repeat("}", opens-closes)
	}
	return pattern
}

func globFiles(ctx context.Context, pattern, searchPath string, limit int, hidden, gitignore bool) ([]string, bool, error) {
	cmdRg := getRgCmd(ctx, pattern, hidden, gitignore)
	if cmdRg != nil {
		cmdRg.Dir = searchPath
		matches, err := runRipgrep(cmdRg, pattern, searchPath, limit, hidden, gitignore)
		if err == nil {
			return matches, len(matches) >= limit && limit > 0, nil
		}
		slog.Warn("Ripgrep execution failed, falling back to doublestar", "error", err)
	}

	return fsext.GlobWithOptions(pattern, searchPath, limit, gitignore, hidden)
}

func runRipgrep(cmd *exec.Cmd, pattern, searchRoot string, limit int, hidden, gitignore bool) ([]string, error) {
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("ripgrep: %w\n%s", err, out)
	}

	var walker *fsext.FastGlobWalker
	if gitignore {
		walker = fsext.NewFastGlobWalker(searchRoot)
	}

	type fileWithModTime struct {
		path    string
		modTime int64
	}
	var matches []fileWithModTime
	for p := range bytes.SplitSeq(out, []byte{0}) {
		if len(p) == 0 {
			continue
		}
		absPath := string(p)
		if !filepath.IsAbs(absPath) {
			absPath = filepath.Join(searchRoot, absPath)
		}
		relPath, relErr := filepath.Rel(searchRoot, absPath)
		if relErr != nil {
			relPath = absPath
		}
		if !fsext.MatchesGlob(pattern, relPath) || fsext.SkipCommonIgnored(absPath) {
			continue
		}
		if !hidden && fsext.IsHidden(absPath) {
			continue
		}
		if gitignore && walker.ShouldSkip(absPath) {
			continue
		}
		info, statErr := os.Stat(absPath)
		if statErr != nil {
			continue
		}
		matches = append(matches, fileWithModTime{path: absPath, modTime: info.ModTime().Unix()})
	}

	// Sort by modification time (newest first) for consistency with fallback implementation.
	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].modTime > matches[j].modTime
	})

	if limit > 0 && len(matches) > limit {
		matches = matches[:limit]
	}

	result := make([]string, len(matches))
	for i, m := range matches {
		result[i] = m.path
	}
	return result, nil
}

func normalizeFilePaths(paths []string) {
	for i, p := range paths {
		paths[i] = filepath.ToSlash(p)
	}
}
