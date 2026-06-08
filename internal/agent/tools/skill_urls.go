package tools

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/crush/internal/skills"
)

const SkillURLScheme = "skill://"

// ResolveSkillURL resolves a skill:// URL to an absolute filesystem path.
//
// Supported forms:
//   - skill://<name>          → skill's SKILL.md
//   - skill://<name>/<path>   → relative path within skill's directory
//
// The resolver validates against path traversal and ensures resolved paths
// remain within the skill's base directory.
func ResolveSkillURL(rawURL string, skillList []*skills.Skill) (string, error) {
	if !strings.HasPrefix(rawURL, SkillURLScheme) {
		return "", fmt.Errorf("not a skill:// URL: %s", rawURL)
	}

	rest := rawURL[len(SkillURLScheme):]
	if rest == "" {
		return "", fmt.Errorf("skill:// URL requires a skill name: skill://<name>")
	}

	// Parse the skill name and optional relative path.
	// Format: <name> or <name>/<relative-path>
	slashIdx := strings.Index(rest, "/")
	var skillName, relativePath string
	if slashIdx < 0 {
		skillName = rest
	} else {
		skillName = rest[:slashIdx]
		relativePath = rest[slashIdx+1:]
	}

	// URL-decode the skill name (handles percent-encoded characters).
	decodedName, err := url.PathUnescape(skillName)
	if err != nil {
		return "", fmt.Errorf("invalid skill name encoding in URL: %s: %w", rawURL, err)
	}
	skillName = decodedName

	// Find the skill by name.
	var skill *skills.Skill
	for _, s := range skillList {
		if s.Name == skillName {
			skill = s
			break
		}
	}
	if skill == nil {
		available := make([]string, 0, len(skillList))
		for _, s := range skillList {
			available = append(available, s.Name)
		}
		availStr := "none"
		if len(available) > 0 {
			availStr = strings.Join(available, ", ")
		}
		return "", fmt.Errorf("unknown skill: %s (available: %s)", skillName, availStr)
	}

	// No relative path → resolve to SKILL.md.
	if relativePath == "" {
		return skill.SkillFilePath, nil
	}

	// Validate relative path: no traversal, no absolute paths.
	if filepath.IsAbs(relativePath) {
		return "", fmt.Errorf("absolute paths are not allowed in skill:// URLs: %s", rawURL)
	}
	cleaned := filepath.Clean(relativePath)
	if strings.HasPrefix(cleaned, "..") {
		return "", fmt.Errorf("path traversal (..) is not allowed in skill:// URLs: %s", rawURL)
	}

	// Resolve to absolute path within skill's base directory.
	targetPath := filepath.Join(skill.Path, relativePath)
	resolvedPath, err := filepath.Abs(targetPath)
	if err != nil {
		return "", fmt.Errorf("failed to resolve skill path: %w", err)
	}

	resolvedBaseDir, err := filepath.Abs(skill.Path)
	if err != nil {
		return "", fmt.Errorf("failed to resolve skill base dir: %w", err)
	}

	// Ensure resolved path stays within the skill's base directory.
	if !strings.HasPrefix(resolvedPath, resolvedBaseDir+string(filepath.Separator)) && resolvedPath != resolvedBaseDir {
		return "", fmt.Errorf("path traversal is not allowed in skill:// URLs: %s", rawURL)
	}

	return resolvedPath, nil
}

// ExpandSkillURLs replaces all skill:// URLs in a command string with their
// resolved absolute filesystem paths, shell-escaped with single quotes.
// URLs inside single or double quotes are NOT expanded (they are literal strings).
// If no skill:// URLs are found, the command is returned unchanged.
func ExpandSkillURLs(command string, skillList []*skills.Skill) string {
	if len(skillList) == 0 || !strings.Contains(command, SkillURLScheme) {
		return command
	}

	var result strings.Builder
	i := 0
	inSingleQuote := false
	inDoubleQuote := false

	for i < len(command) {
		c := command[i]

		// Track quote state.
		if c == '\'' && !inDoubleQuote {
			inSingleQuote = !inSingleQuote
			result.WriteByte(c)
			i++
			continue
		}
		if c == '"' && !inSingleQuote {
			inDoubleQuote = !inDoubleQuote
			result.WriteByte(c)
			i++
			continue
		}
		// Handle backslash escapes inside double quotes.
		if c == '\\' && inDoubleQuote && i+1 < len(command) {
			result.WriteByte(c)
			result.WriteByte(command[i+1])
			i += 2
			continue
		}

		// Only expand skill:// URLs outside of quotes.
		if !inSingleQuote && !inDoubleQuote && strings.HasPrefix(command[i:], SkillURLScheme) {
			end := i + len(SkillURLScheme)
			for end < len(command) {
				c := command[end]
				if c == ' ' || c == '\t' || c == '\n' || c == '\'' || c == '"' ||
					c == ')' || c == '(' || c == '`' || c == '|' || c == '&' ||
					c == ';' || c == '<' || c == '>' || c == '\\' {
					break
				}
				end++
			}

			rawURL := command[i:end]
			resolvedPath, err := ResolveSkillURL(rawURL, skillList)
			if err != nil {
				result.WriteString(rawURL)
			} else {
				result.WriteString(shellEscape(resolvedPath))
			}
			i = end
			continue
		}

		result.WriteByte(c)
		i++
	}

	return result.String()
}

// shellEscape wraps a path in single quotes, escaping any embedded single quotes.
func shellEscape(p string) string {
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}

// IsSkillURL checks if a path string is a skill:// URL.
func IsSkillURL(path string) bool {
	return strings.HasPrefix(path, SkillURLScheme)
}
