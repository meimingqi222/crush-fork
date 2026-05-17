package permission

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/google/uuid"
)

type PermissionErrorKind string

const (
	PermissionErrorKindUserDenied   PermissionErrorKind = "user_denied"
	PermissionErrorKindPolicyDenied PermissionErrorKind = "policy_denied"
)

type PermissionError struct {
	Kind    PermissionErrorKind
	Message string
	Details string
}

func (e *PermissionError) Error() string {
	if e == nil {
		return "permission error"
	}
	if strings.TrimSpace(e.Message) != "" {
		return e.Message
	}
	if e.Kind == PermissionErrorKindPolicyDenied {
		return "permission blocked by safety policy"
	}
	return "user denied permission"
}

func (e *PermissionError) Is(target error) bool {
	t, ok := target.(*PermissionError)
	if !ok {
		return false
	}
	return e.Kind == t.Kind
}

var (
	ErrorPermissionDenied  = &PermissionError{Kind: PermissionErrorKindUserDenied, Message: "user denied permission"}
	ErrorPermissionBlocked = &PermissionError{Kind: PermissionErrorKindPolicyDenied, Message: "permission blocked by safety policy"}
)

func NewPermissionBlockedError(message, details string) error {
	if strings.TrimSpace(message) == "" {
		message = ErrorPermissionBlocked.Error()
	}
	return &PermissionError{
		Kind:    PermissionErrorKindPolicyDenied,
		Message: strings.TrimSpace(message),
		Details: strings.TrimSpace(details),
	}
}

func AsPermissionError(err error) (*PermissionError, bool) {
	var permissionErr *PermissionError
	if errors.As(err, &permissionErr) {
		return permissionErr, true
	}
	return nil, false
}

func IsPermissionError(err error) bool {
	return errors.Is(err, ErrorPermissionDenied) || errors.Is(err, ErrorPermissionBlocked)
}

type CreatePermissionRequest struct {
	SessionID          string `json:"session_id"`
	AuthoritySessionID string `json:"authority_session_id,omitempty"`
	ToolCallID         string `json:"tool_call_id"`
	ToolName           string `json:"tool_name"`
	Description        string `json:"description"`
	Action             string `json:"action"`
	Params             any    `json:"params"`
	Path               string `json:"path"`
}

type PermissionNotification struct {
	ToolCallID string `json:"tool_call_id"`
	Granted    bool   `json:"granted"`
	Denied     bool   `json:"denied"`
}

type PermissionRequest struct {
	ID                 string      `json:"id"`
	SessionID          string      `json:"session_id"`
	AuthoritySessionID string      `json:"authority_session_id,omitempty"`
	ToolCallID         string      `json:"tool_call_id"`
	ToolName           string      `json:"tool_name"`
	Description        string      `json:"description"`
	Action             string      `json:"action"`
	Params             any         `json:"params"`
	Path               string      `json:"path"`
	AutoReview         *AutoReview `json:"auto_review,omitempty"`
}

type EvaluationDecision string

const (
	EvaluationDecisionAllow EvaluationDecision = "allow"
	EvaluationDecisionAsk   EvaluationDecision = "ask"
	EvaluationDecisionDeny  EvaluationDecision = "deny"
)

type EvaluationResult struct {
	Decision   EvaluationDecision `json:"decision"`
	Permission PermissionRequest  `json:"permission"`
}

type AutoApprovalConfidence string

const (
	AutoApprovalConfidenceLow    AutoApprovalConfidence = "low"
	AutoApprovalConfidenceMedium AutoApprovalConfidence = "medium"
	AutoApprovalConfidenceHigh   AutoApprovalConfidence = "high"
)

type AutoClassification struct {
	AllowAuto  bool                   `json:"allow_auto"`
	Reason     string                 `json:"reason"`
	Confidence AutoApprovalConfidence `json:"confidence"`
	// SoftDeny indicates that while the classifier blocked this request,
	// it's a "soft" denial that can be overridden by user approval.
	// Hard denials (SoftDeny=false) are for truly dangerous operations
	// that should not be retryable even with user consent.
	// Default is true (soft deny) for backward compatibility.
	SoftDeny bool `json:"soft_deny,omitempty"`
}

type AutoReviewTrigger string

const (
	AutoReviewTriggerClassifierBlock       AutoReviewTrigger = "classifier_block"
	AutoReviewTriggerClassifierUnavailable AutoReviewTrigger = "classifier_unavailable"
	AutoReviewTriggerClassifierFailed      AutoReviewTrigger = "classifier_failed"
	AutoReviewTriggerClassifierSuspended   AutoReviewTrigger = "classifier_suspended"
)

type AutoReview struct {
	Trigger    AutoReviewTrigger      `json:"trigger,omitempty"`
	Reason     string                 `json:"reason,omitempty"`
	Confidence AutoApprovalConfidence `json:"confidence,omitempty"`
}

type Classifier interface {
	ClassifyPermission(ctx context.Context, req PermissionRequest) (AutoClassification, error)
}

type Service interface {
	pubsub.Subscriber[PermissionRequest]
	GrantPersistent(permission PermissionRequest)
	Grant(permission PermissionRequest)
	Deny(permission PermissionRequest)
	HasPersistentPermission(permission PermissionRequest) bool
	ClearPersistentPermissions(sessionID string)
	EvaluateRequest(ctx context.Context, opts CreatePermissionRequest) (EvaluationResult, error)
	Prompt(ctx context.Context, permission PermissionRequest) (bool, error)
	Request(ctx context.Context, opts CreatePermissionRequest) (bool, error)
	AutoApproveSession(sessionID string)
	SetSessionAutoApprove(sessionID string, enabled bool)
	IsSessionAutoApprove(sessionID string) bool
	SetSkipRequests(skip bool)
	SkipRequests() bool
	SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[PermissionNotification]
	// SkillContext methods for per-skill permission control
	SetSkillContext(skillName string, allowedTools []string)
	ClearSkillContext()
	GetSkillContext() (skillName string, allowedTools []string)
	// GetDenialQueue returns the denial queue for the given session.
	// Returns nil if the session has no denial queue (e.g., not in auto mode).
	// This is used by the UI to display recent Guardian denials and allow retry.
	GetDenialQueue(sessionID string) DenialQueueReader
	// GetDenialQueueEditor returns an editor for the denial queue.
	// Returns nil if the session has no denial queue.
	// This is used for modifying the queue (e.g., removing approved entries).
	GetDenialQueueEditor(sessionID string) DenialQueueEditor
}

// DenialQueueReader provides read-only access to a denial queue.
type DenialQueueReader interface {
	Entries() []*DenialQueueEntry
	Size() int
	IsEmpty() bool
}

// DenialQueueEditor provides write access to a denial queue.
type DenialQueueEditor interface {
	DenialQueueReader
	// Take removes and returns the entry with the given ID.
	// Returns nil if not found.
	Take(id string) *DenialQueueEntry
}

// DenialQueueEntry represents a permission request that was blocked by the Guardian classifier.
type DenialQueueEntry struct {
	ID        string
	Request   PermissionRequest
	Reason    string
	Timestamp time.Time
	Retryable bool
}

type permissionService struct {
	*pubsub.Broker[PermissionRequest]

	notificationBroker    *pubsub.Broker[PermissionNotification]
	workingDir            string
	sessionPermissions    []PermissionRequest
	sessionPermissionsMu  sync.RWMutex
	pendingRequests       *csync.Map[string, chan bool]
	autoApproveSessions   map[string]bool
	autoApproveSessionsMu sync.RWMutex
	skip                  bool
	allowedTools          []string

	// used to make sure we only process one request at a time
	requestMu       sync.Mutex
	activeRequest   *PermissionRequest
	activeRequestMu sync.Mutex

	// Skill context for per-skill permission control
	skillName      string
	skillAllowed   []string
	skillContextMu sync.RWMutex
}

func (s *permissionService) GrantPersistent(permission PermissionRequest) {
	s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
		ToolCallID: permission.ToolCallID,
		Granted:    true,
	})
	respCh, ok := s.pendingRequests.Get(permission.ID)
	if ok {
		respCh <- true
	}

	s.sessionPermissionsMu.Lock()
	s.sessionPermissions = append(s.sessionPermissions, permission)
	s.sessionPermissionsMu.Unlock()

	s.activeRequestMu.Lock()
	if s.activeRequest != nil && s.activeRequest.ID == permission.ID {
		s.activeRequest = nil
	}
	s.activeRequestMu.Unlock()
}

func (s *permissionService) HasPersistentPermission(permission PermissionRequest) bool {
	s.sessionPermissionsMu.RLock()
	defer s.sessionPermissionsMu.RUnlock()

	for _, p := range s.sessionPermissions {
		if matchesPersistentPermission(p, permission) {
			return true
		}
	}
	return false
}

func (s *permissionService) ClearPersistentPermissions(sessionID string) {
	s.sessionPermissionsMu.Lock()
	defer s.sessionPermissionsMu.Unlock()

	filtered := s.sessionPermissions[:0]
	for _, p := range s.sessionPermissions {
		if p.SessionID != sessionID {
			filtered = append(filtered, p)
		}
	}
	s.sessionPermissions = filtered
}

func (s *permissionService) Grant(permission PermissionRequest) {
	s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
		ToolCallID: permission.ToolCallID,
		Granted:    true,
	})
	respCh, ok := s.pendingRequests.Get(permission.ID)
	if ok {
		respCh <- true
	}

	s.activeRequestMu.Lock()
	if s.activeRequest != nil && s.activeRequest.ID == permission.ID {
		s.activeRequest = nil
	}
	s.activeRequestMu.Unlock()
}

func (s *permissionService) Deny(permission PermissionRequest) {
	s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
		ToolCallID: permission.ToolCallID,
		Granted:    false,
		Denied:     true,
	})
	respCh, ok := s.pendingRequests.Get(permission.ID)
	if ok {
		respCh <- false
	}

	s.activeRequestMu.Lock()
	if s.activeRequest != nil && s.activeRequest.ID == permission.ID {
		s.activeRequest = nil
	}
	s.activeRequestMu.Unlock()
}

func (s *permissionService) Request(ctx context.Context, opts CreatePermissionRequest) (bool, error) {
	eval, err := s.EvaluateRequest(ctx, opts)
	if err != nil {
		return false, err
	}

	switch eval.Decision {
	case EvaluationDecisionAllow:
		return true, nil
	case EvaluationDecisionDeny:
		return false, ErrorPermissionBlocked
	default:
		return s.Prompt(ctx, eval.Permission)
	}
}

func (s *permissionService) EvaluateRequest(_ context.Context, opts CreatePermissionRequest) (EvaluationResult, error) {
	permission, err := s.buildPermissionRequest(opts)
	if err != nil {
		return EvaluationResult{}, err
	}

	if s.skip {
		return EvaluationResult{Decision: EvaluationDecisionAllow, Permission: permission}, nil
	}

	s.autoApproveSessionsMu.RLock()
	autoApprove := s.autoApproveSessions[opts.SessionID]
	s.autoApproveSessionsMu.RUnlock()
	if autoApprove {
		return EvaluationResult{Decision: EvaluationDecisionAllow, Permission: permission}, nil
	}

	return EvaluationResult{Decision: EvaluationDecisionAsk, Permission: permission}, nil
}

func (s *permissionService) Prompt(ctx context.Context, permission PermissionRequest) (bool, error) {
	s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
		ToolCallID: permission.ToolCallID,
	})
	s.requestMu.Lock()
	defer s.requestMu.Unlock()

	s.activeRequestMu.Lock()
	s.activeRequest = &permission
	s.activeRequestMu.Unlock()

	respCh := make(chan bool, 1)
	s.pendingRequests.Set(permission.ID, respCh)
	defer s.pendingRequests.Del(permission.ID)

	s.Publish(pubsub.CreatedEvent, permission)

	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case granted := <-respCh:
		return granted, nil
	}
}

func (s *permissionService) buildPermissionRequest(opts CreatePermissionRequest) (PermissionRequest, error) {
	fileInfo, err := os.Stat(opts.Path)
	dir := opts.Path
	if err == nil {
		if fileInfo.IsDir() {
			dir = opts.Path
		} else {
			dir = filepath.Dir(opts.Path)
		}
	}

	if dir == "." {
		dir = s.workingDir
	}

	authoritySessionID := opts.AuthoritySessionID
	if authoritySessionID == "" {
		authoritySessionID = opts.SessionID
	}

	return PermissionRequest{
		ID:                 uuid.New().String(),
		Path:               dir,
		SessionID:          opts.SessionID,
		AuthoritySessionID: authoritySessionID,
		ToolCallID:         opts.ToolCallID,
		ToolName:           opts.ToolName,
		Description:        opts.Description,
		Action:             opts.Action,
		Params:             opts.Params,
	}, nil
}

func (s *permissionService) AutoApproveSession(sessionID string) {
	s.SetSessionAutoApprove(sessionID, true)
}

func (s *permissionService) SetSessionAutoApprove(sessionID string, enabled bool) {
	s.autoApproveSessionsMu.Lock()
	if enabled {
		s.autoApproveSessions[sessionID] = true
	} else {
		delete(s.autoApproveSessions, sessionID)
	}
	s.autoApproveSessionsMu.Unlock()
}

func (s *permissionService) IsSessionAutoApprove(sessionID string) bool {
	s.autoApproveSessionsMu.RLock()
	enabled := s.autoApproveSessions[sessionID]
	s.autoApproveSessionsMu.RUnlock()
	return enabled
}

func (s *permissionService) SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[PermissionNotification] {
	return s.notificationBroker.Subscribe(ctx)
}

func (s *permissionService) SetSkipRequests(skip bool) {
	s.skip = skip
}

func (s *permissionService) SkipRequests() bool {
	return s.skip
}

// SetSkillContext sets the active skill context for per-skill permission control.
// When a skill context is active, tool permissions are evaluated against the skill's
// allowed_tools list in addition to global permissions.
func (s *permissionService) SetSkillContext(skillName string, allowedTools []string) {
	s.skillContextMu.Lock()
	defer s.skillContextMu.Unlock()
	s.skillName = skillName
	s.skillAllowed = allowedTools
}

// ClearSkillContext clears the active skill context.
func (s *permissionService) ClearSkillContext() {
	s.skillContextMu.Lock()
	defer s.skillContextMu.Unlock()
	s.skillName = ""
	s.skillAllowed = nil
}

// GetSkillContext returns the current skill context if any.
func (s *permissionService) GetSkillContext() (string, []string) {
	s.skillContextMu.RLock()
	defer s.skillContextMu.RUnlock()
	return s.skillName, s.skillAllowed
}

// GetDenialQueue returns nil for the base permission service.
// The autopermission wrapper provides the actual implementation.
func (s *permissionService) GetDenialQueue(sessionID string) DenialQueueReader {
	return nil
}

// GetDenialQueueEditor returns nil for the base permission service.
// The autopermission wrapper provides the actual implementation.
func (s *permissionService) GetDenialQueueEditor(sessionID string) DenialQueueEditor {
	return nil
}

// isToolAllowedBySkill checks if a tool is allowed by the current skill context.
// Returns true if:
// - No skill context is active (no restriction)
// - The tool matches one of the allowed patterns
func (s *permissionService) isToolAllowedBySkill(toolName, action string) bool {
	s.skillContextMu.RLock()
	defer s.skillContextMu.RUnlock()

	// No skill context active, allow all tools.
	if s.skillName == "" || len(s.skillAllowed) == 0 {
		return true
	}

	// Check if the tool matches any allowed pattern.
	for _, pattern := range s.skillAllowed {
		if matchesToolPattern(pattern, toolName, action) {
			return true
		}
	}

	return false
}

// matchesToolPattern checks if a tool/action matches an allowed pattern.
// Patterns can be:
// - "ToolName" - matches the tool exactly
// - "ToolName(action)" - matches tool with specific action
// - "ToolName(prefix:*)" - matches tool with action starting with prefix
func matchesToolPattern(pattern, toolName, action string) bool {
	// Simple tool name match
	if pattern == toolName {
		return true
	}

	// Pattern with action: ToolName(action) or ToolName(prefix:*).
	if idx := strings.IndexByte(pattern, '('); idx > 0 {
		patternTool := pattern[:idx]
		if patternTool != toolName {
			return false
		}

		// Extract action pattern.
		if !strings.HasSuffix(pattern, ")") {
			return false
		}
		actionPattern := pattern[idx+1 : len(pattern)-1] // Remove trailing ')'

		// Wildcard suffix: prefix:*
		if strings.HasSuffix(actionPattern, ":*") {
			prefix := actionPattern[:len(actionPattern)-2]
			return strings.HasPrefix(action, prefix)
		}

		// Exact action match.
		return actionPattern == action
	}

	return false
}

func matchesPersistentPermission(granted PermissionRequest, requested PermissionRequest) bool {
	return granted.ToolName == requested.ToolName &&
		granted.Action == requested.Action &&
		granted.SessionID == requested.SessionID &&
		granted.Path == requested.Path
}

func NewPermissionService(workingDir string, skip bool, allowedTools []string) Service {
	return &permissionService{
		Broker:              pubsub.NewBroker[PermissionRequest](),
		notificationBroker:  pubsub.NewBroker[PermissionNotification](),
		workingDir:          workingDir,
		sessionPermissions:  make([]PermissionRequest, 0),
		autoApproveSessions: make(map[string]bool),
		skip:                skip,
		allowedTools:        allowedTools,
		pendingRequests:     csync.NewMap[string, chan bool](),
	}
}
