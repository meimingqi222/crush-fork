package agent

import (
	"context"
	"strings"
	"time"

	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/timeline"
)

type SubagentProfileKind string

const (
	SubagentProfileCoordinator SubagentProfileKind = "coordinator"
	SubagentProfileGeneral     SubagentProfileKind = "general"
	SubagentProfileExplore     SubagentProfileKind = "explore"
	SubagentProfilePlan        SubagentProfileKind = "plan"
	SubagentProfileReview      SubagentProfileKind = "review"
	SubagentProfileGuardian    SubagentProfileKind = "guardian"
)

type SubagentProfile struct {
	Name        string
	Kind        SubagentProfileKind
	Mode        string
	Description string
	CanSpawn    bool
	ReadOnly    bool
	ToolNames   []string
	DenyTools   []string
	Spawns      []string
}

type SubagentToolProfile struct {
	Allowed map[string]struct{}
	Denied  map[string]struct{}
}

func (p SubagentToolProfile) Allows(toolName string) bool {
	toolName = strings.TrimSpace(toolName)
	if toolName == "" {
		return false
	}
	if _, denied := p.Denied[toolName]; denied {
		return false
	}
	if len(p.Allowed) == 0 {
		return true
	}
	_, ok := p.Allowed[toolName]
	return ok
}

type DerivedSubagentPermissions struct {
	AllowedTools              map[string]struct{}
	DeniedTools               map[string]struct{}
	ReadOnly                  bool
	CanSpawn                  bool
	AgentToolExternallyDenied bool
}

type SubagentWorkspacePolicy struct {
	Root      string
	WriteMode string
}

type SubagentIsolationKind string

const (
	SubagentIsolationNone            SubagentIsolationKind = "none"
	SubagentIsolationWorktree        SubagentIsolationKind = "worktree"
	SubagentIsolationExternalSandbox SubagentIsolationKind = "external_sandbox"
	SubagentIsolationManagedSandbox  SubagentIsolationKind = "managed_sandbox"
)

type SubagentIsolation struct {
	Kind      SubagentIsolationKind
	Available bool
	Path      string
	Metadata  map[string]string
}

type MissingFinishPolicy string

const (
	MissingFinishWarn          MissingFinishPolicy = "warn"
	MissingFinishFail          MissingFinishPolicy = "fail"
	MissingFinishRetryThenWarn MissingFinishPolicy = "retry_then_warn"
	MissingFinishRetryThenFail MissingFinishPolicy = "retry_then_fail"
)

// SubagentResultContract defines the expected completion contract for a subagent.
// MaxSummaryBytes and MaxStructuredDataBytes are reserved for future size limits.
type SubagentResultContract struct {
	Required             bool
	AllowRawTextFallback bool
	MissingFinishPolicy  MissingFinishPolicy
}

type SubagentEventType string

const (
	SubagentEventStarted  SubagentEventType = "started"
	SubagentEventProgress SubagentEventType = "progress"
	SubagentEventFinish   SubagentEventType = "finish"
	SubagentEventFailed   SubagentEventType = "failed"
	SubagentEventCanceled SubagentEventType = "canceled"
	SubagentEventBlocked  SubagentEventType = "blocked"
)

type SubagentEvent struct {
	Type            SubagentEventType
	ParentSessionID string
	ChildSessionID  string
	TaskID          string
	Message         string
	Status          string
	Timestamp       time.Time
}

type SubagentEventSink interface {
	PublishSubagentEvent(ctx context.Context, event SubagentEvent)
}

type coordinatorSubagentEventSink struct {
	timeline timeline.Service
}

func (s coordinatorSubagentEventSink) PublishSubagentEvent(_ context.Context, event SubagentEvent) {
	if s.timeline == nil {
		return
	}
	switch event.Type {
	case SubagentEventStarted:
		s.timeline.Publish(timeline.ChildSessionStartedEvent(event.ParentSessionID, event.ChildSessionID, event.Message))
	case SubagentEventProgress:
		s.timeline.Publish(timeline.ChildSessionProgressEvent(event.ParentSessionID, event.ChildSessionID, event.Message, event.Status, event.Message))
	case SubagentEventFailed:
		s.timeline.Publish(timeline.ChildSessionFinishedEvent(event.ParentSessionID, event.ChildSessionID, event.Message, "failed", event.Message))
	case SubagentEventCanceled:
		s.timeline.Publish(timeline.ChildSessionFinishedEvent(event.ParentSessionID, event.ChildSessionID, event.Message, "canceled", event.Message))
	case SubagentEventBlocked:
		s.timeline.Publish(timeline.ChildSessionFinishedEvent(event.ParentSessionID, event.ChildSessionID, event.Message, "blocked", event.Message))
	default:
		s.timeline.Publish(timeline.ChildSessionFinishedEvent(event.ParentSessionID, event.ChildSessionID, event.Message, event.Status, event.Message))
	}
}

type SubagentRuntimeContext struct {
	ParentSessionID  string
	ChildSessionID   string
	ParentMessageID  string
	ParentToolCallID string
	TaskID           string
	TaskDescription  string

	AgentProfile SubagentProfile
	ToolProfile  SubagentToolProfile
	Permissions  DerivedSubagentPermissions
	Workspace    SubagentWorkspacePolicy
	Isolation    SubagentIsolation
	Result       SubagentResultContract
	Events       SubagentEventSink

	// MaxTurns is the maximum number of LLM turns the subagent is allowed.
	// Zero means no limit.
	MaxTurns int

	// OutputSchema is the JSON schema that the subagent's finish output
	// must conform to. Nil means no schema validation.
	OutputSchema any

	// FinishRequired indicates whether the subagent must call yield to complete.
	// When true, the agent loop enforces completion via the tool.
	FinishRequired bool
}

type subagentRuntimeContextKey struct{}

func withSubagentRuntimeContext(ctx context.Context, runtime SubagentRuntimeContext) context.Context {
	return context.WithValue(ctx, subagentRuntimeContextKey{}, runtime)
}

func subagentRuntimeFromContext(ctx context.Context) (SubagentRuntimeContext, bool) {
	runtime, ok := ctx.Value(subagentRuntimeContextKey{}).(SubagentRuntimeContext)
	return runtime, ok
}

func buildSubagentRuntimeContext(parentSessionID, childSessionID, parentMessageID, parentToolCallID string, task subagentTask, agentCfg config.Agent, parentPermissions ParentPermissionContext, availableTools []string, effectiveIsolation, workspaceRoot string, eventSink SubagentEventSink) SubagentRuntimeContext {
	profile := subagentProfileForAgent(agentCfg)
	permissions := DeriveSubagentPermissions(parentPermissions, profile, availableTools)
	toolProfile := subagentToolProfileFromPermissions(permissions)
	return SubagentRuntimeContext{
		ParentSessionID:  strings.TrimSpace(parentSessionID),
		ChildSessionID:   strings.TrimSpace(childSessionID),
		ParentMessageID:  strings.TrimSpace(parentMessageID),
		ParentToolCallID: strings.TrimSpace(parentToolCallID),
		TaskID:           strings.TrimSpace(task.Name),
		TaskDescription:  strings.TrimSpace(task.Description),
		AgentProfile:     profile,
		ToolProfile:      toolProfile,
		Permissions:      permissions,
		Workspace: SubagentWorkspacePolicy{
			Root:      strings.TrimSpace(workspaceRoot),
			WriteMode: subagentWorkspaceWriteMode(profile),
		},
		Isolation: SubagentIsolation{
			Kind:      subagentIsolationKind(effectiveIsolation),
			Available: true,
			Path:      strings.TrimSpace(workspaceRoot),
		},
		Result:       subagentResultContract(profile),
		Events:       eventSink,
		MaxTurns:     agentCfg.MaxTurns,
		OutputSchema: agentCfg.OutputSchema,
	}
}

func subagentProfileForAgent(agentCfg config.Agent) SubagentProfile {
	canonicalID := config.CanonicalSubagentID(agentCfg.ID)
	profile := SubagentProfile{
		Name:        canonicalID,
		Description: strings.TrimSpace(agentCfg.Description),
		Mode:        string(config.NormalizeAgentMode(agentCfg.Mode)),
		CanSpawn:    len(agentCfg.Spawns) > 0,
		ReadOnly:    isReadOnlyRuntime(agentCfg),
		ToolNames:   append([]string(nil), agentCfg.AllowedTools...),
		Spawns:      append([]string(nil), agentCfg.Spawns...),
	}
	switch canonicalID {
	case config.AgentExplore:
		profile.Kind = SubagentProfileExplore
	case config.AgentReview:
		profile.Kind = SubagentProfileReview
	case config.AgentPlan:
		profile.Kind = SubagentProfilePlan
	case config.AgentLibrarian:
		profile.Kind = SubagentProfileExplore
	case config.AgentGeneral, config.AgentCoder, config.AgentDesigner, config.AgentQuickTask:
		profile.Kind = SubagentProfileGeneral
	default:
		profile.Kind = SubagentProfileGeneral
	}
	return profile
}

// isReadOnlyAgentID is the fallback for agents without an explicit
// AllowedTools list. It matches the built-in read-only subagent IDs.
func isReadOnlyAgentID(canonicalID string) bool {
	switch canonicalID {
	case config.AgentExplore, config.AgentPlan, config.AgentReview, config.AgentLibrarian:
		return true
	default:
		return false
	}
}

// isReadOnlyRuntime determines whether an agent is read-only from its resolved
// AllowedTools list, falling back to the built-in profile for the canonical ID
// when no tool list is configured.
func isReadOnlyRuntime(agentCfg config.Agent) bool {
	if len(agentCfg.AllowedTools) == 0 {
		if strings.TrimSpace(agentCfg.ID) == "" {
			return false
		}
		return isReadOnlyAgentID(config.RequestedSubagentID(agentCfg.ID))
	}

	readOnlyTools := map[string]struct{}{
		agenttools.BashToolName:             {},
		agenttools.GlobToolName:             {},
		agenttools.GrepToolName:             {},
		agenttools.ReadToolName:             {},
		agenttools.ToolSearchToolName:       {},
		agenttools.LSPToolName:              {},
		agenttools.YieldToolName:            {},
		agenttools.AgenticFetchToolName:     {},
		agenttools.SourcegraphToolName:      {},
		agenttools.DescribeImageToolName:    {},
		agenttools.MemoryStatusToolName:     {},
		agenttools.CrushInfoToolName:        {},
		agenttools.CrushLogsToolName:        {},
		agenttools.RecallToolName:           {},
		agenttools.ReflectToolName:          {},
		agenttools.GraphToolName:            {},
		agenttools.RequestUserInputToolName: {},
	}
	for _, tool := range config.ReadOnlyResearchToolNames {
		readOnlyTools[tool] = struct{}{}
	}
	for _, tool := range agentCfg.AllowedTools {
		if _, ok := readOnlyTools[tool]; !ok {
			return false
		}
	}
	return true
}

func cloneToolSet(src map[string]struct{}) map[string]struct{} {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]struct{}, len(src))
	for key := range src {
		dst[key] = struct{}{}
	}
	return dst
}

func subagentWorkspaceWriteMode(profile SubagentProfile) string {
	if profile.ReadOnly {
		return "deny"
	}
	return "allow"
}

func subagentIsolationKind(value string) SubagentIsolationKind {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "worktree":
		return SubagentIsolationWorktree
	case "external_sandbox":
		return SubagentIsolationExternalSandbox
	case "managed_sandbox":
		return SubagentIsolationManagedSandbox
	default:
		return SubagentIsolationNone
	}
}

func subagentResultContract(profile SubagentProfile) SubagentResultContract {
	contract := SubagentResultContract{
		Required:             true,
		AllowRawTextFallback: true,
	}
	switch profile.Kind {
	case SubagentProfileExplore:
		contract.MissingFinishPolicy = MissingFinishRetryThenWarn
	case SubagentProfilePlan, SubagentProfileReview:
		contract.MissingFinishPolicy = MissingFinishRetryThenWarn
	case SubagentProfileGuardian:
		contract.AllowRawTextFallback = false
		contract.MissingFinishPolicy = MissingFinishFail
	default:
		contract.MissingFinishPolicy = MissingFinishRetryThenFail
	}
	return contract
}

func applySubagentRuntimeConfig(runtime *SubagentRuntimeContext, runtimeCfg config.SubagentRuntimeConfig) {
	if runtime == nil {
		return
	}
	runtime.Result.Required = runtimeCfg.StructuredCompletionRequired
	runtime.FinishRequired = runtimeCfg.StructuredCompletionRequired || runtime.OutputSchema != nil

	// CanSpawn is true if:
	// 1. The agent has a non-empty Spawns list (from config or builtin), OR
	// 2. AllowRecursiveAgents is true and the agent is not read-only (legacy fallback)
	canSpawn := len(runtime.AgentProfile.Spawns) > 0
	if !canSpawn {
		canSpawn = runtimeCfg.AllowRecursiveAgents && !runtime.AgentProfile.ReadOnly
	}
	if runtime.Permissions.AgentToolExternallyDenied {
		canSpawn = false
	}
	runtime.AgentProfile.CanSpawn = canSpawn
	runtime.Permissions.CanSpawn = canSpawn
	if canSpawn {
		delete(runtime.Permissions.DeniedTools, AgentToolName)
		delete(runtime.ToolProfile.Denied, AgentToolName)
		if runtime.Permissions.AllowedTools != nil {
			runtime.Permissions.AllowedTools[AgentToolName] = struct{}{}
		}
		if runtime.ToolProfile.Allowed != nil {
			runtime.ToolProfile.Allowed[AgentToolName] = struct{}{}
		}
	} else {
		if runtime.Permissions.DeniedTools == nil {
			runtime.Permissions.DeniedTools = make(map[string]struct{})
		}
		if runtime.ToolProfile.Denied == nil {
			runtime.ToolProfile.Denied = make(map[string]struct{})
		}
		runtime.Permissions.DeniedTools[AgentToolName] = struct{}{}
		runtime.ToolProfile.Denied[AgentToolName] = struct{}{}
		delete(runtime.Permissions.AllowedTools, AgentToolName)
		delete(runtime.ToolProfile.Allowed, AgentToolName)
	}
	if policy := parseMissingFinishPolicy(runtimeCfg.MissingFinishPolicy); policy != "" {
		runtime.Result.MissingFinishPolicy = policy
	}
}

func parseMissingFinishPolicy(value string) MissingFinishPolicy {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case string(MissingFinishWarn):
		return MissingFinishWarn
	case string(MissingFinishFail):
		return MissingFinishFail
	case string(MissingFinishRetryThenWarn):
		return MissingFinishRetryThenWarn
	case string(MissingFinishRetryThenFail):
		return MissingFinishRetryThenFail
	default:
		return ""
	}
}
