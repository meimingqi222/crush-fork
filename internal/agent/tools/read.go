package tools

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/fantasy"
	md "github.com/JohannesKaufmann/html-to-markdown"
	"github.com/PuerkitoBio/goquery"
	"github.com/charmbracelet/crush/internal/clientfs"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/filetracker"
	"github.com/charmbracelet/crush/internal/fsext"
	"github.com/charmbracelet/crush/internal/imageutil"
	"github.com/charmbracelet/crush/internal/lsp"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/skills"
)

//go:embed read.md
var readDescription []byte

type ReadParams struct {
	Path string `json:"path" description:"path or url; append :<sel> for line ranges or raw mode (e.g. \"src/foo.ts:50-200\")"`
}

type ReadPermissionsParams struct {
	Path string `json:"path"`
}

type ReadResourceType string

const (
	ReadResourceUnset ReadResourceType = ""
	ReadResourceSkill ReadResourceType = "skill"
)

type ReadResponseMetadata struct {
	ToolPathMetadata
	Path                string           `json:"path"`
	Content             string           `json:"content"`
	Hashline            bool             `json:"hashline,omitempty"`
	ResourceType        ReadResourceType `json:"resource_type,omitempty"`
	ResourceName        string           `json:"resource_name,omitempty"`
	ResourceDescription string           `json:"resource_description,omitempty"`
	MissingPath         string           `json:"missing_path,omitempty"`
	SuggestedGlobs      []string         `json:"suggested_globs,omitempty"`
	SuggestedParentDirs []string         `json:"suggested_parent_dirs,omitempty"`
	RecoveryAvailable   *bool            `json:"recovery_available,omitempty"`
	RecoveredBy         string           `json:"recovered_by,omitempty"`
	RecoveryAction      string           `json:"recovery_action,omitempty"`
	FallbackTool        string           `json:"fallback_tool,omitempty"`
	FallbackToolQuery   string           `json:"fallback_tool_query,omitempty"`
	ErrorKind           string           `json:"error_kind,omitempty"`
	IsDirectory         bool             `json:"is_directory,omitempty"`
	IsURL               bool             `json:"is_url,omitempty"`
	TotalLines          int              `json:"total_lines,omitempty"`
	StartLine           int              `json:"start_line,omitempty"`
	SourceURI           string           `json:"source_uri,omitempty"`
	Revision            string           `json:"revision,omitempty"`
}

const (
	ReadToolName     = "read"
	MaxReadSize      = 1 * 1024 * 1024 // 1MB
	MaxURLReadSize   = 1 * 1024 * 1024 // 1MB
	DefaultReadLimit = 2000
	MaxLineLength    = 2000
)

type textReadLine struct {
	Raw     string
	Display string
}

type textReadResult struct {
	Lines      []textReadLine
	HasMore    bool
	Total      int
	TotalKnown bool
}

var errReadOffsetBeyondEOF = errors.New("offset is beyond end of file")

func NewReadTool(
	lspManager *lsp.Manager,
	permissions permission.Service,
	filetracker filetracker.Service,
	workingDir string,
	lsConfig config.ToolLs,
	httpClient *http.Client,
	skillList []*skills.Skill,
	skillsPaths ...string,
) fantasy.AgentTool {
	return NewReadToolWithArchiveDir(lspManager, permissions, filetracker, workingDir, lsConfig, httpClient, "", skillList, skillsPaths...)
}

func NewReadToolWithArchiveDir(
	lspManager *lsp.Manager,
	permissions permission.Service,
	filetracker filetracker.Service,
	workingDir string,
	lsConfig config.ToolLs,
	httpClient *http.Client,
	archiveDir string,
	skillList []*skills.Skill,
	skillsPaths ...string,
) fantasy.AgentTool {
	if httpClient == nil {
		httpClient = NewSafeHTTPClient(30 * time.Second)
	}

	return fantasy.NewParallelAgentTool(
		ReadToolName,
		string(readDescription),
		func(ctx context.Context, params ReadParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Path == "" {
				return fantasy.NewTextErrorResponse("path is required"), nil
			}

			// Parse path selectors (line ranges, raw mode).
			// URLs pass through unmodified.
			sel := parsePathSelector(params.Path)
			params.Path = sel.filePath

			if strings.HasPrefix(params.Path, "archive://") {
				return handleArchiveRead(ctx, params.Path, sel, archiveDir, filetracker)
			}

			// Check if it's a URL.
			if strings.HasPrefix(params.Path, "http://") || strings.HasPrefix(params.Path, "https://") {
				return handleURLRead(ctx, params, call, permissions, httpClient)
			}

			// Check if it's a skill:// URL and resolve to filesystem path.
			if IsSkillURL(params.Path) {
				resolvedPath, err := ResolveSkillURL(params.Path, skillList)
				if err != nil {
					return fantasy.NewTextErrorResponse(err.Error()), nil
				}
				params.Path = resolvedPath

				// Builtin skills use a virtual crush:// path and are embedded
				// in the binary, not on disk. Read them directly.
				if strings.HasPrefix(params.Path, "crush://") {
					return handleEmbeddedSkillRead(ctx, params, sel, call, skillList, filetracker)
				}

				// Re-parse selectors for the resolved path, preserving any
				// line selectors that were already parsed from the original
				// skill:// URL (e.g. skill://pdf:10-50 → sel has line range).
				resolvedSel := parsePathSelector(params.Path)
				params.Path = resolvedSel.filePath
				// If the original selector had line ranges but the resolved
				// path doesn't, preserve the original line selectors.
				if sel.hasLineSel && !resolvedSel.hasLineSel {
					resolvedSel.offset = sel.offset
					resolvedSel.limit = sel.limit
					resolvedSel.raw = sel.raw
					resolvedSel.hasLineSel = sel.hasLineSel
					resolvedSel.hasSelector = sel.hasSelector
				}
				sel = resolvedSel
			}

			// Handle file path. Normalize Windows separators so paths copied
			// from cross-platform tool output resolve on the host platform.
			params.Path = strings.ReplaceAll(params.Path, "\\\\", "/")
			return handleFileRead(ctx, params, sel, call, lspManager, permissions, filetracker, workingDir, lsConfig, skillsPaths...)
		})
}

func handleURLRead(
	ctx context.Context,
	params ReadParams,
	call fantasy.ToolCall,
	permissions permission.Service,
	client *http.Client,
) (fantasy.ToolResponse, error) {
	format := "markdown"

	sessionID := GetSessionFromContext(ctx)
	if sessionID == "" {
		return fantasy.ToolResponse{}, fmt.Errorf("session ID is required")
	}

	permissionResponse, err := RequestPermission(ctx, permissions,
		permission.CreatePermissionRequest{
			SessionID:   sessionID,
			Path:        params.Path,
			ToolCallID:  call.ID,
			ToolName:    ReadToolName,
			Action:      "read_url",
			Description: fmt.Sprintf("Read URL content: %s", params.Path),
			Params:      ReadPermissionsParams(params),
		},
	)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if permissionResponse != nil {
		return *permissionResponse, nil
	}

	// Use a fixed 2-minute timeout for URL reads.
	const maxURLReadTimeoutSeconds = 120

	requestCtx, cancel := context.WithTimeout(ctx, maxURLReadTimeoutSeconds*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(requestCtx, "GET", params.Path, nil)
	if err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("User-Agent", "crush/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("failed to read URL: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("Request failed with status code: %d", resp.StatusCode)), nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxURLReadSize))
	if err != nil {
		return fantasy.NewTextErrorResponse("Failed to read response body: " + err.Error()), nil
	}

	content := string(body)

	validUTF8 := utf8.ValidString(content)
	if !validUTF8 {
		return fantasy.NewTextErrorResponse("Response content is not valid UTF-8"), nil
	}
	contentType := resp.Header.Get("Content-Type")

	switch format {
	case "text":
		if strings.Contains(contentType, "text/html") {
			text, err := extractTextFromHTML(content)
			if err != nil {
				return fantasy.NewTextErrorResponse("Failed to extract text from HTML: " + err.Error()), nil
			}
			content = text
		}

	case "markdown":
		if strings.Contains(contentType, "text/html") {
			markdown, err := convertHTMLToMarkdown(content)
			if err != nil {
				return fantasy.NewTextErrorResponse("Failed to convert HTML to Markdown: " + err.Error()), nil
			}
			content = markdown
		}

		content = wrapInMarkdownCodeBlock(content)

	case "html":
		// Return only the body of the HTML document.
		if strings.Contains(contentType, "text/html") {
			doc, err := goquery.NewDocumentFromReader(strings.NewReader(content))
			if err != nil {
				return fantasy.NewTextErrorResponse("Failed to parse HTML: " + err.Error()), nil
			}
			body, err := doc.Find("body").Html()
			if err != nil {
				return fantasy.NewTextErrorResponse("Failed to extract body from HTML: " + err.Error()), nil
			}
			if body == "" {
				return fantasy.NewTextErrorResponse("No body content found in HTML"), nil
			}
			content = "<html>\n<body>\n" + body + "\n</body>\n</html>"
		}
	}
	// Truncate content if it exceeds max URL read size.
	if int64(len(content)) > MaxURLReadSize {
		i := MaxURLReadSize
		for i > 0 && (content[i]&0xC0) == 0x80 {
			i--
		}
		content = content[:i]
		content += fmt.Sprintf("\n\n[Content truncated to %d bytes]", MaxURLReadSize)
	}

	meta := ReadResponseMetadata{
		Path:  params.Path,
		IsURL: true,
	}
	return fantasy.WithResponseMetadata(fantasy.NewTextResponse(content), meta), nil
}

func handleEmbeddedSkillRead(
	ctx context.Context,
	params ReadParams,
	sel pathSelector,
	call fantasy.ToolCall,
	skillList []*skills.Skill,
	filetracker filetracker.Service,
) (fantasy.ToolResponse, error) {
	content, err := skills.ReadSkillFile(params.Path)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("error reading skill file: %v", err)), nil
	}

	// Split embedded content into lines.
	var allLines []string
	scanner := NewLineScanner(bytes.NewReader(content))
	for scanner.Scan() {
		allLines = append(allLines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("error reading skill file: %v", err)), nil
	}

	readOffset := sel.offset
	readLimit := sel.limit
	if readLimit <= 0 {
		readLimit = len(allLines)
	}
	readResult, err := extractReadResultFromLines(allLines, readOffset, readLimit)
	if err != nil {
		if errors.Is(err, errReadOffsetBeyondEOF) {
			msg := fmt.Sprintf("Line %d is beyond end of file (%d lines total).", readOffset+1, readResult.Total)
			return fantasy.WithResponseMetadata(
				fantasy.NewTextErrorResponse(msg),
				ReadResponseMetadata{Path: params.Path, TotalLines: readResult.Total},
			), nil
		}
		return fantasy.ToolResponse{}, fmt.Errorf("error reading skill file: %w", err)
	}

	lines := readResult.Lines
	displayContent := joinDisplayLines(lines)
	if !utf8.ValidString(displayContent) {
		return fantasy.NewTextErrorResponse("Skill file content is not valid UTF-8"), nil
	}

	useHashline := sel.hasLineSel && !sel.raw
	var output string
	if sel.raw {
		output = displayContent
		// Surface silent truncation when raw mode hits an explicit line limit.
		if readResult.HasMore {
			nextLine := readOffset + len(lines) + 1
			output += fmt.Sprintf("\n(Truncated after line %d. Use path=%q to continue reading.)\n", readOffset+len(lines), fmt.Sprintf("%s:%d", params.Path, nextLine))
		}
	} else {
		output = "<file>\n"
		if useHashline {
			output += addHashlineLineNumbers(lines, readOffset+1)
		} else {
			output += addLineNumbers(lines, readOffset+1)
		}
	}

	nextLine := readOffset + len(lines) + 1
	if !sel.raw && len(lines) > 0 {
		startLine := readOffset + 1
		endLine := readOffset + len(lines)
		if readResult.HasMore {
			slashPath := filepath.ToSlash(params.Path)
			output += fmt.Sprintf("\n\n(Showing lines %d-%d. Use path=%q to continue.)",
				startLine, endLine, fmt.Sprintf("%s:%d", slashPath, nextLine))
		} else if readResult.TotalKnown {
			output += fmt.Sprintf("\n\n(Showing lines %d-%d. End of file - total %d lines.)",
				startLine, endLine, readResult.Total)
		}
	} else if !sel.raw && readResult.TotalKnown {
		output += fmt.Sprintf("\n\n(End of file - total %d lines.)", readResult.Total)
	}
	if !sel.raw {
		output += "\n</file>\n"
	} else {
		output += "\n"
	}

	sessionID := GetSessionFromContext(ctx)
	filetracker.RecordRead(ctx, sessionID, params.Path)

	meta := ReadResponseMetadata{
		Path:      params.Path,
		Content:   displayContent,
		Hashline:  useHashline,
		StartLine: readOffset + 1,
	}
	if readResult.TotalKnown {
		meta.TotalLines = readResult.Total
	}
	if s := findSkillByFilePath(params.Path, skillList); s != nil {
		meta.ResourceType = ReadResourceSkill
		meta.ResourceName = s.Name
		meta.ResourceDescription = s.Description
	}

	return fantasy.WithResponseMetadata(fantasy.NewTextResponse(output), meta), nil
}

func findSkillByFilePath(path string, skillList []*skills.Skill) *skills.Skill {
	for _, s := range skillList {
		if s.SkillFilePath == path {
			return s
		}
	}
	return nil
}

func handleFileRead(
	ctx context.Context,
	params ReadParams,
	sel pathSelector,
	call fantasy.ToolCall,
	lspManager *lsp.Manager,
	permissions permission.Service,
	filetracker filetracker.Service,
	workingDir string,
	lsConfig config.ToolLs,
	skillsPaths ...string,
) (fantasy.ToolResponse, error) {
	effectiveWorkingDir := EffectiveWorkingDir(ctx, workingDir)

	resolvedPath, err := resolveLocalPlanURI(ctx, params.Path, effectiveWorkingDir)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}

	toolPath := ResolveToolPath(ctx, effectiveWorkingDir, resolvedPath)
	filePath := toolPath.AbsolutePath

	// Check if file is outside working directory and request permission if needed.
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
				ToolName:    ReadToolName,
				Action:      "read",
				Description: fmt.Sprintf("Read file outside working directory: %s", absFilePath),
				Params:      ReadPermissionsParams(params),
			},
		)
		if permReqErr != nil {
			return fantasy.ToolResponse{}, permReqErr
		}
		if permissionResponse != nil {
			return *permissionResponse, nil
		}
	}

	// Check if file exists.
	requestedFilePath := filePath
	var recovery readPathRecovery
	fileInfo, err := clientfs.Stat(ctx, filePath)
	if err != nil {
		if os.IsNotExist(err) {
			if !isOutsideWorkDir {
				if recovered, ok := recoverMissingReadPath(absWorkingDir, relPath); ok {
					if recoveredInfo, statErr := clientfs.Stat(ctx, recovered.FilePath); statErr == nil {
						filePath = recovered.FilePath
						fileInfo = recoveredInfo
						recovery = recovered
						err = nil
					} else {
						err = statErr
					}
				}
			}
			if err != nil && os.IsNotExist(err) {
				suggestions := findReadSuggestions(requestedFilePath)
				advice := buildMissingReadAdvice(absWorkingDir, requestedFilePath, relPath)
				message := fmt.Sprintf("File not found: %s", advice.MissingPath)
				if len(suggestions) > 0 {
					message += fmt.Sprintf("\n\nDid you mean one of these?\n%s", strings.Join(suggestions, "\n"))
				}
				message += "\n\nNext steps:"
				for _, glob := range advice.SuggestedGlobs {
					message += fmt.Sprintf("\n- Use glob pattern: %s", glob)
				}
				for _, parentDir := range advice.SuggestedParentDirs {
					message += fmt.Sprintf("\n- Read parent directory: %s", parentDir)
				}
				message += "\n- Search symbol/content with grep before retrying read"
				missingMetadata := newMissingReadMetadata(advice, suggestions, effectiveWorkingDir)
				missingMetadata.ErrorKind = "path_not_found"
				if hint := DuplicateWorkingDirPrefixHint(params.Path, effectiveWorkingDir); hint != "" {
					message += "\n- " + hint
					missingMetadata.PrefixHint = hint
				}
				return fantasy.WithResponseMetadata(
					fantasy.NewTextErrorResponse(message),
					missingMetadata,
				), nil
			}
		}
		if err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("error accessing file: %v", err)), nil
		}
	}

	// Check if it's a directory — automatically list its contents.
	if fileInfo.IsDir() {
		out, _, lsErr := ListDirectoryTree(filePath, LSParams{}, lsConfig)
		if lsErr != nil {
			return fantasy.NewTextErrorResponse(lsErr.Error()), nil
		}
		meta := ReadResponseMetadata{
			ToolPathMetadata: NewToolPathMetadata(toolPath),
			Path:             filePath,
			Content:          out,
			IsDirectory:      true,
		}
		meta.applyRecovery(recovery)
		if notice := recovery.notice(absWorkingDir); notice != "" {
			out = notice + "\n\n" + out
		}
		return fantasy.WithResponseMetadata(
			fantasy.NewTextResponse(out),
			meta,
		), nil
	}

	isSupportedImage, mimeType := getImageMimeType(filePath)

	// Resolve read limit from selector or defaults.
	readLimit := sel.limit
	if readLimit <= 0 {
		if isSkillFile {
			readLimit = 1000000 // Effectively no limit for skill files.
		} else {
			readLimit = DefaultReadLimit
		}
	}
	readOffset := sel.offset

	if isSupportedImage {
		if !GetSupportsImagesFromContext(ctx) {
			vision := GetVisionServiceFromContext(ctx)
			if vision != nil && vision.IsAvailable() {
				imageData, readErr := clientfs.ReadFile(ctx, filePath)
				if readErr != nil {
					return fantasy.ToolResponse{}, fmt.Errorf("error reading image file: %w", readErr)
				}
				desc, descErr := vision.DescribeImage(ctx, imageData, mimeType, "")
				if descErr != nil {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to describe image: %v", descErr)), nil
				}
				meta := ReadResponseMetadata{
					ToolPathMetadata: NewToolPathMetadata(toolPath),
					Path:             filePath,
				}
				applyClientFSReadMetadata(ctx, filePath, &meta)
				meta.applyRecovery(recovery)
				return fantasy.WithResponseMetadata(fantasy.NewTextResponse(desc), meta), nil
			}
			modelName := GetModelNameFromContext(ctx)
			return fantasy.NewTextErrorResponse(fmt.Sprintf("This model (%s) does not support image data.", modelName)), nil
		}

		imageData, readErr := clientfs.ReadFile(ctx, filePath)
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

		meta := ReadResponseMetadata{
			ToolPathMetadata: NewToolPathMetadata(toolPath),
			Path:             filePath,
		}
		applyClientFSReadMetadata(ctx, filePath, &meta)
		meta.applyRecovery(recovery)
		return fantasy.WithResponseMetadata(fantasy.NewImageResponse(result.Data, result.MimeType), meta), nil
	}

	// Read the file content.
	var readResult textReadResult
	if sessionID != "" {
		allLines, rErr := readAllFileLinesContext(ctx, filePath)
		if rErr != nil {
			return fantasy.ToolResponse{}, fmt.Errorf("error reading file: %w", rErr)
		}
		GlobalFileCache.Put(sessionID, filePath, allLines)
		readResult, err = extractReadResultFromLines(allLines, readOffset, readLimit)
	} else {
		readResult, err = readTextFileLines(filePath, readOffset, readLimit)
	}
	if err != nil {
		if errors.Is(err, errReadOffsetBeyondEOF) {
			slashPath := filepath.ToSlash(params.Path)
			suggestion := fmt.Sprintf("Use path=%q to read from the start", slashPath)
			if readResult.Total > 0 {
				suggestion = fmt.Sprintf("Use path=%q to read from the start, or path=%q to read the last line", slashPath, fmt.Sprintf("%s:%d", slashPath, readResult.Total))
			}
			msg := fmt.Sprintf("Line %d is beyond end of file (%d lines total). %s.", readOffset+1, readResult.Total, suggestion)
			meta := ReadResponseMetadata{
				ToolPathMetadata: NewToolPathMetadata(toolPath),
				Path:             filePath,
				TotalLines:       readResult.Total,
			}
			return fantasy.WithResponseMetadata(
				fantasy.NewTextErrorResponse(msg),
				meta,
			), nil
		}
		return fantasy.ToolResponse{}, fmt.Errorf("error reading file: %w", err)
	}
	lines := readResult.Lines
	content := joinDisplayLines(lines)
	if !utf8.ValidString(content) {
		return fantasy.NewTextErrorResponse("File content is not valid UTF-8"), nil
	}

	openInLSPs(ctx, lspManager, filePath)
	waitForLSPDiagnostics(ctx, lspManager, filePath, 300*time.Millisecond)

	// Determine output mode: raw, hashline (auto-enabled by line selector),
	// or plain line numbers.
	useHashline := sel.hasLineSel && !sel.raw
	var output string
	if sel.raw {
		// Raw mode: verbatim text, no line numbers, no wrapping.
		output = content
		// Raw mode has no line-number footer; surface silent truncation at the
		// default line limit so the caller knows to paginate.
		if readResult.HasMore {
			nextLine := readOffset + len(lines) + 1
			slashPath := filepath.ToSlash(params.Path)
			output += fmt.Sprintf("\n(Truncated after line %d. Use path=%q to continue reading.)\n", readOffset+len(lines), fmt.Sprintf("%s:%d", slashPath, nextLine))
		}
	} else {
		output = "<file>\n"
		if useHashline {
			output += addHashlineLineNumbers(lines, readOffset+1)
		} else {
			output += addLineNumbers(lines, readOffset+1)
		}
	}

	nextLine := readOffset + len(lines) + 1 // 1-indexed next line for pagination.
	if !sel.raw && len(lines) > 0 {
		startLine := readOffset + 1
		endLine := readOffset + len(lines)
		if readResult.HasMore {
			slashPath := filepath.ToSlash(params.Path)
			output += fmt.Sprintf("\n\n(Showing lines %d-%d. Use path=%q to continue.)",
				startLine, endLine, fmt.Sprintf("%s:%d", slashPath, nextLine))
		} else if readResult.TotalKnown {
			output += fmt.Sprintf("\n\n(Showing lines %d-%d. End of file - total %d lines.)",
				startLine, endLine, readResult.Total)
		}
	} else if !sel.raw && readResult.TotalKnown {
		output += fmt.Sprintf("\n\n(End of file - total %d lines.)", readResult.Total)
	}
	if !sel.raw {
		output += "\n</file>\n"
	} else {
		output += "\n"
	}
	output += getDiagnostics(filePath, lspManager)
	filetracker.RecordRead(ctx, sessionID, filePath)

	meta := ReadResponseMetadata{
		ToolPathMetadata: NewToolPathMetadata(toolPath),
		Path:             filePath,
		Content:          content,
		Hashline:         useHashline,
		StartLine:        readOffset + 1,
	}
	applyClientFSReadMetadata(ctx, filePath, &meta)
	if readResult.TotalKnown {
		meta.TotalLines = readResult.Total
	}
	meta.applyRecovery(recovery)
	if notice := recovery.notice(absWorkingDir); notice != "" {
		output = notice + "\n\n" + output
	}
	if isSkillFile {
		if skill, err := skills.Parse(filePath); err == nil {
			meta.ResourceType = ReadResourceSkill
			meta.ResourceName = skill.Name
			meta.ResourceDescription = skill.Description
		}
	}

	return fantasy.WithResponseMetadata(
		fantasy.NewTextResponse(output),
		meta,
	), nil
}

type missingReadAdvice struct {
	MissingPath         string
	SuggestedGlobs      []string
	SuggestedParentDirs []string
}

func newMissingReadMetadata(advice missingReadAdvice, suggestions []string, workingDir string) ReadResponseMetadata {
	recoveryAvailable := false
	path := ResolveToolPath(context.Background(), workingDir, advice.MissingPath)
	metadata := ReadResponseMetadata{
		ToolPathMetadata:    NewToolPathMetadata(path),
		Path:                advice.MissingPath,
		MissingPath:         advice.MissingPath,
		SuggestedGlobs:      advice.SuggestedGlobs,
		SuggestedParentDirs: advice.SuggestedParentDirs,
		RecoveryAvailable:   &recoveryAvailable,
	}
	metadata.RecoveredBy = "file_not_found_suggestions"
	if len(suggestions) > 0 {
		metadata.RecoveryAction = fmt.Sprintf("File %q was not found. Try one of the suggested paths from the error message, or ground the path with glob/grep before retrying read.", advice.MissingPath)
	} else {
		metadata.RecoveryAction = fmt.Sprintf("File %q was not found. Ground the path with glob/grep or read an existing parent directory before retrying read.", advice.MissingPath)
	}
	return metadata
}

func buildMissingReadAdvice(absWorkingDir, requestedFilePath, requestedRelPath string) missingReadAdvice {
	missingPath := displayMissingReadPath(absWorkingDir, requestedFilePath, requestedRelPath)
	base := pathpkg.Base(slashClean(missingPath))
	if base == "." || base == "/" {
		base = filepath.Base(requestedFilePath)
	}

	advice := missingReadAdvice{
		MissingPath: missingPath,
	}
	if base != "" && base != "." && base != string(filepath.Separator) {
		advice.SuggestedGlobs = []string{"**/" + base}
	}
	if parentDir := nearestExistingParentDir(absWorkingDir, requestedFilePath); parentDir != "" {
		advice.SuggestedParentDirs = []string{parentDir}
	}
	return advice
}

func displayMissingReadPath(absWorkingDir, requestedFilePath, requestedRelPath string) string {
	if requestedRelPath != "" && requestedRelPath != ".." && !strings.HasPrefix(requestedRelPath, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(requestedRelPath)
	}
	if rel, err := filepath.Rel(absWorkingDir, requestedFilePath); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(requestedFilePath)
}

func nearestExistingParentDir(absWorkingDir, requestedFilePath string) string {
	dir := filepath.Dir(requestedFilePath)
	for dir != "." && dir != "" {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			if rel, relErr := filepath.Rel(absWorkingDir, dir); relErr == nil {
				if rel == "." {
					return ""
				}
				if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					return filepath.ToSlash(rel)
				}
			}
			return filepath.ToSlash(dir)
		}
		next := filepath.Dir(dir)
		if next == dir {
			break
		}
		dir = next
	}
	return ""
}

type readPathRecovery struct {
	FilePath       string
	RecoveredBy    string
	RecoveryAction string
	RequestedPath  string
}

func (recovery readPathRecovery) notice(absWorkingDir string) string {
	if recovery.RecoveredBy != "unique_suffix_recovery" || recovery.RequestedPath == "" {
		return ""
	}
	rel, err := filepath.Rel(absWorkingDir, recovery.FilePath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		rel = filepath.ToSlash(recovery.FilePath)
	} else {
		rel = filepath.ToSlash(rel)
	}
	return fmt.Sprintf("[Path %q not found; resolved to %q via suffix match]", recovery.RequestedPath, rel)
}

func (metadata *ReadResponseMetadata) applyRecovery(recovery readPathRecovery) {
	if recovery.RecoveredBy == "" {
		return
	}
	metadata.RecoveredBy = recovery.RecoveredBy
	metadata.RecoveryAction = recovery.RecoveryAction
}

func slashClean(path string) string {
	return pathpkg.Clean(filepath.ToSlash(path))
}

func recoverMissingReadPath(absWorkingDir, requestedRelPath string) (readPathRecovery, bool) {
	if requestedRelPath == "." || requestedRelPath == "" || strings.HasPrefix(requestedRelPath, ".."+string(filepath.Separator)) {
		return readPathRecovery{}, false
	}

	requestedSuffix := slashClean(requestedRelPath)
	if requestedSuffix == "." || strings.HasPrefix(requestedSuffix, "../") {
		return readPathRecovery{}, false
	}

	var suffixMatches []string
	var extensionMatches []string
	walkErr := filepath.WalkDir(absWorkingDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path == absWorkingDir {
			return nil
		}

		relPath, relErr := filepath.Rel(absWorkingDir, path)
		if relErr != nil || strings.HasPrefix(relPath, ".."+string(filepath.Separator)) {
			return nil
		}

		normalizedRelPath := slashClean(relPath)
		if normalizedRelPath == requestedSuffix || strings.HasSuffix(normalizedRelPath, "/"+requestedSuffix) {
			suffixMatches = append(suffixMatches, path)
		}
		if entry.IsDir() && pathpkg.Ext(requestedSuffix) != "" {
			correctedSuffix := slashClean(strings.TrimSuffix(requestedSuffix, pathpkg.Ext(requestedSuffix)))
			if correctedSuffix != "." && (normalizedRelPath == correctedSuffix || strings.HasSuffix(normalizedRelPath, "/"+correctedSuffix)) {
				extensionMatches = append(extensionMatches, path)
			}
		}

		return nil
	})
	if walkErr != nil {
		return readPathRecovery{}, false
	}

	if len(suffixMatches) == 1 {
		return readPathRecovery{
			FilePath:       suffixMatches[0],
			RecoveredBy:    "unique_suffix_recovery",
			RecoveryAction: fmt.Sprintf("Recovered missing read path %q to unique workspace match %q.", requestedRelPath, suffixMatches[0]),
			RequestedPath:  filepath.ToSlash(requestedRelPath),
		}, true
	}
	if len(suffixMatches) == 0 && len(extensionMatches) == 1 {
		return readPathRecovery{
			FilePath:       extensionMatches[0],
			RecoveredBy:    "directory_extension_recovery",
			RecoveryAction: fmt.Sprintf("Recovered missing read path %q to unique directory match %q after removing the file extension.", requestedRelPath, extensionMatches[0]),
		}, true
	}

	return readPathRecovery{}, false
}

func findReadSuggestions(filePath string) []string {
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

func addLineNumbers(lines []textReadLine, startLine int) string {
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

func addHashlineLineNumbers(lines []textReadLine, startLine int) string {
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

func joinDisplayLines(lines []textReadLine) string {
	if len(lines) == 0 {
		return ""
	}

	displayLines := make([]string, 0, len(lines))
	for _, line := range lines {
		displayLines = append(displayLines, line.Display)
	}

	return strings.Join(displayLines, "\n")
}

func readTextFileLines(filePath string, offset, limit int) (textReadResult, error) {
	if limit < 0 {
		limit = 0
	}
	file, err := os.Open(filePath)
	if err != nil {
		return textReadResult{}, err
	}
	defer file.Close()

	scanner := NewLineScanner(file)
	skipped := 0
	if offset > 0 {
		for skipped < offset && scanner.Scan() {
			skipped++
		}
		if err = scanner.Err(); err != nil {
			return textReadResult{}, err
		}
		if skipped < offset {
			return textReadResult{Total: skipped, TotalKnown: true}, errReadOffsetBeyondEOF
		}
	}

	// Pre-allocate slice with expected capacity.
	lines := make([]textReadLine, 0, limit)

	for len(lines) < limit && scanner.Scan() {
		rawLine := scanner.Text()
		displayLine := rawLine
		if len(displayLine) > MaxLineLength {
			displayLine = displayLine[:MaxLineLength] + "..."
		}
		lines = append(lines, textReadLine{
			Raw:     rawLine,
			Display: displayLine,
		})
	}

	// Peek one more line only when we filled the limit.
	hasMore := len(lines) == limit && scanner.Scan()

	if err := scanner.Err(); err != nil {
		return textReadResult{}, err
	}

	result := textReadResult{
		Lines:      lines,
		HasMore:    hasMore,
		TotalKnown: !hasMore,
	}
	if result.TotalKnown {
		result.Total = offset + len(lines)
	}

	return result, nil
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
	// Increase buffer size to handle large lines (e.g., minified JSON, HTML).
	// Default is 64KB, set to 1MB.
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

func extractTextFromHTML(html string) (string, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return "", err
	}

	text := doc.Find("body").Text()
	text = strings.Join(strings.Fields(text), " ")

	return text, nil
}

func convertHTMLToMarkdown(html string) (string, error) {
	converter := md.NewConverter("", true, nil)

	markdown, err := converter.ConvertString(html)
	if err != nil {
		return "", err
	}

	return markdown, nil
}

// LSParams holds parameters for directory listing.
type LSParams struct {
	Path   string   `json:"path,omitempty" description:"The path to the directory to list (defaults to current working directory)"`
	Ignore []string `json:"ignore,omitempty" description:"List of glob patterns to ignore"`
	Depth  int      `json:"depth,omitempty" description:"The maximum depth to traverse"`
}

type NodeType string

const (
	NodeTypeFile      NodeType = "file"
	NodeTypeDirectory NodeType = "directory"
)

type TreeNode struct {
	Name     string      `json:"name"`
	Path     string      `json:"path"`
	Type     NodeType    `json:"type"`
	Children []*TreeNode `json:"children,omitempty"`
}

type LSResponseMetadata struct {
	NumberOfFiles int  `json:"number_of_files"`
	Truncated     bool `json:"truncated"`
}

const maxLSFiles = 1000

func ListDirectoryTree(searchPath string, params LSParams, lsConfig config.ToolLs) (string, LSResponseMetadata, error) {
	if _, err := os.Stat(searchPath); os.IsNotExist(err) {
		return "", LSResponseMetadata{}, fmt.Errorf("path does not exist: %s", searchPath)
	}

	depth, limit := lsConfig.Limits()
	maxFiles := cmp.Or(limit, maxLSFiles)
	files, truncated, err := fsext.ListDirectory(
		searchPath,
		params.Ignore,
		cmp.Or(params.Depth, depth),
		maxFiles,
	)
	if err != nil {
		return "", LSResponseMetadata{}, fmt.Errorf("error listing directory: %w", err)
	}

	metadata := LSResponseMetadata{
		NumberOfFiles: len(files),
		Truncated:     truncated,
	}
	tree := createFileTree(files, searchPath)

	var outputParts []string
	if truncated {
		outputParts = append(outputParts, fmt.Sprintf("There are more than %d files in the directory. Use a more specific path or use the Glob tool to find specific files. The first %[1]d files and directories are included below.", maxFiles))
	}
	if depth > 0 {
		outputParts = append(outputParts, fmt.Sprintf("The directory tree is shown up to a depth of %d. Use a higher depth and a specific path to see more levels.", cmp.Or(params.Depth, depth)))
	}
	var output string
	if len(outputParts) > 0 {
		output = strings.Join(outputParts, "\n") + "\n"
	}
	return output + "\n" + printTree(tree, searchPath), metadata, nil
}

func createFileTree(sortedPaths []string, rootPath string) []*TreeNode {
	root := []*TreeNode{}
	pathMap := make(map[string]*TreeNode)

	for _, path := range sortedPaths {
		relativePath := strings.TrimPrefix(path, rootPath)
		parts := strings.Split(relativePath, string(filepath.Separator))
		currentPath := ""
		var parentPath string

		var cleanParts []string
		for _, part := range parts {
			if part != "" {
				cleanParts = append(cleanParts, part)
			}
		}
		parts = cleanParts

		if len(parts) == 0 {
			continue
		}

		for i, part := range parts {
			if currentPath == "" {
				currentPath = part
			} else {
				currentPath = filepath.Join(currentPath, part)
			}

			if _, exists := pathMap[currentPath]; exists {
				parentPath = currentPath
				continue
			}

			isLastPart := i == len(parts)-1
			isDir := !isLastPart || strings.HasSuffix(relativePath, string(filepath.Separator))
			nodeType := NodeTypeFile
			if isDir {
				nodeType = NodeTypeDirectory
			}
			newNode := &TreeNode{
				Name:     part,
				Path:     currentPath,
				Type:     nodeType,
				Children: []*TreeNode{},
			}

			pathMap[currentPath] = newNode

			if i > 0 && parentPath != "" {
				if parent, ok := pathMap[parentPath]; ok {
					parent.Children = append(parent.Children, newNode)
				}
			} else {
				root = append(root, newNode)
			}

			parentPath = currentPath
		}
	}

	return root
}

func printTree(tree []*TreeNode, rootPath string) string {
	var result strings.Builder

	result.WriteString("- ")
	result.WriteString(filepath.ToSlash(rootPath))
	if rootPath != "" && rootPath[len(rootPath)-1] != '/' {
		result.WriteByte('/')
	}
	result.WriteByte('\n')

	for _, node := range tree {
		printNode(&result, node, 1)
	}

	return result.String()
}

func printNode(builder *strings.Builder, node *TreeNode, level int) {
	indent := strings.Repeat("  ", level)

	nodeName := node.Name
	if node.Type == NodeTypeDirectory {
		nodeName = nodeName + "/"
	}

	fmt.Fprintf(builder, "%s- %s\n", indent, nodeName)

	if node.Type == NodeTypeDirectory && len(node.Children) > 0 {
		for _, child := range node.Children {
			printNode(builder, child, level+1)
		}
	}
}

func wrapInMarkdownCodeBlock(content string) string {
	maxTicks := 0
	currentTicks := 0
	for _, r := range content {
		if r == '`' {
			currentTicks++
			if currentTicks > maxTicks {
				maxTicks = currentTicks
			}
		} else {
			currentTicks = 0
		}
	}
	numTicks := max(3, maxTicks+1)
	ticks := strings.Repeat("`", numTicks)
	return ticks + "\n" + content + "\n" + ticks
}

func readAllFileLines(filePath string) ([]string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var lines []string
	scanner := NewLineScanner(file)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines, scanner.Err()
}

func readAllFileLinesContext(ctx context.Context, filePath string) ([]string, error) {
	if _, ok := clientfs.FromContext(ctx); !ok {
		return readAllFileLines(filePath)
	}
	data, err := clientfs.ReadFile(ctx, filePath)
	if err != nil {
		return nil, err
	}
	defer clear(data)
	var lines []string
	scanner := NewLineScanner(bytes.NewReader(data))
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines, scanner.Err()
}

func applyClientFSReadMetadata(ctx context.Context, path string, metadata *ReadResponseMetadata) {
	if metadata == nil {
		return
	}
	if current, ok := clientfs.MetadataFor(ctx, path); ok {
		metadata.SourceURI = current.SourceURI
		metadata.Revision = current.Revision
	}
}

func extractReadResultFromLines(allLines []string, offset, limit int) (textReadResult, error) {
	if limit < 0 {
		limit = 0
	}
	total := len(allLines)
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		return textReadResult{Total: total, TotalKnown: true}, errReadOffsetBeyondEOF
	}

	end := offset + limit
	if end > total {
		end = total
	}

	lines := make([]textReadLine, 0, end-offset)
	for i := offset; i < end; i++ {
		rawLine := allLines[i]
		displayLine := rawLine
		if len(displayLine) > MaxLineLength {
			displayLine = displayLine[:MaxLineLength] + "..."
		}
		lines = append(lines, textReadLine{
			Raw:     rawLine,
			Display: displayLine,
		})
	}

	hasMore := end < total
	result := textReadResult{
		Lines:      lines,
		HasMore:    hasMore,
		TotalKnown: true,
		Total:      total,
	}
	return result, nil
}
