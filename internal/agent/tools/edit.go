package tools

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/clientfs"
	"github.com/charmbracelet/crush/internal/diff"
	"github.com/charmbracelet/crush/internal/filetracker"
	"github.com/charmbracelet/crush/internal/fsext"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/message"

	"github.com/charmbracelet/crush/internal/lsp"
	"github.com/charmbracelet/crush/internal/permission"
)

// EditEntry is a single edit operation used when making multiple edits.
type EditEntry struct {
	OldString  string `json:"old_string" description:"The text to replace"`
	NewString  string `json:"new_string" description:"The text to replace it with"`
	ReplaceAll bool   `json:"replace_all,omitempty" description:"Replace all occurrences (default false)"`
}

type EditParams struct {
	FilePath       string                   `json:"file_path,omitempty" description:"The absolute path to the file to modify"`
	OldString      string                   `json:"old_string,omitempty" description:"The text to replace"`
	NewString      string                   `json:"new_string,omitempty" description:"The text to replace it with"`
	ReplaceAll     bool                     `json:"replace_all,omitempty" description:"Replace all occurrences of old_string (default false)"`
	Edits          []EditEntry              `json:"edits,omitempty" description:"Array of edit operations to perform sequentially on the file. When provided, old_string/new_string/replace_all are ignored."`
	Operations     []HashlineEditOperation  `json:"operations,omitempty" description:"Array of hashline operations using LINE#HASH references from read with a line selector (e.g. path=\"file.ts:50-200\"). Supports replace_line, replace_range, prepend, append, cut, and paste. When provided, all other parameters except file_path are ignored."`
	Patch          string                   `json:"patch,omitempty" description:"Unified diff format patch containing changes to apply to files"`
	FileOperations []FileHashlineOperations `json:"file_operations,omitempty" description:"Array of per-file hashline operations for multi-file atomic edits with cross-file CUT/PASTE support. Each entry specifies a file_path and its operations array. When provided, all other parameters are ignored."`
}

// FileHashlineOperations groups hashline operations targeting a single file.
// Used by the file_operations parameter to edit multiple files atomically in one call.
type FileHashlineOperations struct {
	FilePath   string                  `json:"file_path" description:"The absolute path to the file to modify"`
	Operations []HashlineEditOperation `json:"operations" description:"Array of hashline operations to apply to this file"`
}

type EditPermissionsParams struct {
	FilePath   string `json:"file_path"`
	OldContent string `json:"old_content,omitempty"`
	NewContent string `json:"new_content,omitempty"`
}

type EditResponseMetadata struct {
	ToolPathMetadata
	FilePath     string       `json:"file_path,omitempty"`
	Additions    int          `json:"additions"`
	Removals     int          `json:"removals"`
	OldContent   string       `json:"old_content,omitempty"`
	NewContent   string       `json:"new_content,omitempty"`
	EditsApplied int          `json:"edits_applied,omitempty"`
	EditsFailed  []FailedEdit `json:"edits_failed,omitempty"`
	SourceURI    string       `json:"source_uri,omitempty"`
	Revision     string       `json:"revision,omitempty"`
}

// FailedEdit records a single edit operation that could not be applied.
type FailedEdit struct {
	Index int       `json:"index"`
	Error string    `json:"error"`
	Edit  EditEntry `json:"edit"`
}

func enrichClientFSEditMetadata(ctx context.Context, response *fantasy.ToolResponse, workingDir string) {
	if response == nil || strings.TrimSpace(response.Metadata) == "" {
		return
	}
	apply := func(metadata *EditResponseMetadata) {
		if metadata == nil || metadata.FilePath == "" {
			return
		}
		path := ResolveToolPath(ctx, workingDir, metadata.FilePath)
		metadata.ToolPathMetadata = NewToolPathMetadata(path)
		response.Content = strings.ReplaceAll(response.Content, metadata.FilePath, path.DisplayPath)
		if current, ok := clientfs.MetadataFor(ctx, metadata.FilePath); ok {
			metadata.SourceURI = current.SourceURI
			metadata.Revision = current.Revision
		}
	}
	if strings.HasPrefix(strings.TrimSpace(response.Metadata), "[") {
		var values []EditResponseMetadata
		if json.Unmarshal([]byte(response.Metadata), &values) != nil {
			return
		}
		for index := range values {
			apply(&values[index])
		}
		if payload, err := json.Marshal(values); err == nil {
			response.Metadata = string(payload)
		}
		return
	}
	var value EditResponseMetadata
	if json.Unmarshal([]byte(response.Metadata), &value) != nil {
		return
	}
	apply(&value)
	if payload, err := json.Marshal(value); err == nil {
		response.Metadata = string(payload)
	}
}

const EditToolName = "edit"

var (
	oldStringNotFoundErr        = fantasy.NewTextErrorResponse("old_string not found in file. Make sure it matches exactly, including whitespace and line breaks.")
	oldStringMultipleMatchesErr = fantasy.NewTextErrorResponse("old_string appears multiple times in the file. Please provide more context to ensure a unique match, or set replace_all to true")
)

//go:embed edit.md
var editDescription []byte

type editContext struct {
	ctx            context.Context
	permissions    permission.Service
	files          history.Service
	filetracker    filetracker.Service
	workingDir     string
	fuzzyThreshold float64
}

func NewEditTool(
	lspManager *lsp.Manager,
	permissions permission.Service,
	files history.Service,
	filetracker filetracker.Service,
	workingDir string,
	fuzzyThreshold float64,
) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		EditToolName,
		string(editDescription),
		func(ctx context.Context, params EditParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.FilePath == "" {
				return fantasy.NewTextErrorResponse("file_path is required"), nil
			}

			effectiveWorkingDir := EffectiveWorkingDir(ctx, workingDir)

			resolvedPath, err := resolveLocalPlanURI(ctx, params.FilePath, effectiveWorkingDir)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			params.FilePath = ResolveToolPath(ctx, effectiveWorkingDir, resolvedPath).AbsolutePath

			var response fantasy.ToolResponse
			var opErr error

			editCtx := editContext{ctx, permissions, files, filetracker, effectiveWorkingDir, fuzzyThreshold}

			if len(params.FileOperations) > 0 {
				// Multi-file hashline mode: resolve and lock all target paths.
				absPaths := make([]string, 0, len(params.FileOperations))
				for _, fo := range params.FileOperations {
					if fo.FilePath == "" {
						return fantasy.NewTextErrorResponse("file_path is required for each file_operations entry"), nil
					}
					absPath := ResolveToolPath(ctx, effectiveWorkingDir, fo.FilePath).AbsolutePath
					absPaths = append(absPaths, absPath)
				}
				unlock := LockFilePaths(absPaths)
				defer unlock()
				response, opErr = applyFileHashlineOperations(editCtx, params.FileOperations, call)
			} else if params.Patch != "" {
				// Parse the patch to determine the real files being touched and
				// lock each one so that concurrent writers serialize on the
				// actual target paths, not just the primary file_path.
				patches, parseErr := ParseUnifiedPatch(params.Patch)
				if parseErr != nil {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to parse unified patch: %v", parseErr)), nil
				}
				if len(patches) == 0 {
					return fantasy.NewTextErrorResponse("No file patches found in unified patch content."), nil
				}
				pathSet := make(map[string]struct{})
				for _, p := range patches {
					pPath := p.NewPath
					if pPath == "" {
						pPath = p.OldPath
					}
					if pPath == "" || pPath == "/dev/null" {
						pPath = params.FilePath
					}
					if pPath == "" {
						return fantasy.NewTextErrorResponse("missing file path in patch header"), nil
					}
					absPath := ResolveToolPath(ctx, effectiveWorkingDir, pPath).AbsolutePath
					pathSet[absPath] = struct{}{}
				}
				absPaths := make([]string, 0, len(pathSet))
				for p := range pathSet {
					absPaths = append(absPaths, p)
				}
				unlock := LockFilePaths(absPaths)
				defer unlock()
				response, opErr = applyUnifiedPatch(editCtx, params.FilePath, params.Patch, call)
			} else {
				if response, blocked, guardErr := enforcePlanModeWriteTarget(ctx, params.FilePath); blocked || guardErr != nil {
					return response, guardErr
				}
				// Serialize concurrent writers to the same file path.
				pathMu := FilePathLockFor(params.FilePath)
				pathMu.Lock()
				defer pathMu.Unlock()
				if len(params.Operations) > 0 {
					response, opErr = applyHashlineEdit(editCtx, params.FilePath, params.Operations, call)
				} else if len(params.Edits) > 0 {
					response, opErr = applyEditEntries(editCtx, params.FilePath, params.Edits, call)
				} else if params.OldString == "" {
					response, opErr = createNewFile(editCtx, params.FilePath, params.NewString, call)
				} else if params.NewString == "" {
					response, opErr = deleteContent(editCtx, params.FilePath, params.OldString, params.ReplaceAll, call)
				} else {
					response, opErr = replaceContent(editCtx, params.FilePath, params.OldString, params.NewString, params.ReplaceAll, call)
				}
			}

			if opErr != nil {
				return response, opErr
			}
			if response.IsError {
				response.Content = strings.ReplaceAll(response.Content, params.FilePath, FormatToolPath(params.FilePath, effectiveWorkingDir))
				return response, nil
			}
			enrichClientFSEditMetadata(ctx, &response, effectiveWorkingDir)

			modifiedFiles := []string{params.FilePath}
			if len(params.FileOperations) > 0 && response.Metadata != "" {
				var meta []EditResponseMetadata
				if err := json.Unmarshal([]byte(response.Metadata), &meta); err == nil {
					modifiedFiles = nil
					for _, m := range meta {
						modifiedFiles = append(modifiedFiles, m.FilePath)
					}
				}
			} else if params.Patch != "" && response.Metadata != "" {
				var meta []EditResponseMetadata
				if err := json.Unmarshal([]byte(response.Metadata), &meta); err == nil {
					modifiedFiles = nil
					for _, m := range meta {
						modifiedFiles = append(modifiedFiles, m.FilePath)
					}
				}
			}

			sessionID := GetSessionFromContext(ctx)

			for _, file := range modifiedFiles {
				if client, _, _, ok := lspClientForFile(ctx, lspManager, file); ok {
					_ = client.OpenFileOnDemand(ctx, file)
					_ = client.NotifyChange(ctx, file)
				}
			}

			text := fmt.Sprintf("<result>\n%s\n</result>\n", response.Content)
			response.Content = text

			if !response.IsError {
				if adoptErr := adoptPlanFilePathIfNeeded(ctx, params.FilePath); adoptErr != nil {
					slog.Error("Failed to adopt plan file path", "error", adoptErr)
				}
			}

			messageSvc := GetMessageServiceFromContext(ctx)
			toolCallID := call.ID

			go func() {
				if lspManager == nil {
					return
				}
				bgCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
				defer cancel()

				var wg sync.WaitGroup
				for _, file := range modifiedFiles {
					for client := range lspManager.Clients().Seq() {
						if !client.HandlesFile(file) {
							continue
						}
						wg.Add(1)
						go func(c *lsp.Client, f string) {
							defer wg.Done()
							c.WaitForDiagnostics(bgCtx, 25*time.Second)
						}(client, file)
					}
				}
				wg.Wait()

				diags := getDiagnosticsForFiles(modifiedFiles, lspManager)
				if diags == "" {
					return
				}

				if messageSvc == nil || sessionID == "" || toolCallID == "" {
					return
				}

				var toolMsg message.Message
				var found bool
				for i := 0; i < 20; i++ {
					msgs, err := messageSvc.List(bgCtx, sessionID)
					if err == nil {
						for _, m := range msgs {
							if m.Role == message.Tool {
								for _, part := range m.Parts {
									if tr, ok := part.(message.ToolResult); ok && tr.ToolCallID == toolCallID {
										toolMsg = m
										found = true
										break
									}
								}
							}
							if found {
								break
							}
						}
					}
					if found {
						break
					}
					select {
					case <-bgCtx.Done():
						return
					case <-time.After(200 * time.Millisecond):
					}
				}

				if !found {
					slog.Warn("Async diagnostics: tool result message not found", "toolCallID", toolCallID)
					return
				}

				for idx, part := range toolMsg.Parts {
					if tr, ok := part.(message.ToolResult); ok && tr.ToolCallID == toolCallID {
						tr.Content = tr.Content + diags
						toolMsg.Parts[idx] = tr
						break
					}
				}

				err := messageSvc.Update(bgCtx, toolMsg)
				if err != nil {
					slog.Error("Async diagnostics: failed to update message", "error", err)
				}
			}()

			return response, nil
		})
}

func applyEditEntries(edit editContext, filePath string, entries []EditEntry, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	// Handle file creation case (first entry has empty old_string).
	if len(entries) > 0 && entries[0].OldString == "" {
		return applyEditEntriesWithCreation(edit, filePath, entries, call)
	}
	return applyEditEntriesExistingFile(edit, filePath, entries, call)
}

func applyEditEntriesWithCreation(edit editContext, filePath string, entries []EditEntry, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	firstEntry := entries[0]
	if firstEntry.OldString != "" {
		return fantasy.NewTextErrorResponse("first edit must have empty old_string for file creation"), nil
	}

	if _, err := clientfs.Stat(edit.ctx, filePath); err == nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("file already exists: %s", filePath)), nil
	} else if !os.IsNotExist(err) {
		return fantasy.ToolResponse{}, fmt.Errorf("failed to access file: %w", err)
	}

	dir := filepath.Dir(filePath)
	if err := clientfs.MkdirAll(edit.ctx, dir, 0o755); err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("failed to create parent directories: %w", err)
	}

	currentContent := firstEntry.NewString

	var failedEdits []FailedEdit
	for i := 1; i < len(entries); i++ {
		e := entries[i]
		newContent, err := applyEntryToContent(currentContent, e, edit.fuzzyThreshold)
		if err != nil {
			failedEdits = append(failedEdits, FailedEdit{
				Index: i + 1,
				Error: err.Error(),
				Edit:  EditEntry{OldString: e.OldString, NewString: e.NewString, ReplaceAll: e.ReplaceAll},
			})
			continue
		}
		currentContent = newContent
	}

	if len(failedEdits) > 0 {
		return fantasy.WithResponseMetadata(
			fantasy.NewTextErrorResponse(fmt.Sprintf("Preflight validation failed: %d edit(s) failed to apply. File was not created.", len(failedEdits))),
			EditResponseMetadata{
				FilePath:     filePath,
				EditsApplied: 0,
				EditsFailed:  failedEdits,
			},
		), nil
	}

	sessionID := GetSessionFromContext(edit.ctx)
	if sessionID == "" {
		return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for creating a new file")
	}

	_, additions, removals := diff.GenerateDiff("", currentContent, strings.TrimPrefix(filePath, edit.workingDir))

	editsApplied := len(entries) - len(failedEdits)
	var description string
	if len(failedEdits) > 0 {
		description = fmt.Sprintf("Create file %s with %d of %d edits (%d failed)", filePath, editsApplied, len(entries), len(failedEdits))
	} else {
		description = fmt.Sprintf("Create file %s with %d edits", filePath, editsApplied)
	}
	p, err := edit.permissions.Request(edit.ctx, permission.CreatePermissionRequest{
		SessionID:          sessionID,
		AuthoritySessionID: ResolveAuthoritySessionID(edit.ctx, sessionID),
		Path:               fsext.PathOrPrefix(filePath, edit.workingDir),
		ToolCallID:         call.ID,
		ToolName:           EditToolName,
		Action:             "write",
		Description:        description,
		Params: EditPermissionsParams{
			FilePath:   filePath,
			OldContent: "",
			NewContent: currentContent,
		},
	})
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if !p {
		return fantasy.ToolResponse{}, permission.ErrorPermissionDenied
	}

	if err := clientfs.WriteFile(edit.ctx, filePath, []byte(currentContent), 0o644); err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("failed to write file: %w", err)
	}
	GlobalFileCache.Put(sessionID, filePath, splitIntoLines(currentContent))

	_, err = edit.files.Create(edit.ctx, sessionID, filePath, "")
	if err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("error creating file history: %w", err)
	}
	_, err = edit.files.CreateVersion(edit.ctx, sessionID, filePath, currentContent)
	if err != nil {
		slog.Error("Error creating file history version", "error", err)
	}

	edit.filetracker.RecordRead(edit.ctx, sessionID, filePath)

	var message string
	if len(failedEdits) > 0 {
		message = fmt.Sprintf("File created with %d of %d edits: %s (%d edit(s) failed)", editsApplied, len(entries), filePath, len(failedEdits))
	} else {
		message = fmt.Sprintf("File created with %d edits: %s", len(entries), filePath)
	}

	return fantasy.WithResponseMetadata(
		fantasy.NewTextResponse(message),
		EditResponseMetadata{
			FilePath:     filePath,
			OldContent:   "",
			NewContent:   currentContent,
			Additions:    additions,
			Removals:     removals,
			EditsApplied: editsApplied,
			EditsFailed:  failedEdits,
		},
	), nil
}

func applyEditEntriesExistingFile(edit editContext, filePath string, entries []EditEntry, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	if err := checkPreflightLimits(edit.ctx, []string{filePath}); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}

	path := ResolveToolPath(edit.ctx, edit.workingDir, filePath)
	fileInfo, err := clientfs.Stat(edit.ctx, filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return fantasy.WithResponseMetadata(
				fantasy.NewTextErrorResponse(fmt.Sprintf("file not found: %s", path.DisplayPath)),
				NewToolPathErrorMetadata(path, "path_not_found", "Use glob or read the parent directory to verify the path before editing."),
			), nil
		}
		return fantasy.WithResponseMetadata(
			fantasy.NewTextErrorResponse(fmt.Sprintf("failed to access file: %v", err)),
			NewToolPathErrorMetadata(path, "stat_failed", "Verify the parent directory and path before retrying."),
		), nil
	}

	if fileInfo.IsDir() {
		return fantasy.WithResponseMetadata(
			fantasy.NewTextErrorResponse(fmt.Sprintf("path is a directory, not a file: %s", path.DisplayPath)),
			NewToolPathErrorMetadata(path, "directory_not_file", "Use read on the directory to inspect its contents."),
		), nil
	}

	sessionID := GetSessionFromContext(edit.ctx)
	if sessionID == "" {
		return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for editing file")
	}

	content, err := clientfs.ReadFile(edit.ctx, filePath)
	if err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("failed to read file: %w", err)
	}

	oldContent, isCrlf := fsext.ToUnixLineEndings(string(content))
	currentContent := oldContent

	var failedEdits []FailedEdit
	for i, e := range entries {
		newContent, applyErr := applyEntryToContent(currentContent, e, edit.fuzzyThreshold)
		if applyErr != nil {
			failedEdits = append(failedEdits, FailedEdit{
				Index: i + 1,
				Error: applyErr.Error(),
				Edit:  EditEntry{OldString: e.OldString, NewString: e.NewString, ReplaceAll: e.ReplaceAll},
			})
			continue
		}
		currentContent = newContent
	}

	if len(failedEdits) > 0 {
		return fantasy.WithResponseMetadata(
			fantasy.NewTextErrorResponse(fmt.Sprintf("Preflight validation failed: %d edit(s) failed to apply. No changes were written.", len(failedEdits))),
			EditResponseMetadata{
				FilePath:     filePath,
				EditsApplied: 0,
				EditsFailed:  failedEdits,
			},
		), nil
	}

	if oldContent == currentContent {
		return fantasy.WithResponseMetadata(
			fantasy.NewTextResponse("No changes were needed. The file is already in the desired state."),
			EditResponseMetadata{
				FilePath:     filePath,
				OldContent:   oldContent,
				NewContent:   currentContent,
				Additions:    0,
				Removals:     0,
				EditsApplied: 0,
				EditsFailed:  failedEdits,
			},
		), nil
	}

	_, additions, removals := diff.GenerateDiff(oldContent, currentContent, strings.TrimPrefix(filePath, edit.workingDir))

	editsApplied := len(entries) - len(failedEdits)
	var description string
	if len(failedEdits) > 0 {
		description = fmt.Sprintf("Apply %d of %d edits to file %s (%d failed)", editsApplied, len(entries), filePath, len(failedEdits))
	} else {
		description = fmt.Sprintf("Apply %d edits to file %s", editsApplied, filePath)
	}

	p, err := edit.permissions.Request(edit.ctx, permission.CreatePermissionRequest{
		SessionID:          sessionID,
		AuthoritySessionID: ResolveAuthoritySessionID(edit.ctx, sessionID),
		Path:               fsext.PathOrPrefix(filePath, edit.workingDir),
		ToolCallID:         call.ID,
		ToolName:           EditToolName,
		Action:             "write",
		Description:        description,
		Params: EditPermissionsParams{
			FilePath:   filePath,
			OldContent: oldContent,
			NewContent: currentContent,
		},
	})
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if !p {
		return fantasy.ToolResponse{}, permission.ErrorPermissionDenied
	}

	fileContent := currentContent
	if isCrlf {
		fileContent, _ = fsext.ToWindowsLineEndings(currentContent)
	}

	if err := clientfs.WriteFile(edit.ctx, filePath, []byte(fileContent), 0o644); err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("failed to write file: %w", err)
	}
	GlobalFileCache.Put(sessionID, filePath, splitIntoLines(currentContent))

	file, err := edit.files.GetByPathAndSession(edit.ctx, filePath, sessionID)
	if err != nil {
		_, err = edit.files.Create(edit.ctx, sessionID, filePath, oldContent)
		if err != nil {
			return fantasy.ToolResponse{}, fmt.Errorf("error creating file history: %w", err)
		}
	} else if file.Content != oldContent {
		_, err = edit.files.CreateVersion(edit.ctx, sessionID, filePath, oldContent)
		if err != nil {
			slog.Error("Error creating file history version", "error", err)
		}
	}

	_, err = edit.files.CreateVersion(edit.ctx, sessionID, filePath, currentContent)
	if err != nil {
		slog.Error("Error creating file history version", "error", err)
	}

	edit.filetracker.RecordRead(edit.ctx, sessionID, filePath)

	var message string
	if len(failedEdits) > 0 {
		message = fmt.Sprintf("Applied %d of %d edits to file: %s (%d edit(s) failed)", editsApplied, len(entries), FormatToolPath(filePath, edit.workingDir), len(failedEdits))
	} else {
		message = fmt.Sprintf("Applied %d edits to file: %s", len(entries), FormatToolPath(filePath, edit.workingDir))
	}

	return fantasy.WithResponseMetadata(
		fantasy.NewTextResponse(message),
		EditResponseMetadata{
			FilePath:     filePath,
			OldContent:   oldContent,
			NewContent:   currentContent,
			Additions:    additions,
			Removals:     removals,
			EditsApplied: editsApplied,
			EditsFailed:  failedEdits,
		},
	), nil
}

// applyEntryToContent applies a single EditEntry to content, using fuzzy matching as fallback.
func applyEntryToContent(content string, entry EditEntry, fuzzyThreshold float64) (string, error) {
	if entry.OldString == "" && entry.NewString == "" {
		return content, nil
	}
	if entry.OldString == "" {
		return "", fmt.Errorf("old_string cannot be empty for content replacement")
	}

	if entry.ReplaceAll {
		if strings.Contains(content, entry.OldString) {
			return strings.ReplaceAll(content, entry.OldString, entry.NewString), nil
		}
		result, ok := fuzzyReplace(content, entry.OldString, entry.NewString, true, fuzzyThreshold)
		if !ok {
			return "", fmt.Errorf("%s", buildDetailedMatchError(content, entry.OldString))
		}
		return result, nil
	}

	index := strings.Index(content, entry.OldString)
	if index == -1 {
		result, ok := fuzzyReplace(content, entry.OldString, entry.NewString, false, fuzzyThreshold)
		if !ok {
			return "", fmt.Errorf("%s", buildDetailedMatchError(content, entry.OldString))
		}
		return result, nil
	}

	lastIndex := strings.LastIndex(content, entry.OldString)
	if index != lastIndex {
		return "", fmt.Errorf("old_string appears multiple times in the content. Please provide more context to ensure a unique match, or set replace_all to true")
	}

	return content[:index] + entry.NewString + content[index+len(entry.OldString):], nil
}

func createNewFile(edit editContext, filePath, content string, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	fileInfo, err := clientfs.Stat(edit.ctx, filePath)
	if err == nil {
		if fileInfo.IsDir() {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("path is a directory, not a file: %s", filePath)), nil
		}
		return fantasy.NewTextErrorResponse(fmt.Sprintf("file already exists: %s", filePath)), nil
	} else if !os.IsNotExist(err) {
		return fantasy.ToolResponse{}, fmt.Errorf("failed to access file: %w", err)
	}

	dir := filepath.Dir(filePath)
	if err = clientfs.MkdirAll(edit.ctx, dir, 0o755); err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("failed to create parent directories: %w", err)
	}

	sessionID := GetSessionFromContext(edit.ctx)
	if sessionID == "" {
		return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for creating a new file")
	}

	_, additions, removals := diff.GenerateDiff(
		"",
		content,
		strings.TrimPrefix(filePath, edit.workingDir),
	)
	p, err := edit.permissions.Request(edit.ctx,
		permission.CreatePermissionRequest{
			SessionID:          sessionID,
			AuthoritySessionID: ResolveAuthoritySessionID(edit.ctx, sessionID),
			Path:               fsext.PathOrPrefix(filePath, edit.workingDir),
			ToolCallID:         call.ID,
			ToolName:           EditToolName,
			Action:             "write",
			Description:        fmt.Sprintf("Create file %s", filePath),
			Params: EditPermissionsParams{
				FilePath:   filePath,
				OldContent: "",
				NewContent: content,
			},
		},
	)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if !p {
		return fantasy.ToolResponse{}, permission.ErrorPermissionDenied
	}

	err = clientfs.WriteFile(edit.ctx, filePath, []byte(content), 0o644)
	if err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("failed to write file: %w", err)
	}
	GlobalFileCache.Put(sessionID, filePath, splitIntoLines(content))

	// File can't be in the history so we create a new file history
	_, err = edit.files.Create(edit.ctx, sessionID, filePath, "")
	if err != nil {
		// Log error but don't fail the operation
		return fantasy.ToolResponse{}, fmt.Errorf("error creating file history: %w", err)
	}

	// Add the new content to the file history
	_, err = edit.files.CreateVersion(edit.ctx, sessionID, filePath, content)
	if err != nil {
		// Log error but don't fail the operation
		slog.Error("Error creating file history version", "error", err)
	}

	edit.filetracker.RecordRead(edit.ctx, sessionID, filePath)

	return fantasy.WithResponseMetadata(
		fantasy.NewTextResponse("File created: "+FormatToolPath(filePath, edit.workingDir)),
		EditResponseMetadata{
			FilePath:   filePath,
			OldContent: "",
			NewContent: content,
			Additions:  additions,
			Removals:   removals,
		},
	), nil
}

func deleteContent(edit editContext, filePath, oldString string, replaceAll bool, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	fileInfo, err := clientfs.Stat(edit.ctx, filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("file not found: %s", filePath)), nil
		}
		return fantasy.ToolResponse{}, fmt.Errorf("failed to access file: %w", err)
	}

	if fileInfo.IsDir() {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("path is a directory, not a file: %s", filePath)), nil
	}

	sessionID := GetSessionFromContext(edit.ctx)
	if sessionID == "" {
		return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for deleting content")
	}

	content, err := clientfs.ReadFile(edit.ctx, filePath)
	if err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("failed to read file: %w", err)
	}

	oldContent, isCrlf := fsext.ToUnixLineEndings(string(content))

	var newContent string

	if replaceAll {
		if strings.Contains(oldContent, oldString) {
			newContent = strings.ReplaceAll(oldContent, oldString, "")
		} else {
			var ok bool
			newContent, ok = fuzzyReplace(oldContent, oldString, "", true, edit.fuzzyThreshold)
			if !ok {
				return fantasy.NewTextErrorResponse(buildDetailedMatchError(oldContent, oldString)), nil
			}
		}
	} else {
		index := strings.Index(oldContent, oldString)
		if index == -1 {
			var ok bool
			newContent, ok = fuzzyReplace(oldContent, oldString, "", false, edit.fuzzyThreshold)
			if !ok {
				return fantasy.NewTextErrorResponse(buildDetailedMatchError(oldContent, oldString)), nil
			}
		} else {
			lastIndex := strings.LastIndex(oldContent, oldString)
			if index != lastIndex {
				return fantasy.NewTextErrorResponse("old_string appears multiple times in the file. Please provide more context to ensure a unique match, or set replace_all to true"), nil
			}
			newContent = oldContent[:index] + oldContent[index+len(oldString):]
		}
	}

	_, additions, removals := diff.GenerateDiff(
		oldContent,
		newContent,
		strings.TrimPrefix(filePath, edit.workingDir),
	)

	p, err := edit.permissions.Request(edit.ctx,
		permission.CreatePermissionRequest{
			SessionID:          sessionID,
			AuthoritySessionID: ResolveAuthoritySessionID(edit.ctx, sessionID),
			Path:               fsext.PathOrPrefix(filePath, edit.workingDir),
			ToolCallID:         call.ID,
			ToolName:           EditToolName,
			Action:             "write",
			Description:        fmt.Sprintf("Delete content from file %s", filePath),
			Params: EditPermissionsParams{
				FilePath:   filePath,
				OldContent: oldContent,
				NewContent: newContent,
			},
		},
	)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if !p {
		return fantasy.ToolResponse{}, permission.ErrorPermissionDenied
	}

	fileContent := newContent
	if isCrlf {
		fileContent, _ = fsext.ToWindowsLineEndings(newContent)
	}

	err = clientfs.WriteFile(edit.ctx, filePath, []byte(fileContent), 0o644)
	if err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("failed to write file: %w", err)
	}
	GlobalFileCache.Put(sessionID, filePath, splitIntoLines(newContent))

	// Check if file exists in history
	file, err := edit.files.GetByPathAndSession(edit.ctx, filePath, sessionID)
	if err != nil {
		_, err = edit.files.Create(edit.ctx, sessionID, filePath, oldContent)
		if err != nil {
			// Log error but don't fail the operation
			return fantasy.ToolResponse{}, fmt.Errorf("error creating file history: %w", err)
		}
	} else if file.Content != oldContent {
		// User manually changed the content; store an intermediate version
		_, err = edit.files.CreateVersion(edit.ctx, sessionID, filePath, oldContent)
		if err != nil {
			slog.Error("Error creating file history version", "error", err)
		}
	}
	// Store the new version
	_, err = edit.files.CreateVersion(edit.ctx, sessionID, filePath, newContent)
	if err != nil {
		slog.Error("Error creating file history version", "error", err)
	}

	edit.filetracker.RecordRead(edit.ctx, sessionID, filePath)

	return fantasy.WithResponseMetadata(
		fantasy.NewTextResponse("Content deleted from file: "+FormatToolPath(filePath, edit.workingDir)),
		EditResponseMetadata{
			FilePath:   filePath,
			OldContent: oldContent,
			NewContent: newContent,
			Additions:  additions,
			Removals:   removals,
		},
	), nil
}

func replaceContent(edit editContext, filePath, oldString, newString string, replaceAll bool, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	fileInfo, err := clientfs.Stat(edit.ctx, filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("file not found: %s", filePath)), nil
		}
		return fantasy.ToolResponse{}, fmt.Errorf("failed to access file: %w", err)
	}

	if fileInfo.IsDir() {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("path is a directory, not a file: %s", filePath)), nil
	}

	sessionID := GetSessionFromContext(edit.ctx)
	if sessionID == "" {
		return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for edit a file")
	}

	content, err := clientfs.ReadFile(edit.ctx, filePath)
	if err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("failed to read file: %w", err)
	}

	oldContent, isCrlf := fsext.ToUnixLineEndings(string(content))

	var newContent string

	if replaceAll {
		if strings.Contains(oldContent, oldString) {
			newContent = strings.ReplaceAll(oldContent, oldString, newString)
		} else {
			var ok bool
			newContent, ok = fuzzyReplace(oldContent, oldString, newString, true, edit.fuzzyThreshold)
			if !ok {
				return fantasy.NewTextErrorResponse(buildDetailedMatchError(oldContent, oldString)), nil
			}
		}
	} else {
		index := strings.Index(oldContent, oldString)
		if index == -1 {
			var ok bool
			newContent, ok = fuzzyReplace(oldContent, oldString, newString, false, edit.fuzzyThreshold)
			if !ok {
				return fantasy.NewTextErrorResponse(buildDetailedMatchError(oldContent, oldString)), nil
			}
		} else {
			lastIndex := strings.LastIndex(oldContent, oldString)
			if index != lastIndex {
				return oldStringMultipleMatchesErr, nil
			}
			newContent = oldContent[:index] + newString + oldContent[index+len(oldString):]
		}
	}

	if oldContent == newContent {
		return fantasy.WithResponseMetadata(
			fantasy.NewTextResponse("No changes were needed. The file is already in the desired state."),
			EditResponseMetadata{
				FilePath:     filePath,
				OldContent:   oldContent,
				NewContent:   newContent,
				Additions:    0,
				Removals:     0,
				EditsApplied: 0,
			},
		), nil
	}
	_, additions, removals := diff.GenerateDiff(
		oldContent,
		newContent,
		strings.TrimPrefix(filePath, edit.workingDir),
	)

	p, err := edit.permissions.Request(edit.ctx,
		permission.CreatePermissionRequest{
			SessionID:          sessionID,
			AuthoritySessionID: ResolveAuthoritySessionID(edit.ctx, sessionID),
			Path:               fsext.PathOrPrefix(filePath, edit.workingDir),
			ToolCallID:         call.ID,
			ToolName:           EditToolName,
			Action:             "write",
			Description:        fmt.Sprintf("Replace content in file %s", filePath),
			Params: EditPermissionsParams{
				FilePath:   filePath,
				OldContent: oldContent,
				NewContent: newContent,
			},
		},
	)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if !p {
		return fantasy.ToolResponse{}, permission.ErrorPermissionDenied
	}

	fileContent := newContent
	if isCrlf {
		fileContent, _ = fsext.ToWindowsLineEndings(newContent)
	}

	err = clientfs.WriteFile(edit.ctx, filePath, []byte(fileContent), 0o644)
	if err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("failed to write file: %w", err)
	}
	GlobalFileCache.Put(sessionID, filePath, splitIntoLines(newContent))

	// Check if file exists in history
	file, err := edit.files.GetByPathAndSession(edit.ctx, filePath, sessionID)
	if err != nil {
		_, err = edit.files.Create(edit.ctx, sessionID, filePath, oldContent)
		if err != nil {
			// Log error but don't fail the operation
			return fantasy.ToolResponse{}, fmt.Errorf("error creating file history: %w", err)
		}
	} else if file.Content != oldContent {
		// User manually changed the content; store an intermediate version
		_, err = edit.files.CreateVersion(edit.ctx, sessionID, filePath, oldContent)
		if err != nil {
			slog.Debug("Error creating file history version", "error", err)
		}
	}
	// Store the new version
	_, err = edit.files.CreateVersion(edit.ctx, sessionID, filePath, newContent)
	if err != nil {
		slog.Error("Error creating file history version", "error", err)
	}

	edit.filetracker.RecordRead(edit.ctx, sessionID, filePath)

	return fantasy.WithResponseMetadata(
		fantasy.NewTextResponse("Content replaced in file: "+FormatToolPath(filePath, edit.workingDir)),
		EditResponseMetadata{
			FilePath:   filePath,
			OldContent: oldContent,
			NewContent: newContent,
			Additions:  additions,
			Removals:   removals,
		}), nil
}

// splitIntoLines splits text into lines, removing a single trailing newline if present.
func splitIntoLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// getLineIndent returns the leading whitespace of a line.
func getLineIndent(line string) string {
	for i, c := range line {
		if c != ' ' && c != '\t' {
			return line[:i]
		}
	}
	return line
}

// getFirstNonEmptyLineIndent returns the indentation of the first non-empty line in text.
func getFirstNonEmptyLineIndent(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) != "" {
			return getLineIndent(line)
		}
	}
	return ""
}

// adjustIndentation adjusts newStr's indentation to match the actual matched text.
// When fuzzy matching finds oldStr at a different indentation than provided,
// this corrects newStr to use the same indentation as the actual match.
func adjustIndentation(oldStr, actualMatch, newStr string) string {
	oldIndent := getFirstNonEmptyLineIndent(oldStr)
	actualIndent := getFirstNonEmptyLineIndent(actualMatch)
	if oldIndent == actualIndent {
		return newStr
	}
	lines := strings.Split(newStr, "\n")
	result := make([]string, len(lines))
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			result[i] = line
			continue
		}
		lineIndent := getLineIndent(line)
		if strings.HasPrefix(lineIndent, oldIndent) {
			result[i] = actualIndent + line[len(oldIndent):]
		} else {
			result[i] = line
		}
	}
	return strings.Join(result, "\n")
}

func checkPreflightLimits(ctx context.Context, filePaths []string) error {
	var totalSize int64
	for _, fp := range filePaths {
		fi, err := clientfs.Stat(ctx, fp)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		sz := fi.Size()
		if sz > 1*1024*1024 {
			return fmt.Errorf("file %s size exceeds the 1MB preflight limit (%d bytes)", fp, sz)
		}
		totalSize += sz
	}
	if totalSize > 5*1024*1024 {
		return fmt.Errorf("total file size exceeds the 5MB preflight limit (%d bytes)", totalSize)
	}
	return nil
}

// findBlockMatch searches contentLines for a consecutive block matching searchLines
// using the given line comparison function. Returns matched blocks or nil if not
// found or ambiguous (multiple matches for non-replaceAll).
func findBlockMatch(contentLines, searchLines []string, lineEq func(a, b string) bool, replaceAll bool) ([]string, bool) {
	n := len(searchLines)
	if n == 0 {
		return nil, false
	}
	var matches []string
	for i := 0; i <= len(contentLines)-n; i++ {
		ok := true
		for j := 0; j < n; j++ {
			if !lineEq(contentLines[i+j], searchLines[j]) {
				ok = false
				break
			}
		}
		if ok {
			matches = append(matches, strings.Join(contentLines[i:i+n], "\n"))
		}
	}
	if len(matches) == 0 {
		return nil, false
	}
	if !replaceAll && len(matches) > 1 {
		return nil, false // ambiguous for single replace
	}
	return matches, true
}

// commonIndentPrefix returns the common leading whitespace shared by all non-empty lines.
func commonIndentPrefix(lines []string) string {
	prefix := ""
	first := true
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		indent := getLineIndent(l)
		if first {
			prefix = indent
			first = false
		} else {
			for len(prefix) > 0 && !strings.HasPrefix(indent, prefix) {
				prefix = prefix[:len(prefix)-1]
			}
		}
	}
	return prefix
}

// applyMatches applies replacements for all found matches.
func applyMatches(content, oldString, newString string, matches []string, replaceAll bool) string {
	result := content
	for _, match := range matches {
		adjusted := adjustIndentation(oldString, match, newString)
		if replaceAll {
			result = strings.ReplaceAll(result, match, adjusted)
		} else {
			result = strings.Replace(result, match, adjusted, 1)
			break
		}
	}
	return result
}

// applyHashlineEdit applies LINE#HASH-addressed operations to an existing file.
// It is used when the model provides params.Operations instead of params.Edits or params.OldString.
func applyHashlineEdit(edit editContext, filePath string, operations []HashlineEditOperation, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	sessionID := GetSessionFromContext(edit.ctx)
	if sessionID == "" {
		return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for editing file")
	}

	fileInfo, err := clientfs.Stat(edit.ctx, filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("file not found: %s", filePath)), nil
		}
		return fantasy.ToolResponse{}, fmt.Errorf("failed to access file: %w", err)
	}
	if fileInfo.IsDir() {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("path is a directory, not a file: %s", filePath)), nil
	}

	content, err := clientfs.ReadFile(edit.ctx, filePath)
	if err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("failed to read file: %w", err)
	}

	oldContent, isCrlf := fsext.ToUnixLineEndings(string(content))
	oldLines, hadTrailingNewline := splitHashlineFileLines(oldContent)

	parsedOps, err := parseHashlineOperations(operations, oldLines)

	// Pre-pass: capture CUT operations' lines and convert them to replace_range
	// deletions. The captures are NOT committed to the global clipboard here --
	// only once permission is granted and the file is actually written (below) --
	// so a denied or failed edit never leaves a register claiming lines were cut
	// when the source file was never modified. pendingCuts lets a PASTE later in
	// this same batch see freshly cut lines before that commit happens.
	pendingCuts := collectPendingHashlineCuts(parsedOps, oldLines)
	for i, op := range parsedOps {
		if op.Operation == hashlineEditOpCut {
			startLine := op.Start.Line
			endLine := op.End.Line
			if startLine < 1 || endLine > len(oldLines) || startLine > endLine {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("operation %d (cut): invalid line range %d-%d (file has %d lines)", i+1, startLine, endLine, len(oldLines))), nil
			}
			parsedOps[i].Operation = hashlineEditOpReplaceRange
			parsedOps[i].ContentLines = nil
		}
	}

	// Resolve PASTE operations: prefer lines cut earlier in this same batch,
	// falling back to the persisted clipboard for cross-call transfers.
	if resp := resolvePendingHashlinePastes(parsedOps, pendingCuts, sessionID); resp != nil {
		return *resp, nil
	}

	var newLines []string
	var recovered bool
	if err != nil {
		var recoverErr error
		newLines, recoverErr = tryRecoverHashline(sessionID, filePath, oldLines, operations)
		if recoverErr != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("%s; recovery failed: %v", err, recoverErr)), nil
		}
		recovered = true
	} else {
		var applyErr error
		newLines, applyErr = applyHashlineOperations(oldLines, parsedOps)
		if applyErr != nil {
			var recoverErr error
			newLines, recoverErr = tryRecoverHashline(sessionID, filePath, oldLines, operations)
			if recoverErr != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("%s; recovery failed: %v", applyErr, recoverErr)), nil
			}
			recovered = true
		}
	}

	newContent := joinHashlineFileLines(newLines, hadTrailingNewline)
	if newContent == oldContent {
		// Still commit any named-register CUTs even though the net content is
		// unchanged (e.g. a cut immediately pasted back in place) -- the
		// register capture is a real outcome the model may rely on later.
		commitPendingHashlineCuts(pendingCuts, sessionID)
		GlobalClipboard.ClearAnonymous(sessionID)
		return fantasy.WithResponseMetadata(
			fantasy.NewTextResponse("No changes were needed. The file is already in the desired state."),
			EditResponseMetadata{
				FilePath:   filePath,
				OldContent: oldContent,
				NewContent: newContent,
				Additions:  0,
				Removals:   0,
			},
		), nil
	}

	_, additions, removals := diff.GenerateDiff(
		oldContent,
		newContent,
		strings.TrimPrefix(filePath, edit.workingDir),
	)

	permissionResponse, err := RequestPermission(edit.ctx, edit.permissions,
		permission.CreatePermissionRequest{
			SessionID:          sessionID,
			AuthoritySessionID: ResolveAuthoritySessionID(edit.ctx, sessionID),
			Path:               fsext.PathOrPrefix(filePath, edit.workingDir),
			ToolCallID:         call.ID,
			ToolName:           EditToolName,
			Action:             "write",
			Description:        fmt.Sprintf("Apply %d hashline operation(s) to file %s", len(operations), filePath),
			Params: EditPermissionsParams{
				FilePath:   filePath,
				OldContent: oldContent,
				NewContent: newContent,
			},
		},
	)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if permissionResponse != nil {
		return *permissionResponse, nil
	}

	fileContent := newContent
	if isCrlf {
		fileContent, _ = fsext.ToWindowsLineEndings(newContent)
	}

	if err := clientfs.WriteFile(edit.ctx, filePath, []byte(fileContent), 0o644); err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("failed to write file: %w", err)
	}
	GlobalFileCache.Put(sessionID, filePath, newLines)
	// Commit CUT captures to named registers now that the edit has actually
	// been written, and clear the anonymous register at the end of the batch.
	// Named registers persist across edit calls for cross-file move operations.
	commitPendingHashlineCuts(pendingCuts, sessionID)
	GlobalClipboard.ClearAnonymous(sessionID)

	file, err := edit.files.GetByPathAndSession(edit.ctx, filePath, sessionID)
	if err != nil {
		_, err = edit.files.Create(edit.ctx, sessionID, filePath, oldContent)
		if err != nil {
			return fantasy.ToolResponse{}, fmt.Errorf("error creating file history: %w", err)
		}
	} else if file.Content != oldContent {
		_, err = edit.files.CreateVersion(edit.ctx, sessionID, filePath, oldContent)
		if err != nil {
			slog.Error("Error creating file history version", "error", err)
		}
	}
	_, err = edit.files.CreateVersion(edit.ctx, sessionID, filePath, newContent)
	if err != nil {
		slog.Error("Error creating file history version", "error", err)
	}

	edit.filetracker.RecordRead(edit.ctx, sessionID, filePath)

	msgText := fmt.Sprintf("Applied %d hashline operation(s) to file: %s", len(operations), FormatToolPath(filePath, edit.workingDir))
	if recovered {
		msgText += "\n[Warning: Hashline anchor mismatch occurred due to concurrent changes. Successfully recovered and merged edits using 3-Way Merge.]"
	}

	return fantasy.WithResponseMetadata(
		fantasy.NewTextResponse(msgText),
		EditResponseMetadata{
			FilePath:   filePath,
			OldContent: oldContent,
			NewContent: newContent,
			Additions:  additions,
			Removals:   removals,
		},
	), nil
}

// applyFileHashlineOperations applies hashline operations across multiple files
// in a single atomic call. CUT operations in any file populate clipboard registers
// that PASTE operations in subsequent files can read, enabling cross-file moves.
func applyFileHashlineOperations(edit editContext, fileOps []FileHashlineOperations, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	sessionID := GetSessionFromContext(edit.ctx)
	if sessionID == "" {
		return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for multi-file hashline edit")
	}

	// Resolve all file paths and enforce plan-mode guards.
	absPaths := make([]string, 0, len(fileOps))
	for _, fo := range fileOps {
		absPath := ResolveToolPath(edit.ctx, edit.workingDir, fo.FilePath).AbsolutePath
		if response, blocked, guardErr := enforcePlanModeWriteTarget(edit.ctx, absPath); blocked || guardErr != nil {
			return response, guardErr
		}
		absPaths = append(absPaths, absPath)
	}

	if err := checkPreflightLimits(edit.ctx, absPaths); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}

	// Read all files upfront and parse operations.
	type fileState struct {
		path               string
		oldContent         string
		oldLines           []string
		isCrlf             bool
		hadTrailingNewline bool
		parsedOps          []parsedHashlineOperation
	}

	states := make([]fileState, 0, len(fileOps))
	// pendingCuts accumulates CUT captures across every file in the batch,
	// uncommitted, so a PASTE in a later file within this same call can see
	// lines cut earlier without touching the persisted clipboard yet (see
	// commitPendingHashlineCuts below for why that commit is deferred).
	var pendingCuts []pendingHashlineCut
	for i, fo := range fileOps {
		absPath := absPaths[i]

		fileInfo, err := clientfs.Stat(edit.ctx, absPath)
		if err != nil {
			if os.IsNotExist(err) {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("file not found: %s", FormatToolPath(absPath, edit.workingDir))), nil
			}
			return fantasy.ToolResponse{}, fmt.Errorf("failed to access file %s: %w", absPath, err)
		}
		if fileInfo.IsDir() {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("path is a directory, not a file: %s", FormatToolPath(absPath, edit.workingDir))), nil
		}

		content, err := clientfs.ReadFile(edit.ctx, absPath)
		if err != nil {
			return fantasy.ToolResponse{}, fmt.Errorf("failed to read file %s: %w", absPath, err)
		}

		oldContent, isCrlf := fsext.ToUnixLineEndings(string(content))
		oldLines, hadTrailingNewline := splitHashlineFileLines(oldContent)

		parsedOps, err := parseHashlineOperations(fo.Operations, oldLines)
		if err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("file %s: %s", FormatToolPath(absPath, edit.workingDir), err)), nil
		}

		// Capture CUT lines (without committing to the clipboard yet) and
		// convert them to replace_range deletions.
		pendingCuts = append(pendingCuts, collectPendingHashlineCuts(parsedOps, oldLines)...)
		for j, op := range parsedOps {
			if op.Operation == hashlineEditOpCut {
				startLine := op.Start.Line
				endLine := op.End.Line
				if startLine < 1 || endLine > len(oldLines) || startLine > endLine {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("file %s, operation %d (cut): invalid line range %d-%d (file has %d lines)", FormatToolPath(absPath, edit.workingDir), j+1, startLine, endLine, len(oldLines))), nil
				}
				parsedOps[j].Operation = hashlineEditOpReplaceRange
				parsedOps[j].ContentLines = nil
			}
		}

		states = append(states, fileState{
			path:               absPath,
			oldContent:         oldContent,
			oldLines:           oldLines,
			isCrlf:             isCrlf,
			hadTrailingNewline: hadTrailingNewline,
			parsedOps:          parsedOps,
		})
	}

	// Resolve PASTE operations across the whole batch: prefer lines cut
	// earlier in this call, falling back to the persisted clipboard for
	// transfers started in a previous call.
	for i := range states {
		if resp := resolvePendingHashlinePastes(states[i].parsedOps, pendingCuts, sessionID); resp != nil {
			return *resp, nil
		}
	}

	// Pass 1: compute every file's new content and request permission for
	// all of them. Nothing is written and no CUT is committed to the
	// clipboard yet, so a denial on any one file leaves every file (and the
	// clipboard) untouched -- matching the atomic guarantee this tool
	// documents.
	type preparedFile struct {
		fileState
		newLines   []string
		newContent string
		additions  int
		removals   int
		recovered  bool
	}

	prepared := make([]preparedFile, 0, len(states))
	for _, st := range states {
		newLines, applyErr := applyHashlineOperations(st.oldLines, st.parsedOps)
		recovered := false
		if applyErr != nil {
			var origOps []HashlineEditOperation
			for _, fo := range fileOps {
				if ResolveToolPath(edit.ctx, edit.workingDir, fo.FilePath).AbsolutePath == st.path {
					origOps = fo.Operations
					break
				}
			}
			var recoverErr error
			newLines, recoverErr = tryRecoverHashline(sessionID, st.path, st.oldLines, origOps)
			if recoverErr != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("file %s: %s; recovery failed: %v", FormatToolPath(st.path, edit.workingDir), applyErr, recoverErr)), nil
			}
			recovered = true
		}

		newContent := joinHashlineFileLines(newLines, st.hadTrailingNewline)

		_, additions, removals := diff.GenerateDiff(
			st.oldContent,
			newContent,
			strings.TrimPrefix(st.path, edit.workingDir),
		)

		p, permErr := edit.permissions.Request(edit.ctx,
			permission.CreatePermissionRequest{
				SessionID:          sessionID,
				AuthoritySessionID: ResolveAuthoritySessionID(edit.ctx, sessionID),
				Path:               fsext.PathOrPrefix(st.path, edit.workingDir),
				ToolCallID:         call.ID,
				ToolName:           EditToolName,
				Action:             "write",
				Description:        fmt.Sprintf("Apply hashline operations to file %s", st.path),
				Params: EditPermissionsParams{
					FilePath:   st.path,
					OldContent: st.oldContent,
					NewContent: newContent,
				},
			},
		)
		if permErr != nil {
			return fantasy.ToolResponse{}, permErr
		}
		if !p {
			return fantasy.ToolResponse{}, permission.ErrorPermissionDenied
		}

		prepared = append(prepared, preparedFile{
			fileState:  st,
			newLines:   newLines,
			newContent: newContent,
			additions:  additions,
			removals:   removals,
			recovered:  recovered,
		})
	}

	// Pass 2: every file's permission is granted, so the batch is committed
	// to happen -- commit CUT captures to the clipboard now, then write each
	// file. If a write fails partway through, best-effort roll back the
	// files already written in this call so a mid-batch I/O failure doesn't
	// leave a silent partial edit on disk.
	commitPendingHashlineCuts(pendingCuts, sessionID)

	var fileMetadata []EditResponseMetadata
	var successMessage strings.Builder
	successMessage.WriteString("Multi-file hashline operations applied:\n")
	var written []preparedFile

	for _, pf := range prepared {
		fileContent := pf.newContent
		if pf.isCrlf {
			fileContent, _ = fsext.ToWindowsLineEndings(pf.newContent)
		}

		if err := clientfs.WriteFile(edit.ctx, pf.path, []byte(fileContent), 0o644); err != nil {
			for _, w := range written {
				restoreContent := w.oldContent
				if w.isCrlf {
					restoreContent, _ = fsext.ToWindowsLineEndings(w.oldContent)
				}
				if rbErr := clientfs.WriteFile(edit.ctx, w.path, []byte(restoreContent), 0o644); rbErr != nil {
					slog.Error("Failed to roll back file after partial multi-file write failure", "path", w.path, "error", rbErr)
				} else {
					GlobalFileCache.Put(sessionID, w.path, w.oldLines)
				}
			}
			return fantasy.ToolResponse{}, fmt.Errorf("failed to write file %s: %w (rolled back %d previously written file(s) in this batch)", pf.path, err, len(written))
		}
		written = append(written, pf)

		GlobalFileCache.Put(sessionID, pf.path, pf.newLines)

		file, err := edit.files.GetByPathAndSession(edit.ctx, pf.path, sessionID)
		if err != nil {
			_, err = edit.files.Create(edit.ctx, sessionID, pf.path, pf.oldContent)
			if err != nil {
				slog.Error("Failed to create file history", "error", err, "path", pf.path)
			}
		} else if file.Content != pf.oldContent {
			_, err = edit.files.CreateVersion(edit.ctx, sessionID, pf.path, pf.oldContent)
			if err != nil {
				slog.Error("Failed to create version in history", "error", err, "path", pf.path)
			}
		}
		_, err = edit.files.CreateVersion(edit.ctx, sessionID, pf.path, pf.newContent)
		if err != nil {
			slog.Error("Failed to create version in history", "error", err, "path", pf.path)
		}

		edit.filetracker.RecordRead(edit.ctx, sessionID, pf.path)

		pathMeta := NewToolPathMetadata(ResolveToolPath(edit.ctx, edit.workingDir, pf.path))
		fileMetadata = append(fileMetadata, EditResponseMetadata{
			ToolPathMetadata: pathMeta,
			FilePath:         pf.path,
			OldContent:       pf.oldContent,
			NewContent:       pf.newContent,
			Additions:        pf.additions,
			Removals:         pf.removals,
		})

		msgLine := fmt.Sprintf("- %s (+%d, -%d)", FormatToolPath(pf.path, edit.workingDir), pf.additions, pf.removals)
		if pf.recovered {
			msgLine += " [recovered]"
		}
		fmt.Fprintf(&successMessage, "%s\n", msgLine)
	}

	// Clear the anonymous clipboard register after the batch completes.
	// Named registers persist across edit calls for cross-file move operations.
	GlobalClipboard.ClearAnonymous(sessionID)

	return fantasy.WithResponseMetadata(
		fantasy.NewTextResponse(successMessage.String()),
		fileMetadata,
	), nil
}

// fuzzyReplace attempts to match oldString in content using normalization strategies.
// Returns (newContent, true) on success. Tries:
// 1. Trim trailing whitespace on each line
// 2. Trim all whitespace on each line
// 3. Indentation-flexible (strip common indent prefix from both sides).
// 4. Comment-prefix strip matching.
// 5. Levenshtein similarity matching (average similarity >= 0.92).
func fuzzyReplace(content, oldString, newString string, replaceAll bool, threshold float64) (string, bool) {
	contentLines := strings.Split(content, "\n")
	oldLines := splitIntoLines(oldString)
	if len(oldLines) == 0 {
		return "", false
	}

	// Strategy 1: trim trailing whitespace per line.
	matches, ok := findBlockMatch(contentLines, oldLines,
		func(a, b string) bool {
			return strings.TrimRight(a, " \t") == strings.TrimRight(b, " \t")
		}, replaceAll)
	if ok {
		return applyMatches(content, oldString, newString, matches, replaceAll), true
	}

	// Strategy 2: trim all surrounding whitespace per line.
	matches, ok = findBlockMatch(contentLines, oldLines,
		func(a, b string) bool {
			return strings.TrimSpace(a) == strings.TrimSpace(b)
		}, replaceAll)
	if ok {
		return applyMatches(content, oldString, newString, matches, replaceAll), true
	}

	// Strategy 3: indentation-flexible (strip common indent prefix).
	minIndent := commonIndentPrefix(oldLines)
	if minIndent != "" {
		strippedOld := make([]string, len(oldLines))
		for i, l := range oldLines {
			if strings.TrimSpace(l) == "" {
				strippedOld[i] = ""
			} else {
				strippedOld[i] = strings.TrimRight(l[len(minIndent):], " \t")
			}
		}
		matches, ok = findBlockMatch(contentLines, strippedOld,
			func(a, b string) bool {
				stripped := a
				if len(a) >= len(minIndent) && strings.HasPrefix(a, minIndent) {
					stripped = a[len(minIndent):]
				} else {
					stripped = strings.TrimLeft(a, " \t")
				}
				return strings.TrimRight(stripped, " \t") == b
			}, replaceAll)
		if ok {
			return applyMatches(content, oldString, newString, matches, replaceAll), true
		}
	}

	// Strategy 4: Comment-prefix strip matching.
	// Cross-prefix substitution is intentionally restricted to unambiguous
	// single-line comment tokens ("//", "#", ";"). Block-comment markers
	// ("/*", "*/", "*") are excluded because they overlap with multiplication
	// and pointer syntax, which would cause incorrect matches in non-comment code.
	matches, ok = findBlockMatch(contentLines, oldLines,
		func(a, b string) bool {
			pa, pb := commentLinePrefix(a), commentLinePrefix(b)
			// Both non-comment lines: compare directly after whitespace trim.
			if pa == "" && pb == "" {
				return strings.TrimSpace(a) == strings.TrimSpace(b)
			}
			// Require both lines to be comments, then allow cross-prefix match
			// only when both prefixes are safe single-line comment tokens or
			// when the prefixes are identical.
			if pa == "" || pb == "" {
				return false
			}
			if pa == pb || (isSingleLineCommentPrefix(pa) && isSingleLineCommentPrefix(pb)) {
				return stripCommentPrefix(a) == stripCommentPrefix(b)
			}
			return false
		}, replaceAll)
	if ok {
		return applyMatches(content, oldString, newString, matches, replaceAll), true
	}

	// Strategy 5: Levenshtein similarity matching (average similarity >= threshold).
	if threshold <= 0 {
		return "", false
	}
	matches, ok = fuzzySimilarityMatch(contentLines, oldLines, threshold, replaceAll)
	if ok {
		return applyMatches(content, oldString, newString, matches, replaceAll), true
	}

	return "", false
}

func applyUnifiedPatch(edit editContext, fallbackPath string, patchText string, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	sessionID := GetSessionFromContext(edit.ctx)
	if sessionID == "" {
		return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for applying patch")
	}

	patches, err := ParseUnifiedPatch(patchText)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to parse unified patch: %v", err)), nil
	}
	if len(patches) == 0 {
		return fantasy.NewTextErrorResponse("No file patches found in unified patch content."), nil
	}

	var absPaths []string
	pathToPatch := make(map[string]*FilePatch)
	for _, p := range patches {
		pPath := p.NewPath
		if pPath == "" {
			pPath = p.OldPath
		}
		if pPath == "" || pPath == "/dev/null" {
			pPath = fallbackPath
		}
		if pPath == "" {
			return fantasy.NewTextErrorResponse("missing file path in patch header"), nil
		}

		absPath := ResolveToolPath(edit.ctx, edit.workingDir, pPath).AbsolutePath
		if existing, ok := pathToPatch[absPath]; ok {
			// The same file appeared in more than one "--- "/"+++ " section
			// (e.g. two independently-generated single-hunk diffs
			// concatenated together). Append this section's hunks to the
			// file's hunk list in order instead of discarding the earlier
			// section, so no hunk is silently dropped.
			existing.Hunks = append(existing.Hunks, p.Hunks...)
			continue
		}
		pathToPatch[absPath] = p
		absPaths = append(absPaths, absPath)
	}
	for _, absPath := range absPaths {
		if response, blocked, guardErr := enforcePlanModeWriteTarget(edit.ctx, absPath); blocked || guardErr != nil {
			return response, guardErr
		}
	}

	if err := checkPreflightLimits(edit.ctx, absPaths); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}

	memoryFiles := make(map[string][]string)
	originalContents := make(map[string]string)
	originalCrlf := make(map[string]bool)
	originalHadTrailingNewline := make(map[string]bool)

	for _, absPath := range absPaths {
		fileInfo, statErr := clientfs.Stat(edit.ctx, absPath)
		var fileLines []string
		var crlf bool
		var origContent string
		hadTrailingNewline := true

		if statErr == nil && !fileInfo.IsDir() {
			content, readErr := clientfs.ReadFile(edit.ctx, absPath)
			if readErr != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to read file %s: %w", absPath, readErr)
			}
			origContent, crlf = fsext.ToUnixLineEndings(string(content))
			fileLines, hadTrailingNewline = splitHashlineFileLines(origContent)
		} else {
			origContent = ""
		}

		originalContents[absPath] = origContent
		originalCrlf[absPath] = crlf
		originalHadTrailingNewline[absPath] = hadTrailingNewline

		patch := pathToPatch[absPath]
		newLines, applyErr := ApplyPatchToLines(fileLines, patch.Hunks)
		if applyErr != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("Preflight validation failed for %s: %v. No changes were written.", absPath, applyErr)), nil
		}
		memoryFiles[absPath] = newLines
	}

	// Pass 1: compute every file's new content and request permission for all
	// of them, without writing anything. A denial on any one file must leave
	// every file untouched.
	type preparedPatchFile struct {
		absPath    string
		oldContent string
		newContent string
		newLines   []string
		crlf       bool
		additions  int
		removals   int
	}

	prepared := make([]preparedPatchFile, 0, len(absPaths))
	for _, absPath := range absPaths {
		newLines := memoryFiles[absPath]
		crlf := originalCrlf[absPath]
		oldContent := originalContents[absPath]
		hadTrailingNewline := originalHadTrailingNewline[absPath]
		newContent := joinHashlineFileLines(newLines, hadTrailingNewline)

		_, additions, removals := diff.GenerateDiff(
			oldContent,
			newContent,
			strings.TrimPrefix(absPath, edit.workingDir),
		)

		p, permErr := edit.permissions.Request(edit.ctx, permission.CreatePermissionRequest{
			SessionID:          sessionID,
			AuthoritySessionID: ResolveAuthoritySessionID(edit.ctx, sessionID),
			Path:               fsext.PathOrPrefix(absPath, edit.workingDir),
			ToolCallID:         call.ID,
			ToolName:           EditToolName,
			Action:             "write",
			Description:        fmt.Sprintf("Apply patch hunks to %s", absPath),
			Params: EditPermissionsParams{
				FilePath:   absPath,
				OldContent: oldContent,
				NewContent: newContent,
			},
		})
		if permErr != nil {
			return fantasy.ToolResponse{}, permErr
		}
		if !p {
			return fantasy.ToolResponse{}, permission.ErrorPermissionDenied
		}

		prepared = append(prepared, preparedPatchFile{
			absPath:    absPath,
			oldContent: oldContent,
			newContent: newContent,
			newLines:   newLines,
			crlf:       crlf,
			additions:  additions,
			removals:   removals,
		})
	}

	// Pass 2: every file's permission is granted -- write them all. If a
	// write fails partway through, best-effort roll back the files already
	// written in this call so a mid-batch I/O failure doesn't leave a silent
	// partial patch applied to disk.
	var fileMetadata []EditResponseMetadata
	var successMessage strings.Builder
	successMessage.WriteString("Patch applied successfully:\n")
	var written []preparedPatchFile

	for _, pf := range prepared {
		dir := filepath.Dir(pf.absPath)
		if err := clientfs.MkdirAll(edit.ctx, dir, 0o755); err != nil {
			return fantasy.ToolResponse{}, fmt.Errorf("failed to create parent directories for %s: %w", pf.absPath, err)
		}

		fileContent := pf.newContent
		if pf.crlf {
			fileContent, _ = fsext.ToWindowsLineEndings(pf.newContent)
		}

		if err := clientfs.WriteFile(edit.ctx, pf.absPath, []byte(fileContent), 0o644); err != nil {
			for _, w := range written {
				restoreContent := w.oldContent
				if w.crlf {
					restoreContent, _ = fsext.ToWindowsLineEndings(w.oldContent)
				}
				if rbErr := clientfs.WriteFile(edit.ctx, w.absPath, []byte(restoreContent), 0o644); rbErr != nil {
					slog.Error("Failed to roll back file after partial multi-file patch write failure", "path", w.absPath, "error", rbErr)
				} else {
					oldLines, _ := splitHashlineFileLines(w.oldContent)
					GlobalFileCache.Put(sessionID, w.absPath, oldLines)
				}
			}
			return fantasy.ToolResponse{}, fmt.Errorf("failed to write file %s: %w (rolled back %d previously written file(s) in this batch)", pf.absPath, err, len(written))
		}
		written = append(written, pf)
		GlobalFileCache.Put(sessionID, pf.absPath, pf.newLines)

		fileHist, err := edit.files.GetByPathAndSession(edit.ctx, pf.absPath, sessionID)
		if err != nil {
			_, err = edit.files.Create(edit.ctx, sessionID, pf.absPath, pf.oldContent)
			if err != nil {
				slog.Error("Failed to create file history", "error", err, "path", pf.absPath)
			}
		} else if fileHist.Content != pf.oldContent {
			_, err = edit.files.CreateVersion(edit.ctx, sessionID, pf.absPath, pf.oldContent)
			if err != nil {
				slog.Error("Failed to create version in history", "error", err, "path", pf.absPath)
			}
		}
		_, err = edit.files.CreateVersion(edit.ctx, sessionID, pf.absPath, pf.newContent)
		if err != nil {
			slog.Error("Failed to create version in history", "error", err, "path", pf.absPath)
		}

		edit.filetracker.RecordRead(edit.ctx, sessionID, pf.absPath)

		fileMetadata = append(fileMetadata, EditResponseMetadata{
			ToolPathMetadata: NewToolPathMetadata(ResolveToolPath(edit.ctx, edit.workingDir, pf.absPath)),
			FilePath:         pf.absPath,
			OldContent:       pf.oldContent,
			NewContent:       pf.newContent,
			Additions:        pf.additions,
			Removals:         pf.removals,
		})

		fmt.Fprintf(&successMessage, "- %s (+%d, -%d)\n", FormatToolPath(pf.absPath, edit.workingDir), pf.additions, pf.removals)
	}

	return fantasy.WithResponseMetadata(
		fantasy.NewTextResponse(successMessage.String()),
		fileMetadata,
	), nil
}

func levenshteinDistance(s, t string) int {
	sRunes := []rune(s)
	tRunes := []rune(t)
	const maxLineLen = 200
	if len(sRunes) > maxLineLen {
		sRunes = sRunes[:maxLineLen]
	}
	if len(tRunes) > maxLineLen {
		tRunes = tRunes[:maxLineLen]
	}
	sLen := len(sRunes)
	tLen := len(tRunes)
	if sLen == 0 {
		return tLen
	}
	if tLen == 0 {
		return sLen
	}
	d := make([][]int, sLen+1)
	for i := range d {
		d[i] = make([]int, tLen+1)
		d[i][0] = i
	}
	for j := 0; j <= tLen; j++ {
		d[0][j] = j
	}
	for i := 1; i <= sLen; i++ {
		for j := 1; j <= tLen; j++ {
			cost := 1
			if sRunes[i-1] == tRunes[j-1] {
				cost = 0
			}
			d[i][j] = min(d[i-1][j]+1, min(d[i][j-1]+1, d[i-1][j-1]+cost))
		}
	}
	return d[sLen][tLen]
}

func lineSimilarity(s, t string) float64 {
	s = strings.TrimSpace(s)
	t = strings.TrimSpace(t)
	if s == "" && t == "" {
		return 1.0
	}
	sLen := utf8.RuneCountInString(s)
	tLen := utf8.RuneCountInString(t)
	maxLen := sLen
	if tLen > maxLen {
		maxLen = tLen
	}
	dist := levenshteinDistance(s, t)
	return 1.0 - float64(dist)/float64(maxLen)
}

func isCommentLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	prefixes := []string{"//", "/*", "*/", "*", "#", ";"}
	for _, prefix := range prefixes {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}

// commentLinePrefix returns the comment prefix token found at the start of line
// (after trimming whitespace), or "" if the line is not a comment line.
func commentLinePrefix(line string) string {
	trimmed := strings.TrimSpace(line)
	for _, prefix := range []string{"//", "/*", "*/", "*", "#", ";"} {
		if strings.HasPrefix(trimmed, prefix) {
			return prefix
		}
	}
	return ""
}

// isSingleLineCommentPrefix reports whether prefix belongs to the set of
// unambiguous single-line comment tokens. Multi-line block markers ("/*", "*/",
// "*") are excluded because they overlap with multiplication and pointer syntax,
// making cross-prefix substitution unsafe.
func isSingleLineCommentPrefix(prefix string) bool {
	switch prefix {
	case "//", "#", ";":
		return true
	}
	return false
}

func stripCommentPrefix(line string) string {
	trimmed := strings.TrimSpace(line)
	prefixes := []string{"//", "/*", "*/", "*", "#", ";"}
	for _, prefix := range prefixes {
		if strings.HasPrefix(trimmed, prefix) {
			trimmed = strings.TrimSpace(trimmed[len(prefix):])
			break
		}
	}
	return trimmed
}

func fuzzySimilarityMatch(contentLines, searchLines []string, threshold float64, replaceAll bool) ([]string, bool) {
	n := len(searchLines)
	if n == 0 {
		return nil, false
	}
	var matches []string
	for i := 0; i <= len(contentLines)-n; i++ {
		var sumSim float64
		for j := 0; j < n; j++ {
			sumSim += lineSimilarity(contentLines[i+j], searchLines[j])
		}
		avgSim := sumSim / float64(n)
		if avgSim >= threshold {
			matches = append(matches, strings.Join(contentLines[i:i+n], "\n"))
		}
	}
	if len(matches) == 0 {
		return nil, false
	}
	if !replaceAll && len(matches) > 1 {
		return nil, false
	}
	return matches, true
}

type ClosestMatchInfo struct {
	Lines      []string
	StartLine  int
	Similarity float64
}

func findClosestMatch(content string, oldString string) (*ClosestMatchInfo, int) {
	contentLines := strings.Split(content, "\n")
	oldLines := splitIntoLines(oldString)
	n := len(oldLines)
	if n == 0 || len(contentLines) < n {
		return nil, 0
	}

	var best *ClosestMatchInfo
	var matchesAboveThreshold int

	for i := 0; i <= len(contentLines)-n; i++ {
		var sumSim float64
		for j := 0; j < n; j++ {
			sumSim += lineSimilarity(contentLines[i+j], oldLines[j])
		}
		avgSim := sumSim / float64(n)

		if best == nil || avgSim > best.Similarity {
			linesCopy := make([]string, len(contentLines[i:i+n]))
			copy(linesCopy, contentLines[i:i+n])
			best = &ClosestMatchInfo{
				Lines:      linesCopy,
				StartLine:  i + 1,
				Similarity: avgSim,
			}
		}
		if avgSim >= 0.8 {
			matchesAboveThreshold++
		}
	}

	return best, matchesAboveThreshold
}

func buildDetailedMatchError(content, oldString string) string {
	closest, countAbove := findClosestMatch(content, oldString)
	if closest == nil || closest.Similarity < 0.4 {
		return "old_string not found in file. Make sure it matches exactly, including whitespace and line breaks."
	}

	similarityPercent := int(closest.Similarity * 100)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("old_string not found in file. Make sure it matches exactly.\n\nClosest match (%d%% similar) found at line %d:\n", similarityPercent, closest.StartLine))

	sb.WriteString("  Expected (LLM):\n")
	expectedLines := splitIntoLines(oldString)
	for _, l := range expectedLines {
		sb.WriteString(fmt.Sprintf("    %s\n", l))
	}
	sb.WriteString("  Actual (File):\n")
	for _, l := range closest.Lines {
		sb.WriteString(fmt.Sprintf("    %s\n", l))
	}

	if countAbove > 1 && closest.Similarity >= 0.8 {
		sb.WriteString(fmt.Sprintf("\nNote: Found %d high-similarity matches. Please provide more context to make it unique.", countAbove))
	}

	return sb.String()
}
