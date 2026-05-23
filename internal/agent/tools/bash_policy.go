package tools

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/crush/internal/shell"
	"mvdan.cc/sh/v3/syntax"
)

type BashToolOptions struct {
	RestrictedToGitReadOnly bool
	DisableBackground       bool
	DescriptionOverride     string
}

var restrictedGitGlobalFlags = map[string]bool{
	"-C":                  true,
	"--git-dir":           true,
	"--no-optional-locks": false,
	"--no-pager":          false,
	"--work-tree":         true,
}

var restrictedGitSubcommands = map[string]struct{}{
	"blame":      {},
	"describe":   {},
	"diff":       {},
	"grep":       {},
	"log":        {},
	"ls-files":   {},
	"merge-base": {},
	"rev-parse":  {},
	"show":       {},
	"status":     {},
}

var restrictedGitBlockedFlags = map[string]struct{}{
	"--ext-diff":            {},
	"--open-files-in-pager": {},
	"--output":              {},
	"--textconv":            {},
}

// restrictedGitAllowedPipeCommands is the set of read-only filter programs
// that may appear after a pipe following a git command. These commands can
// only consume stdin and write to stdout — they cannot mutate repository
// state, network, or the filesystem.
var restrictedGitAllowedPipeCommands = map[string]struct{}{
	"grep":  {},
	"egrep": {},
	"fgrep": {},
	"head":  {},
	"tail":  {},
	"sort":  {},
	"uniq":  {},
	"wc":    {},
	"tr":    {},
	"cut":   {},
}

func restrictedGitBashDescription() string {
	return `Executes a restricted shell for local git inspection only.

Allowed usage:
- Exactly one direct git command per call
- Read-only subcommands only: git blame, git describe, git diff, git grep, git log, git ls-files, git merge-base, git rev-parse, git show, git status
- Optional global git flags: -C, --git-dir, --work-tree, --no-pager, --no-optional-locks
- Stderr suppression: 2>/dev/null is permitted on the git command
- Piping git output through read-only filters: grep, egrep, fgrep, head, tail, sort, uniq, wc, cut, tr

Blocked usage:
- Any non-git command or wrapper shell such as bash -lc, sh -c, cmd /c, or powershell -c
- Any mutating git command such as checkout, switch, restore, reset, clean, stash, commit, merge, rebase, cherry-pick, revert, apply, push, pull, or fetch
- Multiple commands with ; && || separators, or background execution
- Any redirection other than 2>/dev/null

Use this tool only for inspecting repository state and history.`
}

func RestrictedGitBashDescription() string {
	return restrictedGitBashDescription()
}

func validateNoWrapperShellCommand(command string) error {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return fmt.Errorf("failed to parse shell command: %w", err)
	}

	var wrapperErr error
	syntax.Walk(file, func(node syntax.Node) bool {
		if wrapperErr != nil {
			return false
		}

		call, ok := node.(*syntax.CallExpr)
		if !ok {
			return true
		}

		args := make([]string, 0, len(call.Args))
		for _, word := range call.Args {
			arg, ok := literalWord(word)
			if !ok {
				return true
			}
			args = append(args, arg)
		}

		wrapperErr = validateWrapperShellArgs(args)
		return wrapperErr == nil
	})

	return wrapperErr
}

func validateWrapperShellArgs(args []string) error {
	if len(args) < 2 {
		return nil
	}

	command := strings.ToLower(args[0])
	switch command {
	case "bash", "sh", "zsh", "dash":
		if hasLeadingShellFlag(args[1:], []string{"-c", "-lc"}, "-") {
			return fmt.Errorf("bash tool does not allow wrapper shells like %s -c; run the command directly", args[0])
		}
	case "cmd", "cmd.exe":
		if hasLeadingShellFlag(args[1:], []string{"/c"}, "/") {
			return fmt.Errorf("bash tool does not allow wrapper shells like %s /c; run the command directly", args[0])
		}
	case "powershell", "powershell.exe", "pwsh", "pwsh.exe":
		if hasLeadingShellFlag(args[1:], []string{"-c", "-command"}, "-") {
			return fmt.Errorf("bash tool does not allow wrapper shells like %s -Command; run the command directly", args[0])
		}
	}

	return nil
}

func hasLeadingShellFlag(args []string, blocked []string, prefix string) bool {
	for _, arg := range args {
		lower := strings.ToLower(arg)
		for _, candidate := range blocked {
			if lower == candidate {
				return true
			}
		}
		if !strings.HasPrefix(arg, prefix) {
			return false
		}
	}
	return false
}

func restrictedGitBlockFunc() shell.BlockFunc {
	return func(args []string) bool {
		if validateRestrictedGitArgs(args) == nil {
			return false
		}
		return validateRestrictedGitPipeCommand(args) != nil
	}
}

// isAllowedRedirect returns true for a redirect that is exactly "2>/dev/null"
// or "2>&1", which are safe redirections of stderr in restricted-git-bash mode.
func isAllowedRedirect(r *syntax.Redirect) bool {
	if r.N == nil || r.N.Value != "2" {
		return false
	}
	target, ok := literalWord(r.Word)
	if !ok {
		return false
	}
	if r.Op == syntax.RdrOut && target == "/dev/null" {
		return true
	}
	if r.Op == syntax.DplOut && target == "1" {
		return true
	}
	return false
}

// validateRestrictedGitCommand validates that command is a single git
// read-only invocation, optionally with 2>/dev/null and/or piped through
// approved read-only filter commands (grep, head, tail, …).
//
// If the POSIX shell parser fails (e.g., due to backticks or unusual quoting
// in grep patterns), we fall back to simple word-splitting and validate the
// resulting args directly. This avoids false positives where a safe git
// command is rejected because the mvdan.cc/sh parser cannot parse the raw
// string (e.g., git grep -n "CanSpawn(" -- "internal/agent/").
func validateRestrictedGitCommand(command string) error {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		// Parser failed — likely due to backticks, unusual quoting, or
		// Windows paths in the command string. Fall back to simple
		// word-splitting and validate the args directly. This is safe
		// because validateRestrictedGitArgs checks the command name and
		// subcommand, which is the core security gate.
		args := simpleWordSplit(command)
		if len(args) == 0 {
			return fmt.Errorf("restricted git bash requires a git command")
		}
		return validateRestrictedGitArgs(args)
	}

	if len(file.Stmts) == 0 {
		return fmt.Errorf("restricted git bash requires a git command")
	}

	for _, stmt := range file.Stmts {
		if err := validateStmt(stmt); err != nil {
			return err
		}
	}

	return nil
}

func validateStmt(stmt *syntax.Stmt) error {
	if stmt == nil {
		return fmt.Errorf("restricted git bash requires a git command")
	}
	if stmt.Negated || stmt.Background || stmt.Coprocess || stmt.Disown {
		return fmt.Errorf("restricted git bash does not allow shell control operators")
	}

	for _, r := range stmt.Redirs {
		if !isAllowedRedirect(r) {
			return fmt.Errorf("restricted git bash does not allow redirection")
		}
	}

	if stmt.Cmd == nil {
		return fmt.Errorf("restricted git bash requires a git command")
	}
	return validateCmd(stmt.Cmd)
}

func validateCmd(cmd syntax.Command) error {
	switch c := cmd.(type) {
	case *syntax.CallExpr:
		if len(c.Assigns) > 0 {
			return fmt.Errorf("restricted git bash does not allow shell assignments")
		}
		args := make([]string, 0, len(c.Args))
		for _, word := range c.Args {
			arg, ok := literalWord(word)
			if !ok {
				return fmt.Errorf("restricted git bash does not allow shell expansions or substitutions")
			}
			args = append(args, arg)
		}
		if len(args) > 0 && args[0] == "cd" {
			if len(args) > 2 {
				return fmt.Errorf("restricted git bash does not allow cd with more than one argument")
			}
			return nil
		}
		return validateRestrictedGitArgs(args)

	case *syntax.BinaryCmd:
		if c.Op == syntax.Pipe {
			return validateRestrictedGitPipeline(c)
		}
		if c.Op == syntax.AndStmt || c.Op == syntax.OrStmt {
			if err := validateStmt(c.X); err != nil {
				return err
			}
			return validateStmt(c.Y)
		}
		return fmt.Errorf("restricted git bash does not allow shell control operators")

	default:
		return fmt.Errorf("restricted git bash only allows a direct git command")
	}
}

// validateRestrictedGitPipeline validates a pipeline whose first stage is a
// git read-only command and whose subsequent stages are approved read-only
// filter commands. Redirections inside pipeline stages are not inspected
// because the only allowed form (2>/dev/null) is placed on the top-level
// statement and already validated above.
func validateRestrictedGitPipeline(pipeline *syntax.BinaryCmd) error {
	stages, err := collectPipelineStages(pipeline)
	if err != nil {
		return err
	}
	if len(stages) == 0 {
		return fmt.Errorf("restricted git bash requires a git command")
	}

	// First stage: must be a plain git invocation.
	firstArgs, err := callExprArgs(stages[0])
	if err != nil {
		return fmt.Errorf("restricted git bash: first pipeline stage must be a direct git command: %w", err)
	}
	if err := validateRestrictedGitArgs(firstArgs); err != nil {
		return fmt.Errorf("%w. Use glob, grep, or read tools for file exploration instead of bash", err)
	}

	// Remaining stages: must be approved read-only filter commands.
	for idx, stage := range stages[1:] {
		args, err := callExprArgs(stage)
		if err != nil {
			return fmt.Errorf("restricted git bash: pipeline stage %d must be a direct command: %w", idx+2, err)
		}
		if len(args) == 0 {
			return fmt.Errorf("restricted git bash: empty pipeline stage %d", idx+2)
		}
		if err := validateRestrictedGitPipeCommand(args); err != nil {
			return err
		}
	}
	return nil
}

func validateRestrictedGitPipeCommand(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("restricted git bash requires a pipeline command")
	}
	command := args[0]
	if _, ok := restrictedGitAllowedPipeCommands[command]; !ok {
		return fmt.Errorf("restricted git bash does not allow %q after a pipe (only read-only filters are permitted)", command)
	}

	switch command {
	case "grep", "egrep", "fgrep":
		return validateRestrictedGitGrepPipeArgs(command, args)
	case "head":
		return validateRestrictedGitNoFileOperandPipeArgs(command, args, map[string]struct{}{
			"-c": {}, "--bytes": {}, "-n": {}, "--lines": {},
		}, nil, nil)
	case "tail":
		return validateRestrictedGitNoFileOperandPipeArgs(command, args, map[string]struct{}{
			"-c": {}, "--bytes": {}, "-n": {}, "--lines": {},
		}, map[string]struct{}{
			"-f": {}, "-F": {}, "--follow": {},
		}, nil)
	case "sort":
		return validateRestrictedGitNoFileOperandPipeArgs(command, args, map[string]struct{}{
			"-k": {}, "--key": {}, "-S": {}, "--buffer-size": {}, "-t": {}, "--field-separator": {},
		}, map[string]struct{}{
			"-o": {}, "--output": {},
		}, []string{"-o"})
	case "uniq":
		return validateRestrictedGitNoFileOperandPipeArgs(command, args, map[string]struct{}{
			"-f": {}, "--skip-fields": {}, "-s": {}, "--skip-chars": {}, "-w": {}, "--check-chars": {},
		}, nil, nil)
	case "wc":
		return validateRestrictedGitNoFileOperandPipeArgs(command, args, nil, nil, nil)
	case "cut":
		return validateRestrictedGitNoFileOperandPipeArgs(command, args, map[string]struct{}{
			"-b": {}, "--bytes": {}, "-c": {}, "--characters": {}, "-d": {}, "--delimiter": {}, "-f": {}, "--fields": {}, "--output-delimiter": {},
		}, nil, nil)
	case "tr":
		return validateRestrictedGitTrPipeArgs(command, args)
	default:
		return nil
	}
}

func validateRestrictedGitGrepPipeArgs(command string, args []string) error {
	patternByOption := false
	positionals := 0
	valueOptions := map[string]struct{}{
		"-A": {}, "--after-context": {}, "-B": {}, "--before-context": {}, "-C": {}, "--context": {}, "-D": {}, "--devices": {}, "-d": {}, "--directories": {}, "-e": {}, "--regexp": {}, "-m": {}, "--max-count": {}, "--binary-files": {}, "--label": {},
	}
	blockedOptions := map[string]struct{}{
		"-f": {}, "--file": {}, "--exclude-from": {}, "-r": {}, "-R": {}, "--recursive": {}, "--dereference-recursive": {},
	}
	blockedInlinePrefixes := []string{"-f"}

	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positionals += len(args) - i - 1
			break
		}
		if !isRestrictedGitPipeFlag(arg) {
			positionals++
			continue
		}
		flag, _, hasInlineValue := strings.Cut(arg, "=")
		if isRestrictedGitBlockedPipeOption(flag, arg, blockedOptions, blockedInlinePrefixes) {
			return fmt.Errorf("restricted git bash does not allow %s option %q in a pipeline", command, flag)
		}
		if strings.HasPrefix(arg, "-e") && arg != "-e" && !strings.HasPrefix(arg, "--") {
			patternByOption = true
			continue
		}
		if flag == "-e" || flag == "--regexp" {
			patternByOption = true
		}
		if _, needsValue := valueOptions[flag]; needsValue && !hasInlineValue {
			i++
			if i >= len(args) {
				return fmt.Errorf("restricted git bash requires a value after %s option %q", command, flag)
			}
		}
	}

	if patternByOption && positionals > 0 {
		return fmt.Errorf("restricted git bash does not allow file operands for %s in a pipeline", command)
	}
	if !patternByOption && positionals > 1 {
		return fmt.Errorf("restricted git bash only allows one pattern operand for %s in a pipeline", command)
	}
	return nil
}

func validateRestrictedGitNoFileOperandPipeArgs(command string, args []string, valueOptions, blockedOptions map[string]struct{}, blockedInlinePrefixes []string) error {
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			if i+1 < len(args) {
				return fmt.Errorf("restricted git bash does not allow file operands for %s in a pipeline", command)
			}
			break
		}
		if !isRestrictedGitPipeFlag(arg) {
			return fmt.Errorf("restricted git bash does not allow file operands for %s in a pipeline", command)
		}
		flag, _, hasInlineValue := strings.Cut(arg, "=")
		if isRestrictedGitBlockedPipeOption(flag, arg, blockedOptions, blockedInlinePrefixes) {
			return fmt.Errorf("restricted git bash does not allow %s option %q in a pipeline", command, flag)
		}
		if _, needsValue := valueOptions[flag]; needsValue && !hasInlineValue {
			i++
			if i >= len(args) {
				return fmt.Errorf("restricted git bash requires a value after %s option %q", command, flag)
			}
		}
	}
	return nil
}

func validateRestrictedGitTrPipeArgs(command string, args []string) error {
	positionals := 0
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positionals += len(args) - i - 1
			break
		}
		if isRestrictedGitPipeFlag(arg) {
			continue
		}
		positionals++
	}
	if positionals > 2 {
		return fmt.Errorf("restricted git bash only allows stdin filter operands for %s in a pipeline", command)
	}
	return nil
}

func isRestrictedGitPipeFlag(arg string) bool {
	return arg != "" && arg != "-" && strings.HasPrefix(arg, "-")
}

func hasRestrictedGitPipeFlagPrefix(arg string, prefixes []string) bool {
	if strings.HasPrefix(arg, "--") {
		return false
	}
	for _, prefix := range prefixes {
		if arg != prefix && strings.HasPrefix(arg, prefix) {
			return true
		}
	}
	return false
}

func isRestrictedGitBlockedPipeOption(flag string, arg string, blockedOptions map[string]struct{}, blockedInlinePrefixes []string) bool {
	if _, blocked := blockedOptions[flag]; blocked {
		return true
	}
	if hasRestrictedGitPipeFlagPrefix(arg, blockedInlinePrefixes) {
		return true
	}
	if !strings.HasPrefix(flag, "--") {
		return false
	}
	for blockedFlag := range blockedOptions {
		if strings.HasPrefix(blockedFlag, "--") {
			if flag == blockedFlag {
				return true
			}
			// Check if arg is exactly blockedFlag=value (not a prefix match like --file blocking --filename=value)
			if strings.HasPrefix(arg, blockedFlag+"=") && len(arg) > len(blockedFlag) && arg[len(blockedFlag)] == '=' {
				return true
			}
		}
	}
	return false
}

// collectPipelineStages flattens a left-recursive pipe chain into an ordered
// slice of Stmts. For  a | b | c  the tree is
//
//	BinaryCmd{X: BinaryCmd{X:a, Y:b}, Y:c}
//
// so we walk left-first to produce [a, b, c].
func collectPipelineStages(b *syntax.BinaryCmd) ([]*syntax.Stmt, error) {
	var stages []*syntax.Stmt

	var flatten func(b *syntax.BinaryCmd) error
	flatten = func(b *syntax.BinaryCmd) error {
		if b.Op != syntax.Pipe {
			return fmt.Errorf("restricted git bash does not allow shell control operators")
		}
		// Recurse into left side if it is also a pipeline.
		if left, ok := b.X.Cmd.(*syntax.BinaryCmd); ok {
			if err := flatten(left); err != nil {
				return err
			}
		} else {
			stages = append(stages, b.X)
		}
		stages = append(stages, b.Y)
		return nil
	}
	if err := flatten(b); err != nil {
		return nil, err
	}
	return stages, nil
}

// callExprArgs extracts the literal args from a Stmt whose Cmd is a CallExpr.
// Assignments and shell expansions are rejected.
func callExprArgs(s *syntax.Stmt) ([]string, error) {
	if s.Negated || s.Background || s.Coprocess || s.Disown {
		return nil, fmt.Errorf("shell control operators not allowed")
	}
	// Allow safe stderr redirections inside pipeline stages too.
	for _, r := range s.Redirs {
		if !isAllowedRedirect(r) {
			return nil, fmt.Errorf("redirection not allowed in pipeline stage")
		}
	}
	call, ok := s.Cmd.(*syntax.CallExpr)
	if !ok {
		return nil, fmt.Errorf("only simple commands are allowed")
	}
	if len(call.Assigns) > 0 {
		return nil, fmt.Errorf("shell assignments are not allowed")
	}
	args := make([]string, 0, len(call.Args))
	for _, word := range call.Args {
		arg, ok := literalWord(word)
		if !ok {
			return nil, fmt.Errorf("shell expansions or substitutions are not allowed")
		}
		args = append(args, arg)
	}
	return args, nil
}

// simpleWordSplit splits a command string into words using basic shell-like
// quoting rules. It handles single-quoted and double-quoted strings, and
// treats backslash as an escape character inside double quotes. This is a
// simplified parser used as a fallback when the mvdan.cc/sh POSIX parser
// fails (e.g., due to backticks or unusual quoting in grep patterns).
func simpleWordSplit(command string) []string {
	var args []string
	var current strings.Builder
	inSingleQuote := false
	inDoubleQuote := false

	for i := 0; i < len(command); i++ {
		ch := command[i]

		if inSingleQuote {
			if ch == '\'' {
				inSingleQuote = false
			} else {
				current.WriteByte(ch)
			}
			continue
		}

		if inDoubleQuote {
			if ch == '\\' && i+1 < len(command) {
				next := command[i+1]
				// Only escape special characters inside double quotes
				switch next {
				case '"', '\\', '$', '`':
					current.WriteByte(next)
					i++
				default:
					// Backslash is literal inside double quotes for non-special chars
					current.WriteByte(ch)
				}
			} else if ch == '"' {
				inDoubleQuote = false
			} else {
				current.WriteByte(ch)
			}
			continue
		}

		// Outside quotes
		if ch == '\'' {
			inSingleQuote = true
			continue
		}
		if ch == '"' {
			inDoubleQuote = true
			continue
		}
		if ch == '\\' && i+1 < len(command) {
			current.WriteByte(command[i+1])
			i++
			continue
		}
		if ch == ' ' || ch == '\t' {
			if current.Len() > 0 {
				args = append(args, current.String())
				current.Reset()
			}
			continue
		}
		current.WriteByte(ch)
	}

	if current.Len() > 0 {
		args = append(args, current.String())
	}
	return args
}

func validateRestrictedGitArgs(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("restricted git bash requires a git command")
	}
	if args[0] != "git" {
		return fmt.Errorf("restricted git bash only allows direct git commands (got %q). Use the glob, grep, or read tools for file exploration instead", args[0])
	}

	i := 1
	for i < len(args) {
		arg := args[i]
		if arg == "" || arg == "-" || !strings.HasPrefix(arg, "-") {
			break
		}

		flag, _, hasInlineValue := strings.Cut(arg, "=")
		takesValue, ok := restrictedGitGlobalFlags[flag]
		if !ok {
			break
		}

		if takesValue && !hasInlineValue {
			i++
			if i >= len(args) {
				return fmt.Errorf("restricted git bash requires a value after %s", flag)
			}
		}
		i++
	}

	if i >= len(args) {
		return fmt.Errorf("restricted git bash requires a git subcommand")
	}

	subcommand := args[i]
	if strings.HasPrefix(subcommand, "-") {
		return fmt.Errorf("restricted git bash does not allow git option %q", subcommand)
	}
	if _, ok := restrictedGitSubcommands[subcommand]; !ok {
		return fmt.Errorf("restricted git bash does not allow git %s", subcommand)
	}

	for _, arg := range args[i+1:] {
		if arg == "--" {
			break
		}
		if arg == "" || arg == "-" || !strings.HasPrefix(arg, "-") {
			continue
		}

		flag, _, _ := strings.Cut(arg, "=")
		for blockedFlag := range restrictedGitBlockedFlags {
			if strings.HasPrefix(blockedFlag, flag) {
				return fmt.Errorf("restricted git bash does not allow git option %q", flag)
			}
		}
	}

	return nil
}

func literalWord(word *syntax.Word) (string, bool) {
	if word == nil {
		return "", false
	}
	return literalWordParts(word.Parts)
}

func literalWordParts(parts []syntax.WordPart) (string, bool) {
	var b strings.Builder
	for _, part := range parts {
		switch x := part.(type) {
		case *syntax.Lit:
			b.WriteString(x.Value)
		case *syntax.SglQuoted:
			b.WriteString(x.Value)
		case *syntax.DblQuoted:
			value, ok := literalWordParts(x.Parts)
			if !ok {
				return "", false
			}
			b.WriteString(value)
		default:
			return "", false
		}
	}
	return b.String(), true
}
