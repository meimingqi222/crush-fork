package tools

import (
	"bufio"
	"cmp"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/filepathext"
	"github.com/charmbracelet/crush/internal/filetracker"
	"github.com/charmbracelet/crush/internal/imageutil"
	"github.com/charmbracelet/crush/internal/lsp"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/skills"
)

//go:embed view.md
var viewDescription []byte

type ViewParams struct {
	FilePath           string   `json:"file_path" description:"The path to the file to read"`
	Offset             int      `json:"offset,omitempty" description:"The line number to start reading from (0-based)"`
	Limit              int      `json:"limit,omitempty" description:"The number of lines to read (defaults to 2000)"`
	Hashline           bool     `json:"hashline,omitempty" description:"If true, include hashline anchors in the output for line-addressable editing"`
	WaitForDiagnostics *bool    `json:"wait_for_diagnostics,omitempty" description:"If true, wait for LSP diagnostics (default: true)"`
	Ignore             []string `json:"ignore,omitempty" description:"List of glob patterns to ignore (when file_path is a directory)"`
	Depth              int      `json:"depth,omitempty" description:"The maximum directory depth to traverse (when file_path is a directory)"`
}

type ViewPermissionsParams struct {
	FilePath           string   `json:"file_path"`
	Offset             int      `json:"offset"`
	Limit              int      `json:"limit"`
	Hashline           bool     `json:"hashline,omitempty"`
	WaitForDiagnostics *bool    `json:"wait_for_diagnostics,omitempty"`
	Ignore             []string `json:"ignore,omitempty"`
	Depth              int      `json:"depth,omitempty"`
}

type ViewResourceType string

const (
	ViewResourceUnset ViewResourceType = ""
	ViewResourceSkill ViewResourceType = "skill"
)

type ViewResponseMetadata struct {
	FilePath            string           `json:"file_path"`
	Content             string           `json:"content"`
	Hashline            bool             `json:"hashline,omitempty"`
	ResourceType        ViewResourceType `json:"resource_type,omitempty"`
	ResourceName        string           `json:"resource_name,omitempty"`
	ResourceDescription string           `json:"resource_description,omitempty"`
	RecoveredBy         string           `json:"recovered_by,omitempty"`
	RecoveryAction      string           `json:"recovery_action,omitempty"`
	FallbackTool        string           `json:"fallback_tool,omitempty"`
	FallbackToolQuery   string           `json:"fallback_tool_query,omitempty"`
	IsDirectory         bool             `json:"is_directory,omitempty"`
}

const (
	ReadToolName     = "read"
	ViewToolName     = "view"
	LSToolName       = "ls"
	MaxViewSize      = 1 * 1024 * 1024 // 1MB
	DefaultReadLimit = 2000
	MaxLineLength    = 2000
)

type textViewLine struct {
	Raw     string
	Display string
}

var errViewOffsetBeyondEOF = errors.New("offset is beyond end of file")

// NewViewTool is an alias for NewReadTool for backward compatibility.
func NewViewTool(
	lspManager *lsp.Manager,
	permissions permission.Service,
	filetracker filetracker.Service,
	workingDir string,
	lsConfig config.ToolLs,
	skillsPaths ...string,
) fantasy.AgentTool {
	return NewReadTool(lspManager, permissions, filetracker, workingDir, lsConfig, skillsPaths...)
}

func NewReadTool(
	lspManager *lsp.Manager,
	permissions permission.Service,
	filetracker filetracker.Service,
	workingDir string,
	lsConfig config.ToolLs,
	skillsPaths ...string,
) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		ViewToolName,
		string(viewDescription),
		func(ctx context.Context, params ViewParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.FilePath == "" {
				return fantasy.NewTextErrorResponse("file_path is required"), nil
			}

			// Use session-specific working directory from context if available.
			effectiveWorkingDir := cmp.Or(GetWorkingDirFromContext(ctx), workingDir)

			// Handle relative paths
			filePath := filepathext.SmartJoin(effectiveWorkingDir, params.FilePath)

			// Check if file is outside working directory and request permission if needed
			absWorkingDir, err := filepath.Abs(effectiveWorkingDir)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("error resolving working directory: %w", err)
			}

			absFilePath, err := filepath.Abs(filePath)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("error resolving file path: %w", err)
			}

			relPath, err := filepath.Rel(absWorkingDir, absFilePath)
			isOutsideWorkDir := err != nil || strings.HasPrefix(relPath, "..")
			isSkillFile := isInSkillsPath(absFilePath, skillsPaths)

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for accessing files outside working directory")
			}

			// Request permission for files outside working directory, unless it's a skill file.
			if isOutsideWorkDir && !isSkillFile {
				permissionResponse, permReqErr := RequestPermission(ctx, permissions,
					permission.CreatePermissionRequest{
						SessionID:   sessionID,
						Path:        absFilePath,
						ToolCallID:  call.ID,
						ToolName:    ViewToolName,
						Action:      "read",
						Description: fmt.Sprintf("Read file outside working directory: %s", absFilePath),
						Params:      ViewPermissionsParams(params),
					},
				)
				if permReqErr != nil {
					return fantasy.ToolResponse{}, permReqErr
				}
				if permissionResponse != nil {
					return *permissionResponse, nil
				}
			}

			// Check if file exists
			fileInfo, err := os.Stat(filePath)
			if err != nil {
				if os.IsNotExist(err) {
					suggestions := findViewSuggestions(filePath)
					message := fmt.Sprintf("File not found: %s", filePath)
					if len(suggestions) > 0 {
						message += fmt.Sprintf("\n\nDid you mean one of these?\n%s", strings.Join(suggestions, "\n"))
					}
					return fantasy.WithResponseMetadata(
						fantasy.NewTextErrorResponse(message),
						newMissingViewMetadata(filePath, suggestions),
					), nil
				}
				return fantasy.NewTextErrorResponse(fmt.Sprintf("error accessing file: %v", err)), nil
			}

			// Check if it's a directory — automatically list its contents.
			if fileInfo.IsDir() {
				lsParams := LSParams{Ignore: params.Ignore, Depth: params.Depth}
				out, _, lsErr := ListDirectoryTree(filePath, lsParams, lsConfig)
				if lsErr != nil {
					return fantasy.NewTextErrorResponse(lsErr.Error()), nil
				}
				return fantasy.WithResponseMetadata(
					fantasy.NewTextResponse(out),
					ViewResponseMetadata{FilePath: filePath, Content: out, IsDirectory: true},
				), nil
			}

			isSupportedImage, mimeType := getImageMimeType(filePath)

			// Set default limit if not provided (no limit for SKILL.md files)
			if params.Limit <= 0 {
				if isSkillFile {
					params.Limit = 1000000 // Effectively no limit for skill files
				} else {
					params.Limit = DefaultReadLimit
				}
			}

			if isSupportedImage {
				if !GetSupportsImagesFromContext(ctx) {
					modelName := GetModelNameFromContext(ctx)
					return fantasy.NewTextErrorResponse(fmt.Sprintf("This model (%s) does not support image data.", modelName)), nil
				}

				imageData, readErr := os.ReadFile(filePath)
				if readErr != nil {
					return fantasy.ToolResponse{}, fmt.Errorf("error reading image file: %w", readErr)
				}

				// Compress image if it exceeds 1MB.
				config := imageutil.DefaultCompressionConfig()
				result, compressErr := imageutil.CompressImage(imageData, mimeType, config)
				if compressErr != nil {
					slog.Warn("Failed to compress image, using original", "error", compressErr, "path", filePath)
					// Fall through with original data.
					result = &imageutil.CompressResult{
						Data:          imageData,
						MimeType:      mimeType,
						WasCompressed: false,
					}
				}

				return fantasy.NewImageResponse(result.Data, result.MimeType), nil
			}

			// Read the file content.
			lines, hasMore, err := readTextFileLines(filePath, params.Offset, params.Limit)
			if err != nil {
				if errors.Is(err, errViewOffsetBeyondEOF) {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("Offset %d is beyond end of file", params.Offset)), nil
				}
				return fantasy.ToolResponse{}, fmt.Errorf("error reading file: %w", err)
			}
			content := joinDisplayLines(lines)
			if !utf8.ValidString(content) {
				return fantasy.NewTextErrorResponse("File content is not valid UTF-8"), nil
			}

			openInLSPs(ctx, lspManager, filePath)
			if shouldWaitForDiagnostics(params.WaitForDiagnostics) {
				waitForLSPDiagnostics(ctx, lspManager, filePath, 300*time.Millisecond)
			}
			output := "<file>\n"
			if params.Hashline {
				output += addHashlineLineNumbers(lines, params.Offset+1)
			} else {
				output += addLineNumbers(lines, params.Offset+1)
			}

			if hasMore {
				output += fmt.Sprintf("\n\n(File has more lines. Use 'offset' parameter to read beyond line %d)",
					params.Offset+len(strings.Split(content, "\n")))
			}
			output += "\n</file>\n"
			output += getDiagnostics(filePath, lspManager)
			filetracker.RecordRead(ctx, sessionID, filePath)

			meta := ViewResponseMetadata{
				FilePath: filePath,
				Content:  content,
				Hashline: params.Hashline,
			}
			if isSkillFile {
				if skill, err := skills.Parse(filePath); err == nil {
					meta.ResourceType = ViewResourceSkill
					meta.ResourceName = skill.Name
					meta.ResourceDescription = skill.Description
				}
			}

			return fantasy.WithResponseMetadata(
				fantasy.NewTextResponse(output),
				meta,
			), nil
		})
}

func newMissingViewMetadata(filePath string, suggestions []string) ViewResponseMetadata {
	metadata := ViewResponseMetadata{FilePath: filePath}
	if len(suggestions) == 0 {
		return metadata
	}
	metadata.RecoveredBy = "file_not_found_suggestions"
	metadata.RecoveryAction = fmt.Sprintf("File %q was not found. Try one of the suggested paths from the error message.", filePath)
	return metadata
}

func findViewSuggestions(filePath string) []string {
	dir := filepath.Dir(filePath)
	base := filepath.Base(filePath)
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var suggestions []string
	for _, entry := range dirEntries {
		if strings.Contains(strings.ToLower(entry.Name()), strings.ToLower(base)) ||
			strings.Contains(strings.ToLower(base), strings.ToLower(entry.Name())) {
			suggestions = append(suggestions, filepath.Join(dir, entry.Name()))
			if len(suggestions) >= 3 {
				break
			}
		}
	}
	return suggestions
}

func shouldWaitForDiagnostics(wait *bool) bool {
	if wait == nil {
		return true
	}
	return *wait
}

func addLineNumbers(lines []textViewLine, startLine int) string {
	if len(lines) == 0 {
		return ""
	}

	var result []string
	for i, line := range lines {
		lineNum := i + startLine
		numStr := fmt.Sprintf("%d", lineNum)

		if len(numStr) >= 6 {
			result = append(result, fmt.Sprintf("%s|%s", numStr, line.Display))
		} else {
			paddedNum := fmt.Sprintf("%6s", numStr)
			result = append(result, fmt.Sprintf("%s|%s", paddedNum, line.Display))
		}
	}

	return strings.Join(result, "\n")
}

func addHashlineLineNumbers(lines []textViewLine, startLine int) string {
	if len(lines) == 0 {
		return ""
	}

	result := make([]string, 0, len(lines))
	for i, line := range lines {
		lineNum := i + startLine
		result = append(result, fmt.Sprintf("%6s#%s|%s", fmt.Sprintf("%d", lineNum), computeHashlineID(lineNum, line.Raw), line.Display))
	}

	return strings.Join(result, "\n")
}

func joinDisplayLines(lines []textViewLine) string {
	if len(lines) == 0 {
		return ""
	}

	displayLines := make([]string, 0, len(lines))
	for _, line := range lines {
		displayLines = append(displayLines, line.Display)
	}

	return strings.Join(displayLines, "\n")
}

func readTextFile(filePath string, offset, limit int) (string, bool, error) {
	lines, hasMore, err := readTextFileLines(filePath, offset, limit)
	if err != nil {
		return "", false, err
	}

	return joinDisplayLines(lines), hasMore, nil
}

func readTextFileLines(filePath string, offset, limit int) ([]textViewLine, bool, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()

	scanner := NewLineScanner(file)
	skipped := 0
	if offset > 0 {
		for skipped < offset && scanner.Scan() {
			skipped++
		}
		if err = scanner.Err(); err != nil {
			return nil, false, err
		}
		if skipped < offset {
			return nil, false, errViewOffsetBeyondEOF
		}
	}

	// Pre-allocate slice with expected capacity.
	lines := make([]textViewLine, 0, limit)

	for len(lines) < limit && scanner.Scan() {
		rawLine := scanner.Text()
		displayLine := rawLine
		if len(displayLine) > MaxLineLength {
			displayLine = displayLine[:MaxLineLength] + "..."
		}
		lines = append(lines, textViewLine{
			Raw:     rawLine,
			Display: displayLine,
		})
	}

	// Peek one more line only when we filled the limit.
	hasMore := len(lines) == limit && scanner.Scan()

	if err := scanner.Err(); err != nil {
		return nil, false, err
	}

	return lines, hasMore, nil
}

func getImageMimeType(filePath string) (bool, string) {
	ext := strings.ToLower(filepath.Ext(filePath))
	switch ext {
	case ".jpg", ".jpeg":
		return true, "image/jpeg"
	case ".png":
		return true, "image/png"
	case ".gif":
		return true, "image/gif"
	case ".webp":
		return true, "image/webp"
	default:
		return false, ""
	}
}

type LineScanner struct {
	scanner *bufio.Scanner
}

func NewLineScanner(r io.Reader) *LineScanner {
	scanner := bufio.NewScanner(r)
	// Increase buffer size to handle large lines (e.g., minified JSON, HTML)
	// Default is 64KB, set to 1MB
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	return &LineScanner{
		scanner: scanner,
	}
}

func (s *LineScanner) Scan() bool {
	return s.scanner.Scan()
}

func (s *LineScanner) Text() string {
	return s.scanner.Text()
}

func (s *LineScanner) Err() error {
	return s.scanner.Err()
}

// isInSkillsPath checks if filePath is within any of the configured skills
// directories. Returns true for files that can be read without permission
// prompts and without size limits.
//
// Note that symlinks are resolved to prevent path traversal attacks via
// symbolic links.
func isInSkillsPath(filePath string, skillsPaths []string) bool {
	if len(skillsPaths) == 0 {
		return false
	}

	absFilePath, err := filepath.Abs(filePath)
	if err != nil {
		return false
	}

	evalFilePath, err := filepath.EvalSymlinks(absFilePath)
	if err != nil {
		return false
	}

	for _, skillsPath := range skillsPaths {
		absSkillsPath, err := filepath.Abs(skillsPath)
		if err != nil {
			continue
		}

		evalSkillsPath, err := filepath.EvalSymlinks(absSkillsPath)
		if err != nil {
			continue
		}

		relPath, err := filepath.Rel(evalSkillsPath, evalFilePath)
		if err == nil && !strings.HasPrefix(relPath, "..") {
			return true
		}
	}

	return false
}
