package prompt

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"text/template"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/home"
	"github.com/charmbracelet/crush/internal/shell"
	"github.com/charmbracelet/crush/internal/skills"
)

// Prompt represents a template-based prompt generator.
type Prompt struct {
	name                 string
	template             string
	now                  func() time.Time
	platform             string
	workingDir           string
	disableGlobalFile    bool
	omitProjectContext   bool
	contextPathsOverride []string
	gitStatus            string
	role                 string
	hasBashTool          bool
}

type PromptDat struct {
	Provider           string
	Model              string
	Config             config.Config
	WorkingDir         string
	IsGitRepo          bool
	Platform           string
	Date               string
	GitStatus          string
	ContextFiles       []ContextFile
	GlobalContextFiles []ContextFile
	AvailSkillXML      string
	Role               string
	// HasBashTool gates the <bash_commands> section: it is only rendered
	// when the bash tool is in the agent's toolset, keeping the prompt
	// stable per agent configuration.
	HasBashTool bool
}

type ContextFile struct {
	Path    string
	Content string
}

type Option func(*Prompt)

func WithTimeFunc(fn func() time.Time) Option {
	return func(p *Prompt) {
		p.now = fn
	}
}

func WithPlatform(platform string) Option {
	return func(p *Prompt) {
		p.platform = platform
	}
}

func WithWorkingDir(workingDir string) Option {
	return func(p *Prompt) {
		p.workingDir = workingDir
	}
}

func WithDisableGlobalContextFile(disable bool) Option {
	return func(p *Prompt) {
		p.disableGlobalFile = disable
	}
}

func WithOmitProjectContextFiles(omit bool) Option {
	return func(p *Prompt) {
		p.omitProjectContext = omit
	}
}

func WithContextPathsOverride(paths []string) Option {
	return func(p *Prompt) {
		if paths == nil {
			p.contextPathsOverride = nil
			return
		}
		p.contextPathsOverride = append([]string(nil), paths...)
	}
}

// WithGitStatus sets a pre-computed git status string to use instead of
// running git commands. This allows callers to freeze the git status for
// prompt caching stability across turns within a session.
func WithGitStatus(status string) Option {
	return func(p *Prompt) {
		p.gitStatus = status
	}
}

// WithRole sets an optional specialist identity for the agent. When non-empty,
// it is exposed to the system prompt template as {{.Role}}.
func WithRole(role string) Option {
	return func(p *Prompt) {
		p.role = role
	}
}

// WithHasBashTool marks whether the bash tool is in the agent's toolset, so
// the template can conditionally render the <bash_commands> section.
func WithHasBashTool(has bool) Option {
	return func(p *Prompt) {
		p.hasBashTool = has
	}
}

func NewPrompt(name, promptTemplate string, opts ...Option) (*Prompt, error) {
	p := &Prompt{
		name:     name,
		template: promptTemplate,
		now:      time.Now,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

func (p *Prompt) Build(ctx context.Context, provider, model string, store *config.ConfigStore) (string, error) {
	t, err := template.New(p.name).Parse(p.template)
	if err != nil {
		return "", fmt.Errorf("parsing template: %w", err)
	}
	var sb strings.Builder
	d, err := p.promptData(ctx, provider, model, store)
	if err != nil {
		return "", err
	}
	if err := t.Execute(&sb, d); err != nil {
		return "", fmt.Errorf("executing template: %w", err)
	}

	return sb.String(), nil
}

// Default context file size limits, aligned with maka-agent's workspace
// instructions budget. 0 disables the per-file cap; a total of 0 disables
// the aggregate cap (restores the previous unbounded behavior).
const (
	defaultContextFileMaxChars       = 6000
	defaultContextFilesMaxTotalChars = 14000
)

// contextFileTruncateMarker is appended to a context file whose content was
// truncated so the model knows the injected text is a prefix, not the whole
// file.
const contextFileTruncateMarker = "\n\n[context truncated: content exceeds the configured context file size limit]"

// contextFileBudgetExhaustedMarker is injected in place of a file that could
// not fit within the aggregate budget, so the model knows the file exists but
// was omitted (it would otherwise silently lose project instructions).
const contextFileBudgetExhaustedMarker = "[context file omitted: aggregate context file budget exhausted — read this file directly if needed]"

// processFile reads a context file, truncating it to maxChars runes when
// maxChars > 0. It returns the content and its rune length so callers can
// account against a shared budget in a single unit (runes).
func processFile(filePath string, maxChars int) (*ContextFile, int) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, 0
	}
	text := string(content)
	runes := []rune(text)
	if maxChars > 0 && len(runes) > maxChars {
		// Truncate on a rune boundary so we never split a multi-byte rune.
		text = string(runes[:maxChars]) + contextFileTruncateMarker
		runes = []rune(text)
	}
	return &ContextFile{
		Path:    filePath,
		Content: text,
	}, len(runes)
}

// contextFileBudget tracks the shared aggregate budget across all context
// paths. It uses a rune counter so per-file truncation (runes) and the
// aggregate cap (runes) are measured in the same unit.
type contextFileBudget struct {
	// remaining is the number of runes still available for context files.
	// 0 or negative means no aggregate cap is active.
	remaining int
	// capped reports whether the aggregate budget is active.
	capped bool
}

func newContextFileBudget(maxTotalChars int) *contextFileBudget {
	if maxTotalChars <= 0 {
		return &contextFileBudget{capped: false}
	}
	return &contextFileBudget{remaining: maxTotalChars, capped: true}
}

// account consumes n runes from the budget. It returns false when the file
// no longer fits and the caller should emit a budget-exhausted marker.
func (b *contextFileBudget) account(n int) bool {
	if !b.capped {
		return true
	}
	if b.remaining <= 0 {
		return false
	}
	if n > b.remaining {
		return false
	}
	b.remaining -= n
	return true
}

// appendGlobalContextFile appends the global AGENTS.md to out, accounting it
// against the shared budget. Project context paths are processed first, so a
// large project AGENTS.md can crowd this file out; when that happens emit the
// same marker processContextPath uses instead of dropping it silently, or the
// user's global instructions disappear with no trace.
func appendGlobalContextFile(out []ContextFile, path string, maxFileChars int, budget *contextFileBudget) []ContextFile {
	result, runeLen := processFile(path, maxFileChars)
	if result == nil {
		return out
	}
	if !budget.account(runeLen) {
		return append(out, ContextFile{
			Path:    path,
			Content: contextFileBudgetExhaustedMarker,
		})
	}
	return append(out, *result)
}

func processContextPath(p string, store *config.ConfigStore, maxFileChars int, budget *contextFileBudget) []ContextFile {
	var contexts []ContextFile
	fullPath := p
	if !filepath.IsAbs(p) {
		fullPath = filepath.Join(store.WorkingDir(), p)
	}
	info, err := os.Stat(fullPath)
	if err != nil {
		return contexts
	}

	appendFile := func(path string) {
		if budget.capped && budget.remaining <= 0 {
			contexts = append(contexts, ContextFile{
				Path:    path,
				Content: contextFileBudgetExhaustedMarker,
			})
			return
		}
		result, runeLen := processFile(path, maxFileChars)
		if result == nil {
			return
		}
		if !budget.account(runeLen) {
			contexts = append(contexts, ContextFile{
				Path:    path,
				Content: contextFileBudgetExhaustedMarker,
			})
			return
		}
		contexts = append(contexts, *result)
	}

	if info.IsDir() {
		filepath.WalkDir(fullPath, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				appendFile(path)
			}
			return nil
		})
	} else {
		appendFile(fullPath)
	}
	return contexts
}

// expandPath expands ~ and environment variables in file paths
func expandPath(path string, store *config.ConfigStore) string {
	path = home.Long(path)
	// Handle environment variable expansion using the same pattern as config
	if strings.HasPrefix(path, "$") {
		if expanded, err := store.Resolver().ResolveValue(path); err == nil {
			path = expanded
		}
	}

	return path
}

func (p *Prompt) promptData(ctx context.Context, provider, model string, store *config.ConfigStore) (PromptDat, error) {
	workingDir := cmp.Or(p.workingDir, store.WorkingDir())
	platform := cmp.Or(p.platform, runtime.GOOS)

	projectFiles := map[string][]ContextFile{}

	cfg := store.Config()

	contextPaths := cfg.Options.ContextPaths
	if p.contextPathsOverride != nil {
		contextPaths = p.contextPathsOverride
	}
	// Pointer semantics: nil (unset) → default; 0 → unbounded; >0 → that cap.
	maxFileChars := defaultContextFileMaxChars
	if cfg.Options.ContextFileMaxChars != nil {
		maxFileChars = *cfg.Options.ContextFileMaxChars
	}
	maxTotalChars := defaultContextFilesMaxTotalChars
	if cfg.Options.ContextFilesMaxTotalChars != nil {
		maxTotalChars = *cfg.Options.ContextFilesMaxTotalChars
	}
	budget := newContextFileBudget(maxTotalChars)
	if !p.omitProjectContext {
		for _, pth := range contextPaths {
			expanded := expandPath(pth, store)
			pathKey := strings.ToLower(expanded)

			if _, ok := projectFiles[pathKey]; ok {
				continue
			}
			content := processContextPath(expanded, store, maxFileChars, budget)
			projectFiles[pathKey] = content
		}
	}

	// Load global AGENTS.md directly to ensure it's always injected into system prompt.
	var globalFiles []ContextFile
	if !p.disableGlobalFile {
		if globalAgentsPath := config.GlobalAgentsMD(); globalAgentsPath != "" {
			globalFiles = appendGlobalContextFile(globalFiles, globalAgentsPath, maxFileChars, budget)
		}
	}

	// Discover and load skills metadata.
	var availSkillXML string
	if len(cfg.Options.SkillsPaths) > 0 {
		expandedPaths := make([]string, 0, len(cfg.Options.SkillsPaths))
		for _, pth := range cfg.Options.SkillsPaths {
			expandedPaths = append(expandedPaths, expandPath(pth, store))
		}
		if discoveredSkills := skills.DiscoverCached(expandedPaths); len(discoveredSkills) > 0 {
			availSkillXML = skills.ToPromptXML(discoveredSkills, skills.SkillsPromptTokenBudget(effectiveContextWindowForConfig(*cfg)))
		}
	}

	isGit := isGitRepo(workingDir)
	data := PromptDat{
		Provider:      provider,
		Model:         model,
		Config:        *cfg,
		WorkingDir:    filepath.ToSlash(workingDir),
		IsGitRepo:     isGit,
		Platform:      platform,
		Date:          p.now().Format("1/2/2006"),
		AvailSkillXML: availSkillXML,
		Role:          p.role,
		HasBashTool:   p.hasBashTool,
	}
	if isGit {
		if p.gitStatus != "" {
			data.GitStatus = p.gitStatus
		} else {
			var err error
			data.GitStatus, err = getGitStatus(ctx, workingDir)
			if err != nil {
				return PromptDat{}, err
			}
		}
	}

	// Sort project file keys for deterministic output. projectFiles is a
	// map whose iteration order is random in Go, so without sorting the
	// ContextFiles slice would have a different order each Build() call,
	// changing the system prompt hash and breaking prompt caching.
	projectKeys := make([]string, 0, len(projectFiles))
	for k := range projectFiles {
		projectKeys = append(projectKeys, k)
	}
	slices.Sort(projectKeys)
	for _, k := range projectKeys {
		data.ContextFiles = append(data.ContextFiles, projectFiles[k]...)
	}
	data.GlobalContextFiles = globalFiles
	return data, nil
}

func isGitRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// effectiveContextWindowForConfig returns the large model's context window,
// or 0 when it cannot be resolved (callers fall back to the minimum budget).
func effectiveContextWindowForConfig(cfg config.Config) int {
	model := cfg.GetModelByType(config.SelectedModelTypeLarge)
	if model == nil || model.ContextWindow <= 0 {
		return 0
	}
	return int(model.ContextWindow)
}

// GetGitStatus computes the git status string for a directory.
// It runs git branch, git status, and git log commands.
func GetGitStatus(ctx context.Context, dir string) (string, error) {
	return getGitStatus(ctx, dir)
}

func getGitStatus(ctx context.Context, dir string) (string, error) {
	sh := shell.NewShell(&shell.Options{
		WorkingDir: dir,
	})
	branch, err := getGitBranch(ctx, sh)
	if err != nil {
		return "", err
	}
	status, err := getGitStatusSummary(ctx, sh)
	if err != nil {
		return "", err
	}
	commits, err := getGitRecentCommits(ctx, sh)
	if err != nil {
		return "", err
	}
	return branch + status + commits, nil
}

func getGitBranch(ctx context.Context, sh *shell.Shell) (string, error) {
	out, _, err := sh.Exec(ctx, "git branch --show-current")
	if err != nil {
		return "", nil
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", nil
	}
	return fmt.Sprintf("Current branch: %s\n", out), nil
}

func getGitStatusSummary(ctx context.Context, sh *shell.Shell) (string, error) {
	out, _, err := sh.Exec(ctx, "git status --short | head -20")
	if err != nil {
		return "", nil
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "Status: clean\n", nil
	}
	return fmt.Sprintf("Status:\n%s\n", out), nil
}

func getGitRecentCommits(ctx context.Context, sh *shell.Shell) (string, error) {
	out, _, err := sh.Exec(ctx, "git log --oneline -n 3")
	if err != nil || out == "" {
		return "", nil
	}
	out = strings.TrimSpace(out)
	return fmt.Sprintf("Recent commits:\n%s\n", out), nil
}

func (p *Prompt) Name() string {
	return p.name
}
