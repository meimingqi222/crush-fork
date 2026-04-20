package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/memory"
)

const (
	autoRecallMemoryLimit      = 3
	autoRecallHistoryLimit     = 3
	autoRecallSectionCharLimit = 600
	autoRecallQueryMaxWords    = 12
)

var recallFilePattern = regexp.MustCompile(`\b[\w/.-]+\.(?:go|ts|tsx|js|jsx|py|rs|rb|java|kt|swift|md|json|yaml|yml|toml|sql|sh|bash)\b`)
var recallIdentPattern = regexp.MustCompile(`\b[A-Z][A-Za-z0-9]{3,}\b`)
var recallAbsolutePathPattern = regexp.MustCompile(`(?i)(?:[A-Z]:[\\/]|/)[^\s"'` + "`" + `<>()\[\]{}]+`)
var recallRelativePathWithExtPattern = regexp.MustCompile(`(?:\.\.?[\\/])?[\w.-]+(?:[\\/][\w.-]+)*\.[A-Za-z0-9]{1,16}`)
var recallSeparatedPathPattern = regexp.MustCompile(`(?:\.\.?[\\/])?[\w.-]+[\\/][\w./\\-]+`)

func buildAutoRecall(historySvc history.Service, memorySvc memory.Service, bgModel *backgroundModel) func(context.Context, string, string) string {
	if historySvc == nil && memorySvc == nil {
		return nil
	}
	return func(ctx context.Context, sessionID, prompt string) string {
		return buildAutoRecallBlock(ctx, historySvc, memorySvc, bgModel, sessionID, prompt)
	}
}

type backgroundModel struct {
	model    Model
	provider config.ProviderConfig
}

func buildAutoRecallBlock(ctx context.Context, historySvc history.Service, memorySvc memory.Service, bgModel *backgroundModel, sessionID, prompt string) string {
	query := extractRecallQuery(prompt)
	if query == "" {
		return ""
	}

	sections := make([]string, 0, 2)

	if memorySvc != nil {
		scope, includeMemory := autoRecallMemoryScope(ctx)
		if includeMemory {
			entries := recallEntriesForSession(ctx, memorySvc, bgModel, sessionID, query, scope)
			if len(entries) > 0 {
				sections = append(sections, formatAutoRecallMemory(ctx, entries))
			}
		}
	}

	if historySvc != nil {
		results, err := historySvc.SearchMessages(ctx, history.SearchParams{
			Query:     query,
			SessionID: sessionID,
			Limit:     autoRecallHistoryLimit,
		})
		if err == nil && len(results) > 0 {
			sections = append(sections, formatAutoRecallHistory(results))
		}
	}

	if len(sections) == 0 {
		return ""
	}
	return strings.Join(sections, "\n\n")
}

func extractRecallQuery(prompt string) string {
	trimmed := strings.TrimSpace(prompt)
	if trimmed == "" {
		return ""
	}

	var tokens []string
	seen := make(map[string]bool)

	addToken := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		tokens = append(tokens, s)
	}

	for _, f := range recallFilePattern.FindAllString(trimmed, -1) {
		addToken(f)
	}
	for _, id := range recallIdentPattern.FindAllString(trimmed, -1) {
		addToken(id)
	}

	if len(tokens) >= 3 {
		return strings.Join(tokens, " ")
	}

	words := strings.Fields(trimmed)
	if len(words) > autoRecallQueryMaxWords {
		words = words[:autoRecallQueryMaxWords]
	}
	return strings.Join(words, " ")
}

func recallEntriesForSession(ctx context.Context, memorySvc memory.Service, bgModel *backgroundModel, sessionID, query, scope string) []memory.Entry {
	entries := make([]memory.Entry, 0, autoRecallMemoryLimit)
	seen := make(map[string]bool)

	if shouldInjectSessionMemory(scope) && strings.TrimSpace(sessionID) != "" {
		if entry, err := memorySvc.Get(ctx, sessionMemoryKey(sessionID)); err == nil {
			entries = append(entries, entry)
			seen[entry.Key] = true
		}
	}

	var matched []memory.Entry
	if bgModel != nil {
		matched = selectRelevantMemories(ctx, memorySvc, bgModel.model, bgModel.provider, query, scope)
	} else {
		search := memory.SearchParams{Query: query, Limit: autoRecallMemoryLimit}
		if scope != "" {
			search.Scope = scope
		}
		var err error
		matched, err = memorySvc.Search(ctx, search)
		if err != nil {
			matched = nil
		}
	}

	for _, entry := range matched {
		if seen[entry.Key] {
			continue
		}
		entries = append(entries, entry)
		seen[entry.Key] = true
		if len(entries) >= autoRecallMemoryLimit {
			break
		}
	}

	return entries
}

func shouldInjectSessionMemory(scope string) bool {
	scope = strings.TrimSpace(scope)
	return scope == "" || strings.EqualFold(scope, "session")
}

func formatAutoRecallMemory(ctx context.Context, entries []memory.Entry) string {
	lines := make([]string, 0, len(entries)+2)
	lines = append(lines, "Relevant long-term memory:")
	lines = append(lines, "Verify remembered file paths, symbols, and commands before relying on them; memories can drift over time.")
	for _, entry := range entries {
		value := truncateRecallText(strings.TrimSpace(entry.Value), autoRecallSectionCharLimit)
		line := fmt.Sprintf("- %s: %s", formatAutoRecallMemoryLabel(entry), value)
		if pathChecks := formatRecallPathChecks(ctx, entry.Value); pathChecks != "" {
			line += fmt.Sprintf("\n  Path checks: %s", pathChecks)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func formatRecallPathChecks(ctx context.Context, text string) string {
	checks := collectRecallPathChecks(ctx, text)
	if len(checks) == 0 {
		return ""
	}

	parts := make([]string, 0, len(checks))
	for _, check := range checks {
		parts = append(parts, fmt.Sprintf("%s (%s)", check.Path, check.Status))
	}
	return strings.Join(parts, ", ")
}

type recallPathCheck struct {
	Path   string
	Status string
}

func collectRecallPathChecks(ctx context.Context, text string) []recallPathCheck {
	candidates := extractRecallPathCandidates(text)
	if len(candidates) == 0 {
		return nil
	}

	workingDir := strings.TrimSpace(tools.GetWorkingDirFromContext(ctx))
	checks := make([]recallPathCheck, 0, len(candidates))
	for _, candidate := range candidates {
		checks = append(checks, recallPathCheck{
			Path:   candidate,
			Status: validateRecallPathCandidate(workingDir, candidate),
		})
	}
	return checks
}

func extractRecallPathCandidates(text string) []string {
	patterns := []*regexp.Regexp{
		recallAbsolutePathPattern,
		recallRelativePathWithExtPattern,
		recallSeparatedPathPattern,
	}

	seen := make(map[string]bool)
	candidates := make([]string, 0)
	for _, pattern := range patterns {
		for _, match := range pattern.FindAllString(text, -1) {
			candidate := normalizeRecallPathCandidate(match)
			if candidate == "" || seen[candidate] || !isRecallPathCandidate(candidate) {
				continue
			}
			seen[candidate] = true
			candidates = append(candidates, candidate)
		}
	}
	return candidates
}

func normalizeRecallPathCandidate(candidate string) string {
	candidate = strings.TrimSpace(candidate)
	candidate = strings.TrimRightFunc(candidate, func(r rune) bool {
		switch r {
		case '.', ',', ';', ':', '!', '?', ')', ']', '}', '"', '\'', '`':
			return true
		default:
			return unicode.IsSpace(r)
		}
	})
	candidate = strings.TrimLeft(candidate, "\"'`")
	return strings.TrimSpace(candidate)
}

func isRecallPathCandidate(candidate string) bool {
	if candidate == "" || strings.Contains(candidate, "://") {
		return false
	}
	if isAbsoluteRecallPath(candidate) {
		return true
	}
	if strings.ContainsAny(candidate, `/\\`) {
		return true
	}
	return filepath.Ext(candidate) != ""
}

func validateRecallPathCandidate(workingDir, candidate string) string {
	resolved, certainty := resolveRecallPathCandidate(workingDir, candidate)
	if resolved == "" {
		return "unverified"
	}
	_, err := os.Stat(resolved)
	if err == nil {
		return "verified"
	}
	if os.IsNotExist(err) {
		if certainty == "strong" {
			return "missing"
		}
		return "unverified"
	}
	return "unverified"
}

func resolveRecallPathCandidate(workingDir, candidate string) (string, string) {
	if isWindowsAbsPath(candidate) {
		if runtime.GOOS != "windows" {
			return "", "weak"
		}
		return filepath.Clean(candidate), "strong"
	}
	if filepath.IsAbs(candidate) || strings.HasPrefix(candidate, "/") {
		return filepath.Clean(candidate), "strong"
	}
	if workingDir == "" {
		return "", recallPathCertainty(candidate)
	}
	return filepath.Join(workingDir, filepath.FromSlash(candidate)), recallPathCertainty(candidate)
}

func recallPathCertainty(candidate string) string {
	if filepath.Ext(candidate) != "" || strings.HasPrefix(candidate, "./") || strings.HasPrefix(candidate, "../") || strings.HasPrefix(candidate, ".\\") || strings.HasPrefix(candidate, "..\\") {
		return "strong"
	}
	return "weak"
}

func isAbsoluteRecallPath(candidate string) bool {
	return filepath.IsAbs(candidate) || strings.HasPrefix(candidate, "/") || isWindowsAbsPath(candidate)
}

func isWindowsAbsPath(candidate string) bool {
	if len(candidate) < 3 {
		return false
	}
	return ((candidate[0] >= 'A' && candidate[0] <= 'Z') || (candidate[0] >= 'a' && candidate[0] <= 'z')) && candidate[1] == ':' && (candidate[2] == '\\' || candidate[2] == '/')
}

func formatAutoRecallMemoryLabel(entry memory.Entry) string {
	label := strings.TrimSpace(entry.Key)
	qualifiers := make([]string, 0, 2)

	switch {
	case entry.Category != "" && entry.Type != "":
		qualifiers = append(qualifiers, fmt.Sprintf("%s/%s", strings.TrimSpace(entry.Category), strings.TrimSpace(entry.Type)))
	case entry.Category != "":
		qualifiers = append(qualifiers, strings.TrimSpace(entry.Category))
	case entry.Type != "":
		qualifiers = append(qualifiers, strings.TrimSpace(entry.Type))
	}

	if len(entry.Tags) > 0 {
		tags := make([]string, 0, len(entry.Tags))
		for _, tag := range entry.Tags {
			trimmed := strings.TrimSpace(tag)
			if trimmed == "" {
				continue
			}
			tags = append(tags, "#"+trimmed)
		}
		if len(tags) > 0 {
			qualifiers = append(qualifiers, strings.Join(tags, " "))
		}
	}

	if entry.UpdatedAt > 0 {
		qualifiers = append(qualifiers, fmt.Sprintf("updated %s", time.Unix(0, entry.UpdatedAt).UTC().Format(time.RFC3339)))
	}

	if len(qualifiers) == 0 {
		return label
	}
	return fmt.Sprintf("%s (%s)", label, strings.Join(qualifiers, "; "))
}

func formatAutoRecallHistory(results []history.MessageSearchResult) string {
	lines := make([]string, 0, len(results)+1)
	lines = append(lines, "Relevant session history:")
	for _, result := range results {
		text := truncateRecallText(strings.TrimSpace(result.Text), autoRecallSectionCharLimit)
		lines = append(lines, fmt.Sprintf("- [%s] %s", result.Role, text))
	}
	return strings.Join(lines, "\n")
}

func autoRecallMemoryScope(ctx context.Context) (string, bool) {
	memoryPolicy := strings.ToLower(strings.TrimSpace(tools.GetAgentMemoryFromContext(ctx)))
	isolationPolicy := strings.ToLower(strings.TrimSpace(tools.GetAgentIsolationFromContext(ctx)))

	switch memoryPolicy {
	case "ephemeral":
		return "", false
	case "isolated", "session":
		return "session", true
	case "project":
		return "project", true
	}

	switch isolationPolicy {
	case "session", "process":
		return "session", true
	case "workspace":
		return "project", true
	}

	return "", true
}

func truncateRecallText(text string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return string(runes[:limit]) + "…"
}
