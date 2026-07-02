package plan

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const defaultPlanSlug = "plan"

// PlansDir returns the path to the plans directory within the given workspace
// root. Plans are stored at <workspace>/.crush/plans/.
func PlansDir(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, ".crush", "plans")
}

// PlanFilePath returns the full filesystem path for a plan file.
func PlanFilePath(workspaceRoot, sessionID, slug string) string {
	filename := fmt.Sprintf("%s-%s.md", sanitizeSlug(sessionID), sanitizeSlug(slug))
	return filepath.Join(PlansDir(workspaceRoot), filename)
}

// EnsureDir creates the plans directory if it does not exist.
func EnsureDir(workspaceRoot string) error {
	dir := PlansDir(workspaceRoot)
	return os.MkdirAll(dir, 0o755)
}

// Write writes plan content to a file. It creates the plans directory if
// needed.
func Write(workspaceRoot, sessionID, slug, content string) (string, error) {
	if err := EnsureDir(workspaceRoot); err != nil {
		return "", fmt.Errorf("failed to create plans directory: %w", err)
	}
	path := PlanFilePath(workspaceRoot, sessionID, slug)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("failed to write plan file: %w", err)
	}
	return path, nil
}

// EnsureSessionFile returns the canonical plan file path for a session and
// creates an empty plan file if one does not already exist.
func EnsureSessionFile(workspaceRoot, sessionID string) (string, error) {
	if err := EnsureDir(workspaceRoot); err != nil {
		return "", fmt.Errorf("failed to create plans directory: %w", err)
	}
	path := PlanFilePath(workspaceRoot, sessionID, defaultPlanSlug)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("failed to access plan file: %w", err)
	}
	if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
		return "", fmt.Errorf("failed to create plan file: %w", err)
	}
	return path, nil
}

// Read reads plan content from a file.
func Read(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("failed to read plan file: %w", err)
	}
	return string(data), nil
}

// Exists returns true if the plan file exists and is non-empty.
func Exists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Size() > 0
}

// List returns all plan files in the plans directory, newest first.
func List(workspaceRoot string) ([]string, error) {
	dir := PlansDir(workspaceRoot)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read plans directory: %w", err)
	}

	var paths []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		paths = append(paths, filepath.Join(dir, entry.Name()))
	}
	return paths, nil
}

// sanitizeSlug converts a string into a safe filename component.
func sanitizeSlug(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, " ", "-")
	var result []byte
	for _, ch := range []byte(s) {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_' {
			result = append(result, ch)
		}
	}
	s = string(result)
	// Collapse consecutive hyphens.
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	s = strings.Trim(s, "-_")
	if s == "" {
		s = "plan"
	}
	return s
}
