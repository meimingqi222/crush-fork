package autopermission

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/permission"
)

type PolicyRequirement = ApprovalRequirement

const (
	PolicyRequirementAllow         = ApprovalRequirementAllow
	PolicyRequirementNeedsApproval = ApprovalRequirementNeedsApproval
	PolicyRequirementForbidden     = ApprovalRequirementForbidden
)

type PolicyCapabilityKind string

const (
	PolicyCapabilityNone    PolicyCapabilityKind = "none"
	PolicyCapabilitySandbox PolicyCapabilityKind = "sandbox"
	PolicyCapabilityNetwork PolicyCapabilityKind = "network"
)

type PolicyCapability struct {
	Kind      PolicyCapabilityKind
	Name      string
	Available bool
}

func (c PolicyCapability) ID() string {
	kind := strings.TrimSpace(string(c.Kind))
	name := strings.TrimSpace(c.Name)
	if kind == "" {
		kind = string(PolicyCapabilityNone)
	}
	if name == "" {
		name = "default"
	}
	return kind + ":" + name
}

type PolicyRequestContext struct {
	SessionID          string
	WorkingDir         string
	Mode               string
	Approval           ApprovalPolicyConfig
	WorkspaceWriteMode string
	Capabilities       []PolicyCapability
}

type PolicyDecision struct {
	Requirement PolicyRequirement
	Reason      string
	Cacheable   bool
	CacheKeys   []ApprovalCacheKey
	Source      string
}

type PolicyEvaluator interface {
	EvaluatePolicy(context.Context, permission.PermissionRequest, PolicyRequestContext) PolicyDecision
}

type PolicyEvaluatorFunc func(context.Context, permission.PermissionRequest, PolicyRequestContext) PolicyDecision

func (fn PolicyEvaluatorFunc) EvaluatePolicy(ctx context.Context, req permission.PermissionRequest, pctx PolicyRequestContext) PolicyDecision {
	return fn(ctx, req, pctx)
}

type PolicyEngine struct {
	evaluators []PolicyEvaluator
}

func NewPolicyEngine(evaluators ...PolicyEvaluator) *PolicyEngine {
	filtered := make([]PolicyEvaluator, 0, len(evaluators))
	for _, evaluator := range evaluators {
		if evaluator != nil {
			filtered = append(filtered, evaluator)
		}
	}
	return &PolicyEngine{evaluators: filtered}
}

func NewDefaultPolicyEngine(execRules []ExecPolicyRule) *PolicyEngine {
	return NewPolicyEngine(
		PolicyEvaluatorFunc(preapprovedNetworkToolPolicyEvaluator),
		PolicyEvaluatorFunc(workspaceWritePolicyEvaluator),
		PolicyEvaluatorFunc(readOnlyToolPolicyEvaluator),
		BashPolicyEvaluator{Rules: execRules},
	)
}

func (e *PolicyEngine) Evaluate(ctx context.Context, req permission.PermissionRequest, pctx PolicyRequestContext) PolicyDecision {
	if e == nil || len(e.evaluators) == 0 {
		return PolicyDecision{Requirement: PolicyRequirementNeedsApproval, Reason: "no policy evaluator handled this request", Source: "none"}
	}

	decision := PolicyDecision{Requirement: PolicyRequirementNeedsApproval, Reason: "no policy evaluator handled this request", Source: "none"}
	handled := false
	for _, evaluator := range e.evaluators {
		next := evaluator.EvaluatePolicy(ctx, req, pctx)
		if next.Requirement == "" {
			continue
		}
		next = normalizePolicyDecision(next)
		if next.Requirement == PolicyRequirementForbidden {
			return next
		}
		if !handled || policyRequirementPriority(next.Requirement) > policyRequirementPriority(decision.Requirement) {
			decision = next
			handled = true
		}
	}
	return decision
}

func policyRequirementPriority(requirement PolicyRequirement) int {
	switch requirement {
	case PolicyRequirementForbidden:
		return 3
	case PolicyRequirementNeedsApproval:
		return 2
	case PolicyRequirementAllow:
		return 1
	default:
		return 0
	}
}

func normalizePolicyDecision(decision PolicyDecision) PolicyDecision {
	if strings.TrimSpace(decision.Source) == "" {
		decision.Source = "policy"
	}
	if strings.TrimSpace(decision.Reason) == "" {
		switch decision.Requirement {
		case PolicyRequirementAllow:
			decision.Reason = "policy allowed this request"
		case PolicyRequirementForbidden:
			decision.Reason = "policy blocked this request"
		default:
			decision.Reason = "policy requires approval"
		}
	}
	return decision
}

func workspaceWritePolicyEvaluator(_ context.Context, req permission.PermissionRequest, pctx PolicyRequestContext) PolicyDecision {
	if !isAcceptEditsEquivalentRequest(req, pctx.WorkingDir) {
		return PolicyDecision{}
	}

	switch workspaceWriteRequirement(req, pctx) {
	case ApprovalRequirementAllow:
		return PolicyDecision{Requirement: PolicyRequirementAllow, Reason: "workspace write policy allowed this action", Source: "workspace_write"}
	case ApprovalRequirementForbidden:
		return PolicyDecision{Requirement: PolicyRequirementForbidden, Reason: "Workspace write approval is disabled by Auto Mode policy.", Source: "workspace_write"}
	default:
		return PolicyDecision{Requirement: PolicyRequirementNeedsApproval, Reason: "Auto Mode workspace write policy requires manual approval.", Source: "workspace_write"}
	}
}

func preapprovedNetworkToolPolicyEvaluator(_ context.Context, req permission.PermissionRequest, _ PolicyRequestContext) PolicyDecision {
	// Only consider network tools that fetch external content.
	if req.ToolName != tools.DownloadToolName &&
		req.ToolName != tools.ReadToolName &&
		req.ToolName != tools.AgenticFetchToolName {
		return PolicyDecision{}
	}

	urlStr := tools.ExtractURLFromPermissionRequest(req.Params)
	if urlStr == "" {
		return PolicyDecision{}
	}

	if tools.IsPreapprovedURL(urlStr) {
		return PolicyDecision{
			Requirement: PolicyRequirementAllow,
			Reason:      "preapproved host allowed this network request",
			Source:      "preapproved_host",
		}
	}
	return PolicyDecision{}
}

func readOnlyToolPolicyEvaluator(_ context.Context, req permission.PermissionRequest, _ PolicyRequestContext) PolicyDecision {
	if !isAutoModeAllowlistedRequest(req) {
		return PolicyDecision{}
	}
	return PolicyDecision{Requirement: PolicyRequirementAllow, Reason: "read-only tool allowlist allowed this action", Source: "read_only_tool"}
}

type BashPolicyEvaluator struct {
	Rules []ExecPolicyRule
}

func (e BashPolicyEvaluator) EvaluatePolicy(_ context.Context, req permission.PermissionRequest, pctx PolicyRequestContext) PolicyDecision {
	if req.ToolName != tools.BashToolName || req.Action != "execute" {
		return PolicyDecision{}
	}

	decision := EvaluateBashExecPolicyWithConfig(req, pctx.Approval, e.Rules)
	policyDecision := PolicyDecision{
		Requirement: decision.Requirement,
		Reason:      decision.Reason,
		Source:      "bash_exec",
	}
	if decision.Requirement == ApprovalRequirementNeedsApproval && decision.Fingerprint != "" {
		key := policyCacheKey(req, pctx, decision.Fingerprint)
		policyDecision.Cacheable = !isAlwaysManual(req, pctx.WorkingDir)
		policyDecision.CacheKeys = []ApprovalCacheKey{key}
	}
	return policyDecision
}

func workspaceWriteRequirement(_ permission.PermissionRequest, pctx PolicyRequestContext) ApprovalRequirement {
	mode := strings.ToLower(strings.TrimSpace(pctx.WorkspaceWriteMode))
	if mode != "" {
		switch mode {
		case "forbid", "forbidden", "deny":
			return ApprovalRequirementForbidden
		case "ask", "prompt":
			return ApprovalRequirementNeedsApproval
		default:
			return ApprovalRequirementAllow
		}
	}
	if pctx.Approval.Policy == ApprovalPolicyNever {
		return ApprovalRequirementForbidden
	}
	return ApprovalRequirementAllow
}

func policyCacheKey(req permission.PermissionRequest, pctx PolicyRequestContext, fingerprint string) ApprovalCacheKey {
	capabilityID := PolicyCapability{Kind: PolicyCapabilityNone}.ID()
	if len(pctx.Capabilities) > 0 {
		capabilityID = pctx.Capabilities[0].ID()
	}
	return ApprovalCacheKey{
		SessionID:    strings.TrimSpace(pctx.SessionID),
		ToolName:     strings.TrimSpace(req.ToolName),
		Action:       strings.TrimSpace(req.Action),
		WorkingDir:   strings.TrimSpace(pctx.WorkingDir),
		Policy:       pctx.Approval.Policy,
		CapabilityID: capabilityID,
		Fingerprint:  strings.TrimSpace(fingerprint),
	}
}

func policyBlockedError(summary, reason string) error {
	return permission.NewPermissionBlockedError(summary, firstNonEmpty(reason, "The request cannot be approved under the current Auto Mode policy."))
}

func policyReasonWithSource(decision PolicyDecision) string {
	if decision.Source == "" {
		return decision.Reason
	}
	return fmt.Sprintf("%s: %s", decision.Source, decision.Reason)
}
