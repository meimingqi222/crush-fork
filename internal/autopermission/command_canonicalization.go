package autopermission

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

func CanonicalizeBashCommand(command string) (string, *syntax.File, error) {
	file, err := syntax.NewParser().Parse(strings.NewReader(strings.TrimSpace(command)), "")
	if err != nil {
		return "", nil, err
	}
	return canonicalizeShellFile(file), file, nil
}

func canonicalizeShellFile(file *syntax.File) string {
	if file == nil || len(file.Stmts) == 0 {
		return ""
	}
	parts := make([]string, 0, len(file.Stmts))
	for _, stmt := range file.Stmts {
		part := canonicalizeShellStmt(stmt)
		if part == "" {
			return ""
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ";")
}

func canonicalizeShellStmt(stmt *syntax.Stmt) string {
	if stmt == nil || stmt.Negated || stmt.Background || stmt.Coprocess || stmt.Disown {
		return ""
	}
	command := canonicalizeShellCommand(stmt.Cmd)
	if command == "" {
		return ""
	}
	redirs := canonicalizeShellRedirects(stmt.Redirs)
	if redirs == "" {
		return command
	}
	return command + " " + redirs
}

func canonicalizeShellCommand(cmd syntax.Command) string {
	switch x := cmd.(type) {
	case *syntax.CallExpr:
		args := shellCallArgs(x)
		if len(args) == 0 {
			return ""
		}
		parts := make([]string, 0, len(args))
		for _, arg := range args {
			if !arg.literal {
				return ""
			}
			parts = append(parts, strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(arg.value))), " "))
		}
		return strings.Join(parts, " ")
	case *syntax.BinaryCmd:
		operator := canonicalizeShellBinaryOperator(x.Op)
		if operator == "" {
			return ""
		}
		left := canonicalizeShellStmt(x.X)
		right := canonicalizeShellStmt(x.Y)
		if left == "" || right == "" {
			return ""
		}
		return left + " " + operator + " " + right
	default:
		return ""
	}
}

func canonicalizeShellBinaryOperator(op syntax.BinCmdOperator) string {
	switch op {
	case syntax.Pipe:
		return "|"
	case syntax.AndStmt:
		return "&&"
	case syntax.OrStmt:
		return "||"
	default:
		return ""
	}
}

func canonicalizeShellRedirects(redirs []*syntax.Redirect) string {
	if len(redirs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(redirs))
	for _, redir := range redirs {
		if !isExecPolicyNullRedirect(redir) {
			return ""
		}
		parts = append(parts, "2>/dev/null")
	}
	return strings.Join(parts, " ")
}
