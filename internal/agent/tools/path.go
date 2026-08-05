package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charmbracelet/crush/internal/filepathext"
)

type ToolPath struct {
	InputPath        string
	WorkingDir       string
	AbsolutePath     string
	DisplayPath      string
	IsOutsideSession bool
}

type ToolPathMetadata struct {
	InputPath               string `json:"input_path,omitempty"`
	WorkingDirectory        string `json:"working_directory,omitempty"`
	CommandWorkingDirectory string `json:"command_working_directory,omitempty"`
	ResolvedPath            string `json:"resolved_path,omitempty"`
	DisplayPath             string `json:"display_path,omitempty"`
	IsOutsideSession        bool   `json:"is_outside_session,omitempty"`
	PrefixHint              string `json:"prefix_hint,omitempty"`
}

func NewToolPathMetadata(path ToolPath) ToolPathMetadata {
	return ToolPathMetadata{
		InputPath:        path.InputPath,
		WorkingDirectory: path.WorkingDir,
		ResolvedPath:     path.AbsolutePath,
		DisplayPath:      path.DisplayPath,
		IsOutsideSession: path.IsOutsideSession,
		PrefixHint:       DuplicateWorkingDirPrefixHint(path.InputPath, path.WorkingDir),
	}
}

type ToolPathErrorMetadata struct {
	ToolPathMetadata
	ErrorKind      string `json:"error_kind"`
	RecoveryAction string `json:"recovery_action,omitempty"`
}

func NewToolPathErrorMetadata(path ToolPath, errorKind, recoveryAction string) ToolPathErrorMetadata {
	return ToolPathErrorMetadata{
		ToolPathMetadata: NewToolPathMetadata(path),
		ErrorKind:        errorKind,
		RecoveryAction:   recoveryAction,
	}
}

func NewCommandToolPathMetadata(sessionWorkingDir, commandWorkingDir, inputPath string) ToolPathMetadata {
	metadata := NewToolPathMetadata(ToolPath{
		InputPath:    inputPath,
		WorkingDir:   sessionWorkingDir,
		AbsolutePath: commandWorkingDir,
		DisplayPath:  FormatToolPath(commandWorkingDir, sessionWorkingDir),
	})
	metadata.CommandWorkingDirectory = commandWorkingDir
	return metadata
}

func EffectiveWorkingDir(ctx context.Context, fallback string) string {
	if workingDir := GetWorkingDirFromContext(ctx); workingDir != "" {
		return workingDir
	}
	return fallback
}

func DuplicateWorkingDirPrefixHint(inputPath, workingDir string) string {
	inputPath = filepath.ToSlash(strings.TrimSpace(inputPath))
	workingDir = filepath.Clean(strings.TrimSpace(workingDir))
	if inputPath == "" || workingDir == "" {
		return ""
	}
	base := filepath.Base(workingDir)
	prefix := base + "/"
	if !strings.HasPrefix(inputPath, prefix) {
		return ""
	}
	candidate := strings.TrimPrefix(inputPath, prefix)
	if candidate == "" || candidate == inputPath {
		return ""
	}
	return fmt.Sprintf("Input path %q repeats the working-directory name. If the intended path is inside the session cwd, try %q.", inputPath, candidate)
}

func ResolveToolPath(ctx context.Context, fallbackWorkingDir, inputPath string) ToolPath {
	workingDir := EffectiveWorkingDir(ctx, fallbackWorkingDir)
	absolutePath := filepathext.SmartJoin(workingDir, inputPath)
	return ToolPath{
		InputPath:        inputPath,
		WorkingDir:       workingDir,
		AbsolutePath:     absolutePath,
		DisplayPath:      FormatToolPath(absolutePath, workingDir),
		IsOutsideSession: isPathOutsideWorkingDir(absolutePath, workingDir),
	}
}

// isPathOutsideWorkingDir reports whether absPath is outside the workingDir
// tree. Both paths must be absolute. A path that resolves to workingDir itself
// is not outside.
func isPathOutsideWorkingDir(absPath, workingDir string) bool {
	absWorking, err := filepath.Abs(workingDir)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absWorking, absPath)
	if err != nil {
		return true
	}
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func FormatToolPath(path, workingDir string) string {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	absoluteWorkingDir, err := filepath.Abs(workingDir)
	if err != nil {
		return filepath.ToSlash(absolutePath)
	}
	relativePath, err := filepath.Rel(absoluteWorkingDir, absolutePath)
	if err == nil && relativePath != ".." && !strings.HasPrefix(relativePath, ".."+string(filepath.Separator)) {
		if relativePath == "" {
			return "."
		}
		return filepath.ToSlash(relativePath)
	}
	return filepath.ToSlash(absolutePath)
}

func ToolPathContext(path ToolPath) map[string]string {
	return map[string]string{
		"input_path":         path.InputPath,
		"working_directory":  path.WorkingDir,
		"resolved_path":      path.AbsolutePath,
		"display_path":       path.DisplayPath,
		"is_outside_session": strconv.FormatBool(path.IsOutsideSession),
		"prefix_hint":        DuplicateWorkingDirPrefixHint(path.InputPath, path.WorkingDir),
	}
}
