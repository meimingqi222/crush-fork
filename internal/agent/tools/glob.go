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

type GlobParams struct {
	Pattern  string   `json:"pattern,omitempty" description:"A single glob pattern to match files against. Use patterns for multiple patterns."`
	Patterns []string `json:"patterns,omitempty" description:"Multiple glob patterns to search for in a single call (e.g. [\"**/*.ts\", \"**/*.tsx\"]). Preferred over multiple separate calls."`
	Path     string   `json:"path,omitempty" description:"The directory to search in. Defaults to the current working directory."`
}

type GlobResponseMetadata struct {
	NumberOfFiles int  `json:"number_of_files"`
	Truncated     bool `json:"truncated"`
}

func NewGlobTool(workingDir string) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		GlobToolName,
		string(globDescription),
		func(ctx context.Context, params GlobParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			// Build effective pattern list from both Pattern and Patterns fields.
			var effectivePatterns []string
			if params.Pattern != "" {
				effectivePatterns = append(effectivePatterns, params.Pattern)
			}
			effectivePatterns = append(effectivePatterns, params.Patterns...)
			if len(effectivePatterns) == 0 {
				return fantasy.NewTextErrorResponse("pattern or patterns is required"), nil
			}

			// Use session-specific working directory from context if available.
			effectiveWorkingDir := cmp.Or(GetWorkingDirFromContext(ctx), workingDir)
			searchPath := filepath.FromSlash(cmp.Or(params.Path, effectiveWorkingDir))

			var allFiles []string
			truncated := false
			perPatternLimit := 100
			for _, pat := range effectivePatterns {
				files, patTruncated, err := globFiles(ctx, pat, searchPath, perPatternLimit)
				if err != nil {
					slog.Warn("Glob search failed", "error", err, "pattern", pat, "path", searchPath)
					continue
				}
				allFiles = append(allFiles, files...)
				if patTruncated {
					truncated = true
				}
			}

			// Deduplicate files while preserving order.
			seen := make(map[string]struct{})
			unique := make([]string, 0, len(allFiles))
			for _, f := range allFiles {
				if _, ok := seen[f]; !ok {
					seen[f] = struct{}{}
					unique = append(unique, f)
				}
			}

			var output string
			if len(unique) == 0 {
				output = "No files found"
			} else {
				normalizeFilePaths(unique)
				output = strings.Join(unique, "\n")
				if truncated {
					output += "\n\n(Results are truncated. Consider using a more specific path or pattern.)"
				}
			}

			return fantasy.WithResponseMetadata(
				fantasy.NewTextResponse(output),
				GlobResponseMetadata{
					NumberOfFiles: len(unique),
					Truncated:     truncated,
				},
			), nil
		})
}

func globFiles(ctx context.Context, pattern, searchPath string, limit int) ([]string, bool, error) {
	cmdRg := getRgCmd(ctx, pattern)
	if cmdRg != nil {
		cmdRg.Dir = searchPath
		matches, err := runRipgrep(cmdRg, pattern, searchPath, limit)
		if err == nil {
			return matches, len(matches) >= limit && limit > 0, nil
		}
		slog.Warn("Ripgrep execution failed, falling back to doublestar", "error", err)
	}

	return fsext.GlobGitignoreAware(pattern, searchPath, limit)
}

func runRipgrep(cmd *exec.Cmd, pattern, searchRoot string, limit int) ([]string, error) {
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("ripgrep: %w\n%s", err, out)
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
