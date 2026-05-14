package autopermission

import (
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/permission"
	"mvdan.cc/sh/v3/syntax"
)

type ApprovalPolicy string

const (
	ApprovalPolicyUntrusted     ApprovalPolicy = "untrusted"
	ApprovalPolicyUnlessTrusted ApprovalPolicy = ApprovalPolicyUntrusted
	ApprovalPolicyOnFailure     ApprovalPolicy = "on-failure"
	ApprovalPolicyOnRequest     ApprovalPolicy = "on-request"
	ApprovalPolicyGranular      ApprovalPolicy = "granular"
	ApprovalPolicyNever         ApprovalPolicy = "never"
)

type GranularApprovalConfig struct {
	SandboxApproval    bool
	Rules              bool
	SkillApproval      bool
	RequestPermissions bool
	MCPElicitations    bool
}

type ApprovalPolicyConfig struct {
	Policy   ApprovalPolicy
	Granular GranularApprovalConfig
}

type ApprovalRequirement string

const (
	ApprovalRequirementAllow         ApprovalRequirement = "allow"
	ApprovalRequirementNeedsApproval ApprovalRequirement = "needs_approval"
	ApprovalRequirementForbidden     ApprovalRequirement = "forbidden"
)

type ExecPolicyDecision struct {
	Requirement ApprovalRequirement
	Reason      string
	Fingerprint string
}

type ExecPolicyRuleDecision string

const (
	ExecPolicyRuleAllow  ExecPolicyRuleDecision = "allow"
	ExecPolicyRulePrompt ExecPolicyRuleDecision = "prompt"
	ExecPolicyRuleForbid ExecPolicyRuleDecision = "forbid"
)

type ExecPolicyRule struct {
	Decision ExecPolicyRuleDecision
	Exact    []string
	Prefix   []string
	Reason   string
}

type approvalDomain string

const (
	approvalDomainSandbox approvalDomain = "sandbox"
	approvalDomainRules   approvalDomain = "rules"
)

var defaultExecPolicyRules = []ExecPolicyRule{
	{Decision: ExecPolicyRuleForbid, Exact: []string{"shutdown", "reboot", "halt", "poweroff"}, Reason: "system power commands are forbidden"},
	{Decision: ExecPolicyRuleForbid, Exact: []string{"mkfs", "dd", "fdisk", "diskpart", "format"}, Reason: "disk formatting commands are forbidden"},
	{Decision: ExecPolicyRuleForbid, Exact: []string{"zmodload", "emulate", "sysopen", "sysread", "syswrite", "sysseek", "zpty", "ztcp", "zsocket", "zf_rm", "zf_mv", "zf_ln", "zf_chmod", "zf_chown", "zf_mkdir", "zf_rmdir", "zf_chgrp"}, Reason: "dangerous shell builtins are forbidden"},
	{Decision: ExecPolicyRulePrompt, Exact: []string{"aws", "az", "curl", "gcloud", "gh", "kubectl", "remove-item", "scp", "ssh", "sudo", "wget"}, Reason: "network, privilege, cloud, cluster, or destructive commands require approval"},
	{Decision: ExecPolicyRulePrompt, Prefix: []string{"rm -", "git push", "git reset --hard", "terraform apply", "terraform destroy", "docker push", "npm publish"}, Reason: "dangerous mutating commands require approval"},
}

func DefaultApprovalPolicyConfig() ApprovalPolicyConfig {
	return ApprovalPolicyConfig{
		Policy: ApprovalPolicyUntrusted,
		Granular: GranularApprovalConfig{
			SandboxApproval:    true,
			Rules:              true,
			SkillApproval:      true,
			RequestPermissions: true,
			MCPElicitations:    true,
		},
	}
}

func ParseApprovalPolicy(value string) (ApprovalPolicy, bool) {
	switch ApprovalPolicy(strings.ToLower(strings.TrimSpace(value))) {
	case ApprovalPolicyUntrusted:
		return ApprovalPolicyUntrusted, true
	case ApprovalPolicyOnFailure:
		return ApprovalPolicyOnFailure, true
	case ApprovalPolicyOnRequest:
		return ApprovalPolicyOnRequest, true
	case ApprovalPolicyGranular:
		return ApprovalPolicyGranular, true
	case ApprovalPolicyNever:
		return ApprovalPolicyNever, true
	default:
		return "", false
	}
}

func EvaluateBashExecPolicy(req permission.PermissionRequest, policy ApprovalPolicy) ExecPolicyDecision {
	cfg := DefaultApprovalPolicyConfig()
	if policy != "" {
		cfg.Policy = policy
	}
	return EvaluateBashExecPolicyWithConfig(req, cfg, nil)
}

func EvaluateBashExecPolicyWithConfig(req permission.PermissionRequest, cfg ApprovalPolicyConfig, rules []ExecPolicyRule) ExecPolicyDecision {
	return evaluateBashExecPolicyWithConfigAndRules(req, normalizeApprovalPolicyConfig(cfg), appendExecPolicyRules(defaultExecPolicyRules, rules))
}

func evaluateBashExecPolicyWithRules(req permission.PermissionRequest, policy ApprovalPolicy, rules []ExecPolicyRule) ExecPolicyDecision {
	cfg := DefaultApprovalPolicyConfig()
	if policy != "" {
		cfg.Policy = policy
	}
	return evaluateBashExecPolicyWithConfigAndRules(req, cfg, rules)
}

func evaluateBashExecPolicyWithConfigAndRules(req permission.PermissionRequest, cfg ApprovalPolicyConfig, rules []ExecPolicyRule) ExecPolicyDecision {
	if req.ToolName != tools.BashToolName || req.Action != "execute" {
		return ExecPolicyDecision{Requirement: ApprovalRequirementNeedsApproval, Reason: "request is not a bash execute request"}
	}

	params, ok := req.Params.(tools.BashPermissionsParams)
	if !ok {
		return ExecPolicyDecision{Requirement: ApprovalRequirementNeedsApproval, Reason: "bash permission parameters are unavailable"}
	}
	if params.RunInBackground {
		return policyDecision(cfg, approvalDomainSandbox, "background execution requires approval")
	}

	command := strings.TrimSpace(params.Command)
	if command == "" {
		return ExecPolicyDecision{Requirement: ApprovalRequirementForbidden, Reason: "empty bash command is forbidden"}
	}

	fingerprint, file, err := CanonicalizeBashCommand(command)
	if err != nil {
		return policyDecision(cfg, approvalDomainSandbox, fmt.Sprintf("shell command could not be parsed: %v", err))
	}
	if len(file.Stmts) == 0 {
		return ExecPolicyDecision{Requirement: ApprovalRequirementForbidden, Reason: "empty bash command is forbidden"}
	}

	decision := ExecPolicyDecision{Requirement: ApprovalRequirementAllow}
	for _, stmt := range file.Stmts {
		stmtDecision := evaluateExecPolicyStmt(stmt, cfg, rules)
		decision = aggregateExecPolicyDecision(decision, stmtDecision)
		if decision.Requirement == ApprovalRequirementForbidden {
			return decision
		}
	}
	if strings.TrimSpace(decision.Reason) == "" {
		decision.Reason = "all shell statements are trusted read-only commands"
	}
	decision.Fingerprint = fingerprint
	return decision
}

func normalizeApprovalPolicyConfig(cfg ApprovalPolicyConfig) ApprovalPolicyConfig {
	if cfg.Policy == "" {
		cfg = DefaultApprovalPolicyConfig()
	}
	return cfg
}

func appendExecPolicyRules(defaults, overrides []ExecPolicyRule) []ExecPolicyRule {
	if len(overrides) == 0 {
		return slices.Clone(defaults)
	}
	rules := make([]ExecPolicyRule, 0, len(overrides)+len(defaults))
	rules = append(rules, overrides...)
	rules = append(rules, defaults...)
	return rules
}

func evaluateExecPolicyStmt(stmt *syntax.Stmt, cfg ApprovalPolicyConfig, rules []ExecPolicyRule) ExecPolicyDecision {
	if stmt == nil {
		return ExecPolicyDecision{Requirement: ApprovalRequirementForbidden, Reason: "empty shell statement is forbidden"}
	}
	if stmt.Negated || stmt.Background || stmt.Coprocess || stmt.Disown {
		return policyDecision(cfg, approvalDomainSandbox, "shell control operators require approval")
	}
	if hasUnsafeExecPolicyRedirection(stmt.Redirs) {
		return policyDecision(cfg, approvalDomainSandbox, "shell redirection requires approval")
	}
	return evaluateExecPolicyCommand(stmt.Cmd, cfg, rules)
}

func evaluateExecPolicyCommand(cmd syntax.Command, cfg ApprovalPolicyConfig, rules []ExecPolicyRule) ExecPolicyDecision {
	switch x := cmd.(type) {
	case *syntax.CallExpr:
		return evaluateExecPolicyCall(x, cfg, rules)
	case *syntax.BinaryCmd:
		return evaluateExecPolicyBinary(x, cfg, rules)
	case *syntax.Subshell, *syntax.Block, *syntax.IfClause, *syntax.WhileClause, *syntax.ForClause, *syntax.CaseClause, *syntax.FuncDecl, *syntax.ArithmCmd, *syntax.TestClause, *syntax.DeclClause, *syntax.LetClause, *syntax.TimeClause:
		return policyDecision(cfg, approvalDomainSandbox, "compound shell commands require approval")
	default:
		return policyDecision(cfg, approvalDomainSandbox, "unknown shell command form requires approval")
	}
}

func evaluateExecPolicyBinary(cmd *syntax.BinaryCmd, cfg ApprovalPolicyConfig, rules []ExecPolicyRule) ExecPolicyDecision {
	if cmd == nil {
		return ExecPolicyDecision{Requirement: ApprovalRequirementForbidden, Reason: "empty shell command is forbidden"}
	}
	if !isTrustedExecPolicyBinaryOperator(cmd.Op) {
		return policyDecision(cfg, approvalDomainSandbox, "shell control operator requires approval")
	}
	if cmd.Op == syntax.Pipe && pipelineExecPolicyTargetRequiresApproval(cmd.Y) {
		return policyDecision(cfg, approvalDomainSandbox, "piping into a shell interpreter requires approval")
	}

	left := evaluateExecPolicyStmt(cmd.X, cfg, rules)
	right := evaluateExecPolicyStmt(cmd.Y, cfg, rules)
	return aggregateExecPolicyDecision(left, right)
}

func isTrustedExecPolicyBinaryOperator(op syntax.BinCmdOperator) bool {
	switch op {
	case syntax.Pipe, syntax.AndStmt, syntax.OrStmt:
		return true
	default:
		return false
	}
}

func evaluateExecPolicyCall(call *syntax.CallExpr, cfg ApprovalPolicyConfig, rules []ExecPolicyRule) ExecPolicyDecision {
	if call == nil || len(call.Args) == 0 {
		return policyDecision(cfg, approvalDomainSandbox, "shell assignments require approval")
	}
	if len(call.Assigns) > 0 {
		return policyDecision(cfg, approvalDomainSandbox, "shell assignments require approval")
	}

	args := shellCallArgs(call)
	if len(args) == 0 || !args[0].literal {
		return policyDecision(cfg, approvalDomainSandbox, "dynamic command names require approval")
	}
	for _, arg := range args[1:] {
		if !arg.literal {
			return policyDecision(cfg, approvalDomainSandbox, "shell expansions or substitutions require approval")
		}
	}

	cmd := normalizeExecPolicyCommandName(args[0].value)
	commandLine := normalizeExecPolicyCommandLine(args)
	if ruleDecision, ok := matchExecPolicyRules(cmd, commandLine, rules); ok {
		return execPolicyRuleOutcome(cfg, ruleDecision)
	}

	if decision, ok := dangerousExecPolicyHeuristic(cmd, args, cfg); ok {
		return decision
	}
	if safeReadOnlyExecPolicyHeuristic(cmd, args) {
		return ExecPolicyDecision{Requirement: ApprovalRequirementAllow, Reason: "known read-only command"}
	}

	return policyDecision(cfg, approvalDomainSandbox, fmt.Sprintf("untrusted command %q requires approval", cmd))
}

func matchExecPolicyRules(cmd, commandLine string, rules []ExecPolicyRule) (ExecPolicyDecision, bool) {
	for _, rule := range rules {
		for _, exact := range rule.Exact {
			exact = strings.ToLower(strings.TrimSpace(exact))
			if cmd == normalizeExecPolicyCommandName(exact) || commandLine == exact {
				return ExecPolicyDecision{Requirement: ruleDecisionRequirement(rule.Decision), Reason: firstNonEmpty(rule.Reason, fmt.Sprintf("matched %s rule", rule.Decision))}, true
			}
		}
		for _, prefix := range rule.Prefix {
			prefix = strings.ToLower(strings.TrimSpace(prefix))
			if commandLine == prefix || strings.HasPrefix(commandLine, prefix+" ") {
				return ExecPolicyDecision{Requirement: ruleDecisionRequirement(rule.Decision), Reason: firstNonEmpty(rule.Reason, fmt.Sprintf("matched %s rule", rule.Decision))}, true
			}
		}
	}
	return ExecPolicyDecision{}, false
}

func execPolicyRuleOutcome(cfg ApprovalPolicyConfig, decision ExecPolicyDecision) ExecPolicyDecision {
	if decision.Requirement == ApprovalRequirementNeedsApproval {
		return policyDecision(cfg, approvalDomainRules, decision.Reason)
	}
	return decision
}

func ruleDecisionRequirement(decision ExecPolicyRuleDecision) ApprovalRequirement {
	switch decision {
	case ExecPolicyRuleAllow:
		return ApprovalRequirementAllow
	case ExecPolicyRuleForbid:
		return ApprovalRequirementForbidden
	default:
		return ApprovalRequirementNeedsApproval
	}
}

func dangerousExecPolicyHeuristic(cmd string, args []shellCallArg, cfg ApprovalPolicyConfig) (ExecPolicyDecision, bool) {
	if _, ok := highRiskBashDirectCommands[cmd]; ok {
		return policyDecision(cfg, approvalDomainSandbox, fmt.Sprintf("command %q requires approval", cmd)), true
	}
	if _, ok := forbiddenExecPolicyCommands[cmd]; ok {
		return ExecPolicyDecision{Requirement: ApprovalRequirementForbidden, Reason: fmt.Sprintf("command %q is forbidden", cmd)}, true
	}

	switch cmd {
	case "rm":
		for _, arg := range args[1:] {
			if arg.literal && strings.HasPrefix(arg.value, "-") {
				return policyDecision(cfg, approvalDomainSandbox, "dangerous mutating commands require approval"), true
			}
		}
	case "git":
		subcommand, ok := firstShellSubcommand(args[1:], highRiskGitFlagsWithValue)
		if !ok {
			return policyDecision(cfg, approvalDomainSandbox, "dynamic git command requires approval"), true
		}
		if subcommand == "push" || (subcommand == "reset" && containsLiteralShellArg(args[1:], "--hard")) {
			return policyDecision(cfg, approvalDomainSandbox, "dangerous git command requires approval"), true
		}
	case "terraform":
		subcommand, ok := firstShellSubcommand(args[1:], highRiskTerraformFlagsWithValue)
		if ok && (subcommand == "apply" || subcommand == "destroy") {
			return policyDecision(cfg, approvalDomainSandbox, "terraform apply/destroy requires approval"), true
		}
	case "docker":
		subcommand, ok := firstShellSubcommand(args[1:], highRiskDockerFlagsWithValue)
		if ok && subcommand == "push" {
			return policyDecision(cfg, approvalDomainSandbox, "docker push requires approval"), true
		}
	case "npm":
		subcommand, ok := firstShellSubcommand(args[1:], highRiskNPMFlagsWithValue)
		if ok && subcommand == "publish" {
			return policyDecision(cfg, approvalDomainSandbox, "npm publish requires approval"), true
		}
	}

	return ExecPolicyDecision{}, false
}

var forbiddenExecPolicyCommands = map[string]struct{}{
	"dd":       {},
	"diskpart": {},
	"emulate":  {},
	"fdisk":    {},
	"format":   {},
	"halt":     {},
	"mkfs":     {},
	"poweroff": {},
	"reboot":   {},
	"shutdown": {},
	"sysopen":  {},
	"sysread":  {},
	"sysseek":  {},
	"syswrite": {},
	"zf_chgrp": {},
	"zf_chmod": {},
	"zf_chown": {},
	"zf_ln":    {},
	"zf_mkdir": {},
	"zf_mv":    {},
	"zf_rm":    {},
	"zf_rmdir": {},
	"zmodload": {},
	"zpty":     {},
	"zsocket":  {},
	"ztcp":     {},
}

func safeReadOnlyExecPolicyHeuristic(cmd string, args []shellCallArg) bool {
	switch cmd {
	case "cat", "cd", "cut", "echo", "expr", "false", "grep", "head", "id", "ls", "nl", "paste", "pwd", "rev", "seq", "stat", "tail", "tr", "true", "uname", "uniq", "wc", "which", "whoami":
		return true
	case "dir", "tree", "type", "where":
		return true
	case "get-location", "get-childitem", "get-content", "select-string", "get-item", "get-command":
		return true
	case "select-object", "sort-object", "measure-object", "format-table", "out-string":
		return true
	case "base64":
		return isSafeBase64ExecPolicyCommand(args)
	case "find":
		return isSafeFindCommand(shellArgsToStrings(args))
	case "rg":
		return isSafeRipgrepExecPolicyCommand(args)
	case "git":
		return isSafeGitExecPolicyCommand(args)
	case "sed":
		return isSafeSedExecPolicyCommand(args)
	default:
		return false
	}
}

func isSafeBase64ExecPolicyCommand(args []shellCallArg) bool {
	for _, arg := range args[1:] {
		if !arg.literal {
			return false
		}
		value := strings.TrimSpace(arg.value)
		if value == "-o" || value == "--output" || strings.HasPrefix(value, "--output=") || (strings.HasPrefix(value, "-o") && value != "-o") {
			return false
		}
	}
	return true
}

func isSafeRipgrepExecPolicyCommand(args []shellCallArg) bool {
	for _, arg := range args[1:] {
		if !arg.literal {
			return false
		}
		value := strings.TrimSpace(arg.value)
		if value == "--pre" || strings.HasPrefix(value, "--pre=") || value == "--hostname-bin" || strings.HasPrefix(value, "--hostname-bin=") {
			return false
		}
		if value == "--search-zip" || value == "-z" {
			return false
		}
	}
	return true
}

func isSafeGitExecPolicyCommand(args []shellCallArg) bool {
	fields := shellArgsToStrings(args)
	if len(fields) < 2 || fields[0] != "git" {
		return false
	}

	subcommandIdx := -1
	for i := 1; i < len(fields); i++ {
		arg := fields[i]
		if isUnsafeGitGlobalOption(arg) {
			return false
		}
		if isGitGlobalOptionWithValue(arg) {
			i++
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		subcommandIdx = i
		break
	}
	if subcommandIdx < 0 {
		return false
	}

	subcommand := fields[subcommandIdx]
	subcommandArgs := fields[subcommandIdx+1:]
	if gitSubcommandHasUnsafeOption(subcommandArgs) {
		return false
	}

	switch subcommand {
	case "blame", "describe", "diff", "grep", "log", "ls-files", "merge-base", "rev-parse", "show", "status", "shortlog", "ls-remote":
		return true
	case "stash":
		return len(subcommandArgs) > 0 && subcommandArgs[0] == "list"
	case "config":
		return len(subcommandArgs) > 0 && (subcommandArgs[0] == "--get" || subcommandArgs[0] == "--list")
	case "remote":
		return len(subcommandArgs) > 0 && (subcommandArgs[0] == "-v" || (subcommandArgs[0] == "get-url" && len(subcommandArgs) <= 3))
	case "tag":
		return len(subcommandArgs) == 0 || slices.Contains(subcommandArgs, "--list")
	case "branch":
		return gitBranchExecPolicyArgsAreReadOnly(subcommandArgs)
	default:
		return false
	}
}

func isUnsafeGitGlobalOption(arg string) bool {
	return arg == "-C" || strings.HasPrefix(arg, "-C") && len(arg) > 2 ||
		arg == "-c" || strings.HasPrefix(arg, "-c") && len(arg) > 2 ||
		arg == "-p" || arg == "--paginate" ||
		arg == "--config-env" || strings.HasPrefix(arg, "--config-env=") ||
		arg == "--exec-path" || strings.HasPrefix(arg, "--exec-path=") ||
		arg == "--git-dir" || strings.HasPrefix(arg, "--git-dir=") ||
		arg == "--namespace" || strings.HasPrefix(arg, "--namespace=") ||
		arg == "--super-prefix" || strings.HasPrefix(arg, "--super-prefix=") ||
		arg == "--work-tree" || strings.HasPrefix(arg, "--work-tree=")
}

func isGitGlobalOptionWithValue(arg string) bool {
	return arg == "--no-pager" || arg == "--no-optional-locks"
}

func gitSubcommandHasUnsafeOption(args []string) bool {
	for _, arg := range args {
		if arg == "--output" || strings.HasPrefix(arg, "--output=") || arg == "--ext-diff" || arg == "--textconv" || arg == "--exec" || strings.HasPrefix(arg, "--exec=") {
			return true
		}
	}
	return false
}

func gitBranchExecPolicyArgsAreReadOnly(args []string) bool {
	if len(args) == 0 {
		return true
	}
	for _, arg := range args {
		switch {
		case arg == "--list", arg == "-l", arg == "--show-current", arg == "-a", arg == "--all", arg == "-r", arg == "--remotes", arg == "-v", arg == "-vv", arg == "--verbose":
			continue
		case strings.HasPrefix(arg, "--format="):
			continue
		default:
			return false
		}
	}
	return true
}

func isSafeSedExecPolicyCommand(args []shellCallArg) bool {
	fields := shellArgsToStrings(args)
	return len(fields) <= 4 && len(fields) >= 3 && fields[1] == "-n" && isValidSedPrintAddress(fields[2])
}

func isValidSedPrintAddress(value string) bool {
	core, ok := strings.CutSuffix(value, "p")
	if !ok || core == "" {
		return false
	}
	parts := strings.Split(core, ",")
	if len(parts) > 2 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

func shellArgsToStrings(args []shellCallArg) []string {
	fields := make([]string, 0, len(args))
	for _, arg := range args {
		if !arg.literal {
			return nil
		}
		fields = append(fields, strings.ToLower(strings.TrimSpace(arg.value)))
	}
	return fields
}

func hasUnsafeExecPolicyRedirection(redirs []*syntax.Redirect) bool {
	return slices.ContainsFunc(redirs, func(r *syntax.Redirect) bool {
		return !isExecPolicyNullRedirect(r)
	})
}

func isExecPolicyNullRedirect(r *syntax.Redirect) bool {
	if r == nil || r.Op != syntax.RdrOut {
		return false
	}
	if r.N != nil && r.N.Value != "2" {
		return false
	}
	target, ok := literalWord(r.Word)
	if !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(target)) {
	case "/dev/null", "nul", "$null":
		return true
	default:
		return false
	}
}

func pipelineExecPolicyTargetRequiresApproval(stmt *syntax.Stmt) bool {
	if stmt == nil {
		return false
	}

	invokesShell := false
	syntax.Walk(stmt, func(node syntax.Node) bool {
		if invokesShell {
			return false
		}
		call, ok := node.(*syntax.CallExpr)
		if !ok {
			return true
		}
		args := shellCallArgs(call)
		if len(args) == 0 || !args[0].literal {
			return true
		}
		_, invokesShell = highRiskBashPipelineTargets[normalizeExecPolicyCommandName(args[0].value)]
		return !invokesShell
	})
	return invokesShell
}

func aggregateExecPolicyDecision(current, next ExecPolicyDecision) ExecPolicyDecision {
	if next.Requirement == ApprovalRequirementForbidden || current.Requirement == ApprovalRequirementForbidden {
		if next.Requirement == ApprovalRequirementForbidden {
			return next
		}
		return current
	}
	if next.Requirement == ApprovalRequirementNeedsApproval || current.Requirement == ApprovalRequirementNeedsApproval {
		if next.Requirement == ApprovalRequirementNeedsApproval {
			return next
		}
		return current
	}
	if strings.TrimSpace(current.Reason) == "" {
		return next
	}
	return current
}

func policyDecision(cfg ApprovalPolicyConfig, domain approvalDomain, reason string) ExecPolicyDecision {
	if !approvalPolicyAllowsPrompt(cfg, domain) {
		return ExecPolicyDecision{Requirement: ApprovalRequirementForbidden, Reason: firstNonEmpty(reason, "approval is forbidden by policy")}
	}
	return ExecPolicyDecision{Requirement: ApprovalRequirementNeedsApproval, Reason: reason}
}

func approvalPolicyAllowsPrompt(cfg ApprovalPolicyConfig, domain approvalDomain) bool {
	switch cfg.Policy {
	case ApprovalPolicyNever:
		return false
	case ApprovalPolicyGranular:
		switch domain {
		case approvalDomainRules:
			return cfg.Granular.Rules
		default:
			return cfg.Granular.SandboxApproval
		}
	default:
		return true
	}
}

func normalizeExecPolicyCommandName(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	return strings.ToLower(path.Base(filepath.ToSlash(raw)))
}

func normalizeExecPolicyCommandLine(args []shellCallArg) string {
	parts := make([]string, 0, len(args))
	for _, arg := range args {
		if !arg.literal {
			return ""
		}
		parts = append(parts, strings.ToLower(strings.TrimSpace(arg.value)))
	}
	return strings.Join(parts, " ")
}
