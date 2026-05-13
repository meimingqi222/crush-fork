package tools

import (
	"cmp"
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/diff"
	"github.com/charmbracelet/crush/internal/filepathext"
	"github.com/charmbracelet/crush/internal/filetracker"
	"github.com/charmbracelet/crush/internal/fsext"
	"github.com/charmbracelet/crush/internal/history"

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
	FilePath   string                  `json:"file_path" description:"The absolute path to the file to modify"`
	OldString  string                  `json:"old_string,omitempty" description:"The text to replace"`
	NewString  string                  `json:"new_string,omitempty" description:"The text to replace it with"`
	ReplaceAll bool                    `json:"replace_all,omitempty" description:"Replace all occurrences of old_string (default false)"`
	Edits      []EditEntry             `json:"edits,omitempty" description:"Array of edit operations to perform sequentially on the file. When provided, old_string/new_string/replace_all are ignored."`
	Operations []HashlineEditOperation `json:"operations,omitempty" description:"Array of hashline operations using LINE#HASH references from view(hashline=true). When provided, all other parameters except file_path are ignored."`
}

type EditPermissionsParams struct {
	FilePath   string `json:"file_path"`
	OldContent string `json:"old_content,omitempty"`
	NewContent string `json:"new_content,omitempty"`
}

type EditResponseMetadata struct {
	FilePath     string       `json:"file_path,omitempty"`
	Additions    int          `json:"additions"`
	Removals     int          `json:"removals"`
	OldContent   string       `json:"old_content,omitempty"`
	NewContent   string       `json:"new_content,omitempty"`
	EditsApplied int          `json:"edits_applied,omitempty"`
	EditsFailed  []FailedEdit `json:"edits_failed,omitempty"`
}

// FailedEdit records a single edit operation that could not be applied.
type FailedEdit struct {
	Index int       `json:"index"`
	Error string    `json:"error"`
	Edit  EditEntry `json:"edit"`
}

const EditToolName = "edit"

var (
	oldStringNotFoundErr        = fantasy.NewTextErrorResponse("old_string not found in file. Make sure it matches exactly, including whitespace and line breaks.")
	oldStringMultipleMatchesErr = fantasy.NewTextErrorResponse("old_string appears multiple times in the file. Please provide more context to ensure a unique match, or set replace_all to true")
)

//go:embed edit.md
var editDescription []byte

type editContext struct {
	ctx         context.Context
	permissions permission.Service
	files       history.Service
	filetracker filetracker.Service
	workingDir  string
}

func NewEditTool(
	lspManager *lsp.Manager,
	permissions permission.Service,
	files history.Service,
	filetracker filetracker.Service,
	workingDir string,
) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		EditToolName,
		string(editDescription),
		func(ctx context.Context, params EditParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.FilePath == "" {
				return fantasy.NewTextErrorResponse("file_path is required"), nil
			}

			// Use session-specific working directory from context if available.
			effectiveWorkingDir := cmp.Or(GetWorkingDirFromContext(ctx), workingDir)

			params.FilePath = filepathext.SmartJoin(effectiveWorkingDir, params.FilePath)

			var response fantasy.ToolResponse
			var opErr error

			editCtx := editContext{ctx, permissions, files, filetracker, effectiveWorkingDir}

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

			if opErr != nil {
				return response, opErr
			}
			if response.IsError {
				// Return early if there was an error during content replacement
				// This prevents unnecessary LSP diagnostics processing
				return response, nil
			}

			notifyLSPs(ctx, lspManager, params.FilePath)

			text := fmt.Sprintf("<result>\n%s\n</result>\n", response.Content)
			text += getDiagnostics(params.FilePath, lspManager)
			response.Content = text
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

	if _, err := os.Stat(filePath); err == nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("file already exists: %s", filePath)), nil
	} else if !os.IsNotExist(err) {
		return fantasy.ToolResponse{}, fmt.Errorf("failed to access file: %w", err)
	}

	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("failed to create parent directories: %w", err)
	}

	currentContent := firstEntry.NewString

	var failedEdits []FailedEdit
	for i := 1; i < len(entries); i++ {
		e := entries[i]
		newContent, err := applyEntryToContent(currentContent, e)
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

	if err := os.WriteFile(filePath, []byte(currentContent), 0o644); err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("failed to write file: %w", err)
	}

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
	fileInfo, err := os.Stat(filePath)
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
		return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for editing file")
	}

	content, err := os.ReadFile(filePath)
	if err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("failed to read file: %w", err)
	}

	oldContent, isCrlf := fsext.ToUnixLineEndings(string(content))
	currentContent := oldContent

	var failedEdits []FailedEdit
	for i, e := range entries {
		newContent, applyErr := applyEntryToContent(currentContent, e)
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

	if oldContent == currentContent {
		if len(failedEdits) > 0 {
			return fantasy.WithResponseMetadata(
				fantasy.NewTextErrorResponse(fmt.Sprintf("no changes made - all %d edit(s) failed", len(failedEdits))),
				EditResponseMetadata{
					FilePath:     filePath,
					EditsApplied: 0,
					EditsFailed:  failedEdits,
				},
			), nil
		}
		return fantasy.NewTextErrorResponse("no changes made - all edits resulted in identical content"), nil
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

	if err := os.WriteFile(filePath, []byte(fileContent), 0o644); err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("failed to write file: %w", err)
	}

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
		message = fmt.Sprintf("Applied %d of %d edits to file: %s (%d edit(s) failed)", editsApplied, len(entries), filePath, len(failedEdits))
	} else {
		message = fmt.Sprintf("Applied %d edits to file: %s", len(entries), filePath)
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
func applyEntryToContent(content string, entry EditEntry) (string, error) {
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
		result, ok := fuzzyReplace(content, entry.OldString, entry.NewString, true)
		if !ok {
			return "", fmt.Errorf("old_string not found in content. Make sure it matches exactly, including whitespace and line breaks")
		}
		return result, nil
	}

	index := strings.Index(content, entry.OldString)
	if index == -1 {
		result, ok := fuzzyReplace(content, entry.OldString, entry.NewString, false)
		if !ok {
			return "", fmt.Errorf("old_string not found in content. Make sure it matches exactly, including whitespace and line breaks")
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
	fileInfo, err := os.Stat(filePath)
	if err == nil {
		if fileInfo.IsDir() {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("path is a directory, not a file: %s", filePath)), nil
		}
		return fantasy.NewTextErrorResponse(fmt.Sprintf("file already exists: %s", filePath)), nil
	} else if !os.IsNotExist(err) {
		return fantasy.ToolResponse{}, fmt.Errorf("failed to access file: %w", err)
	}

	dir := filepath.Dir(filePath)
	if err = os.MkdirAll(dir, 0o755); err != nil {
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

	err = os.WriteFile(filePath, []byte(content), 0o644)
	if err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("failed to write file: %w", err)
	}

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
		fantasy.NewTextResponse("File created: "+filePath),
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
	fileInfo, err := os.Stat(filePath)
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

	content, err := os.ReadFile(filePath)
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
			newContent, ok = fuzzyReplace(oldContent, oldString, "", true)
			if !ok {
				return oldStringNotFoundErr, nil
			}
		}
	} else {
		index := strings.Index(oldContent, oldString)
		if index == -1 {
			var ok bool
			newContent, ok = fuzzyReplace(oldContent, oldString, "", false)
			if !ok {
				return oldStringNotFoundErr, nil
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

	err = os.WriteFile(filePath, []byte(fileContent), 0o644)
	if err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("failed to write file: %w", err)
	}

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
		fantasy.NewTextResponse("Content deleted from file: "+filePath),
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
	fileInfo, err := os.Stat(filePath)
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

	content, err := os.ReadFile(filePath)
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
			newContent, ok = fuzzyReplace(oldContent, oldString, newString, true)
			if !ok {
				return oldStringNotFoundErr, nil
			}
		}
	} else {
		index := strings.Index(oldContent, oldString)
		if index == -1 {
			var ok bool
			newContent, ok = fuzzyReplace(oldContent, oldString, newString, false)
			if !ok {
				return oldStringNotFoundErr, nil
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
		return fantasy.NewTextErrorResponse("new content is the same as old content. No changes made."), nil
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

	err = os.WriteFile(filePath, []byte(fileContent), 0o644)
	if err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("failed to write file: %w", err)
	}

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
		fantasy.NewTextResponse("Content replaced in file: "+filePath),
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

	fileInfo, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("file not found: %s", filePath)), nil
		}
		return fantasy.ToolResponse{}, fmt.Errorf("failed to access file: %w", err)
	}
	if fileInfo.IsDir() {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("path is a directory, not a file: %s", filePath)), nil
	}

	content, err := os.ReadFile(filePath)
	if err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("failed to read file: %w", err)
	}

	oldContent, isCrlf := fsext.ToUnixLineEndings(string(content))
	oldLines, hadTrailingNewline := splitHashlineFileLines(oldContent)

	parsedOps, err := parseHashlineOperations(operations, oldLines)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}

	newLines, err := applyHashlineOperations(oldLines, parsedOps)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}

	newContent := joinHashlineFileLines(newLines, hadTrailingNewline)
	if newContent == oldContent {
		return fantasy.NewTextErrorResponse("new content is the same as old content. No changes made."), nil
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

	if err := os.WriteFile(filePath, []byte(fileContent), 0o644); err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("failed to write file: %w", err)
	}

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

	return fantasy.WithResponseMetadata(
		fantasy.NewTextResponse(fmt.Sprintf("Applied %d hashline operation(s) to file: %s", len(operations), filePath)),
		EditResponseMetadata{
			FilePath:   filePath,
			OldContent: oldContent,
			NewContent: newContent,
			Additions:  additions,
			Removals:   removals,
		},
	), nil
}

// fuzzyReplace attempts to match oldString in content using normalization strategies.
// Returns (newContent, true) on success. Tries:
// 1. Trim trailing whitespace on each line
// 2. Trim all whitespace on each line
// 3. Indentation-flexible (strip common indent prefix from both sides).
func fuzzyReplace(content, oldString, newString string, replaceAll bool) (string, bool) {
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

	return "", false
}
