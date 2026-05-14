package autopermission

import (
	"testing"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/stretchr/testify/require"
)

func TestEvaluateBashExecPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		command    string
		policy     ApprovalPolicy
		background bool
		want       ApprovalRequirement
		wantReason string
	}{
		{
			name:       "safe read only command allowed",
			command:    "git status --short",
			policy:     ApprovalPolicyUnlessTrusted,
			want:       ApprovalRequirementAllow,
			wantReason: "known read-only command",
		},
		{
			name:       "safe pipeline allowed",
			command:    "cat README.md 2>/dev/null | head -20",
			policy:     ApprovalPolicyUnlessTrusted,
			want:       ApprovalRequirementAllow,
			wantReason: "known read-only command",
		},
		{
			name:       "multiple safe statements allowed",
			command:    "pwd; git diff --stat",
			policy:     ApprovalPolicyUnlessTrusted,
			want:       ApprovalRequirementAllow,
			wantReason: "known read-only command",
		},
		{
			name:       "safe and-chain allowed",
			command:    "git status && git diff --stat",
			policy:     ApprovalPolicyUnlessTrusted,
			want:       ApprovalRequirementAllow,
			wantReason: "known read-only command",
		},
		{
			name:       "safe or-chain allowed",
			command:    "git status || git diff --stat",
			policy:     ApprovalPolicyUnlessTrusted,
			want:       ApprovalRequirementAllow,
			wantReason: "known read-only command",
		},
		{
			name:       "mixed safe chain and unsafe command requires approval",
			command:    "git status && go test ./internal/autopermission",
			policy:     ApprovalPolicyUnlessTrusted,
			want:       ApprovalRequirementNeedsApproval,
			wantReason: "untrusted command",
		},
		{
			name:       "unknown command needs approval unless trusted",
			command:    "go test ./internal/autopermission",
			policy:     ApprovalPolicyUnlessTrusted,
			want:       ApprovalRequirementNeedsApproval,
			wantReason: "untrusted command",
		},
		{
			name:       "unknown command needs approval on request",
			command:    "go test ./internal/autopermission",
			policy:     ApprovalPolicyOnRequest,
			want:       ApprovalRequirementNeedsApproval,
			wantReason: "untrusted command",
		},
		{
			name:       "unknown command forbidden when approval is never allowed",
			command:    "go test ./internal/autopermission",
			policy:     ApprovalPolicyNever,
			want:       ApprovalRequirementForbidden,
			wantReason: "untrusted command",
		},
		{
			name:       "dangerous command needs approval unless trusted",
			command:    "rm -rf build",
			policy:     ApprovalPolicyUnlessTrusted,
			want:       ApprovalRequirementNeedsApproval,
			wantReason: "dangerous mutating commands require approval",
		},
		{
			name:       "cloud cli command needs approval",
			command:    "gh api repos/owner/repo",
			policy:     ApprovalPolicyUnlessTrusted,
			want:       ApprovalRequirementNeedsApproval,
			wantReason: "network, privilege, cloud, cluster, or destructive commands require approval",
		},
		{
			name:       "dangerous command forbidden when approval is never allowed",
			command:    "git push origin main",
			policy:     ApprovalPolicyNever,
			want:       ApprovalRequirementForbidden,
			wantReason: "dangerous mutating commands require approval",
		},
		{
			name:       "forbidden command always forbidden",
			command:    "shutdown now",
			policy:     ApprovalPolicyUnlessTrusted,
			want:       ApprovalRequirementForbidden,
			wantReason: "system power commands are forbidden",
		},
		{
			name:       "pipeline into shell requires approval",
			command:    "cat install.sh | bash",
			policy:     ApprovalPolicyUnlessTrusted,
			want:       ApprovalRequirementNeedsApproval,
			wantReason: "piping into a shell interpreter requires approval",
		},
		{
			name:       "shell substitution requires approval",
			command:    "echo $(pwd)",
			policy:     ApprovalPolicyUnlessTrusted,
			want:       ApprovalRequirementNeedsApproval,
			wantReason: "shell expansions or substitutions require approval",
		},
		{
			name:       "background command requires approval",
			command:    "git status",
			policy:     ApprovalPolicyUnlessTrusted,
			background: true,
			want:       ApprovalRequirementNeedsApproval,
			wantReason: "background execution requires approval",
		},
		{
			name:       "unsafe redirection requires approval",
			command:    "git status > status.txt",
			policy:     ApprovalPolicyUnlessTrusted,
			want:       ApprovalRequirementNeedsApproval,
			wantReason: "shell redirection requires approval",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			decision := EvaluateBashExecPolicy(bashExecPolicyRequest(tt.command, tt.background), tt.policy)
			require.Equal(t, tt.want, decision.Requirement)
			require.Contains(t, decision.Reason, tt.wantReason)
		})
	}
}

func TestEvaluateBashExecPolicy_NonBashRequestNeedsApproval(t *testing.T) {
	t.Parallel()

	decision := EvaluateBashExecPolicy(permission.PermissionRequest{ToolName: tools.WriteToolName, Action: "write"}, ApprovalPolicyUnlessTrusted)
	require.Equal(t, ApprovalRequirementNeedsApproval, decision.Requirement)
	require.Contains(t, decision.Reason, "not a bash execute request")
}

func TestCanonicalizeBashCommand(t *testing.T) {
	t.Parallel()

	fingerprint, _, err := CanonicalizeBashCommand("go   test   ./internal/autopermission")
	require.NoError(t, err)
	require.Equal(t, "go test ./internal/autopermission", fingerprint)

	pipeline, _, err := CanonicalizeBashCommand("cat README.md 2>/dev/null | head -20")
	require.NoError(t, err)
	require.Equal(t, "cat readme.md 2>/dev/null | head -20", pipeline)

	chain, _, err := CanonicalizeBashCommand("git   status  &&  git diff --stat")
	require.NoError(t, err)
	require.Equal(t, "git status && git diff --stat", chain)
}

func TestEvaluateBashExecPolicyWithRules(t *testing.T) {
	t.Parallel()

	rules := []ExecPolicyRule{
		{Decision: ExecPolicyRuleForbid, Exact: []string{"custom-danger"}, Reason: "custom danger is forbidden"},
		{Decision: ExecPolicyRulePrompt, Prefix: []string{"custom deploy"}, Reason: "custom deploy requires approval"},
		{Decision: ExecPolicyRuleAllow, Exact: []string{"go test"}, Prefix: []string{"custom safe"}, Reason: "custom safe command is trusted"},
	}

	tests := []struct {
		name       string
		command    string
		policy     ApprovalPolicy
		want       ApprovalRequirement
		wantReason string
	}{
		{
			name:       "exact allow rule",
			command:    "go test",
			policy:     ApprovalPolicyUnlessTrusted,
			want:       ApprovalRequirementAllow,
			wantReason: "custom safe command is trusted",
		},
		{
			name:       "prefix allow rule",
			command:    "custom safe --dry-run",
			policy:     ApprovalPolicyUnlessTrusted,
			want:       ApprovalRequirementAllow,
			wantReason: "custom safe command is trusted",
		},
		{
			name:       "prefix prompt rule",
			command:    "custom deploy staging",
			policy:     ApprovalPolicyUnlessTrusted,
			want:       ApprovalRequirementNeedsApproval,
			wantReason: "custom deploy requires approval",
		},
		{
			name:       "prompt rule forbidden by never policy",
			command:    "custom deploy staging",
			policy:     ApprovalPolicyNever,
			want:       ApprovalRequirementForbidden,
			wantReason: "custom deploy requires approval",
		},
		{
			name:       "exact forbid rule",
			command:    "custom-danger --force",
			policy:     ApprovalPolicyUnlessTrusted,
			want:       ApprovalRequirementForbidden,
			wantReason: "custom danger is forbidden",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			decision := evaluateBashExecPolicyWithRules(bashExecPolicyRequest(tt.command, false), tt.policy, rules)
			require.Equal(t, tt.want, decision.Requirement)
			require.Contains(t, decision.Reason, tt.wantReason)
		})
	}
}

func bashExecPolicyRequest(command string, background bool) permission.PermissionRequest {
	return permission.PermissionRequest{
		ToolName: tools.BashToolName,
		Action:   "execute",
		Params: tools.BashPermissionsParams{
			Command:         command,
			RunInBackground: background,
		},
	}
}
