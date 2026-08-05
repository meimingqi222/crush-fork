package plan

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const defaultPlanSlug = "plan"

// LocalPlanURIPrefix is the URI scheme used to reference plan files within
// Crush. A plan URI has the form local://<slug>-plan.md, where <slug> is a
// short kebab-case identifier chosen by the agent.
const LocalPlanURIPrefix = "local://"

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

// IsLocalPlanURI reports whether path uses the local plan URI scheme.
func IsLocalPlanURI(path string) bool {
	return strings.HasPrefix(path, LocalPlanURIPrefix)
}

// ParseLocalPlanURI parses a local plan URI of the form
// local://<slug>-plan.md and returns the slug. It returns an error if the URI
// is malformed or the slug is empty.
func ParseLocalPlanURI(uri string) (string, error) {
	if !IsLocalPlanURI(uri) {
		return "", fmt.Errorf("not a local plan URI: %s", uri)
	}
	slug := strings.TrimPrefix(uri, LocalPlanURIPrefix)
	slug = strings.TrimSpace(slug)
	slug = strings.TrimSuffix(slug, "-plan.md")
	slug = strings.TrimSuffix(slug, ".md")
	slug = sanitizeSlug(slug)
	if slug == "" || slug == "plan" {
		return "", fmt.Errorf("local plan URI must include a non-empty slug: %s", uri)
	}
	return slug, nil
}

// ResolveLocalPlanURI converts a local plan URI into an absolute filesystem
// path for the given workspace and session.
func ResolveLocalPlanURI(workspaceRoot, sessionID, uri string) (string, error) {
	slug, err := ParseLocalPlanURI(uri)
	if err != nil {
		return "", err
	}
	return PlanFilePath(workspaceRoot, sessionID, slug), nil
}

// SlugFromPlanPath extracts the slug from an absolute plan file path for the
// given workspace and session. It returns the slug and true if the path is a
// recognized plan file for the session.
//
// The plan filename pattern is <sessionID>-<slug>.md inside the plans
// directory. The prefix is therefore "<plansDir>/<sessionID>-" — not the
// fully-rendered default filename (which would make HasPrefix match only the
// default file and never a custom slug).
func SlugFromPlanPath(workspaceRoot, sessionID, path string) (string, bool) {
	prefix := filepath.Join(PlansDir(workspaceRoot), sanitizeSlug(sessionID)) + "-"
	clean, err := filepath.Abs(path)
	if err != nil {
		clean = path
	}
	clean = filepath.Clean(clean)
	if !strings.HasPrefix(clean, prefix) {
		return "", false
	}
	if !strings.HasSuffix(clean, ".md") {
		return "", false
	}
	slug := strings.TrimPrefix(clean, prefix)
	slug = strings.TrimSuffix(slug, ".md")
	if slug == "" {
		return "", false
	}
	return slug, true
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

// EnsureSessionPlanPath returns existingPath if non-empty, otherwise creates
// a default plan file under workspaceRoot for sessionID and returns its path.
// This is the shared helper used by both coordinator.ensurePlanFileForSession
// and session.service.ensurePlanFile so the "create if missing" logic lives in
// one place. Callers are responsible for persisting the returned path to the
// session if it differs from existingPath.
func EnsureSessionPlanPath(workspaceRoot, sessionID, existingPath string) (string, error) {
	if strings.TrimSpace(existingPath) != "" {
		return existingPath, nil
	}
	return EnsureSessionFile(workspaceRoot, sessionID)
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

var (
	checklistItemRegex = regexp.MustCompile(`(?i)^\s*-\s*\[([ xX])\]\s*(.+)$`)
	headingRegex       = regexp.MustCompile(`^(#{1,6})\s+(.+)$`)
	listItemRegex      = regexp.MustCompile(`^(\s*)[-*]\s+(.+)$`)
	numberedItemRegex  = regexp.MustCompile(`^(\s*)\d+\.\s+(.+)$`)
)

// ExtractGoalAndTasks parses an approved plan markdown and returns the
// objective and a list of actionable task strings. It recognizes GitHub-style
// checklist items (`- [ ] step`) first, then falls back to bulleted or
// numbered items under the "Approach" section. The boolean ok is false when
// no objective can be inferred.
func ExtractGoalAndTasks(plan string) (objective string, tasks []string, ok bool) {
	plan = strings.TrimSpace(plan)
	if plan == "" {
		return "", nil, false
	}

	lines := strings.Split(plan, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if m := headingRegex.FindStringSubmatch(line); m != nil {
			objective = strings.TrimSpace(m[2])
		} else {
			objective = line
		}
		if objective != "" {
			break
		}
	}

	if objective == "" {
		return "", nil, false
	}

	// First pass: collect explicit checklist items anywhere in the file.
	for _, line := range lines {
		if m := checklistItemRegex.FindStringSubmatch(line); m != nil {
			task := strings.TrimSpace(m[2])
			if task != "" {
				tasks = append(tasks, task)
			}
		}
	}
	if len(tasks) > 0 {
		return objective, tasks, true
	}

	// Second pass: find the Approach section and collect its list items.
	inApproach := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if m := headingRegex.FindStringSubmatch(trimmed); m != nil {
			inApproach = strings.EqualFold(strings.TrimSpace(m[2]), "Approach")
			continue
		}
		if !inApproach {
			continue
		}
		// Stop collecting at the next heading or a blank line followed by a new heading.
		if trimmed == "" && i+1 < len(lines) {
			next := strings.TrimSpace(lines[i+1])
			if headingRegex.MatchString(next) {
				inApproach = false
				continue
			}
		}
		if listItem := listItemRegex.FindStringSubmatch(line); listItem != nil {
			// Only top-level bullets under Approach count as tasks.
			if listItem[1] == "" {
				task := strings.TrimSpace(listItem[2])
				if task != "" {
					tasks = append(tasks, task)
				}
			}
		} else if numbered := numberedItemRegex.FindStringSubmatch(line); numbered != nil {
			if numbered[1] == "" {
				task := strings.TrimSpace(numbered[2])
				if task != "" {
					tasks = append(tasks, task)
				}
			}
		}
	}

	return objective, tasks, len(tasks) > 0
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
