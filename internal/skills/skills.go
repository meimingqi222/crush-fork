package skills

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/charlievieth/fastwalk"
	"github.com/charmbracelet/crush/internal/pubsub"
	"gopkg.in/yaml.v3"
)

const (
	SkillFileName          = "SKILL.md"
	MaxNameLength          = 64
	MaxDescriptionLength   = 1024
	MaxCompatibilityLength = 500
	MaxWhenToUseLength     = 2000
	MaxArgumentHintLength  = 200
)

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9]+(-[a-zA-Z0-9]+)*$`)

// SkillContext defines how a skill should be executed.
type SkillContext string

const (
	SkillContextInline SkillContext = "inline" // Default: skill content expands into current conversation
	SkillContextFork   SkillContext = "fork"   // Skill runs as a sub-agent with separate context
)

type SkillState string

const (
	SkillStateAvailable SkillState = "available"
	SkillStateError     SkillState = "error"
)

type DiscoveryState struct {
	Loaded []*Skill
	Errors map[string]error
}

var broker = pubsub.NewBroker[DiscoveryState]()

// SubscribeEvents allows UI components to subscribe to skill discovery events.
func SubscribeEvents(ctx context.Context) <-chan pubsub.Event[DiscoveryState] {
	return broker.Subscribe(ctx)
}

// PublishState publishes the current discovery state.
func PublishState(state DiscoveryState) {
	broker.Publish(pubsub.UpdatedEvent, state)
}

// Skill represents a parsed SKILL.md file.
type Skill struct {
	Name          string            `yaml:"name" json:"name"`
	Description   string            `yaml:"description" json:"description"`
	License       string            `yaml:"license,omitempty" json:"license,omitempty"`
	Compatibility string            `yaml:"compatibility,omitempty" json:"compatibility,omitempty"`
	Metadata      map[string]string `yaml:"metadata,omitempty" json:"metadata,omitempty"`

	// Extended fields from Claude Code skill specification
	WhenToUse          string       `yaml:"when_to_use,omitempty" json:"when_to_use,omitempty"`
	AllowedTools       []string     `yaml:"allowed-tools,omitempty" json:"allowed_tools,omitempty"`
	Arguments          []string     `yaml:"arguments,omitempty" json:"arguments,omitempty"`
	ArgumentHint       string       `yaml:"argument-hint,omitempty" json:"argument_hint,omitempty"`
	Model              string       `yaml:"model,omitempty" json:"model,omitempty"`
	Context            SkillContext `yaml:"context,omitempty" json:"context,omitempty"`
	DisableModelInvoke bool         `yaml:"disable-model-invocation,omitempty" json:"disable_model_invocation,omitempty"`
	UserInvocable      *bool        `yaml:"user-invocable,omitempty" json:"user_invocable,omitempty"`

	Instructions  string `yaml:"-" json:"instructions"`
	Path          string `yaml:"-" json:"path"`
	SkillFilePath string `yaml:"-" json:"skill_file_path"`
}

// Validate checks if the skill meets spec requirements.
func (s *Skill) Validate() error {
	var errs []error

	if s.Name == "" {
		errs = append(errs, errors.New("name is required"))
	} else {
		if len(s.Name) > MaxNameLength {
			errs = append(errs, fmt.Errorf("name exceeds %d characters", MaxNameLength))
		}
		if !namePattern.MatchString(s.Name) {
			errs = append(errs, errors.New("name must be alphanumeric with hyphens, no leading/trailing/consecutive hyphens"))
		}
		if s.Path != "" && !strings.EqualFold(filepath.Base(s.Path), s.Name) {
			errs = append(errs, fmt.Errorf("name %q must match directory %q", s.Name, filepath.Base(s.Path)))
		}
	}

	if s.Description == "" {
		errs = append(errs, errors.New("description is required"))
	} else if len(s.Description) > MaxDescriptionLength {
		errs = append(errs, fmt.Errorf("description exceeds %d characters", MaxDescriptionLength))
	}

	if len(s.Compatibility) > MaxCompatibilityLength {
		errs = append(errs, fmt.Errorf("compatibility exceeds %d characters", MaxCompatibilityLength))
	}

	if len(s.WhenToUse) > MaxWhenToUseLength {
		errs = append(errs, fmt.Errorf("when_to_use exceeds %d characters", MaxWhenToUseLength))
	}

	if len(s.ArgumentHint) > MaxArgumentHintLength {
		errs = append(errs, fmt.Errorf("argument-hint exceeds %d characters", MaxArgumentHintLength))
	}

	// Validate context value
	if s.Context != "" && s.Context != SkillContextInline && s.Context != SkillContextFork {
		errs = append(errs, fmt.Errorf("invalid context value: %s (must be 'inline' or 'fork')", s.Context))
	}

	return errors.Join(errs...)
}

// Parse parses a SKILL.md file.
func Parse(path string) (*Skill, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	frontmatter, body, err := splitFrontmatter(string(content))
	if err != nil {
		return nil, err
	}

	var skill Skill
	if err := yaml.Unmarshal([]byte(frontmatter), &skill); err != nil {
		return nil, fmt.Errorf("parsing frontmatter: %w", err)
	}

	skill.Instructions = strings.TrimSpace(body)
	skill.Path = filepath.Dir(path)
	skill.SkillFilePath = path

	return &skill, nil
}

// splitFrontmatter extracts YAML frontmatter and body from markdown content.
func splitFrontmatter(content string) (frontmatter, body string, err error) {
	// Normalize line endings to \n for consistent parsing.
	content = strings.ReplaceAll(content, "\r\n", "\n")
	if !strings.HasPrefix(content, "---\n") {
		return "", "", errors.New("no YAML frontmatter found")
	}

	rest := strings.TrimPrefix(content, "---\n")
	before, after, ok := strings.Cut(rest, "\n---")
	if !ok {
		return "", "", errors.New("unclosed frontmatter")
	}

	return before, after, nil
}

// Discover finds all valid skills in the given paths.
func Discover(paths []string) []*Skill {
	var skills []*Skill
	var mu sync.Mutex
	seen := make(map[string]bool)
	errorsMap := make(map[string]error)

	for _, base := range paths {
		// We use fastwalk with Follow: true instead of filepath.WalkDir because
		// WalkDir doesn't follow symlinked directories at any depth—only entry
		// points. This ensures skills in symlinked subdirectories are discovered.
		// fastwalk is concurrent, so we protect shared state (seen, skills) with mu.
		conf := fastwalk.Config{
			Follow:  true,
			ToSlash: fastwalk.DefaultToSlash(),
		}
		fastwalk.Walk(&conf, base, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() || d.Name() != SkillFileName {
				return nil
			}
			mu.Lock()
			if seen[path] {
				mu.Unlock()
				return nil
			}
			seen[path] = true
			mu.Unlock()
			skill, err := Parse(path)
			if err != nil {
				slog.Warn("Failed to parse skill file", "path", path, "error", err)
				mu.Lock()
				errorsMap[path] = err
				mu.Unlock()
				return nil
			}
			if err := skill.Validate(); err != nil {
				slog.Warn("Skill validation failed", "path", path, "error", err)
				mu.Lock()
				errorsMap[path] = err
				mu.Unlock()
				return nil
			}
			slog.Debug("Successfully loaded skill", "name", skill.Name, "path", path)
			mu.Lock()
			skills = append(skills, skill)
			mu.Unlock()
			return nil
		})
	}

	// Sort skills by name for deterministic output. The fastwalk traversal
	// is concurrent, so append order varies between calls. Without sorting,
	// the generated prompt XML (ToPromptXML) differs each run, which
	// changes the system prompt hash and breaks prompt caching.
	slices.SortFunc(skills, func(a, b *Skill) int {
		return strings.Compare(a.Name, b.Name)
	})

	PublishState(DiscoveryState{
		Loaded: skills,
		Errors: errorsMap,
	})

	return skills
}

// ToPromptXML generates XML for injection into the system prompt.
func ToPromptXML(skills []*Skill) string {
	if len(skills) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("<available_skills>\n")
	for _, s := range skills {
		sb.WriteString("  <skill>\n")
		fmt.Fprintf(&sb, "    <name>%s</name>\n", escape(s.Name))
		fmt.Fprintf(&sb, "    <description>%s</description>\n", escape(s.Description))
		// Use skill:// virtual URL instead of absolute filesystem path.
		// This hides the physical location and enables portable skill references.
		// url.PathEscape encodes spaces and special characters in the skill name
		// so the resulting URL can be correctly resolved by ResolveSkillURL.
		fmt.Fprintf(&sb, "    <location>skill://%s</location>\n", escape(url.PathEscape(s.Name)))

		// Write when_to_use if present
		if s.WhenToUse != "" {
			fmt.Fprintf(&sb, "    <when_to_use>%s</when_to_use>\n", escape(s.WhenToUse))
		}

		// Write allowed_tools if present
		if len(s.AllowedTools) > 0 {
			sb.WriteString("    <allowed_tools>\n")
			for _, tool := range s.AllowedTools {
				fmt.Fprintf(&sb, "      <tool>%s</tool>\n", escape(tool))
			}
			sb.WriteString("    </allowed_tools>\n")
		}

		// Write arguments if present
		if len(s.Arguments) > 0 {
			sb.WriteString("    <arguments>\n")
			for _, arg := range s.Arguments {
				fmt.Fprintf(&sb, "      <arg>%s</arg>\n", escape(arg))
			}
			sb.WriteString("    </arguments>\n")
		}

		// Write argument_hint if present
		if s.ArgumentHint != "" {
			fmt.Fprintf(&sb, "    <argument_hint>%s</argument_hint>\n", escape(s.ArgumentHint))
		}

		// Write context if present (non-default)
		if s.Context != "" && s.Context != SkillContextInline {
			fmt.Fprintf(&sb, "    <context>%s</context>\n", s.Context)
		}

		// Write model if present
		if s.Model != "" {
			fmt.Fprintf(&sb, "    <model>%s</model>\n", escape(s.Model))
		}

		sb.WriteString("  </skill>\n")
	}
	sb.WriteString("</available_skills>")
	return sb.String()
}

func escape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&apos;")
	return r.Replace(s)
}

// SubstituteArguments replaces $ARGUMENTS placeholders in content with actual argument values.
// Supports:
//   - $ARGUMENTS - replaced with the full arguments string
//   - $ARGUMENTS[0], $ARGUMENTS[1], etc. - replaced with individual indexed arguments
//   - $0, $1, etc. - shorthand for $ARGUMENTS[0], $ARGUMENTS[1]
//   - Named arguments (e.g., $foo, $bar) - when argument names are defined in skill.Arguments
//
// The function parses arguments using shell-like quoting rules.
func SubstituteArguments(content, args string, argNames []string) string {
	if args == "" {
		return content
	}

	originalContent := content
	parsedArgs := parseArguments(args)

	// Replace named arguments (e.g., $foo, $bar) with their values
	// Named arguments map to positions: argNames[0] -> parsedArgs[0], etc.
	for i, name := range argNames {
		if name == "" {
			continue
		}
		replacement := ""
		if i < len(parsedArgs) {
			replacement = parsedArgs[i]
		}
		content = replaceNamedArg(content, name, replacement)
	}

	// Replace indexed arguments ($ARGUMENTS[0], $ARGUMENTS[1], etc.)
	indexedPattern := regexp.MustCompile(`\$ARGUMENTS\[(\d+)\]`)
	content = indexedPattern.ReplaceAllStringFunc(content, func(match string) string {
		submatches := indexedPattern.FindStringSubmatch(match)
		if len(submatches) < 2 {
			return match
		}
		index := 0
		fmt.Sscanf(submatches[1], "%d", &index)
		if index < len(parsedArgs) {
			return parsedArgs[index]
		}
		return ""
	})

	// Replace shorthand indexed arguments ($0, $1, etc.)
	// Only match $N where N is digits and not followed by word character
	shorthandPattern := regexp.MustCompile(`\$(\d+)`)
	content = shorthandPattern.ReplaceAllStringFunc(content, func(match string) string {
		submatches := shorthandPattern.FindStringSubmatch(match)
		if len(submatches) < 2 {
			return match
		}
		// Check if followed by word character - if so, don't replace
		// We do this by finding the position in the original content
		index := 0
		fmt.Sscanf(submatches[1], "%d", &index)
		if index < len(parsedArgs) {
			return parsedArgs[index]
		}
		return ""
	})

	// Replace $ARGUMENTS with the full arguments string
	content = strings.ReplaceAll(content, "$ARGUMENTS", args)

	// If no placeholders were found, append the arguments
	if content == originalContent {
		content = content + "\n\nARGUMENTS: " + args
	}

	return content
}

// replaceNamedArg replaces $name in content with replacement, but only if
// $name is not followed by [ or word characters.
func replaceNamedArg(content, name, replacement string) string {
	prefix := "$" + name
	result := strings.Builder{}
	i := 0
	for i < len(content) {
		// Look for $name
		if strings.HasPrefix(content[i:], prefix) {
			// Check what follows
			endPos := i + len(prefix)
			if endPos >= len(content) {
				// $name is at end of string
				result.WriteString(replacement)
				break
			}
			nextChar := content[endPos]
			// Don't replace if followed by [ or word character
			if nextChar == '[' || isWordChar(nextChar) {
				result.WriteByte(content[i])
				i++
				continue
			}
			// Safe to replace
			result.WriteString(replacement)
			i = endPos
			continue
		}
		result.WriteByte(content[i])
		i++
	}
	return result.String()
}

func isWordChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
}

// parseArguments parses an arguments string into an array of individual arguments.
// Uses shell-like argument parsing including quoted strings.
func parseArguments(args string) []string {
	if args == "" {
		return nil
	}

	var result []string
	var current strings.Builder
	inQuote := false
	quoteChar := byte(0)
	escaped := false

	for i := 0; i < len(args); i++ {
		c := args[i]

		if escaped {
			current.WriteByte(c)
			escaped = false
			continue
		}

		if c == '\\' {
			escaped = true
			continue
		}

		if inQuote {
			if c == quoteChar {
				inQuote = false
				quoteChar = 0
			} else {
				current.WriteByte(c)
			}
			continue
		}

		if c == '"' || c == '\'' {
			inQuote = true
			quoteChar = c
			continue
		}

		if c == ' ' || c == '\t' {
			if current.Len() > 0 {
				result = append(result, current.String())
				current.Reset()
			}
			continue
		}

		current.WriteByte(c)
	}

	if current.Len() > 0 {
		result = append(result, current.String())
	}

	return result
}
