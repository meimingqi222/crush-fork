package autopermission

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/fsext"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/plugin"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/session"
	"mvdan.cc/sh/v3/syntax"
)

const (
	defaultMaxConsecutiveClassifierBlocks = 3
	defaultMaxTotalClassifierBlocks       = 20
)

var highRiskBashDirectCommands = map[string]struct{}{
	"aws":         {},
	"az":          {},
	"curl":        {},
	"gcloud":      {},
	"gh":          {},
	"kubectl":     {},
	"remove-item": {},
	"scp":         {},
	"ssh":         {},
	"sudo":        {},
	"wget":        {},
	"del":         {},
}

var highRiskBashPipelineTargets = map[string]struct{}{
	"sh":   {},
	"bash": {},
}

var highRiskGitFlagsWithValue = map[string]bool{
	"-C":                  true,
	"-c":                  true,
	"--git-dir":           true,
	"--work-tree":         true,
	"--namespace":         true,
	"--exec-path":         true,
	"--no-pager":          false,
	"--no-optional-locks": false,
}

var highRiskTerraformFlagsWithValue = map[string]bool{
	"-chdir": true,
}

var highRiskDockerFlagsWithValue = map[string]bool{
	"-c":          true,
	"-h":          true,
	"-l":          true,
	"--config":    true,
	"--context":   true,
	"--host":      true,
	"--log-level": true,
}

var highRiskNPMFlagsWithValue = map[string]bool{
	"-c":           true,
	"--cache":      true,
	"--loglevel":   true,
	"--prefix":     true,
	"--userconfig": true,
	"-w":           true,
	"--workspace":  true,
}

type sessionClassifierState struct {
	lastMode            session.PermissionMode
	consecutiveBlocks   int
	totalBlocks         int
	suspendAutoApproval bool
	denialQueue         *DenialQueue
}

type service struct {
	base                        permission.Service
	sessions                    session.Service
	pluginRuntime               *plugin.Runtime
	classifierFn                func() permission.Classifier
	workingDir                  string
	failClosedOnClassifierError bool
	allowedTools                []string
	autoModeConfig              *config.AutoMode
	approvalPolicy              ApprovalPolicyConfig
	execPolicyRules             []ExecPolicyRule
	policyEngine                *PolicyEngine
	approvalCache               *approvalCache
	classifierMu                sync.Mutex
	sessionStates               map[string]sessionClassifierState
}

func New(
	base permission.Service,
	sessions session.Service,
	pluginRuntime *plugin.Runtime,
	classifierFn func() permission.Classifier,
	workingDir string,
	failClosedOnClassifierError bool,
	allowedTools []string,
	autoModeConfig ...*config.AutoMode,
) permission.Service {
	var cfg *config.AutoMode
	if len(autoModeConfig) > 0 {
		cfg = autoModeConfig[0]
	}
	approvalPolicy, execPolicyRules := policyFromConfig(cfg)
	return &service{
		base:                        base,
		sessions:                    sessions,
		pluginRuntime:               pluginRuntime,
		classifierFn:                classifierFn,
		workingDir:                  workingDir,
		failClosedOnClassifierError: failClosedOnClassifierError,
		allowedTools:                slices.Clone(allowedTools),
		autoModeConfig:              cfg,
		approvalPolicy:              approvalPolicy,
		execPolicyRules:             execPolicyRules,
		policyEngine:                NewDefaultPolicyEngine(execPolicyRules),
		approvalCache:               newApprovalCache(defaultApprovalCacheEntries),
		sessionStates:               map[string]sessionClassifierState{},
	}
}

func (s *service) Subscribe(ctx context.Context) <-chan pubsub.Event[permission.PermissionRequest] {
	return s.base.Subscribe(ctx)
}

func (s *service) GrantPersistent(p permission.PermissionRequest) {
	s.base.GrantPersistent(p)
}

func (s *service) Grant(p permission.PermissionRequest) {
	s.base.Grant(p)
}

func (s *service) Deny(p permission.PermissionRequest) {
	s.base.Deny(p)
}

func (s *service) HasPersistentPermission(p permission.PermissionRequest) bool {
	return s.base.HasPersistentPermission(p)
}

func (s *service) ClearPersistentPermissions(sessionID string) {
	s.base.ClearPersistentPermissions(sessionID)
	if s.approvalCache != nil {
		s.approvalCache.clearSession(sessionID)
	}
}

func (s *service) EvaluateRequest(ctx context.Context, opts permission.CreatePermissionRequest) (permission.EvaluationResult, error) {
	return s.base.EvaluateRequest(ctx, opts)
}

func (s *service) Prompt(ctx context.Context, p permission.PermissionRequest) (bool, error) {
	return s.promptWithEscalation(ctx, p)
}

func (s *service) Request(ctx context.Context, opts permission.CreatePermissionRequest) (bool, error) {
	eval, err := s.base.EvaluateRequest(ctx, opts)
	if err != nil {
		return false, err
	}

	switch eval.Decision {
	case permission.EvaluationDecisionAllow:
		return true, nil
	case permission.EvaluationDecisionDeny:
		return false, permission.ErrorPermissionBlocked
	}

	sessionAuthorityID := effectivePermissionSessionID(eval.Permission)
	mode, err := s.sessionPermissionMode(ctx, sessionAuthorityID)
	if err != nil {
		return s.promptWithEscalation(ctx, eval.Permission)
	}

	if mode == session.PermissionModeYolo {
		if s.isBypassImmuneRequest(eval.Permission) {
			slog.Debug("YOLO Mode allowed bypass-immune permission request",
				"session_id", sessionAuthorityID,
				"tool", eval.Permission.ToolName,
				"action", eval.Permission.Action,
			)
		}
		return true, nil
	}

	if s.base.HasPersistentPermission(eval.Permission) {
		slog.Debug("Permission allowed via session grant",
			"session_id", sessionAuthorityID,
			"tool", eval.Permission.ToolName,
			"action", eval.Permission.Action,
		)
		if mode == session.PermissionModeAuto {
			s.resetClassifierBlocks(sessionAuthorityID)
		}
		return true, nil
	}

	if s.isExplicitlyAllowed(opts, eval.Permission) {
		if mode == session.PermissionModeAuto && ignoresExplicitAllowInAutoMode(eval.Permission, s.workingDir) {
			slog.Debug("Auto Mode explicit allowlist ignored for safety-sensitive request",
				"session_id", sessionAuthorityID,
				"tool", eval.Permission.ToolName,
				"action", eval.Permission.Action,
			)
		} else {
			slog.Debug("Permission explicitly allowed",
				"session_id", sessionAuthorityID,
				"tool", eval.Permission.ToolName,
				"action", eval.Permission.Action,
			)
			if mode == session.PermissionModeAuto {
				s.resetClassifierBlocks(sessionAuthorityID)
			}
			return true, nil
		}
	}

	switch classifyPluginDecisionWithRuntime(s.pluginRuntime, eval.Permission) {
	case permission.EvaluationDecisionAllow:
		return true, nil
	case permission.EvaluationDecisionDeny:
		return false, permission.ErrorPermissionBlocked
	}

	if mode == session.PermissionModeAuto && isAutoModeAllowlistedRequest(eval.Permission) {
		slog.Debug("Auto Mode permission allowed via read-only allowlist",
			"session_id", sessionAuthorityID,
			"tool", eval.Permission.ToolName,
			"action", eval.Permission.Action,
		)
		s.resetClassifierBlocks(sessionAuthorityID)
		return true, nil
	}

	if mode == session.PermissionModeDefault {
		return s.promptWithEscalation(ctx, eval.Permission)
	}

	if mode != session.PermissionModeAuto {
		return s.promptWithEscalation(ctx, eval.Permission)
	}

	if s.shouldSuspendAutoApproval(sessionAuthorityID, mode) {
		slog.Debug("Auto Mode permission auto-approval suspended",
			"session_id", sessionAuthorityID,
			"tool", eval.Permission.ToolName,
			"action", eval.Permission.Action,
		)
		return s.promptWithEscalation(ctx, withAutoReview(eval.Permission, permission.AutoReview{
			Trigger: permission.AutoReviewTriggerClassifierSuspended,
			Reason:  "Auto approval is paused after repeated classifier blocks.",
		}))
	}

	return s.handleAutoModeRequest(ctx, sessionAuthorityID, eval.Permission)
}

func effectivePermissionSessionID(req permission.PermissionRequest) string {
	if strings.TrimSpace(req.AuthoritySessionID) != "" {
		return strings.TrimSpace(req.AuthoritySessionID)
	}
	return strings.TrimSpace(req.SessionID)
}

func (s *service) AutoApproveSession(sessionID string) {
	s.base.AutoApproveSession(sessionID)
}

func (s *service) SetSessionAutoApprove(sessionID string, enabled bool) {
	s.base.SetSessionAutoApprove(sessionID, enabled)
}

func (s *service) IsSessionAutoApprove(sessionID string) bool {
	return s.base.IsSessionAutoApprove(sessionID)
}

func (s *service) SetSkipRequests(skip bool) {
	s.base.SetSkipRequests(skip)
}

func (s *service) SkipRequests() bool {
	return s.base.SkipRequests()
}

func (s *service) SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[permission.PermissionNotification] {
	return s.base.SubscribeNotifications(ctx)
}

func (s *service) SetSkillContext(skillName string, allowedTools []string) {
	s.base.SetSkillContext(skillName, allowedTools)
}

func (s *service) ClearSkillContext() {
	s.base.ClearSkillContext()
}

func (s *service) GetSkillContext() (string, []string) {
	return s.base.GetSkillContext()
}

func (s *service) classifier() permission.Classifier {
	if s.classifierFn == nil {
		return nil
	}
	return s.classifierFn()
}

func (s *service) handleAutoModeRequest(ctx context.Context, sessionAuthorityID string, req permission.PermissionRequest) (bool, error) {
	policyCtx := PolicyRequestContext{
		SessionID:          sessionAuthorityID,
		WorkingDir:         s.workingDir,
		Mode:               string(session.PermissionModeAuto),
		Approval:           s.approvalPolicy,
		WorkspaceWriteMode: s.workspaceWriteMode(),
		Capabilities: []PolicyCapability{
			{Kind: PolicyCapabilityNone, Name: "direct", Available: true},
		},
	}
	decision := s.policyEngine.Evaluate(ctx, req, policyCtx)
	if decision.Cacheable && s.approvalCache.approved(decision.CacheKeys) {
		slog.Debug("Auto Mode permission allowed via approval cache",
			"session_id", sessionAuthorityID,
			"tool", req.ToolName,
			"action", req.Action,
			"source", decision.Source,
		)
		s.resetClassifierBlocks(sessionAuthorityID)
		return true, nil
	}

	slog.Debug("Auto Mode policy evaluated request",
		"session_id", sessionAuthorityID,
		"tool", req.ToolName,
		"action", req.Action,
		"requirement", decision.Requirement,
		"source", decision.Source,
		"reason", decision.Reason,
	)
	switch decision.Requirement {
	case PolicyRequirementAllow:
		s.resetClassifierBlocks(sessionAuthorityID)
		return true, nil
	case PolicyRequirementForbidden:
		return false, policyBlockedError("Auto Mode policy blocked this action.", decision.Reason)
	case PolicyRequirementNeedsApproval:
		if decision.Source == "always_manual" {
			return s.promptWithEscalation(ctx, withAutoReview(req, permission.AutoReview{
				Trigger: permission.AutoReviewTriggerAlwaysManual,
				Reason:  decision.Reason,
			}))
		}
		return s.handlePolicyApproval(ctx, sessionAuthorityID, req, decision)
	default:
		return s.handleGuardianReview(ctx, sessionAuthorityID, req, "Auto Mode policy requires guardian review for this action.")
	}
}

func (s *service) handlePolicyApproval(ctx context.Context, sessionAuthorityID string, req permission.PermissionRequest, decision PolicyDecision) (bool, error) {
	if s.useGuardianReview() {
		return s.handleGuardianPolicyReview(ctx, sessionAuthorityID, req, decision)
	}

	granted, err := s.promptWithEscalation(ctx, withAutoReview(req, permission.AutoReview{
		Trigger: permission.AutoReviewTriggerClassifierBlock,
		Reason:  policyReasonWithSource(decision),
	}))
	if err != nil || !granted {
		return granted, err
	}
	if decision.Cacheable {
		s.approvalCache.approve(decision.CacheKeys)
	}
	s.resetClassifierBlocks(sessionAuthorityID)
	return true, nil
}

func (s *service) handleGuardianPolicyReview(ctx context.Context, sessionAuthorityID string, req permission.PermissionRequest, decision PolicyDecision) (bool, error) {
	granted, err := s.handleGuardianReview(ctx, sessionAuthorityID, req, firstNonEmpty(policyReasonWithSource(decision), "Auto Mode policy requires approval."))
	if err != nil || !granted {
		return granted, err
	}
	if decision.Cacheable {
		s.approvalCache.approve(decision.CacheKeys)
	}
	return true, nil
}

func (s *service) handleGuardianReview(ctx context.Context, sessionAuthorityID string, req permission.PermissionRequest, reason string) (bool, error) {
	if !s.useGuardianReview() {
		return s.promptWithEscalation(ctx, withAutoReview(req, permission.AutoReview{
			Trigger: permission.AutoReviewTriggerClassifierBlock,
			Reason:  reason,
		}))
	}

	classifier := s.classifier()
	if classifier == nil {
		return s.handleClassifierUnavailable(ctx, req, "Auto Mode guardian review is unavailable.")
	}

	classification, err := classifier.ClassifyPermission(ctx, withAutoReview(req, permission.AutoReview{
		Trigger: permission.AutoReviewTriggerClassifierBlock,
		Reason:  reason,
	}))
	if err != nil {
		return s.handleClassifierFailure(ctx, req, err)
	}
	if classification.AllowAuto {
		slog.Debug("Auto Mode guardian allowed request",
			"session_id", sessionAuthorityID,
			"tool", req.ToolName,
			"action", req.Action,
			"reason", strings.TrimSpace(classification.Reason),
			"confidence", classification.Confidence,
		)
		s.resetClassifierBlocks(sessionAuthorityID)
		return true, nil
	}

	slog.Debug("Auto Mode guardian blocked request, falling back to manual approval",
		"session_id", sessionAuthorityID,
		"tool", req.ToolName,
		"action", req.Action,
		"reason", strings.TrimSpace(classification.Reason),
		"confidence", classification.Confidence,
		"soft_deny", classification.SoftDeny,
	)
	s.recordClassifierBlock(sessionAuthorityID)
	// Soft deny (default true) means user can override; hard deny means not retryable
	retryable := classification.SoftDeny // defaults to false if not set, but we want true
	// For backward compatibility: if SoftDeny is not explicitly set, treat as soft deny (retryable)
	if !classification.AllowAuto && classification.SoftDeny == false && classification.Confidence == "" {
		retryable = true // Default to retryable for old classifiers that don't set SoftDeny
	}
	s.pushDenialEntry(sessionAuthorityID, req, classification.Reason, retryable)
	// Fall back to manual approval instead of outright denying.
	// This allows users to override Guardian decisions when appropriate.
	return s.promptWithEscalation(ctx, withAutoReview(req, permission.AutoReview{
		Trigger: permission.AutoReviewTriggerClassifierBlock,
		Reason:  classification.Reason,
	}))
}

func (s *service) useGuardianReview() bool {
	return s.autoModeConfig == nil || s.autoModeConfig.UseGuardianReview == nil || *s.autoModeConfig.UseGuardianReview
}

func (s *service) workspaceWriteMode() string {
	if s.autoModeConfig == nil {
		return ""
	}
	return s.autoModeConfig.WorkspaceWriteMode
}

func (s *service) isBypassImmuneRequest(req permission.PermissionRequest) bool {
	return isAlwaysManual(req, s.workingDir)
}

func (s *service) handleClassifierUnavailable(ctx context.Context, req permission.PermissionRequest, message string) (bool, error) {
	slog.Debug("Auto Mode classifier unavailable",
		"session_id", req.SessionID,
		"tool", req.ToolName,
		"action", req.Action,
		"fail_closed", s.failClosedOnClassifierError,
	)
	if s.failClosedOnClassifierError {
		return false, permission.NewPermissionBlockedError(message, "Set permissions.fail_closed_on_classifier_error=false to fall back to manual confirmation.")
	}
	return s.promptWithEscalation(ctx, withAutoReview(req, permission.AutoReview{
		Trigger: permission.AutoReviewTriggerClassifierUnavailable,
		Reason:  message,
	}))
}

func (s *service) handleClassifierFailure(ctx context.Context, req permission.PermissionRequest, err error) (bool, error) {
	reason := fmt.Sprintf("Auto Mode permission classification failed: %v", err)
	slog.Warn("Auto Mode permission classification failed",
		"session_id", req.SessionID,
		"tool", req.ToolName,
		"action", req.Action,
		"err", err,
	)
	if s.failClosedOnClassifierError {
		slog.Debug("Auto Mode classifier failure blocks request (fail closed)",
			"session_id", req.SessionID,
			"tool", req.ToolName,
			"action", req.Action,
		)
		return false, permission.NewPermissionBlockedError(
			"Auto Mode permission classification failed.",
			reason,
		)
	}
	slog.Debug("Auto Mode classifier failure falls back to manual review",
		"session_id", req.SessionID,
		"tool", req.ToolName,
		"action", req.Action,
	)
	return s.promptWithEscalation(ctx, withAutoReview(req, permission.AutoReview{
		Trigger: permission.AutoReviewTriggerClassifierFailed,
		Reason:  reason,
	}))
}

func (s *service) promptWithEscalation(ctx context.Context, req permission.PermissionRequest) (bool, error) {
	identity := permission.WorkerIdentityFromContext(ctx)
	if identity.AgentID != "" {
		if strings.TrimSpace(req.AuthoritySessionID) == "" {
			req.AuthoritySessionID = strings.TrimSpace(identity.ParentSessionID)
		}
		if strings.TrimSpace(req.AuthoritySessionID) == "" || strings.TrimSpace(req.AuthoritySessionID) == strings.TrimSpace(req.SessionID) {
			return false, permission.NewPermissionBlockedError(
				"Subagent permission approval is unavailable.",
				"Auto Mode could not route this subagent permission request to the parent session.",
			)
		}
	}
	return s.base.Prompt(ctx, req)
}

func withAutoReview(req permission.PermissionRequest, review permission.AutoReview) permission.PermissionRequest {
	req.AutoReview = &review
	return req
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func (s *service) sessionPermissionMode(ctx context.Context, sessionID string) (session.PermissionMode, error) {
	if sessionID == "" {
		return session.PermissionModeDefault, nil
	}
	sess, err := s.sessions.Get(ctx, sessionID)
	if err != nil {
		return session.PermissionModeDefault, err
	}
	return sess.PermissionMode, nil
}

func (s *service) shouldSuspendAutoApproval(sessionID string, mode session.PermissionMode) bool {
	s.classifierMu.Lock()
	defer s.classifierMu.Unlock()

	state := s.sessionStates[sessionID]
	if mode != session.PermissionModeAuto {
		if _, ok := s.sessionStates[sessionID]; ok {
			slog.Debug("Auto Mode classifier state cleared for non-auto mode",
				"session_id", sessionID,
				"mode", mode,
			)
		}
		delete(s.sessionStates, sessionID)
		return false
	}
	if state.lastMode != session.PermissionModeAuto {
		state = sessionClassifierState{
			lastMode:    mode,
			denialQueue: NewDenialQueue(defaultMaxDenialQueueSize),
		}
		s.sessionStates[sessionID] = state
		slog.Debug("Auto Mode classifier state initialized",
			"session_id", sessionID,
		)
		return false
	}
	return state.suspendAutoApproval
}

func (s *service) resetClassifierBlocks(sessionID string) {
	s.classifierMu.Lock()
	defer s.classifierMu.Unlock()

	state := s.sessionStates[sessionID]
	prevConsecutive := state.consecutiveBlocks
	prevSuspended := state.suspendAutoApproval
	state.lastMode = session.PermissionModeAuto
	state.consecutiveBlocks = 0
	if state.totalBlocks < defaultMaxTotalClassifierBlocks {
		state.suspendAutoApproval = false
	}
	s.sessionStates[sessionID] = state
	if prevConsecutive > 0 || prevSuspended {
		slog.Debug("Auto Mode classifier block counters reset",
			"session_id", sessionID,
			"previous_consecutive_blocks", prevConsecutive,
			"total_blocks", state.totalBlocks,
			"previous_suspended", prevSuspended,
			"suspended", state.suspendAutoApproval,
		)
	}
}

func (s *service) recordClassifierBlock(sessionID string) {
	s.classifierMu.Lock()
	defer s.classifierMu.Unlock()

	state := s.sessionStates[sessionID]
	prevSuspended := state.suspendAutoApproval
	state.lastMode = session.PermissionModeAuto
	state.consecutiveBlocks++
	state.totalBlocks++
	if state.consecutiveBlocks >= defaultMaxConsecutiveClassifierBlocks || state.totalBlocks >= defaultMaxTotalClassifierBlocks {
		state.suspendAutoApproval = true
	}
	s.sessionStates[sessionID] = state
	slog.Debug("Auto Mode classifier block recorded",
		"session_id", sessionID,
		"consecutive_blocks", state.consecutiveBlocks,
		"total_blocks", state.totalBlocks,
		"suspended", state.suspendAutoApproval,
	)
	if !prevSuspended && state.suspendAutoApproval {
		slog.Debug("Auto Mode auto-approval suspended due to classifier blocks",
			"session_id", sessionID,
			"consecutive_blocks", state.consecutiveBlocks,
			"total_blocks", state.totalBlocks,
		)
	}
}

func (s *service) pushDenialEntry(sessionID string, req permission.PermissionRequest, reason string, retryable bool) {
	s.classifierMu.Lock()
	state := s.sessionStates[sessionID]
	if state.denialQueue == nil {
		state.denialQueue = NewDenialQueue(defaultMaxDenialQueueSize)
		s.sessionStates[sessionID] = state
	}
	q := state.denialQueue
	s.classifierMu.Unlock()

	entry := q.Push(req, reason, retryable)
	slog.Debug("Auto Mode denial entry pushed",
		"session_id", sessionID,
		"entry_id", entry.ID,
		"tool", req.ToolName,
		"action", req.Action,
		"retryable", retryable,
	)
}

// GetDenialQueue returns the denial queue for the given session.
// Returns nil if the session has no denial queue (e.g., not in auto mode).
func (s *service) GetDenialQueue(sessionID string) permission.DenialQueueReader {
	s.classifierMu.Lock()
	state := s.sessionStates[sessionID]
	s.classifierMu.Unlock()
	if state.denialQueue == nil {
		return nil
	}
	return &denialQueueReaderAdapter{q: state.denialQueue}
}

// denialQueueReaderAdapter adapts DenialQueue to permission.DenialQueueReader.
type denialQueueReaderAdapter struct {
	q *DenialQueue
}

func (a *denialQueueReaderAdapter) Entries() []*permission.DenialQueueEntry {
	return a.q.AsPermissionEntries()
}

func (a *denialQueueReaderAdapter) Size() int {
	return a.q.Size()
}

func (a *denialQueueReaderAdapter) IsEmpty() bool {
	return a.q.IsEmpty()
}

// GetDenialQueueEditor returns an editor for the denial queue.
// Returns nil if the session has no denial queue.
func (s *service) GetDenialQueueEditor(sessionID string) permission.DenialQueueEditor {
	s.classifierMu.Lock()
	state := s.sessionStates[sessionID]
	s.classifierMu.Unlock()
	if state.denialQueue == nil {
		return nil
	}
	return &denialQueueEditorAdapter{q: state.denialQueue}
}

// denialQueueEditorAdapter adapts DenialQueue to permission.DenialQueueEditor.
type denialQueueEditorAdapter struct {
	q *DenialQueue
}

func (a *denialQueueEditorAdapter) Entries() []*permission.DenialQueueEntry {
	return a.q.AsPermissionEntries()
}

func (a *denialQueueEditorAdapter) Size() int {
	return a.q.Size()
}

func (a *denialQueueEditorAdapter) IsEmpty() bool {
	return a.q.IsEmpty()
}

func (a *denialQueueEditorAdapter) Take(id string) *permission.DenialQueueEntry {
	entry := a.q.Take(id)
	if entry == nil {
		return nil
	}
	return &permission.DenialQueueEntry{
		ID:        entry.ID,
		Request:   entry.Request,
		Reason:    entry.Reason,
		Timestamp: entry.Timestamp,
		Retryable: entry.Retryable,
	}
}

func (s *service) isExplicitlyAllowed(opts permission.CreatePermissionRequest, req permission.PermissionRequest) bool {
	commandKey := opts.ToolName + ":" + opts.Action
	return slices.Contains(s.allowedTools, commandKey) || slices.Contains(s.allowedTools, req.ToolName)
}

func policyFromConfig(cfg *config.AutoMode) (ApprovalPolicyConfig, []ExecPolicyRule) {
	policy := DefaultApprovalPolicyConfig()
	if cfg == nil {
		return policy, nil
	}
	if parsed, ok := ParseApprovalPolicy(string(cfg.ApprovalPolicy)); ok {
		policy.Policy = parsed
	}
	if cfg.Granular != nil {
		policy.Granular = GranularApprovalConfig{
			SandboxApproval:    cfg.Granular.SandboxApproval,
			Rules:              cfg.Granular.Rules,
			SkillApproval:      cfg.Granular.SkillApproval,
			RequestPermissions: cfg.Granular.RequestPermissions,
			MCPElicitations:    cfg.Granular.MCPElicitations,
		}
	}

	rules := make([]ExecPolicyRule, 0, len(cfg.ExecPolicyRules))
	for _, rule := range cfg.ExecPolicyRules {
		rules = append(rules, ExecPolicyRule{
			Decision: ExecPolicyRuleDecision(strings.ToLower(strings.TrimSpace(rule.Decision))),
			Exact:    slices.Clone(rule.Exact),
			Prefix:   slices.Clone(rule.Prefix),
			Reason:   rule.Reason,
		})
	}
	return policy, rules
}

func isAutoModeAllowlistedRequest(req permission.PermissionRequest) bool {
	switch req.ToolName {
	case tools.ReadToolName:
		return req.Action == "read"
	case tools.GrepToolName, tools.GlobToolName:
		return req.Action == "search"
	case tools.DiagnosticsToolName,
		tools.ReferencesToolName,
		tools.LSPDeclarationToolName,
		tools.LSPDefinitionToolName,
		tools.LSPImplementationToolName,
		tools.LSPTypeDefinitionToolName,
		tools.LSPHoverToolName,
		tools.LSPDocumentSymbolsToolName,
		tools.LSPWorkspaceSymbolsToolName:
		return req.Action == "inspect" || req.Action == "search"
	default:
		return false
	}
}

func isAcceptEditsEquivalentRequest(req permission.PermissionRequest, workingDir string) bool {
	return isSafeWorkspaceWrite(req, workingDir)
}

func isSafeFindCommand(args []string) bool {
	for _, arg := range args[1:] {
		if arg == "-delete" || arg == "-exec" || arg == "-execdir" || arg == "-ok" || arg == "-okdir" {
			return false
		}
		if arg == "-fprint" || arg == "-fprintf" || arg == "-fls" {
			return false
		}
		if strings.HasPrefix(arg, "-exec") || strings.HasPrefix(arg, "-ok") || strings.HasPrefix(arg, "-fprint") || strings.HasPrefix(arg, "-fls") {
			return false
		}
	}
	return true
}

func isSafeWorkspaceWrite(req permission.PermissionRequest, workingDir string) bool {
	if workingDir == "" {
		return false
	}

	switch req.ToolName {
	case tools.EditToolName, tools.WriteToolName:
	default:
		return false
	}

	if !fsext.HasPrefix(req.Path, workingDir) {
		return false
	}

	filePath, ok := permissionRequestFilePath(req)
	if !ok || filePath == "" || !fsext.HasPrefix(filePath, workingDir) {
		return false
	}

	return !isSensitiveWorkspacePath(filePath, workingDir)
}

func permissionRequestFilePath(req permission.PermissionRequest) (string, bool) {
	switch params := req.Params.(type) {
	case tools.EditPermissionsParams:
		return params.FilePath, true
	case tools.WritePermissionsParams:
		return params.FilePath, true
	default:
		return "", false
	}
}

func isSensitiveWorkspacePath(path, workingDir string) bool {
	rel, err := filepath.Rel(workingDir, path)
	if err != nil {
		return true
	}
	rel = filepath.ToSlash(filepath.Clean(rel))
	if rel == "." || strings.HasPrefix(rel, "../") {
		return true
	}

	lowerRel := strings.ToLower(rel)
	lowerBase := strings.ToLower(filepath.Base(path))

	switch {
	case lowerRel == ".cursorrules":
		return true
	case lowerRel == ".github/copilot-instructions.md":
		return true
	case strings.HasPrefix(lowerRel, ".cursor/rules/"):
		return true
	case strings.HasPrefix(lowerRel, ".git/"):
		return true
	case strings.HasPrefix(lowerRel, ".crush/"):
		return true
	case strings.HasPrefix(lowerRel, ".claude/"):
		return true
	case strings.HasPrefix(lowerRel, ".vscode/"):
		return true
	case strings.HasPrefix(lowerRel, ".idea/"):
		return true
	case strings.HasPrefix(lowerBase, ".env"):
		return true
	case isShellStartupFile(lowerBase):
		return true
	}

	switch lowerBase {
	case "agents.md", "agents.local.md",
		"claude.md", "claude.local.md",
		"gemini.md", "gemini.local.md",
		"crush.md", "crush.local.md",
		"crush.json", ".crush.json":
		return true
	default:
		return false
	}
}

func isAlwaysManual(req permission.PermissionRequest, workingDir string) bool {
	switch req.ToolName {
	case tools.DownloadToolName, tools.AgenticFetchToolName:
		return true
	case tools.ReadToolName:
		return req.Action == "read_url"
	case tools.BashToolName:
		return isHighRiskBashRequest(req)
	case tools.EditToolName, tools.WriteToolName:
		filePath, ok := permissionRequestFilePath(req)
		return ok && isSensitiveWorkspacePath(filePath, workingDir)
	default:
		return false
	}
}

func ignoresExplicitAllowInAutoMode(req permission.PermissionRequest, workingDir string) bool {
	return isAlwaysManual(req, workingDir) || isBroadAutoModeAllowTool(req.ToolName)
}

func isBroadAutoModeAllowTool(toolName string) bool {
	switch toolName {
	case tools.BashToolName, "agent":
		return true
	default:
		return false
	}
}

func isShellStartupFile(name string) bool {
	switch name {
	case ".bashrc", ".bash_profile", ".bash_login", ".profile",
		".zshrc", ".zprofile", ".zlogin", ".zshenv",
		".config.fish", "config.fish", "profile.ps1", "microsoft.powershell_profile.ps1":
		return true
	default:
		return false
	}
}

func classifyPluginDecisionWithRuntime(runtime *plugin.Runtime, req permission.PermissionRequest) permission.EvaluationDecision {
	if runtime == nil {
		runtime = plugin.DefaultRuntime()
	}
	hookDecision := runtime.TriggerPermissionAsk(plugin.PermissionAskInput{
		Permission: plugin.PermissionRequest{
			ID:          req.ID,
			SessionID:   req.SessionID,
			ToolCallID:  req.ToolCallID,
			ToolName:    req.ToolName,
			Description: req.Description,
			Action:      req.Action,
			Params:      req.Params,
			Path:        req.Path,
		},
	})
	switch hookDecision.Action {
	case plugin.PermissionAllow:
		return permission.EvaluationDecisionAllow
	case plugin.PermissionDeny:
		return permission.EvaluationDecisionDeny
	default:
		return permission.EvaluationDecisionAsk
	}
}

func isHighRiskBashRequest(req permission.PermissionRequest) bool {
	params, ok := req.Params.(tools.BashPermissionsParams)
	if !ok {
		return false
	}

	command := strings.TrimSpace(params.Command)
	if command == "" {
		return false
	}

	if highRisk, ok := isHighRiskShellCommand(command); ok {
		return highRisk
	}

	return isHighRiskBashTextFallback(strings.ToLower(command))
}

func isHighRiskShellCommand(command string) (bool, bool) {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return false, false
	}

	highRisk := false
	syntax.Walk(file, func(node syntax.Node) bool {
		if highRisk {
			return false
		}

		switch x := node.(type) {
		case *syntax.CallExpr:
			if isHighRiskCallExpr(x) {
				highRisk = true
				return false
			}
		case *syntax.BinaryCmd:
			if isHighRiskPipeline(x) {
				highRisk = true
				return false
			}
		}
		return true
	})

	return highRisk, true
}

func isHighRiskCallExpr(call *syntax.CallExpr) bool {
	args := shellCallArgs(call)
	if len(args) == 0 || !args[0].literal {
		return false
	}

	cmd := normalizeShellCommandName(args[0].value)
	if _, ok := highRiskBashDirectCommands[cmd]; ok {
		return true
	}

	switch cmd {
	case "rm":
		for _, arg := range args[1:] {
			if arg.literal && strings.HasPrefix(arg.value, "-") {
				return true
			}
		}
	case "git":
		subcommand, ok := firstShellSubcommand(args[1:], highRiskGitFlagsWithValue)
		if !ok {
			return false
		}
		return subcommand == "push" || (subcommand == "reset" && containsLiteralShellArg(args[1:], "--hard"))
	case "terraform":
		subcommand, ok := firstShellSubcommand(args[1:], highRiskTerraformFlagsWithValue)
		return ok && (subcommand == "apply" || subcommand == "destroy")
	case "docker":
		subcommand, ok := firstShellSubcommand(args[1:], highRiskDockerFlagsWithValue)
		return ok && subcommand == "push"
	case "npm":
		subcommand, ok := firstShellSubcommand(args[1:], highRiskNPMFlagsWithValue)
		return ok && subcommand == "publish"
	}

	return false
}

func isHighRiskPipeline(cmd *syntax.BinaryCmd) bool {
	if cmd == nil {
		return false
	}
	if op := cmd.Op.String(); op != "|" && op != "|&" {
		return false
	}
	return stmtInvokesHighRiskPipelineTarget(cmd.Y)
}

func stmtInvokesHighRiskPipelineTarget(stmt *syntax.Stmt) bool {
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
		_, invokesShell = highRiskBashPipelineTargets[normalizeShellCommandName(args[0].value)]
		return !invokesShell
	})
	return invokesShell
}

type shellCallArg struct {
	value   string
	literal bool
}

func shellCallArgs(call *syntax.CallExpr) []shellCallArg {
	if call == nil || len(call.Args) == 0 {
		return nil
	}

	args := make([]shellCallArg, 0, len(call.Args))
	for _, word := range call.Args {
		arg, ok := literalWord(word)
		if ok {
			args = append(args, shellCallArg{
				value:   strings.ToLower(strings.TrimSpace(arg)),
				literal: true,
			})
			continue
		}
		args = append(args, shellCallArg{})
	}
	return args
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

func normalizeShellCommandName(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	return strings.ToLower(path.Base(filepath.ToSlash(raw)))
}

func firstShellSubcommand(args []shellCallArg, flagsWithValue map[string]bool) (string, bool) {
	for i := 0; i < len(args); i++ {
		if !args[i].literal {
			return "", false
		}

		arg := strings.TrimSpace(args[i].value)
		if arg == "" {
			continue
		}
		if arg == "--" {
			if i+1 >= len(args) || !args[i+1].literal {
				return "", false
			}
			return strings.ToLower(strings.TrimSpace(args[i+1].value)), true
		}
		if !strings.HasPrefix(arg, "-") {
			return strings.ToLower(arg), true
		}

		flag, _, hasInlineValue := strings.Cut(arg, "=")
		if flagsWithValue[flag] && !hasInlineValue {
			i++
			if i >= len(args) {
				return "", false
			}
		}
	}
	return "", false
}

func containsLiteralShellArg(args []shellCallArg, target string) bool {
	target = strings.ToLower(strings.TrimSpace(target))
	return slices.ContainsFunc(args, func(arg shellCallArg) bool {
		return arg.literal && strings.TrimSpace(arg.value) == target
	})
}

func isHighRiskBashTextFallback(command string) bool {
	highRiskSnippets := []string{
		"aws ",
		"az ",
		"curl ",
		"gcloud ",
		"gh ",
		"git push",
		"git reset --hard",
		"kubectl ",
		"remove-item",
		"rm -",
		"scp ",
		"ssh ",
		"sudo ",
		"wget ",
		"del ",
		"terraform apply",
		"terraform destroy",
		"npm publish",
		"docker push",
		"| sh",
		"| bash",
	}
	for _, snippet := range highRiskSnippets {
		if strings.Contains(command, snippet) {
			return true
		}
	}
	return false
}
