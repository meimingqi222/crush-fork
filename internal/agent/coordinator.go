package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/google/uuid"
	"github.com/kaptinlin/jsonschema"

	"github.com/charmbracelet/crush/internal/agent/mailbox"
	agentNotify "github.com/charmbracelet/crush/internal/agent/notify"
	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/agent/reducer"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/checkpoint"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/filetracker"
	goalruntime "github.com/charmbracelet/crush/internal/goal"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/hooks"
	"github.com/charmbracelet/crush/internal/httpext"
	"github.com/charmbracelet/crush/internal/lsp"
	"github.com/charmbracelet/crush/internal/memory"
	"github.com/charmbracelet/crush/internal/memory/engine"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/oauth/copilot"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/plugin"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/sessionevent"
	"github.com/charmbracelet/crush/internal/skills"
	"github.com/charmbracelet/crush/internal/timeline"
	"github.com/charmbracelet/crush/internal/toolruntime"
	"github.com/charmbracelet/crush/internal/userinput"
	"golang.org/x/sync/errgroup"

	"charm.land/fantasy/providers/openrouter"
)

// Coordinator errors.

var (
	errCoderAgentNotConfigured         = errors.New("coder agent not configured")
	errModelProviderNotConfigured      = errors.New("model provider not configured")
	errLargeModelNotSelected           = fmt.Errorf("%w: large model not selected", ErrUnresolvedModel)
	errSmallModelNotSelected           = fmt.Errorf("%w: small model not selected", ErrUnresolvedModel)
	errLargeModelProviderNotConfigured = fmt.Errorf("%w: large model provider not configured", ErrUnresolvedModel)
	errSmallModelProviderNotConfigured = fmt.Errorf("%w: small model provider not configured", ErrUnresolvedModel)
	errLargeModelNotFound              = fmt.Errorf("%w: large model not found in provider config", ErrUnresolvedModel)
	errSmallModelNotFound              = fmt.Errorf("%w: small model not found in provider config", ErrUnresolvedModel)
	errTargetModelNotFound             = errors.New("target model not found in provider config")
)

// ErrUnresolvedModel indicates that the configured large or small model could
// not be resolved against the current provider configuration. Callers (e.g.,
// app startup) can use errors.Is to detect this recoverable condition and
// allow the user to re-select a model from the UI instead of failing hard.
var ErrUnresolvedModel = errors.New("selected model is unavailable in provider config")

const maxModelSwitchSummaries = 2
const (
	mentalModelTTL     = 5 * time.Minute
	recallContextTurns = 1
)

type goalContinuationDepthKey struct{}

const maxGoalContinuationsPerRun = 8

type Coordinator interface {
	// INFO: (kujtim) this is not used yet we will use this when we have multiple agents
	// SetMainAgent(string)
	Run(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error)
	Cancel(sessionID string)
	CancelAll()
	IsSessionBusy(sessionID string) bool
	IsBusy() bool
	QueuedPrompts(sessionID string) int
	QueuedPromptsList(sessionID string) []string
	RemoveQueuedPrompt(sessionID string, index int) bool
	ClearQueue(sessionID string)
	PauseQueue(sessionID string)
	ResumeQueue(sessionID string)
	IsQueuePaused(sessionID string) bool
	PrioritizeQueuedPrompt(sessionID string, index int) bool
	RemoveQueuedTurn(sessionID, turnID string) bool
	Steer(sessionID, prompt string, attachments ...message.Attachment) bool
	Summarize(context.Context, string, fantasy.ProviderOptions) error
	GenerateHandoff(ctx context.Context, sourceSessionID, goal string) (HandoffDraft, error)
	ClassifyPermission(ctx context.Context, req permission.PermissionRequest) (permission.AutoClassification, error)
	Model() Model
	ModelForSession(sessionID string) (Model, bool)
	PrepareModelSwitch(ctx context.Context, sessionID string, modelType config.SelectedModelType, selectedModel config.SelectedModel) error
	UpdateModels(ctx context.Context) error
	RefreshTools(ctx context.Context) error

	// EnhancePrompt uses the small model to rewrite a prompt to be clearer and
	// more specific, taking conversation history into account.
	// sessionID is the current session; pass empty string to skip history.
	EnhancePrompt(ctx context.Context, sessionID, prompt string) (string, error)

	// RecapSession generates a brief recap of the current session state using
	// the small model (falling back to large). It is intended to be called
	// after a period of user inactivity so the user can quickly re-orient
	// when they return. Returns an empty string when there is nothing worth
	// summarising (e.g. empty session).
	RecapSession(ctx context.Context, sessionID string) (string, error)

	// EscalationBridge returns the permission escalation bridge for worker-to-leader communication.
	EscalationBridge() *permission.EscalationBridge
}

type coordinator struct {
	cfg           *config.ConfigStore
	sessions      session.Service
	messages      message.Service
	permissions   permission.Service
	userInput     userinput.Service
	history       history.Service
	filetracker   filetracker.Service
	lspManager    *lsp.Manager
	notify        pubsub.Publisher[agentNotify.Notification]
	toolRuntime   toolruntime.Service
	timeline      timeline.Service
	hookManager   *hooks.Manager
	checkpoint    checkpoint.Service
	mailbox       mailbox.Service
	pluginRuntime *plugin.Runtime

	currentAgent SessionAgent
	agents       map[string]SessionAgent

	activeSubAgentsMu sync.Mutex
	activeSubAgents   map[string]map[SessionAgent]struct{}

	// childSessionAgents maps child session IDs to the SessionAgent running
	// that child session, for model lookup when viewing subagent sessions.
	childSessionAgents sync.Map

	deferredMu                 sync.Mutex
	activatedDeferredBySession map[string]map[string]struct{}
	knownDeferredToolNames     map[string]bool

	subAgentFactory subAgentFactory
	readyWg         errgroup.Group

	// backgroundAgents tracks asynchronously running background agents.
	backgroundAgents *backgroundAgentRegistry

	// escalationBridge handles permission escalation from workers to leader.
	escalationBridge *permission.EscalationBridge

	// memoryBackend is the memory system abstraction (local or hindsight).
	memoryBackend memory.Backend

	// agentRegistry tracks all running agents for IRC peer discovery.
	agentRegistry *AgentRegistry

	// lifecycle manages the keep-alive window for completed subagents so a
	// follow-up agent tool call with ExistingSessionID can reuse the live
	// SessionAgent instance instead of rebuilding one from disk history.
	lifecycle *subagentLifecycleManager

	// mainAgentID is the ID of the main (coder) agent in the registry.
	mainAgentID string

	// transcriptTurnCounts tracks turn counts per session for transcript backend.
	transcriptTurnCounts   map[string]int
	transcriptTurnCountsMu sync.Mutex

	// gitStatusCache freezes git status per working directory so the system
	// prompt prefix stays stable across turns, enabling prompt cache hits.
	gitStatusCache   map[string]string
	gitStatusCacheMu sync.Mutex

	// visionService describes images using a vision-capable helper model
	// when the primary model does not support image inputs. May be nil.
	visionService *VisionService

	// goalRuntime tracks goal state, token accounting, and wall-clock time.
	goalRuntime *goalruntime.Runtime

	responsesWSPool *httpext.ResponsesWebSocketPool

	responsesWSTransportMu sync.Mutex
	responsesWSTransport   map[responsesWSTransportKey]responsesWSTransportEntry

	sessionEvents *sessionevent.Hub
}

func NewCoordinator(
	ctx context.Context,
	cfg *config.ConfigStore,
	sessions session.Service,
	messages message.Service,
	permissions permission.Service,
	userInput userinput.Service,
	history history.Service,
	filetracker filetracker.Service,
	checkpointSvc checkpoint.Service,
	lspManager *lsp.Manager,
	notify pubsub.Publisher[agentNotify.Notification],
	toolRuntime toolruntime.Service,
	timeline timeline.Service,
	pluginRuntime *plugin.Runtime,
	memoryBackend memory.Backend,
	goalRuntime *goalruntime.Runtime,
	sessionEvents ...*sessionevent.Hub,
) (Coordinator, error) {
	hookMgr, err := hooks.NewManager(cfg.Config().Hooks)
	if err != nil {
		slog.Warn("Failed to initialize hook manager, hooks will be disabled", "error", err)
		hookMgr = nil
	} else {
		slog.Debug("Hook manager initialized", "hooks_count", len(cfg.Config().Hooks))
	}

	var eventHub *sessionevent.Hub
	if len(sessionEvents) > 0 {
		eventHub = sessionEvents[0]
	}
	c := &coordinator{
		cfg:                        cfg,
		sessions:                   sessions,
		messages:                   messages,
		permissions:                permissions,
		userInput:                  userInput,
		history:                    history,
		filetracker:                filetracker,
		checkpoint:                 checkpointSvc,
		pluginRuntime:              pluginRuntime,
		lspManager:                 lspManager,
		notify:                     notify,
		toolRuntime:                toolRuntime,
		timeline:                   timeline,
		hookManager:                hookMgr,
		mailbox:                    mailbox.NewService(),
		agents:                     make(map[string]SessionAgent),
		activatedDeferredBySession: make(map[string]map[string]struct{}),
		knownDeferredToolNames:     make(map[string]bool),
		backgroundAgents:           newBackgroundAgentRegistry(),
		escalationBridge:           permission.NewEscalationBridge(),
		transcriptTurnCounts:       make(map[string]int),
		goalRuntime:                cmp.Or(goalRuntime, goalruntime.NewRuntime(sessions)),
		responsesWSPool:            httpext.NewResponsesWebSocketPool(),
		sessionEvents:              eventHub,
	}
	if c.pluginRuntime == nil {
		c.pluginRuntime = plugin.DefaultRuntime()
	}
	if memoryBackend != nil {
		c.SetMemoryBackend(memoryBackend)
	}
	c.visionService = NewVisionService(c)

	agentCfg, ok := cfg.Config().Agents[config.AgentCoder]
	if !ok {
		return nil, errCoderAgentNotConfigured
	}

	// TODO: make this dynamic when we support multiple agents
	prompt, err := promptForAgent(agentCfg, false, prompt.WithWorkingDir(c.cfg.WorkingDir()))
	if err != nil {
		return nil, err
	}

	// agentRegistry, mainAgentID, and lifecycle must be set before buildAgent
	// is called: for the primary agent, buildAgent kicks off an async runtime
	// config refresh (c.readyWg.Go) that reads c.agentRegistry while building
	// tools (see registerAgentTools), racing with these assignments if they
	// happened afterward.
	c.agentRegistry = GlobalAgentRegistry()
	c.mainAgentID = "0-Main"
	c.lifecycle = newSubagentLifecycleManager(c.agentRegistry, &c.childSessionAgents)

	agent, err := c.buildAgent(ctx, prompt, agentCfg, false)
	if err != nil {
		return nil, err
	}
	c.currentAgent = agent
	c.agents[config.AgentCoder] = agent

	c.agentRegistry.Register(AgentRef{
		ID:          c.mainAgentID,
		DisplayName: "Main",
		Kind:        AgentKindMain,
		Status:      AgentStatusIdle,
		Agent:       agent,
	})

	// Subscribe to background agent lifecycle events and publish notifications.
	go func() {
		subChan := c.backgroundAgents.broker.Subscribe(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-subChan:
				if !ok {
					return
				}
				if event.Type == pubsub.UpdatedEvent {
					entry := event.Payload
					if entry.Status == backgroundAgentStatusCompleted ||
						entry.Status == backgroundAgentStatusFailed ||
						entry.Status == backgroundAgentStatusCanceled {
						if entry.ParentSessionID != "" {
							xmlSummary := formatBackgroundAgentNotification(&entry)
							c.notify.Publish(pubsub.UpdatedEvent, agentNotify.Notification{
								Type:         agentNotify.TypeSubagentFinished,
								SessionID:    entry.ParentSessionID,
								SubagentID:   entry.AgentID,
								SubagentType: entry.AgentType,
								Status:       string(entry.Status),
								Summary:      xmlSummary,
							})
						}
					}
				}
			}
		}
	}()

	tools.SetIrcResponder(func(ctx context.Context, targetID, message string) (string, error) {
		ref, ok := c.agentRegistry.Get(targetID)
		if !ok {
			return "", fmt.Errorf("agent %q not found in registry", targetID)
		}
		if ref.Agent == nil {
			return "", fmt.Errorf("agent %q has no active agent instance", targetID)
		}
		return ref.Agent.RespondAsBackground(ctx, targetID, message)
	})

	return c, nil
}

func (c *coordinator) plugins() *plugin.Runtime {
	if c != nil && c.pluginRuntime != nil {
		return c.pluginRuntime
	}
	return plugin.DefaultRuntime()
}

// getDiscoveredSkills returns cached skills for the given paths. Caching is
// handled by the skills package via DiscoverCached so that all callers
// (coordinator, prompt builder, crush_info tool) share a single cache and
// avoid repeated filesystem scans.
func (c *coordinator) getDiscoveredSkills(skillsPaths []string) []*skills.Skill {
	return skills.DiscoverCached(skillsPaths)
}

// SetMemoryBackend attaches the memory backend to the coordinator. If the
// backend is a local backend, the episodic memory extractor and consolidator
// are wired automatically. The hindsight backend delegates extraction and
// consolidation to the remote Hindsight service, so running them locally
// would be redundant and wasteful.
func (c *coordinator) SetMemoryBackend(b memory.Backend) {
	c.memoryBackend = b
	if b == nil || !b.Enabled() {
		return
	}
	switch backend := b.(type) {
	case *memory.LocalBackend:
		// Wire the local engine's extractor and consolidator only for the
		// local backend (hindsight delegates to its remote service).
		if eng := backend.Engine(); eng != nil {
			c.wireMemoryExtractor(eng)
			c.wireMemoryConsolidator(eng)
		}
	case *memory.HindsightBackend:
		// The hindsight backend needs the coordinator's message store to
		// retain transcript windows and to build compaction-rescue recall
		// queries from recent conversation context. Without this wiring,
		// AfterTurn/BeforeCompaction transcript retention silently no-ops
		// (see docs/refactor-memory.md Phase 5 / review finding A1).
		backend.SetRetainTranscript(c.transcriptAfterTurn)
		backend.SetRescueQueryBuilder(c.buildHindsightRescueQuery)
	}
}

// buildHindsightRescueQuery builds the compaction-rescue recall query for the
// hindsight backend from recent conversation context (there is no "current
// prompt" at compaction time). The query is truncated according to the
// backend's Capabilities.TruncateRecallQuery.
func (c *coordinator) buildHindsightRescueQuery(ctx context.Context, sessionID string) string {
	recent := buildRecentConversation(ctx, c.messages, sessionID, recallContextTurns)
	query := composeRecallQuery("", recent)
	if c.memoryBackend != nil && c.memoryBackend.Capabilities().TruncateRecallQuery {
		query = truncateRecallQuery(query, "", maxAutoRecallQueryChars)
	}
	return strings.TrimSpace(query)
}

// MemoryBackend returns the attached memory backend, if any.
func (c *coordinator) MemoryBackend() memory.Backend {
	return c.memoryBackend
}

// memoryEngineEventStore returns the backend's EventStore, or nil if the
// backend is not configured or disabled.
func (c *coordinator) memoryEngineEventStore() engine.EventStore {
	if c.memoryBackend == nil || !c.memoryBackend.Enabled() {
		return nil
	}
	return c.memoryBackend.EventStore()
}

// memoryEngineRetriever returns the backend's Retriever, or nil if the
// backend is not configured or disabled.
func (c *coordinator) memoryEngineRetriever() engine.Retriever {
	if c.memoryBackend == nil || !c.memoryBackend.Enabled() {
		return nil
	}
	return c.memoryBackend.Retriever()
}

// memoryEngineTripleStore returns the backend's TripleStore, or nil if the
// backend is not configured or disabled.
func (c *coordinator) memoryEngineTripleStore() *engine.TripleStore {
	if c.memoryBackend == nil || !c.memoryBackend.Enabled() {
		return nil
	}
	return c.memoryBackend.TripleStore()
}

// memoryEngineAccessor is implemented by backends that wrap a concrete
// *engine.Engine (both LocalBackend and HindsightBackend do). It exists so
// low-level diagnostic tooling (e.g. the crush info tool) can reach the
// engine's pipeline state without the coordinator's business logic depending
// on the concrete engine type.
type memoryEngineAccessor interface {
	Engine() *engine.Engine
}

// memoryEngine returns the underlying *engine.Engine for diagnostic purposes,
// or nil if the backend is not configured or does not expose one. Business
// logic should use MemoryBackend()/Capabilities() instead of this accessor.
func (c *coordinator) memoryEngine() *engine.Engine {
	if c.memoryBackend == nil {
		return nil
	}
	if ep, ok := c.memoryBackend.(memoryEngineAccessor); ok {
		return ep.Engine()
	}
	return nil
}

// transcriptAfterTurn handles transcript window retention for the transcript backend.
// It increments the turn counter and retains the transcript window every N turns.
func (c *coordinator) transcriptAfterTurn(ctx context.Context, sessionID string) {
	if c.memoryBackend == nil || c.memoryBackend.TranscriptRetainer() == nil {
		return
	}
	retainInterval := 3
	if cfg := c.cfg.Config(); cfg != nil && cfg.Options != nil && cfg.Options.Memory != nil {
		retainInterval = cfg.Options.Memory.GetRetainEveryNTurns()
	}

	c.transcriptTurnCountsMu.Lock()
	c.transcriptTurnCounts[sessionID]++
	turnCount := c.transcriptTurnCounts[sessionID]
	c.transcriptTurnCountsMu.Unlock()

	if turnCount%retainInterval != 0 {
		return
	}

	msgs, err := c.messages.List(ctx, sessionID)
	if err != nil {
		slog.Warn("Failed to list messages for transcript retain", "error", err, "session_id", sessionID)
		return
	}
	content := buildTranscriptWindow(msgs)
	if content == "" {
		return
	}
	retainer := c.memoryBackend.TranscriptRetainer()
	if retainer == nil {
		slog.Warn("Transcript memory backend has no retainer", "session_id", sessionID)
		return
	}
	if err := retainer.RetainTranscript(ctx, sessionID, turnCount, content); err != nil {
		slog.Warn("Failed to retain transcript window", "error", err, "session_id", sessionID)
	}
}

func (c *coordinator) clearTranscriptTurnCountForSession(sessionID string) {
	c.transcriptTurnCountsMu.Lock()
	defer c.transcriptTurnCountsMu.Unlock()
	delete(c.transcriptTurnCounts, sessionID)
}

// onSessionDeleted cleans up coordinator and memory-backend state for a session.
// Fires only when a session is explicitly deleted (not on quit/Ctrl+C -
// those paths use Backend.Close before Close).
func (c *coordinator) onSessionDeleted(ctx context.Context, sessionID string) {
	c.clearTranscriptTurnCountForSession(sessionID)
	c.backgroundAgents.RemoveForSession(sessionID)
	if c.memoryBackend != nil {
		if err := c.memoryBackend.OnSessionDeleted(ctx, sessionID); err != nil {
			slog.Warn("Memory backend OnSessionDeleted failed", "error", err, "session_id", sessionID)
		}
	}
}

// OnSessionDeleted releases coordinator-owned state after persistent session
// deletion. It is exported so App lifecycle wiring can invoke it across the
// package boundary without adding deletion control to the public Coordinator
// interface used by UI and test mocks.
func (c *coordinator) OnSessionDeleted(ctx context.Context, sessionID string) {
	c.onSessionDeleted(ctx, sessionID)
}

// memoryEngineHooks returns lifecycle callbacks for the session agent
// when the memory backend is enabled. Returns nil when the backend is
// disabled or not configured.
func (c *coordinator) memoryEngineHooks() *MemoryEngineHooks {
	if c.memoryBackend == nil || !c.memoryBackend.Enabled() {
		return nil
	}
	return &MemoryEngineHooks{
		OnBeforeCompaction: func(ctx context.Context, sessionID string) string {
			return c.memoryBackend.BeforeCompaction(ctx, sessionID)
		},
		OnSessionDeleted: func(ctx context.Context, sessionID string) {
			c.onSessionDeleted(ctx, sessionID)
		},
	}
}

// compactionRecallConfig returns the active compaction recall config.
// Returns nil when the coordinator config is missing.
func (c *coordinator) compactionRecallConfig() *config.MemoryCompactionRecallConfig {
	if c == nil || c.cfg == nil {
		return nil
	}
	cfg := c.cfg.Config()
	if cfg == nil || cfg.Options == nil || cfg.Options.Memory == nil {
		return nil
	}
	return cfg.Options.Memory.CompactionRecall
}

// workingMemoryMinDiscardedTokens returns the configured minimum discarded-
// token threshold that gates post-compaction session working-memory
// generation. Falls back to MemoryConfig's default when the coordinator
// config or memory options are missing.
func (c *coordinator) workingMemoryMinDiscardedTokens() int64 {
	if c == nil || c.cfg == nil {
		return (&config.MemoryConfig{}).GetWorkingMemoryMinDiscardedTokens()
	}
	cfg := c.cfg.Config()
	if cfg == nil || cfg.Options == nil {
		return (&config.MemoryConfig{}).GetWorkingMemoryMinDiscardedTokens()
	}
	return cfg.Options.Memory.GetWorkingMemoryMinDiscardedTokens()
}

// collectRecentSuccessfulTools scans the session message history and returns
// the names of tools that were successfully invoked in the most recent turn
// (since the last user message). This mirrors Claude Code's approach of
// surfacing active-tool context so the memory selector can de-prioritize
// reference/docs memories for tools the model is already exercising.
func collectRecentSuccessfulTools(ctx context.Context, messagesSvc message.Service, sessionID string) []string {
	msgs, err := messagesSvc.List(ctx, sessionID)
	if err != nil {
		slog.Debug("Failed to list messages for recent-tools collection", "error", err, "session_id", sessionID)
		return nil
	}

	// Find the last user message (the one just created). We only care about
	// tool calls between that message and the *previous* user message.
	var lastUserIdx int = -1
	var prevUserIdx int = -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == message.User {
			if lastUserIdx == -1 {
				lastUserIdx = i
			} else {
				prevUserIdx = i
				break
			}
		}
	}
	if lastUserIdx == -1 {
		return nil
	}

	// Build a map of tool call IDs to tool names from assistant messages.
	toolCalls := make(map[string]string)
	// Build a map of tool call IDs to error status from tool result messages.
	toolErrors := make(map[string]bool)

	// Scan messages between prevUserIdx (exclusive) and lastUserIdx (exclusive).
	// If prevUserIdx is -1 (no previous user message), scan from the start.
	startIdx := 0
	if prevUserIdx != -1 {
		startIdx = prevUserIdx + 1
	}
	for i := startIdx; i < lastUserIdx; i++ {
		msg := msgs[i]
		switch msg.Role {
		case message.Assistant:
			for _, part := range msg.Parts {
				if tc, ok := part.(message.ToolCall); ok {
					toolCalls[tc.ID] = tc.Name
				}
			}
		case message.Tool:
			for _, part := range msg.Parts {
				if tr, ok := part.(message.ToolResult); ok {
					toolErrors[tr.ToolCallID] = tr.IsError
				}
			}
		}
	}

	// Return tool names for calls that succeeded (no error).
	seen := make(map[string]bool)
	var result []string
	for id, name := range toolCalls {
		// Include the tool when there is no matching result (unknown status) or
		// the result explicitly indicates success. Only exclude when we have
		// confirmation that it failed.
		errored, ok := toolErrors[id]
		if !ok || !errored {
			if !seen[name] {
				seen[name] = true
				result = append(result, name)
			}
		}
	}
	return result
}

// collectSurfacedMemories scans the session message history for previously
// injected memory content (wrapped in system-reminder tags) and returns the
// set of surfaced memory keys and the cumulative byte count. This mirrors
// claude-code's collectSurfacedMemories approach: scanning messages means
// compact naturally resets both — old memory content is gone from the
// compacted transcript, so re-surfacing is valid again.
func collectSurfacedMemories(ctx context.Context, messagesSvc message.Service, sessionID string) (map[string]bool, int) {
	msgs, err := messagesSvc.List(ctx, sessionID)
	if err != nil {
		slog.Debug("Failed to list messages for surfaced-memories collection", "error", err, "session_id", sessionID)
		return nil, 0
	}

	surfaced := make(map[string]bool)
	totalBytes := 0

	for _, msg := range msgs {
		// Memory content is now injected as user messages (wrapped in system-reminder).
		if msg.Role != message.User {
			continue
		}
		for _, part := range msg.Parts {
			text, ok := part.(message.TextContent)
			if !ok {
				continue
			}
			content := text.Text
			if !strings.Contains(content, "<system-reminder>") {
				continue
			}
			totalBytes += len(content)
			// Extract keys from lines like "- key (meta): value"
			for line := range strings.SplitSeq(content, "\n") {
				line = strings.TrimSpace(line)
				if !strings.HasPrefix(line, "- ") {
					continue
				}
				line = strings.TrimPrefix(line, "- ")
				// Key is everything before the first " (" or ": "
				if idx := strings.Index(line, " ("); idx > 0 {
					surfaced[line[:idx]] = true
				} else if idx := strings.Index(line, ": "); idx > 0 {
					surfaced[line[:idx]] = true
				}
			}
		}
	}
	return surfaced, totalBytes
}

// buildRecentConversation extracts the last N user turns and their matching
// assistant responses from the session history to provide context for short-query expansion.
//
// Note: msg.Content().Text only extracts plain text content (excluding ToolResult).
// This is intentional, as tool outputs are too verbose to be useful for recall query expansion.
//
// Only User messages increment the turn counter, ensuring we capture full
// conversational context without counting intermediate assistant steps as separate turns.
func buildRecentConversation(ctx context.Context, messagesSvc message.Service, sessionID string, maxTurns int) string {
	msgs, err := messagesSvc.List(ctx, sessionID)
	if err != nil {
		slog.Debug("Failed to list messages for conversation context", "error", err, "session_id", sessionID)
		return ""
	}

	var lines []string
	turns := 0
	for i := len(msgs) - 1; i >= 0 && turns < maxTurns; i-- {
		msg := msgs[i]
		switch msg.Role {
		case message.User, message.Assistant:
			text := msg.Content().Text
			if text = strings.TrimSpace(text); text != "" {
				lines = append(lines, fmt.Sprintf("%s: %s", msg.Role, text))
				if msg.Role == message.User {
					turns++
				}
			}
		}
	}
	slices.Reverse(lines)
	return strings.Join(lines, "\n")
}

// Run implements Coordinator.
func (c *coordinator) Run(ctx context.Context, sessionID string, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	start := time.Now()
	defer func() {
		slog.Debug("[PERF] coordinator.Run total", "duration", time.Since(start), "session_id", sessionID)
	}()

	if err := c.readyWg.Wait(); err != nil {
		return nil, err
	}
	defer c.clearDeferredToolActivationsForSession(sessionID)

	// Get session to retrieve session-specific working directory.
	sess, err := c.sessions.Get(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to get session: %w", err)
	}
	if sess.CollaborationMode == session.CollaborationModePlan {
		sess, err = c.ensurePlanFileForSession(ctx, sess)
		if err != nil {
			return nil, err
		}
	}
	slog.Debug("[PERF] coordinator: got session", "duration", time.Since(start), "session_id", sessionID)

	// Set session-specific working directory in context.
	// Tools will use this instead of the global working dir to avoid
	// race conditions when multiple sessions run concurrently.
	sessionWorkingDir := sess.WorkspaceCWD
	if sessionWorkingDir == "" {
		sessionWorkingDir = c.cfg.WorkingDir()
	}
	ctx = context.WithValue(ctx, tools.WorkingDirContextKey, sessionWorkingDir)
	ctx = context.WithValue(ctx, tools.SessionIDContextKey, sessionID)
	ctx = context.WithValue(ctx, tools.SessionServiceContextKey, c.sessions)
	ctx = freezeInferenceScope(ctx, sess)
	ctx = toolruntime.WithService(ctx, c.toolRuntime)
	ctx = toolruntime.WithSessionID(ctx, sessionID)
	ctx = toolruntime.WithBackgroundAgentLookup(ctx, c.backgroundAgentLookup())
	ctx = toolruntime.WithBackgroundAgentMessenger(ctx, c.backgroundAgentMessenger())
	ctx = toolruntime.WithIrcAgentID(ctx, c.mainAgentID)
	if agentCfg, ok := c.cfg.Config().Agents[config.AgentCoder]; ok {
		ctx = withAgentPolicyContext(ctx, agentCfg)
	}

	if err := c.maybeAppendAutoModeReminder(ctx, sessionID, sess.PermissionMode); err != nil {
		return nil, fmt.Errorf("failed to append auto mode reminder: %w", err)
	}

	transientPrompt := goalruntime.IsSteerPrompt(prompt) || isPlanModeEnforcementPrompt(prompt)
	// Detect the guided-goal dialog prompt prefix once at entry and stash it
	// in a typed flag. Downstream code (continuation chaining, plan-mode
	// enforcement) reads the flag instead of substring-scanning the
	// user-controllable prompt text.
	guidedGoalSetup := goalruntime.IsGuidedGoalPrompt(prompt)
	var userMessage *message.Message
	if !transientPrompt {
		// Create the user message immediately with the original attachments so the
		// UI can display it without waiting for any model-side processing.
		rawParts := []message.ContentPart{message.TextContent{Text: prompt}}
		for _, attachment := range attachments {
			rawParts = append(rawParts, message.BinaryContent{Path: attachment.FilePath, MIMEType: attachment.MimeType, Data: attachment.Content})
		}
		created, err := c.messages.Create(ctx, sessionID, message.CreateMessageParams{
			Role:  message.User,
			Parts: rawParts,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create user message: %w", err)
		}
		userMessage = &created
		slog.Debug("[PERF] coordinator: created user message", "duration", time.Since(start), "session_id", sessionID)
	}

	// Start async memory prefetch immediately after user message creation.
	// This allows the memory recall to happen in parallel with other setup work.
	// Modeled after Claude Code's approach: start prefetch, cache result when
	// settled, and check readiness non-blocking at consume time.
	// Memories are merged into existing user messages (not prepended),
	// preserving prompt cache by keeping the system prompt prefix stable.
	_, surfacedBytes := collectSurfacedMemories(ctx, c.messages, sessionID)
	prefetchCtx, prefetchCancel := context.WithCancel(ctx)
	defer prefetchCancel()
	memoryPrefetch := &MemoryPrefetch{}
	go func() {
		var recall string
		if surfacedBytes < maxSessionRecallBytes {
			if c.memoryBackend != nil && c.memoryBackend.Enabled() {
				retriever := c.memoryBackend.Retriever()
				caps := c.memoryBackend.Capabilities()

				// Load mental models with a 500ms sync wait timeout for
				// backends that support them.
				if caps.MentalModels {
					tryLoadMentalModels(prefetchCtx, retriever, mentalModelTTL, 500*time.Millisecond)
				}

				// Expand all user queries with conversation context to preserve semantic continuity in turns.
				recent := buildRecentConversation(prefetchCtx, c.messages, sessionID, recallContextTurns)

				recall = buildAutoRecallBlock(prefetchCtx, retriever, strings.TrimSpace(prompt), recent, sessionID, caps)
			}
		}
		memoryPrefetch.Settle(recall)
		slog.Debug("[PERF] coordinator: memory prefetch completed", "has_recall", recall != "", "session_id", sessionID)
	}()
	slog.Debug("[PERF] coordinator: started memory prefetch", "session_id", sessionID)

	// refresh models before each run
	runtimeConfig, err := c.updateCurrentAgentRuntime(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to update models: %w", err)
	}
	slog.Debug("[PERF] coordinator: updated agent runtime", "duration", time.Since(start), "session_id", sessionID)

	model := c.currentAgent.Model()
	if runtimeConfig.Model != nil {
		model = *runtimeConfig.Model
	}
	maxTokens := runtimeConfig.MaxOutputTokens
	if maxTokens == 0 {
		maxTokens = model.CatwalkCfg.DefaultMaxTokens
	}

	ctx = context.WithValue(ctx, sessionAgentRuntimeConfigContextKey{}, runtimeConfig)

	providerCfg, ok := c.cfg.Config().Providers.Get(model.ModelCfg.Provider)
	if !ok {
		return nil, errModelProviderNotConfigured
	}

	if providerCfg.OAuthToken != nil && providerCfg.OAuthToken.IsExpired() {
		slog.Debug("Token needs to be refreshed", "provider", providerCfg.ID)
		if err := c.refreshOAuth2Token(ctx, providerCfg); err != nil {
			return nil, err
		}
	}

	// Record the goal turn baseline so token and time accounting is precise.
	turnID := uuid.New().String()
	if err := c.goalRuntime.OnTurnStart(ctx, sessionID, turnID, goalruntime.TokenUsage{}); err != nil {
		slog.Warn("Failed to record goal turn start", "error", err, "session_id", sessionID)
	}

	turnCtx, endResponsesWSTurn := c.beginResponsesWebSocketTurn(ctx, sessionID)
	defer endResponsesWSTurn()
	ctx = turnCtx

	run := func() (*fantasy.AgentResult, error) {
		slog.Debug("[PERF] coordinator: starting sessionAgent.Run", "duration", time.Since(start), "session_id", sessionID)
		call := SessionAgentCall{
			SessionID:           sessionID,
			TurnID:              turnIDFromContext(ctx),
			Prompt:              prompt,
			Attachments:         attachments,
			MaxOutputTokens:     maxTokens,
			ProviderOptions:     runtimeConfig.ProviderOptions,
			Temperature:         runtimeConfig.Temperature,
			TopP:                runtimeConfig.TopP,
			TopK:                runtimeConfig.TopK,
			FrequencyPenalty:    runtimeConfig.FrequencyPenalty,
			PresencePenalty:     runtimeConfig.PresencePenalty,
			UserMessage:         userMessage,
			MemoryPrefetch:      memoryPrefetch,
			TransientPrompt:     transientPrompt,
			NonInteractive:      transientPrompt,
			GoalBudgetExhausted: transientPrompt && sess.Goal.Status == session.GoalStatusBudgetLimited,
			GuidedGoalSetup:     guidedGoalSetup,
		}
		if transientPrompt {
			call.InitiatorType = copilot.InitiatorAgent
		}
		return c.currentAgent.Run(ctx, call)
	}
	// Call backend OnSessionCreated for first-turn initialization.
	if c.memoryBackend != nil && c.memoryBackend.Enabled() {
		c.memoryBackend.OnSessionCreated(ctx, sessionID)
	}

	result, originalErr := run()

	if c.isUnauthorized(originalErr) {
		switch {
		case providerCfg.OAuthToken != nil:
			slog.Debug("Received 401. Refreshing token and retrying", "provider", providerCfg.ID)
			if err := c.refreshOAuth2Token(ctx, providerCfg); err != nil {
				return nil, originalErr
			}
			slog.Debug("Retrying request with refreshed OAuth token", "provider", providerCfg.ID)
			result, originalErr = run()
		case strings.Contains(providerCfg.APIKeyTemplate, "$"):
			slog.Debug("Received 401. Refreshing API Key template and retrying", "provider", providerCfg.ID)
			if err := c.refreshApiKeyTemplate(ctx, providerCfg); err != nil {
				return nil, originalErr
			}
			slog.Debug("Retrying request with refreshed API key", "provider", providerCfg.ID)
			result, originalErr = run()
		}
	}

	if originalErr == nil && result != nil {
		if c.memoryBackend != nil && c.memoryBackend.Enabled() {
			// AfterTurn is the single post-turn entry point. The backend
			// implementation decides whether to run extraction (local),
			// retain transcripts (hindsight), refresh mental models, etc.
			c.memoryBackend.AfterTurn(ctx, sessionID)
		}

		if goalResult, budgetExhausted, goalErr := c.goalRuntime.PostTurn(ctx, sessionID, goalruntime.TokenUsageFromFantasy(result.TotalUsage)); goalErr != nil {
			slog.Warn("Failed to update goal runtime", "error", goalErr, "session_id", sessionID)
		} else if c.currentAgent.QueuedPrompts(sessionID) == 0 {
			depth, _ := ctx.Value(goalContinuationDepthKey{}).(int)
			if depth < maxGoalContinuationsPerRun && goalruntime.ShouldChainContinuation(prompt, depth, guidedGoalSetup) {
				continuationCtx := context.WithValue(ctx, goalContinuationDepthKey{}, depth+1)
				var continuationPrompt string
				if budgetExhausted {
					continuationPrompt = goalruntime.BuildBudgetLimitPrompt(goalResult)
				} else if goalruntime.NeedsContinuation(goalResult) {
					continuationPrompt = goalruntime.BuildContinuationPrompt(goalResult)
				}
				if continuationPrompt != "" {
					contResult, contErr := c.Run(continuationCtx, sessionID, continuationPrompt)
					if contResult != nil {
						result = contResult
					}
					originalErr = contErr
				}
			}
		}

		if originalErr == nil && !transientPrompt && c.currentAgent.QueuedPrompts(sessionID) == 0 {
			if enforceErr := c.maybeEnforcePlanModeToolDecision(ctx, sessionID, prompt, sess, guidedGoalSetup); enforceErr != nil {
				originalErr = enforceErr
			}
		}
	}

	return result, originalErr
}

// requiresAdaptiveThinking returns true for Claude models version 4.6 and above which
// require the "effort" parameter (adaptive thinking) instead of the legacy
// thinking: {type: "enabled", budget_tokens: N}.

// parseLeadingInt parses the leading integer from a string (stops at first non-digit).

func (c *coordinator) resolveBackgroundModel(ctx context.Context) *backgroundModel {
	model, providerCfg, err := c.selectedModel(ctx, config.SelectedModelTypeBackground, false)
	if err != nil {
		return nil
	}
	return &backgroundModel{
		model:    model,
		provider: providerCfg,
	}
}

func (c *coordinator) buildAgent(ctx context.Context, prompt *prompt.Prompt, agent config.Agent, isSubAgent bool) (SessionAgent, error) {
	large, small, err := c.buildAgentModels(ctx, isSubAgent)
	if err != nil {
		return nil, err
	}

	inferenceModel, inferenceProviderCfg, err := c.resolveAgentInferenceModel(ctx, agent, isSubAgent, large, small)
	if err != nil {
		return nil, err
	}

	bgModel := c.resolveBackgroundModel(ctx)

	var result SessionAgent
	result = NewSessionAgent(SessionAgentOptions{
		LargeModel:         inferenceModel,
		SmallModel:         small,
		SystemPromptPrefix: inferenceProviderCfg.SystemPromptPrefix,
		SystemPrompt:       "",
		WorkingDir:         c.cfg.WorkingDir(),
		DataDirectory:      c.cfg.Config().Options.DataDirectory,
		RefreshCallConfig: func(callCtx context.Context) (sessionAgentRuntimeConfig, error) {
			return c.refreshSessionAgentRuntimeConfig(callCtx, result, prompt, agent, isSubAgent)
		},
		DeferredToolRuntime:  c,
		IsSubAgent:           isSubAgent,
		DisableAutoSummarize: c.cfg.Config().Options.DisableAutoSummarize,
		ResponsesChaining:    c.cfg.Config().Options.ResponsesChaining,
		IsYolo:               c.permissions.SkipRequests(),
		Sessions:             c.sessions,
		Messages:             c.messages,
		BackgroundModel:      bgModel,
		ReviewToolResult: func(callCtx context.Context, sessionID string, toolResult message.ToolResult, permissionMode session.PermissionMode) (message.ToolResult, error) {
			return c.reviewToolResultForPromptInjection(callCtx, sessionID, toolResult, permissionMode)
		},
		Tools:         nil,
		Notify:        c.notify,
		HookManager:   c.hookManager,
		Filetracker:   c.filetracker,
		Checkpoint:    c.checkpoint,
		PluginRuntime: c.pluginRuntime,
		// Session working memory generation is gated by the backend's
		// SessionWorkingMemory capability rather than a backend-name string
		// comparison (see memory.Capabilities).
		EnableSessionMemory:             c.memoryBackend != nil && c.memoryBackend.Enabled() && c.memoryBackend.Capabilities().SessionWorkingMemory,
		MemoryEngineEnabled:             c.memoryBackend != nil && c.memoryBackend.Enabled(),
		MemoryEngineEventStore:          c.memoryEngineEventStore(),
		MemoryEngineHooks:               c.memoryEngineHooks(),
		MemoryEngineRetriever:           c.memoryEngineRetriever(),
		WorkingMemoryMinDiscardedTokens: c.workingMemoryMinDiscardedTokens(),
		VisionService:                   c.visionService,
		SessionEvents:                   c.sessionEvents,
	})

	// Only use async initialization for the primary agent (not subagents).
	// Subagents will have their runtime config refreshed synchronously
	// in the Run function via refreshCallConfigIfNeeded.
	if !isSubAgent {
		c.readyWg.Go(func() error {
			_, err := c.refreshSessionAgentRuntimeConfig(ctx, result, prompt, agent, isSubAgent)
			return err
		})
	}

	return result, nil
}

func (c *coordinator) resolveAgentInferenceModel(ctx context.Context, agentCfg config.Agent, isSubAgent bool, large, small Model) (Model, config.ProviderConfig, error) {
	switch agentCfg.Model {
	case "", config.SelectedModelTypeLarge:
		providerCfg, ok := c.cfg.Config().Providers.Get(large.ModelCfg.Provider)
		if !ok {
			return Model{}, config.ProviderConfig{}, errModelProviderNotConfigured
		}
		return large, providerCfg, nil
	case config.SelectedModelTypeSmall:
		providerCfg, ok := c.cfg.Config().Providers.Get(small.ModelCfg.Provider)
		if !ok {
			return Model{}, config.ProviderConfig{}, errModelProviderNotConfigured
		}
		return small, providerCfg, nil
	default:
		return c.selectedModel(ctx, agentCfg.Model, isSubAgent)
	}
}

func (c *coordinator) refreshSessionAgentRuntimeConfig(ctx context.Context, currentAgent SessionAgent, promptBuilder *prompt.Prompt, agentCfg config.Agent, isSubAgent bool) (sessionAgentRuntimeConfig, error) {
	large, small, err := c.buildAgentModels(ctx, isSubAgent)
	if err != nil {
		return sessionAgentRuntimeConfig{}, err
	}

	inferenceModel, providerCfg, err := c.resolveAgentInferenceModel(ctx, agentCfg, isSubAgent, large, small)
	if err != nil {
		return sessionAgentRuntimeConfig{}, err
	}

	mode, err := c.collaborationModeForContext(ctx)
	if err != nil {
		return sessionAgentRuntimeConfig{}, err
	}
	if mode == session.CollaborationModePlan {
		if planModel, planProviderCfg, planErr := c.selectedModel(ctx, config.SelectedModelTypePlan, isSubAgent); planErr == nil {
			inferenceModel = planModel
			providerCfg = planProviderCfg
		}
	}
	inferenceModel, providerCfg, err = c.applyInferenceOverrides(ctx, inferenceModel, providerCfg, isSubAgent)
	if err != nil {
		return sessionAgentRuntimeConfig{}, err
	}
	currentAgent.SetModels(inferenceModel, small)

	permissionMode, err := c.permissionModeForContext(ctx)
	if err != nil {
		return sessionAgentRuntimeConfig{}, err
	}

	toolBuild, err := c.buildToolsWithContext(ctx, agentCfg, mode)
	if err != nil {
		return sessionAgentRuntimeConfig{}, err
	}
	toolSet := toolBuild.Tools

	systemPrompt, err := promptBuilder.Build(ctx, inferenceModel.Model.Provider(), inferenceModel.Model.Model(), c.cfg)
	if err != nil {
		return sessionAgentRuntimeConfig{}, err
	}
	systemPrompt = buildSystemPromptForModes(systemPrompt, mode, permissionMode)
	if mode == session.CollaborationModePlan {
		if sessionID := tools.GetSessionFromContext(ctx); sessionID != "" {
			planSess, planErr := c.sessions.Get(ctx, sessionID)
			if planErr == nil {
				planSess, planErr = c.ensurePlanFileForSession(ctx, planSess)
			}
			if planErr != nil {
				return sessionAgentRuntimeConfig{}, planErr
			}
			systemPrompt += fmt.Sprintf("\n\n<active_plan_file>\n%s\n</active_plan_file>", planSess.PlanFilePath)
		}
	}

	// Append goal mode prompt if the session has an active goal.
	if goalSessionID := tools.GetSessionFromContext(ctx); goalSessionID != "" {
		if goalSess, goalErr := c.sessions.Get(ctx, goalSessionID); goalErr == nil && (goalSess.Goal.IsActive() || goalSess.Goal.Status == session.GoalStatusBudgetLimited) {
			if goalPrompt := GoalPromptForSession(goalSess.Goal); goalPrompt != "" {
				systemPrompt += "\n\n" + goalPrompt
			}
		}
	}

	if hasTool(toolSet, tools.ToolSearchToolName) {
		systemPrompt = appendDeferredToolsPromptSection(systemPrompt, toolBuild.DeferredHints)
	}

	if shouldPersistRuntimeOnAgent(ctx, isSubAgent) {
		currentAgent.SetSystemPromptPrefix(providerCfg.SystemPromptPrefix)
		currentAgent.SetSystemPrompt(systemPrompt)
		currentAgent.SetTools(toolSet)
	}

	maxTokens, clamped := effectiveMaxOutputTokens(inferenceModel)
	if clamped {
		slog.Warn("Configured max_tokens is much larger than model default, using model default", "configured", inferenceModel.ModelCfg.MaxTokens, "default", inferenceModel.CatwalkCfg.DefaultMaxTokens, "model", inferenceModel.ModelCfg.Model)
	}

	mergedOptions, temp, topP, topK, freqPenalty, presPenalty := mergeCallOptions(inferenceModel, providerCfg)
	systemPromptPrefix := providerCfg.SystemPromptPrefix

	return sessionAgentRuntimeConfig{
		Model:              &inferenceModel,
		ProviderOptions:    mergedOptions,
		MaxOutputTokens:    maxTokens,
		Temperature:        temp,
		TopP:               topP,
		TopK:               topK,
		FrequencyPenalty:   freqPenalty,
		PresencePenalty:    presPenalty,
		SystemPrompt:       &systemPrompt,
		SystemPromptPrefix: &systemPromptPrefix,
		CollaborationMode:  mode,
		PermissionMode:     permissionMode,
		Tools:              append([]fantasy.AgentTool(nil), toolSet...),
		// The soft/hard request-step budget is only enforced on subagents.
		// The main agent runs unbounded so interactive sessions are never
		// force-aborted. Values come from EffectiveSubagentRuntime(); the
		// hard cap is ceil(soft * multiplier).
		RequestStepBudget: requestStepBudgetFor(isSubAgent, c.cfg.Config().EffectiveSubagentRuntime()),
		HardRequestBudget: hardRequestBudgetFor(isSubAgent, c.cfg.Config().EffectiveSubagentRuntime()),
		MaxRuntimeMs:      maxRuntimeMsFor(isSubAgent, c.cfg.Config().EffectiveSubagentRuntime()),
	}, nil
}

// requestStepBudgetFor returns the soft step budget for the run, or 0 if the
// budget is disabled (main agent or user-configured disable).
func requestStepBudgetFor(isSubAgent bool, rc config.SubagentRuntimeConfig) int {
	if !isSubAgent {
		return 0
	}
	return rc.SoftRequestBudget
}

// hardRequestBudgetFor returns the hard step ceiling for the run, or 0 if the
// budget is disabled. Computed as ceil(soft * multiplier).
func hardRequestBudgetFor(isSubAgent bool, rc config.SubagentRuntimeConfig) int {
	if !isSubAgent {
		return 0
	}
	if rc.SoftRequestBudget <= 0 {
		return 0
	}
	mult := rc.HardRequestBudgetMultiplier
	if mult < 1.0 {
		mult = 1.0
	}
	hard := int(math.Ceil(float64(rc.SoftRequestBudget) * mult))
	if hard <= rc.SoftRequestBudget {
		hard = rc.SoftRequestBudget + 1
	}
	return hard
}

// maxRuntimeMsFor returns the wall-clock cap for the run, or 0 if disabled.
// Only enforced on subagents — the main interactive agent runs unbounded.
func maxRuntimeMsFor(isSubAgent bool, rc config.SubagentRuntimeConfig) int {
	if !isSubAgent {
		return 0
	}
	return rc.MaxRuntimeMs
}

func shouldPersistRuntimeOnAgent(ctx context.Context, isSubAgent bool) bool {
	if isSubAgent {
		return true
	}
	return tools.GetSessionFromContext(ctx) == ""
}

func (c *coordinator) collaborationModeForContext(ctx context.Context) (session.CollaborationMode, error) {
	sessionID := tools.GetSessionFromContext(ctx)
	if sessionID == "" {
		return session.CollaborationModeDefault, nil
	}

	sess, err := c.sessions.Get(ctx, sessionID)
	if err != nil {
		return session.CollaborationModeDefault, fmt.Errorf("failed to get session collaboration mode: %w", err)
	}
	return sess.CollaborationMode, nil
}

func (c *coordinator) permissionModeForContext(ctx context.Context) (session.PermissionMode, error) {
	sessionID := tools.GetSessionFromContext(ctx)
	if sessionID == "" {
		return session.PermissionModeDefault, nil
	}

	sess, err := c.sessions.Get(ctx, sessionID)
	if err != nil {
		return session.PermissionModeDefault, fmt.Errorf("failed to get session permission mode: %w", err)
	}
	return sess.PermissionMode, nil
}

func (c *coordinator) buildTools(ctx context.Context, agent config.Agent, mode session.CollaborationMode) ([]fantasy.AgentTool, error) {
	build, err := c.buildToolsWithContext(ctx, agent, mode)
	if err != nil {
		return nil, err
	}
	return build.Tools, nil
}

type buildToolsResult struct {
	Tools               []fantasy.AgentTool
	DeferredHints       []tools.RegistryEntry
	RegisteredToolNames []string
}

func (c *coordinator) buildToolsWithContext(ctx context.Context, agent config.Agent, mode session.CollaborationMode) (buildToolsResult, error) {
	registry := newToolRegistry()
	registeredTools, err := c.registerAgentTools(ctx, agent, mode, registry)
	if err != nil {
		return buildToolsResult{}, err
	}

	allowedToolNames := filterToolsForRiskPolicy(agent.AllowedTools, mode, c.cfg.Config().Options.DisabledTools)
	if runtime, ok := subagentRuntimeFromContext(ctx); ok {
		allowedToolNames = unionToolNames(allowedToolNames, toolNamesFromSet(runtime.ToolProfile.Allowed))
	}
	allowedSet := make(map[string]struct{}, len(allowedToolNames))
	for _, name := range allowedToolNames {
		allowedSet[name] = struct{}{}
	}

	registeredToolNames := make([]string, 0, len(registeredTools))
	for _, registered := range registeredTools {
		registeredToolNames = append(registeredToolNames, registered.tool.Info().Name)
	}

	activatedDeferred := c.activatedDeferredTools(ctx)
	filteredTools := make([]fantasy.AgentTool, 0, len(registeredTools))
	exposedByName := make(map[string]bool, len(registeredTools))
	disabledSet := make(map[string]struct{}, len(c.cfg.Config().Options.DisabledTools))
	for _, disabled := range c.cfg.Config().Options.DisabledTools {
		disabledSet[disabled] = struct{}{}
	}
	for _, registered := range registeredTools {
		name := registered.tool.Info().Name
		_, allowed := allowedSet[name]
		if !allowed {
			if mode == session.CollaborationModePlan || !registered.metadata.IsDeferred() {
				continue
			}
			if _, activated := activatedDeferred[name]; !activated {
				continue
			}
			if _, disabled := disabledSet[name]; disabled {
				continue
			}
		}
		if mode != session.CollaborationModePlan && registered.metadata.IsDeferred() {
			if _, activated := activatedDeferred[name]; !activated {
				continue
			}
		}
		filteredTools = append(filteredTools, registered.tool)
		exposedByName[name] = true
	}

	for _, entry := range registry.entries {
		entry.Exposed = exposedByName[entry.Name]
		registry.entries[entry.Name] = entry
	}
	deferredHints := collectDeferredToolHints(registry.entries, disabledSet)

	c.deferredMu.Lock()
	c.knownDeferredToolNames = make(map[string]bool, len(deferredHints))
	for _, hint := range deferredHints {
		c.knownDeferredToolNames[hint.Name] = true
	}
	c.deferredMu.Unlock()

	if runtime, ok := subagentRuntimeFromContext(ctx); ok {
		filteredTools = ShapeToolsForSubagent(filteredTools, runtime.ToolProfile)
		deferredHints = shapeDeferredHintsForSubagent(deferredHints, runtime.ToolProfile)
	}

	if mode == session.CollaborationModePlan {
		filteredTools = removeNonPlanSafeCustomTools(filteredTools, registry)
		slices.SortFunc(filteredTools, func(a, b fantasy.AgentTool) int {
			return strings.Compare(a.Info().Name, b.Info().Name)
		})
		return buildToolsResult{Tools: filteredTools, RegisteredToolNames: registeredToolNames}, nil
	}

	for i, tool := range filteredTools {
		filteredTools[i] = plugin.WrapAgentToolWithRuntime(c.plugins(), tool)
	}

	slices.SortFunc(filteredTools, func(a, b fantasy.AgentTool) int {
		return strings.Compare(a.Info().Name, b.Info().Name)
	})
	return buildToolsResult{Tools: filteredTools, DeferredHints: deferredHints, RegisteredToolNames: registeredToolNames}, nil
}

func removeNonPlanSafeCustomTools(toolsList []fantasy.AgentTool, registry *toolRegistry) []fantasy.AgentTool {
	if len(toolsList) == 0 || registry == nil {
		return toolsList
	}
	filtered := make([]fantasy.AgentTool, 0, len(toolsList))
	for _, tool := range toolsList {
		entry, ok := registry.Resolve(tool.Info().Name)
		if !ok || !strings.HasPrefix(entry.Source, "plugin") {
			filtered = append(filtered, tool)
			continue
		}
		if entry.Metadata.ReadOnly {
			filtered = append(filtered, tool)
		}
	}
	return filtered
}

func (c *coordinator) buildAgentModels(ctx context.Context, isSubAgent bool) (Model, Model, error) {
	largeModelCfg, ok := c.cfg.Config().SelectedModelForType(config.SelectedModelTypeLarge)
	if !ok {
		return Model{}, Model{}, errLargeModelNotSelected
	}
	smallModelCfg, ok := c.cfg.Config().SelectedModelForType(config.SelectedModelTypeSmall)
	if !ok {
		return Model{}, Model{}, errSmallModelNotSelected
	}

	largeProviderCfg, ok := c.cfg.Config().Providers.Get(largeModelCfg.Provider)
	if !ok {
		return Model{}, Model{}, errLargeModelProviderNotConfigured
	}

	smallProviderCfg, ok := c.cfg.Config().Providers.Get(smallModelCfg.Provider)
	if !ok {
		return Model{}, Model{}, errSmallModelProviderNotConfigured
	}

	var largeCatwalkModel catwalk.Model
	var largeFound bool
	for i := range largeProviderCfg.Models {
		if largeProviderCfg.Models[i].ID == largeModelCfg.Model {
			largeCatwalkModel = largeProviderCfg.Models[i].Model
			largeFound = true
			break
		}
	}
	var smallCatwalkModel catwalk.Model
	var smallFound bool
	for i := range smallProviderCfg.Models {
		if smallProviderCfg.Models[i].ID == smallModelCfg.Model {
			smallCatwalkModel = smallProviderCfg.Models[i].Model
			smallFound = true
			break
		}
	}

	if !largeFound {
		return Model{}, Model{}, errLargeModelNotFound
	}

	if largeModelCfg.ContextWindow > 0 {
		largeCatwalkModel.ContextWindow = largeModelCfg.ContextWindow
	}
	if largeModelCfg.MaxPromptTokens > 0 {
		largeCatwalkModel.Options.ProviderOptions = maps.Clone(largeCatwalkModel.Options.ProviderOptions)
		if largeCatwalkModel.Options.ProviderOptions == nil {
			largeCatwalkModel.Options.ProviderOptions = map[string]any{}
		}
		largeCatwalkModel.Options.ProviderOptions["max_prompt_tokens"] = largeModelCfg.MaxPromptTokens
	}

	if !smallFound {
		return Model{}, Model{}, errSmallModelNotFound
	}

	if smallModelCfg.ContextWindow > 0 {
		smallCatwalkModel.ContextWindow = smallModelCfg.ContextWindow
	}
	if smallModelCfg.MaxPromptTokens > 0 {
		smallCatwalkModel.Options.ProviderOptions = maps.Clone(smallCatwalkModel.Options.ProviderOptions)
		if smallCatwalkModel.Options.ProviderOptions == nil {
			smallCatwalkModel.Options.ProviderOptions = map[string]any{}
		}
		smallCatwalkModel.Options.ProviderOptions["max_prompt_tokens"] = smallModelCfg.MaxPromptTokens
	}

	largeThinkingDisabled := largeModelCfg.Think != nil && !*largeModelCfg.Think
	largeProvider, err := c.buildProvider(largeProviderCfg, largeCatwalkModel, isSubAgent, largeThinkingDisabled)
	if err != nil {
		return Model{}, Model{}, err
	}

	smallThinkingDisabled := smallModelCfg.Think != nil && !*smallModelCfg.Think
	smallProvider, err := c.buildProvider(smallProviderCfg, smallCatwalkModel, true, smallThinkingDisabled)
	if err != nil {
		return Model{}, Model{}, err
	}

	largeModelID := largeModelCfg.Model
	smallModelID := smallModelCfg.Model

	if largeModelCfg.Provider == openrouter.Name && isExactoSupported(largeModelID) {
		largeModelID += ":exacto"
	}

	if smallModelCfg.Provider == openrouter.Name && isExactoSupported(smallModelID) {
		smallModelID += ":exacto"
	}

	largeModel, err := largeProvider.LanguageModel(ctx, largeModelID)
	if err != nil {
		return Model{}, Model{}, err
	}
	smallModel, err := smallProvider.LanguageModel(ctx, smallModelID)
	if err != nil {
		return Model{}, Model{}, err
	}

	return Model{
			Model:      largeModel,
			CatwalkCfg: largeCatwalkModel,
			ModelCfg:   largeModelCfg,
		}, Model{
			Model:      smallModel,
			CatwalkCfg: smallCatwalkModel,
			ModelCfg:   smallModelCfg,
		}, nil
}

func (c *coordinator) Cancel(sessionID string) {
	c.currentAgent.Cancel(sessionID)
	for _, subAgent := range c.activeSubAgentsForSession(sessionID) {
		subAgent.CancelAll()
	}
}

func (c *coordinator) trackActiveSubAgent(parentSessionID string, subAgent SessionAgent) func() {
	if c == nil || strings.TrimSpace(parentSessionID) == "" || subAgent == nil {
		return func() {}
	}
	c.activeSubAgentsMu.Lock()
	if c.activeSubAgents == nil {
		c.activeSubAgents = make(map[string]map[SessionAgent]struct{})
	}
	if c.activeSubAgents[parentSessionID] == nil {
		c.activeSubAgents[parentSessionID] = make(map[SessionAgent]struct{})
	}
	c.activeSubAgents[parentSessionID][subAgent] = struct{}{}
	c.activeSubAgentsMu.Unlock()

	return func() {
		c.activeSubAgentsMu.Lock()
		defer c.activeSubAgentsMu.Unlock()
		delete(c.activeSubAgents[parentSessionID], subAgent)
		if len(c.activeSubAgents[parentSessionID]) == 0 {
			delete(c.activeSubAgents, parentSessionID)
		}
	}
}

func (c *coordinator) activeSubAgentsForSession(parentSessionID string) []SessionAgent {
	if c == nil || strings.TrimSpace(parentSessionID) == "" {
		return nil
	}
	c.activeSubAgentsMu.Lock()
	defer c.activeSubAgentsMu.Unlock()
	agents := c.activeSubAgents[parentSessionID]
	if len(agents) == 0 {
		return nil
	}
	result := make([]SessionAgent, 0, len(agents))
	for subAgent := range agents {
		result = append(result, subAgent)
	}
	return result
}

func (c *coordinator) CancelAll() {
	c.currentAgent.CancelAll()
	c.activeSubAgentsMu.Lock()
	defer c.activeSubAgentsMu.Unlock()
	for _, subAgents := range c.activeSubAgents {
		for subAgent := range subAgents {
			subAgent.CancelAll()
		}
	}
}

func (c *coordinator) IsBusy() bool {
	if c.currentAgent.IsBusy() {
		return true
	}
	c.activeSubAgentsMu.Lock()
	defer c.activeSubAgentsMu.Unlock()
	for _, subAgents := range c.activeSubAgents {
		if len(subAgents) > 0 {
			return true
		}
	}
	return false
}

func (c *coordinator) IsSessionBusy(sessionID string) bool {
	if c.currentAgent.IsSessionBusy(sessionID) {
		return true
	}
	return len(c.activeSubAgentsForSession(sessionID)) > 0
}

func (c *coordinator) Model() Model {
	return c.currentAgent.Model()
}

func (c *coordinator) ModelForSession(sessionID string) (Model, bool) {
	if v, ok := c.childSessionAgents.Load(sessionID); ok {
		if sa, ok := v.(SessionAgent); ok {
			return sa.Model(), true
		}
	}
	return Model{}, false
}

func filterAttachmentsForModelSupport(attachments []message.Attachment, supportsImages bool) []message.Attachment {
	if supportsImages || attachments == nil {
		return attachments
	}
	filtered := make([]message.Attachment, 0, len(attachments))
	for _, att := range attachments {
		if att.IsText() {
			filtered = append(filtered, att)
		}
	}
	return filtered
}

func (c *coordinator) resolveCoderModelSupportsImages() (bool, error) {
	agentCfg, ok := c.cfg.Config().Agents[config.AgentCoder]
	if !ok {
		return false, errCoderAgentNotConfigured
	}
	modelCfg, ok := c.cfg.Config().Models[agentCfg.Model]
	if !ok {
		return false, fmt.Errorf("selected model %q not configured", agentCfg.Model)
	}
	catwalkModel := c.cfg.Config().GetModel(modelCfg.Provider, modelCfg.Model)
	if catwalkModel == nil {
		return false, fmt.Errorf("model %q not found", modelCfg.Model)
	}
	return catwalkModel.SupportsImages, nil
}

// missingFinishPolicyAllowsJSONFallback reports whether a subagent that never
// called yield may still complete via JSON schema fallback extraction.
func missingFinishPolicyAllowsJSONFallback(policy MissingFinishPolicy) bool {
	switch policy {
	case MissingFinishWarn, MissingFinishRetryThenWarn:
		return true
	default:
		return false
	}
}

func (c *coordinator) EscalationBridge() *permission.EscalationBridge {
	return c.escalationBridge
}

func (c *coordinator) UpdateModels(ctx context.Context) error {
	_, err := c.updateCurrentAgentRuntime(ctx)
	return err
}

func (c *coordinator) updateCurrentAgentRuntime(ctx context.Context) (sessionAgentRuntimeConfig, error) {
	agentCfg, ok := c.cfg.Config().Agents[config.AgentCoder]
	if !ok {
		return sessionAgentRuntimeConfig{}, errCoderAgentNotConfigured
	}

	// Use session-specific working directory from context if available,
	// otherwise fall back to global working directory.
	workingDir := cmp.Or(tools.GetWorkingDirFromContext(ctx), c.cfg.WorkingDir())
	frozenGitStatus := c.getOrFreezeGitStatus(ctx, workingDir)
	promptBuilder, err := promptForAgent(agentCfg, false, prompt.WithWorkingDir(workingDir), prompt.WithGitStatus(frozenGitStatus))
	if err != nil {
		return sessionAgentRuntimeConfig{}, err
	}

	return c.refreshSessionAgentRuntimeConfig(ctx, c.currentAgent, promptBuilder, agentCfg, false)
}

// getOrFreezeGitStatus returns a cached git status for the working directory,
// computing it once on first access and reusing the frozen value for all
// subsequent calls. This keeps the system prompt prefix stable across turns
// so that prompt caching (xAI prompt_cache_key, Anthropic cache_control, etc.)
// can hit on the unchanged prefix instead of being invalidated by git status
// changes after every file edit.
func (c *coordinator) getOrFreezeGitStatus(ctx context.Context, workingDir string) string {
	c.gitStatusCacheMu.Lock()
	defer c.gitStatusCacheMu.Unlock()
	if c.gitStatusCache == nil {
		c.gitStatusCache = make(map[string]string)
	}
	if cached, ok := c.gitStatusCache[workingDir]; ok {
		return cached
	}
	status, err := prompt.GetGitStatus(ctx, workingDir)
	if err != nil {
		slog.Debug("Failed to compute git status for freezing", "error", err, "working_dir", workingDir)
		c.gitStatusCache[workingDir] = ""
		return ""
	}
	c.gitStatusCache[workingDir] = status
	return status
}

func (c *coordinator) RefreshTools(ctx context.Context) error {
	// Invalidate cached skill discovery so newly added or removed skills are
	// picked up when tools are rebuilt.
	skills.Invalidate(nil)
	agentCfg, ok := c.cfg.Config().Agents[config.AgentCoder]
	if !ok {
		return errors.New("coder agent not configured")
	}

	tools, err := c.buildTools(ctx, agentCfg, session.CollaborationModeDefault)
	if err != nil {
		return err
	}
	c.currentAgent.SetTools(tools)
	slog.Debug("Refreshed agent tools", "count", len(tools))
	return nil
}

func (c *coordinator) activateDeferredTools(ctx context.Context, toolNames []string) []string {
	sessionID := tools.GetSessionFromContext(ctx)
	if sessionID == "" {
		return nil
	}
	// When called with nil/empty, return the list of already-activated tools.
	// This allows tool_search to exclude already-activated tools from results.
	if len(toolNames) == 0 {
		activated := c.activatedDeferredToolsForSession(sessionID)
		if len(activated) == 0 {
			return nil
		}
		names := make([]string, 0, len(activated))
		for name := range activated {
			names = append(names, name)
		}
		slices.Sort(names)
		return names
	}
	return c.activateDeferredToolsForSession(sessionID, toolNames)
}

func (c *coordinator) activateDeferredToolsForSession(sessionID string, toolNames []string) []string {
	if sessionID == "" || len(toolNames) == 0 {
		return nil
	}

	c.deferredMu.Lock()
	defer c.deferredMu.Unlock()

	set, ok := c.activatedDeferredBySession[sessionID]
	if !ok {
		set = make(map[string]struct{})
		c.activatedDeferredBySession[sessionID] = set
	}

	activated := make([]string, 0, len(toolNames))
	for _, name := range toolNames {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			continue
		}
		if _, exists := set[trimmed]; exists {
			activated = append(activated, trimmed)
			continue
		}
		set[trimmed] = struct{}{}
		activated = append(activated, trimmed)
	}
	return activated
}

func (c *coordinator) activatedDeferredTools(ctx context.Context) map[string]struct{} {
	sessionID := tools.GetSessionFromContext(ctx)
	if sessionID == "" {
		return nil
	}
	return c.activatedDeferredToolsForSession(sessionID)
}

func (c *coordinator) activatedDeferredToolsForSession(sessionID string) map[string]struct{} {
	if sessionID == "" {
		return nil
	}

	c.deferredMu.Lock()
	defer c.deferredMu.Unlock()

	set, ok := c.activatedDeferredBySession[sessionID]
	if !ok || len(set) == 0 {
		return nil
	}
	clone := make(map[string]struct{}, len(set))
	for name := range set {
		clone[name] = struct{}{}
	}
	return clone
}

func (c *coordinator) isDeferredTool(name string) bool {
	if name == "" {
		return false
	}
	c.deferredMu.Lock()
	defer c.deferredMu.Unlock()
	return c.knownDeferredToolNames[strings.TrimSpace(name)]
}

func (c *coordinator) clearDeferredToolActivationsForSession(sessionID string) {
	if sessionID == "" {
		return
	}

	c.deferredMu.Lock()
	delete(c.activatedDeferredBySession, sessionID)
	c.deferredMu.Unlock()
}

func (c *coordinator) Summarize(ctx context.Context, sessionID string, opts fantasy.ProviderOptions) error {
	if opts == nil {
		providerCfg, ok := c.cfg.Config().Providers.Get(c.currentAgent.Model().ModelCfg.Provider)
		if !ok {
			return errModelProviderNotConfigured
		}
		opts = getProviderOptions(c.currentAgent.Model(), providerCfg)
	}
	return c.currentAgent.Summarize(ctx, sessionID, opts)
}

func (c *coordinator) PrepareModelSwitch(ctx context.Context, sessionID string, modelType config.SelectedModelType, selectedModel config.SelectedModel) error {
	if sessionID == "" || c.currentAgent == nil {
		return nil
	}

	agentCfg, ok := c.cfg.Config().Agents[config.AgentCoder]
	if !ok || agentCfg.Model != modelType {
		return nil
	}

	targetCatwalkModel, err := c.lookupCatwalkModel(selectedModel)
	if err != nil {
		return err
	}

	targetContextWindow := int64(targetCatwalkModel.ContextWindow)
	if targetContextWindow <= 0 {
		return nil
	}

	currentContextWindow := int64(c.currentAgent.Model().CatwalkCfg.ContextWindow)
	if currentContextWindow > 0 && targetContextWindow >= currentContextWindow {
		return nil
	}

	targetModel := Model{
		CatwalkCfg: targetCatwalkModel,
		ModelCfg:   selectedModel,
	}
	targetMaxOutputTokens, _ := effectiveMaxOutputTokens(targetModel)

	lastEstimate := int64(-1)
	for attempt := 0; attempt <= maxModelSwitchSummaries; attempt++ {
		estimatedInput, err := c.currentAgent.EstimateSessionPromptTokensForModel(ctx, sessionID, targetModel)
		if err != nil {
			return fmt.Errorf("failed to estimate session size for target model: %w", err)
		}
		if !shouldAutoSummarize(targetModel, estimatedInput, targetMaxOutputTokens) {
			return nil
		}
		if attempt == maxModelSwitchSummaries {
			return fmt.Errorf("session is too large to switch to model %q safely; summarize with the current model first or start a new session", selectedModel.Model)
		}
		if lastEstimate >= 0 && estimatedInput >= lastEstimate {
			return fmt.Errorf("session is still too large to switch to model %q after summarization", selectedModel.Model)
		}
		lastEstimate = estimatedInput
		if err := c.Summarize(ctx, sessionID, nil); err != nil {
			return fmt.Errorf("failed to summarize session before model switch: %w", err)
		}
	}

	return nil
}

func (c *coordinator) lookupCatwalkModel(selectedModel config.SelectedModel) (catwalk.Model, error) {
	providerCfg, ok := c.cfg.Config().Providers.Get(selectedModel.Provider)
	if !ok {
		return catwalk.Model{}, errModelProviderNotConfigured
	}

	for _, candidate := range providerCfg.Models {
		if candidate.ID == selectedModel.Model {
			return candidate.Model, nil
		}
	}

	// Fallback: search all providers for matching model metadata. The user
	// may have configured a model under a provider that supports it via its
	// endpoint but doesn't explicitly list it in the config.
	if found, _, ok := c.cfg.Config().FindModelInAnyProvider(selectedModel.Model); ok {
		return found, nil
	}

	return catwalk.Model{}, errTargetModelNotFound
}

func (c *coordinator) isUnauthorized(err error) bool {
	var providerErr *fantasy.ProviderError
	return errors.As(err, &providerErr) && providerErr.StatusCode == http.StatusUnauthorized
}

func (c *coordinator) refreshOAuth2Token(ctx context.Context, providerCfg config.ProviderConfig) error {
	if err := c.cfg.RefreshOAuthToken(ctx, config.ScopeGlobal, providerCfg.ID); err != nil {
		slog.Error("Failed to refresh OAuth token after 401 error", "provider", providerCfg.ID, "error", err)
		return err
	}
	if err := c.UpdateModels(ctx); err != nil {
		return err
	}
	return nil
}

func (c *coordinator) refreshApiKeyTemplate(ctx context.Context, providerCfg config.ProviderConfig) error {
	newAPIKey, err := c.cfg.Resolve(providerCfg.APIKeyTemplate)
	if err != nil {
		slog.Error("Failed to re-resolve API key after 401 error", "provider", providerCfg.ID, "error", err)
		return err
	}

	providerCfg.APIKey = newAPIKey
	c.cfg.Config().Providers.Set(providerCfg.ID, providerCfg)

	if err := c.UpdateModels(ctx); err != nil {
		return err
	}
	return nil
}

// subAgentParams holds the parameters for running a sub-agent.
type subAgentParams struct {
	Agent                     SessionAgent
	SessionID                 string
	ExistingSessionID         string
	AgentMessageID            string
	ParentMessageID           string
	ToolCallID                string
	Prompt                    string
	SessionTitle              string
	SubagentType              string
	DelegationMailbox         string
	AgentMemory               string
	AgentIsolation            string
	AgentBackground           *bool
	SkipHandoffReview         bool
	SkipStructuredFinishCheck bool
	IrcAgentID                string
	Role                      string
	// AgentID is the registry ID (e.g. "0-Main::explore") for this subagent.
	// Used by the lifecycle manager to adopt/park the registry entry on
	// successful completion. Empty when the subagent was not registered.
	AgentID string
	// PrecomputedContext, when non-empty, is used as the parent context
	// prefix instead of calling buildSubagentHandoffSummary. This allows
	// the batch path to compute context once and share it across tasks.
	PrecomputedContext string
	// SessionSetup is an optional callback invoked after session creation
	// but before agent execution, for custom session configuration.
	SessionSetup func(sessionID string)
	OnProgress   func(toolUses int, lastTool string)
}

type subagentTask struct {
	Name         string
	Description  string
	Assignment   string
	SubagentType string
	Role         string
	Isolation    string
}

type subagentBatchParams struct {
	SessionID       string
	AgentMessageID  string
	ToolCallID      string
	Tasks           []subagentTask
	Context         string
	RunInBackground bool
}

// computeBatchIsolationDefault returns "worktree" when the batch contains
// 2+ tasks whose resolved agent profile allows writes (i.e. not read-only
// subagents such as explore/plan/review/librarian), to prevent concurrent
// writers from racing on the shared workspace. Returns "" when batch-level
// defaulting does not apply (single task, or fewer than two writer tasks),
// leaving isolation to per-task overrides, the agent's static config, or
// the global DefaultIsolation.
func computeBatchIsolationDefault(tasks []subagentTask, agents map[string]config.Agent) string {
	if len(tasks) < 2 {
		return ""
	}
	writerCount := 0
	for _, t := range tasks {
		agentID := config.ResolveSubagentID(agents, t.SubagentType)
		agent := agents[agentID]
		if !isReadOnlyRuntime(agent) {
			writerCount++
		}
	}
	if writerCount >= 2 {
		return "worktree"
	}
	return ""
}

// resolveTaskIsolation picks the effective isolation for a single task in
// priority order:
//  1. task.Isolation, unless it is "none" (explicit opt-out → "" falls
//     through to global defaults).
//  2. batchDefaultIsolation (from computeBatchIsolationDefault).
//  3. agentCfgIsolation (the agent's static registration value).
//
// The empty string falls through to prepareSubagentWorkspace's defaults
// (global DefaultIsolation config, then "session").
func resolveTaskIsolation(taskIsolation, batchDefaultIsolation, agentCfgIsolation string) string {
	task := strings.TrimSpace(strings.ToLower(taskIsolation))
	if task == "none" {
		return ""
	}
	if task != "" {
		return task
	}
	if batch := strings.TrimSpace(batchDefaultIsolation); batch != "" {
		return batch
	}
	return strings.TrimSpace(agentCfgIsolation)
}

type subagentResult struct {
	Task           subagentTask
	TaskRef        string
	Status         message.ToolResultSubtaskStatus
	AgentID        string
	ChildSessionID string
	Content        string
	Preview        string
	HasFullOutput  bool
	OutputChars    int
	Yield          message.ToolResultYield
	Warnings       []string
	Error          string
	Attempts       int
	Artifacts      []string
	FilesTouched   []string
	PatchPlan      []string
	TestResults    []string
	Followups      []string
}

type subAgentFactory func(context.Context, string) (SessionAgent, config.Agent, error)

func (p subAgentParams) SubagentTypeOrDefault() string {
	if strings.TrimSpace(p.SubagentType) != "" {
		return strings.TrimSpace(p.SubagentType)
	}
	return config.AgentGeneral
}

func (c *coordinator) runSubagents(ctx context.Context, params subagentBatchParams) (fantasy.ToolResponse, error) {
	if params.RunInBackground {
		return c.runBackgroundTask(ctx, params)
	}

	if len(params.Tasks) == 0 {
		return fantasy.NewTextErrorResponse("tasks is required"), nil
	}

	ctx = toolruntime.WithToolCallID(ctx, params.ToolCallID)

	taskRefs := make(map[string]string, len(params.Tasks))
	for i, task := range params.Tasks {
		taskRefs[task.Name] = SubagentTaskRef(i, task.Name, params.ToolCallID)
	}

	bridge, err := newSubagentBridge(c.mailbox, c.sessions, params.SessionID, params.ToolCallID, params.Tasks)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	defer bridge.Close()

	// preparedTask collects everything needed to run a single subagent task.
	// If Skip is non-empty status, the task is recorded as a result without
	// being submitted to the executor.
	type preparedTask struct {
		Task         subagentTask
		TaskRef      string
		SubagentType string
		AgentID      string
		Params       subAgentParams
		SkipResult   *subagentResult
	}

	orderedResults := make([]subagentResult, len(params.Tasks))
	prepared := make([]preparedTask, len(params.Tasks))

	// Compute batch-level isolation default. When a batch contains 2+
	// writer tasks (agent profiles that allow writes — i.e. not read-only
	// subagents like explore/plan/review/librarian), default those tasks
	// to worktree isolation so concurrent writers do not race on the
	// shared workspace. Per-task Isolation overrides take precedence:
	// "worktree"/"session" opts in, "none" explicitly opts out. This
	// targets the concretely reachable race (parallel general/designer
	// tasks in one agent call) without forcing single-task invocations
	// to pay worktree overhead.
	batchDefaultIsolation := computeBatchIsolationDefault(params.Tasks, c.cfg.Config().Agents)

	baseContext := strings.TrimSpace(params.Context)

	// Compute parent context once for the entire batch, rather than
	// re-computing it inside each runSubAgentDirect call.
	parentContext := c.buildSubagentHandoffSummary(ctx, params.SessionID)

	// Export a condensed history index file for deep context inspection.
	historyFile := c.writeParentHistoryFile(ctx, params.SessionID)
	defer c.cleanupParentHistoryFile(historyFile)
	if historyFile != "" {
		parentContext += fmt.Sprintf(
			"\n<parent_history_file>\nFull conversation index: %s\nRead this file if you need context beyond what is provided above.\n</parent_history_file>\n",
			historyFile,
		)
	}

	// Assemble a unified context prefix combining auto-extracted parent
	// context and user-provided shared context. This is shared across all
	// tasks in the batch via PrecomputedContext.
	batchContextPrefix := assembleSubagentPrompt(parentContext, baseContext, "")

	// Deduplicate task names within this batch so that two tasks sharing the
	// same name do not collide in the AgentRegistry or as IRC peer IDs.
	idAllocator := newSubagentIDAllocator()

	// Prepare each task synchronously: build the subagent, register it,
	// drain mailbox messages, and assemble the runtime parameters.
	for i, t := range params.Tasks {
		prepared[i].Task = t
		prepared[i].TaskRef = taskRefs[t.Name]

		subAgent, agentCfg, buildErr := c.buildSubAgentForType(ctx, t.SubagentType, t.Role)
		if buildErr != nil {
			prepared[i].SkipResult = &subagentResult{
				Task:    t,
				TaskRef: taskRefs[t.Name],
				Status:  message.ToolResultSubtaskStatusFailed,
				Content: strings.TrimSpace(buildErr.Error()),
			}
			continue
		}

		subagentType := config.ResolveSubagentID(c.cfg.Config().Agents, agentCfg.ID)
		description := strings.TrimSpace(t.Description)
		if description == "" {
			description = defaultSubagentDescription(subagentType, t.Assignment)
		}

		// Deduplicate the display name within this batch (so two tasks
		// named "explore" show up as "explore" and "explore-2"), then append
		// a short random suffix to the registry ID so concurrent batches
		// cannot collide on the same key. Without the suffix, a follow-up
		// batch reusing the same task name would register the same agentID,
		// and the prior batch's lifecycle Park() could Unregister the new
		// entry out from under it.
		uniqueName := idAllocator.Alloc(t.Name)
		agentID := fmt.Sprintf("%s::%s-%s", c.mainAgentID, uniqueName, generateAgentID())
		c.agentRegistry.Register(AgentRef{
			ID:          agentID,
			DisplayName: description,
			Kind:        AgentKindSub,
			ParentID:    c.mainAgentID,
			Status:      AgentStatusRunning,
			Agent:       subAgent,
			SessionID:   params.SessionID,
		})
		bridge.MarkInProgress(t.Name)

		prepared[i].SubagentType = subagentType
		prepared[i].AgentID = agentID

		prompt := strings.TrimSpace(t.Assignment)

		if effects, consumeErr := bridge.Consume(t.Name); consumeErr == nil {
			if len(effects.Messages) > 0 {
				prompt = promptWithMailboxMessages(prompt, effects.Messages)
			}
			if effects.Stop {
				prepared[i].SkipResult = &subagentResult{
					Task:    t,
					TaskRef: taskRefs[t.Name],
					Status:  message.ToolResultSubtaskStatusCanceled,
					Content: effects.Reason,
				}
				continue
			}
		}

		if ircRoster := renderIrcPeerRoster(c.agentRegistry, agentID); ircRoster != "" {
			prompt = prompt + "\n\n" + ircRoster
		}

		taskToolCallID := fmt.Sprintf("%s::%s", params.ToolCallID, t.Name)
		prepared[i].Params = subAgentParams{
			Agent:              subAgent,
			SessionID:          params.SessionID,
			AgentMessageID:     params.AgentMessageID,
			ParentMessageID:    params.AgentMessageID,
			ToolCallID:         taskToolCallID,
			Prompt:             prompt,
			SessionTitle:       formatSubagentSessionTitle(description, subagentType),
			SubagentType:       subagentType,
			Role:               t.Role,
			DelegationMailbox:  params.ToolCallID,
			AgentMemory:        agentCfg.Memory,
			AgentIsolation:     resolveTaskIsolation(t.Isolation, batchDefaultIsolation, agentCfg.Isolation),
			AgentBackground:    agentCfg.Background,
			SkipHandoffReview:  true,
			IrcAgentID:         agentID,
			AgentID:            agentID,
			PrecomputedContext: batchContextPrefix,
		}
	}

	// Build the executor and run all prepared tasks in parallel.
	runtimeCfg := c.cfg.Config().EffectiveSubagentRuntime()
	runner := func(ctx context.Context, p subAgentParams) (resp fantasy.ToolResponse, err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error("Subagent task panicked", "tool_call_id", p.ToolCallID, "panic", recovered)
				err = fmt.Errorf("subagent panicked: %v", recovered)
			}
		}()
		if c.hookManager != nil {
			c.hookManager.RunSubagentStart(ctx, p.ToolCallID, p.SubagentType, p.SessionID)
			defer c.hookManager.RunSubagentStop(context.Background(), p.ToolCallID, p.SubagentType, p.SessionID)
		}
		return c.runSubAgentDirect(ctx, p)
	}
	executor := newSubagentExecutor(runtimeCfg.MaxConcurrency, false, runner)

	runIndices := make([]int, 0, len(prepared))
	runParams := make([]subAgentParams, 0, len(prepared))
	for i, p := range prepared {
		if p.SkipResult != nil {
			orderedResults[i] = *p.SkipResult
			continue
		}
		runIndices = append(runIndices, i)
		runParams = append(runParams, p.Params)
	}

	execResults := executor.execute(ctx, runParams)

	// Map executor results back into subagentResult and finalize each task.
	for k, idx := range runIndices {
		t := prepared[idx].Task
		er := execResults[k]
		var result subagentResult
		if er.Err != nil {
			_ = toolruntime.ReportFailure(ctx, "subagent_run", er.Err)
			status := message.ToolResultSubtaskStatusFailed
			if errors.Is(er.Err, context.Canceled) || errors.Is(er.Err, context.DeadlineExceeded) {
				status = message.ToolResultSubtaskStatusCanceled
			}
			result = subagentResult{
				Task:     t,
				TaskRef:  taskRefs[t.Name],
				Status:   status,
				Content:  strings.TrimSpace(er.Err.Error()),
				Attempts: 1,
			}
		} else {
			result = subagentResultFromResponse(t, er.Response)
			result.TaskRef = taskRefs[t.Name]
			result.Attempts = 1
			if parentSession, sessErr := c.sessions.Get(ctx, params.SessionID); sessErr == nil && parentSession.PermissionMode == session.PermissionModeAuto {
				if result.Yield.IsEmpty() && result.Status != message.ToolResultSubtaskStatusFailed {
					result.Content = message.SanitizedToolResultStub
				}
			}
			if result.ChildSessionID != "" && result.Yield.IsEmpty() {
				artifacts, filesTouched, patchPlan, testResults, followups := c.collectSubagentArtifacts(ctx, result.ChildSessionID)
				result.Artifacts = mergeUniqueStrings(result.Artifacts, artifacts)
				result.FilesTouched = mergeUniqueStrings(result.FilesTouched, filesTouched)
				result.PatchPlan = mergeUniqueStrings(result.PatchPlan, patchPlan)
				result.TestResults = mergeUniqueStrings(result.TestResults, testResults)
				result.Followups = mergeUniqueStrings(result.Followups, followups)
			}
			if result.Status == message.ToolResultSubtaskStatusFailed {
				_ = toolruntime.ReportFailure(ctx, "subagent_result", errors.New(result.Content))
			}
		}
		orderedResults[idx] = result
	}

	// Finalize: write metadata, mark bridge results, update registry status,
	// and unregister each subagent.
	for i := range orderedResults {
		t := prepared[i].Task
		result := orderedResults[i]
		if result.Status == "" {
			result = subagentResult{
				Task:    t,
				TaskRef: taskRefs[t.Name],
				Status:  message.ToolResultSubtaskStatusFailed,
				Content: "Task did not produce a result.",
			}
		}
		result = withSubagentOutputMetadata(result)
		orderedResults[i] = result
		bridge.MarkResult(t.Name, result.Status, result.Content)

		if prepared[i].AgentID != "" {
			switch result.Status {
			case message.ToolResultSubtaskStatusCompleted, message.ToolResultSubtaskStatusCompletedWithWarnings:
				// Keep the registry entry Idle and arm a keep-alive timer so
				// a follow-up agent tool call with ExistingSessionID can
				// reuse the live SessionAgent (warm revive). The lifecycle
				// manager parks the entry when the TTL fires.
				c.agentRegistry.SetStatus(prepared[i].AgentID, AgentStatusIdle)
				if c.lifecycle != nil && result.ChildSessionID != "" {
					c.lifecycle.Adopt(result.ChildSessionID, prepared[i].AgentID, defaultSubagentAdoptTTL)
				}
			case message.ToolResultSubtaskStatusCanceled,
				message.ToolResultSubtaskStatusFailed,
				message.ToolResultSubtaskStatusBlocked:
				// Failed/canceled subagents have no revive value: revoke any
				// pending keep-alive timer, clear the childSessionAgents
				// entry and unregister immediately.
				c.agentRegistry.SetStatus(prepared[i].AgentID, AgentStatusAborted)
				if c.lifecycle != nil && result.ChildSessionID != "" {
					c.lifecycle.Revoke(result.ChildSessionID)
				}
				if result.ChildSessionID != "" {
					c.childSessionAgents.Delete(result.ChildSessionID)
				}
				c.agentRegistry.Unregister(prepared[i].AgentID)
			}
		}
	}

	reducerInput := make([]reducer.TaskResult, 0, len(params.Tasks))
	resultByTask := make(map[string]subagentResult, len(params.Tasks))
	lines := make([]string, 0, len(params.Tasks))
	hasFailures := false
	hasCancellations := false

	for _, result := range orderedResults {
		if result.Status == "" {
			result = subagentResult{
				Task:    result.Task,
				TaskRef: result.TaskRef,
				Status:  message.ToolResultSubtaskStatusFailed,
				Content: "Task did not produce a result.",
			}
		}
		reduced := reducer.TaskResult{
			ID:             result.Task.Name,
			Description:    result.Task.Description,
			Status:         result.Status,
			ChildSessionID: result.ChildSessionID,
			Content:        result.Content,
			Artifacts:      result.Artifacts,
			FilesTouched:   result.FilesTouched,
			PatchPlan:      result.PatchPlan,
			TestResults:    result.TestResults,
			Followups:      result.Followups,
		}
		reducerInput = append(reducerInput, reduced)
		resultByTask[result.Task.Name] = result
		lines = append(lines, fmt.Sprintf("- %s: %s", result.Task.Name, result.Status))
		if result.Status == message.ToolResultSubtaskStatusFailed {
			hasFailures = true
		}
		if result.Status == message.ToolResultSubtaskStatusCanceled {
			hasCancellations = true
		}
	}

	reducerResult := reducer.Reduce(reducerInput, func(taskResult reducer.TaskResult) message.ToolResultReducerChildSession {
		return reduceResultToChildSession(resultByTask[taskResult.ID])
	})
	reducerResult.MailboxID = strings.TrimSpace(params.ToolCallID)
	reducerResult.Messages = subagentReducerMessages(orderedResults)
	content := reducerResult.Summary
	if len(lines) > 0 {
		content += "\n" + strings.Join(lines, "\n")
	}
	if details := subagentSessionDetailsForModel(orderedResults); details != "" {
		content += "\n\nChild sessions:\n" + details
	}
	if details := subagentOutputDetailsForModel(orderedResults); details != "" {
		content += "\n\nTask outputs:\n" + details
	}

	var allFilesTouched []string
	for _, result := range orderedResults {
		allFilesTouched = mergeUniqueStrings(allFilesTouched, result.FilesTouched)
	}
	if len(allFilesTouched) > 0 && !hasFailures && !hasCancellations {
		content += fmt.Sprintf("\n\nFiles changed: %s\nVerify these changes before proceeding (run type checks, tests, or lint as appropriate).", strings.Join(allFilesTouched, ", "))
	}

	response := fantasy.NewTextResponse(content)
	if hasFailures || hasCancellations {
		response = fantasy.NewTextErrorResponse(content)
	}
	response.Metadata = message.ToolResult{Metadata: response.Metadata}.WithReducer(reducerResult).Metadata

	if len(orderedResults) == 1 {
		only := orderedResults[0]
		response = withSubtaskToolResponseTaskRefMetadata(response, params.ToolCallID, only.ChildSessionID, params.AgentMessageID, only.TaskRef, only.Status)
		if !only.Yield.IsEmpty() {
			response = withSubagentYieldToolResponseMetadata(response, only.Yield)
		}
	}

	return response, nil
}

func subagentAttemptToolCallID(toolCallID string, attempt int) string {
	if attempt == 0 {
		return toolCallID
	}
	return fmt.Sprintf("%s::retry-%d", toolCallID, attempt)
}

func mergeUniqueStrings(values ...[]string) []string {
	merged := make([]string, 0)
	seen := make(map[string]struct{})
	for _, group := range values {
		for _, value := range group {
			trimmed := strings.TrimSpace(value)
			if trimmed == "" {
				continue
			}
			if _, ok := seen[trimmed]; ok {
				continue
			}
			seen[trimmed] = struct{}{}
			merged = append(merged, trimmed)
		}
	}
	return merged
}

func reduceResultToChildSession(result subagentResult) message.ToolResultReducerChildSession {
	description := strings.TrimSpace(result.Task.Description)
	if description == "" {
		description = strings.TrimSpace(result.Task.Name)
	}
	return message.ToolResultReducerChildSession{
		TaskID:        strings.TrimSpace(result.Task.Name),
		TaskRef:       strings.TrimSpace(result.TaskRef),
		Description:   description,
		SessionID:     strings.TrimSpace(result.ChildSessionID),
		Status:        result.Status,
		Preview:       strings.TrimSpace(result.Preview),
		HasFullOutput: result.HasFullOutput,
		OutputChars:   result.OutputChars,
	}
}

func withSubagentOutputMetadata(result subagentResult) subagentResult {
	content := strings.TrimSpace(result.Content)
	if content == "" {
		content = strings.TrimSpace(result.Yield.Data)
	}
	result.OutputChars = len([]rune(content))
	if content == "" {
		result.Preview = ""
		result.HasFullOutput = false
		return result
	}
	preview, truncated := ellipsizeText(content, subagentOutputPreviewCharsLimit)
	result.Preview = preview
	result.HasFullOutput = truncated
	return result
}

func SubagentTaskRef(index int, taskID string, toolCallID string) string {
	slug := subagentTaskRefSlug(taskID)
	if slug == "" {
		slug = "task"
	}
	if prefix := ShortToolCallPrefix(toolCallID); prefix != "" {
		return fmt.Sprintf("%s-%d-%s", prefix, index, slug)
	}
	return fmt.Sprintf("%d-%s", index, slug)
}

func ShortToolCallPrefix(toolCallID string) string {
	toolCallID = strings.TrimSpace(toolCallID)
	if toolCallID == "" {
		return ""
	}
	id := strings.TrimPrefix(toolCallID, "call_")
	if id == "" {
		return ""
	}
	if len(id) >= 6 {
		return id[:6]
	}
	return id
}

func subagentTaskRefSlug(taskID string) string {
	taskID = strings.ToLower(strings.TrimSpace(taskID))
	var b strings.Builder
	prevDash := false
	for _, r := range taskID {
		if b.Len() >= 48 {
			break
		}
		allowed := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if allowed {
			b.WriteRune(r)
			prevDash = false
			continue
		}
		if !prevDash && b.Len() > 0 {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func subagentResultFromResponse(task subagentTask, response fantasy.ToolResponse) subagentResult {
	result := subagentResult{
		Task:    task,
		Status:  message.ToolResultSubtaskStatusCompleted,
		Content: strings.TrimSpace(response.Content),
	}
	if subtask, ok := message.ParseToolResultSubtaskResult(response.Metadata); ok {
		result.Status = subtask.Status
		result.ChildSessionID = subtask.ChildSessionID
	}
	if yield, ok := message.ParseToolResultYield(response.Metadata); ok {
		result.Yield = yield
		if yield.Status != "" {
			result.Status = message.ToolResultSubtaskStatus(yield.Status)
		}
		if strings.TrimSpace(yield.Error) != "" {
			result.Error = strings.TrimSpace(yield.Error)
		}
		if result.Status == message.ToolResultSubtaskStatusCompletedWithWarnings {
			var warnings []string
			if strings.TrimSpace(yield.Error) != "" {
				warnings = append(warnings, strings.TrimSpace(yield.Error))
			}
			result.Warnings = warnings
		}
	} else if response.IsError {
		result.Status = message.ToolResultSubtaskStatusFailed
	}
	if result.Status == "" {
		result.Status = message.ToolResultSubtaskStatusCompleted
	}
	return result
}

func (c *coordinator) collectSubagentArtifacts(ctx context.Context, childSessionID string) ([]string, []string, []string, []string, []string) {
	if c.messages == nil || strings.TrimSpace(childSessionID) == "" {
		return nil, nil, nil, nil, nil
	}

	msgs, err := c.messages.List(ctx, childSessionID)
	if err != nil {
		return nil, nil, nil, nil, nil
	}

	artifacts := make([]string, 0, 8)
	filesTouched := make([]string, 0, 8)
	patchPlan := make([]string, 0, 8)
	testResults := make([]string, 0, 8)
	followups := make([]string, 0, 8)
	seenArtifacts := make(map[string]struct{}, 8)
	seenFiles := make(map[string]struct{}, 8)
	seenPatchPlan := make(map[string]struct{}, 8)
	seenTests := make(map[string]struct{}, 8)
	seenFollowups := make(map[string]struct{}, 8)
	addArtifact := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, ok := seenArtifacts[value]; ok {
			return
		}
		seenArtifacts[value] = struct{}{}
		artifacts = append(artifacts, value)
	}
	addFile := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, ok := seenFiles[value]; ok {
			return
		}
		seenFiles[value] = struct{}{}
		filesTouched = append(filesTouched, value)
		addArtifact("file:" + value)
	}
	addPatchStep := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, ok := seenPatchPlan[value]; ok {
			return
		}
		seenPatchPlan[value] = struct{}{}
		patchPlan = append(patchPlan, value)
	}
	addTestResult := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, ok := seenTests[value]; ok {
			return
		}
		seenTests[value] = struct{}{}
		testResults = append(testResults, value)
	}
	addFollowup := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, ok := seenFollowups[value]; ok {
			return
		}
		seenFollowups[value] = struct{}{}
		followups = append(followups, value)
	}

	for _, msg := range msgs {
		if msg.Role != message.Tool {
			continue
		}
		for _, toolResult := range msg.ToolResults() {
			if reducerMeta, ok := toolResult.Reducer(); ok {
				for _, artifact := range reducerMeta.Artifacts {
					addArtifact(artifact)
				}
				for _, filePath := range reducerMeta.FilesTouched {
					addFile(filePath)
				}
				for _, step := range reducerMeta.PatchPlan {
					addPatchStep(step)
				}
				for _, testResult := range reducerMeta.TestResults {
					addTestResult(testResult)
				}
				for _, question := range reducerMeta.FollowupQuestions {
					addFollowup(question)
				}
			}
			for _, filePath := range subagentToolResultFiles(toolResult) {
				addFile(filePath)
			}
			for _, artifact := range subagentToolResultArtifacts(toolResult) {
				addArtifact(artifact)
			}
		}
	}

	slices.Sort(artifacts)
	slices.Sort(filesTouched)
	slices.Sort(patchPlan)
	slices.Sort(testResults)
	slices.Sort(followups)
	return artifacts, filesTouched, patchPlan, testResults, followups
}

func subagentToolResultFiles(toolResult message.ToolResult) []string {
	var payload struct {
		FilePath string `json:"file_path"`
	}
	switch toolResult.Name {
	case tools.WriteToolName, tools.EditToolName:
		if strings.TrimSpace(toolResult.Metadata) == "" {
			return nil
		}
		if err := json.Unmarshal([]byte(toolResult.Metadata), &payload); err != nil {
			return nil
		}
		if strings.TrimSpace(payload.FilePath) == "" {
			return nil
		}
		return []string{strings.TrimSpace(payload.FilePath)}
	default:
		return nil
	}
}

func subagentToolResultArtifacts(toolResult message.ToolResult) []string {
	var payload struct {
		ShellID string `json:"shell_id"`
	}
	switch toolResult.Name {
	case tools.BashToolName, tools.JobToolName:
		if strings.TrimSpace(toolResult.Metadata) == "" {
			return nil
		}
		if err := json.Unmarshal([]byte(toolResult.Metadata), &payload); err != nil {
			return nil
		}
		if strings.TrimSpace(payload.ShellID) == "" {
			return nil
		}
		return []string{"shell:" + strings.TrimSpace(payload.ShellID)}
	default:
		return nil
	}
}

func (c *coordinator) latestSubagentYield(ctx context.Context, childSessionID string) (message.ToolResultYield, bool) {
	if c.messages == nil || strings.TrimSpace(childSessionID) == "" {
		return message.ToolResultYield{}, false
	}
	msgs, err := c.messages.List(ctx, childSessionID)
	if err != nil {
		return message.ToolResultYield{}, false
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != message.Tool {
			continue
		}
		toolResults := msgs[i].ToolResults()
		for j := len(toolResults) - 1; j >= 0; j-- {
			if yield, ok := toolResults[j].Yield(); ok {
				return yield, true
			}
		}
	}
	return message.ToolResultYield{}, false
}

func (c *coordinator) ensureSubagentYield(ctx context.Context, params subAgentParams, childSessionID string, runtime SubagentRuntimeContext, result *fantasy.AgentResult, maxOutputTokens int64, providerOptions fantasy.ProviderOptions, temperature, topP *float64, topK *int64, frequencyPenalty, presencePenalty *float64) (message.ToolResultYield, bool) {
	if yield, ok := c.latestSubagentYield(ctx, childSessionID); ok {
		return yield, true
	}
	if !runtime.Result.Required {
		return message.ToolResultYield{}, false
	}
	policy := runtime.Result.MissingFinishPolicy
	if policy == "" {
		policy = MissingFinishWarn
	}
	// If the run was force-aborted by the hard request-step budget, the
	// model already had a soft-steer chance to yield and ignored it.
	// Retrying "call yield now" is wasted tokens — the model showed it
	// won't comply. Skip the retry loop and go straight to the synthetic
	// yield with whatever partial output we have.
	shouldRetry := (policy == MissingFinishRetryThenWarn || policy == MissingFinishRetryThenFail) &&
		!c.childSessionWasBudgetAborted(ctx, childSessionID)
	if shouldRetry {
		for range 2 {
			steerPrompt := "Call yield exactly once now. Summarize only the work already completed. Do not start new work unless needed to determine final status."
			// If the subagent has an output schema, remind it to use the
			// payload field with the correct structure.
			if runtime.OutputSchema != nil {
				steerPrompt += " Use the yield tool's payload field (not data) for your structured result, conforming to the output schema."
			}
			_, runErr := params.Agent.Run(ctx, SessionAgentCall{
				SessionID:        childSessionID,
				Prompt:           steerPrompt,
				MaxOutputTokens:  maxOutputTokens,
				ProviderOptions:  providerOptions,
				Temperature:      temperature,
				TopP:             topP,
				TopK:             topK,
				FrequencyPenalty: frequencyPenalty,
				PresencePenalty:  presencePenalty,
				NonInteractive:   true,
			})
			if runErr != nil {
				break
			}
			if yield, ok := c.latestSubagentYield(ctx, childSessionID); ok {
				return yield, true
			}
		}
	}
	content := c.subAgentResponseText(ctx, childSessionID, result)
	if strings.TrimSpace(content) == "" {
		content = subAgentNoContentText(childSessionID)
	}

	// Fallback completion: if the subagent has an output schema but never
	// called yield, try to parse its last assistant text as JSON and validate
	// it against the schema. If it conforms, use it as the payload. This
	// mirrors oh-my-pi's resolveFallbackCompletion — many models produce
	// valid structured output but forget to call yield. Only apply for
	// warn-style policies; fail policies must not be bypassed.
	if runtime.OutputSchema != nil && missingFinishPolicyAllowsJSONFallback(policy) {
		if payload := tryFallbackPayloadFromOutput(content, runtime.OutputSchema); payload != nil {
			return message.ToolResultYield{
				Status:  string(message.ToolResultSubtaskStatusCompletedWithWarnings),
				Data:    content,
				Payload: payload,
				Error:   "yield was not called; payload extracted from final assistant text",
			}, true
		}
	}

	switch policy {
	case MissingFinishFail, MissingFinishRetryThenFail:
		return message.ToolResultYield{
			Status: string(message.ToolResultSubtaskStatusFailed),
			Data:   content,
			Error:  "yield was not called",
		}, true
	default:
		return message.ToolResultYield{
			Status: string(message.ToolResultSubtaskStatusCompletedWithWarnings),
			Data:   content,
			Error:  "yield was not called",
		}, true
	}
}

// tryFallbackPayloadFromOutput attempts to parse raw assistant text as JSON
// and validate it against the output schema. Returns the validated payload
// bytes if successful, or nil if parsing/validation fails.
func tryFallbackPayloadFromOutput(content string, outputSchema any) json.RawMessage {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}

	// Strip markdown code fences if present.
	if strings.HasPrefix(content, "```") {
		lines := strings.Split(content, "\n")
		if len(lines) >= 2 {
			// Remove first line (```json or ```) and last line (```).
			end := len(lines) - 1
			if strings.TrimSpace(lines[end]) == "```" {
				lines = lines[1:end]
			} else {
				lines = lines[1:]
			}
			content = strings.TrimSpace(strings.Join(lines, "\n"))
		}
	}

	var parsed any
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		return nil
	}

	// If no schema, accept any valid JSON.
	if outputSchema == nil {
		if bytes, err := json.Marshal(parsed); err == nil {
			return bytes
		}
		return nil
	}

	// Validate against the output schema.
	schemaBytes, err := json.Marshal(outputSchema)
	if err != nil {
		return nil
	}
	compiler := jsonschema.NewCompiler()
	compiled, err := compiler.Compile(schemaBytes)
	if err != nil {
		return nil
	}
	if result := compiled.Validate(parsed); result.IsValid() {
		if bytes, err := json.Marshal(parsed); err == nil {
			return bytes
		}
	}
	return nil
}

// childSessionWasBudgetAborted reports whether the most recent assistant
// message on the child session ended with FinishReasonBudgetExceeded. Used
// to skip fruitless yield retries when the model already ignored the soft
// steer.
func (c *coordinator) childSessionWasBudgetAborted(ctx context.Context, childSessionID string) bool {
	if c.messages == nil {
		return false
	}
	msgs, err := c.messages.List(ctx, childSessionID)
	if err != nil {
		return false
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		msg := msgs[i]
		if msg.Role != message.Assistant || msg.IsSummaryMessage {
			continue
		}
		return msg.FinishReason() == message.FinishReasonBudgetExceeded
	}
	return false
}

func (c *coordinator) runSubAgentDirect(ctx context.Context, params subAgentParams) (response fantasy.ToolResponse, err error) {
	untrackSubAgent := c.trackActiveSubAgent(params.SessionID, params.Agent)
	defer untrackSubAgent()

	parentSession, err := c.sessions.Get(ctx, params.SessionID)
	if err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("get parent session: %w", err)
	}
	if parentSession.PermissionMode == session.PermissionModeAuto && !params.SkipHandoffReview {
		review, reviewErr := c.reviewHandoffText(ctx, parentSession, params.SessionTitle, "", params.Prompt, false)
		if reviewErr != nil {
			return withSubtaskToolResponseMetadata(
				fantasy.NewTextErrorResponse("Auto Mode blocked subagent delegation because the handoff review failed."),
				params.ToolCallID,
				"",
				params.ParentMessageID,
				message.ToolResultSubtaskStatusFailed,
			), nil
		}
		if !review.AllowAuto {
			reason := strings.TrimSpace(review.Reason)
			if reason == "" {
				reason = "Auto Mode blocked subagent delegation."
			}
			return withSubtaskToolResponseMetadata(
				fantasy.NewTextErrorResponse(reason),
				params.ToolCallID,
				"",
				params.ParentMessageID,
				message.ToolResultSubtaskStatusFailed,
			), nil
		}
	}

	var subSession session.Session
	var previousChildCost float64
	if strings.TrimSpace(params.ExistingSessionID) != "" {
		subSession, err = c.sessions.Get(ctx, params.ExistingSessionID)
		if err != nil {
			return fantasy.ToolResponse{}, fmt.Errorf("get child session: %w", err)
		}
		previousChildCost = subSession.Cost
	} else {
		agentToolSessionID := c.sessions.CreateAgentToolSessionID(params.AgentMessageID, params.ToolCallID)
		subSession, err = c.sessions.CreateTaskSession(ctx, agentToolSessionID, params.SessionID, params.SessionTitle)
		if err != nil {
			return fantasy.ToolResponse{}, fmt.Errorf("create session: %w", err)
		}
	}
	defer c.clearDeferredToolActivationsForSession(subSession.ID)

	// Cancel any pending keep-alive timer from a previous run on this child
	// session, so the stale timer cannot later evict the freshly-stored
	// SessionAgent we are about to install.
	if c.lifecycle != nil {
		c.lifecycle.Revoke(subSession.ID)
	}
	c.childSessionAgents.Store(subSession.ID, params.Agent)
	// keepAlive is flipped to true on the success path so the deferred
	// cleanup skips evicting the SessionAgent — the lifecycle manager owns
	// its keep-alive window from that point on.
	keepAlive := false
	defer func() {
		if !keepAlive {
			c.childSessionAgents.Delete(subSession.ID)
		}
	}()

	if params.SessionSetup != nil {
		params.SessionSetup(subSession.ID)
	}

	eventSink := coordinatorSubagentEventSink{timeline: c.timeline}

	// Publish a "started" event at spawn time so the UI can resolve this
	// child session immediately via the timeline (the session CreatedEvent
	// already triggers a ChildSessionStartedEvent via app/timeline.go, but
	// emitting here makes the spawn-time signal explicit and records the
	// TaskID association for the UI's task->child session mapping).
	eventSink.PublishSubagentEvent(ctx, SubagentEvent{
		Type:            SubagentEventStarted,
		ParentSessionID: params.SessionID,
		ChildSessionID:  subSession.ID,
		TaskID:          strings.TrimSpace(params.ToolCallID),
		Message:         params.SessionTitle,
		Status:          string(message.ToolResultSubtaskStatusRunning),
		Timestamp:       time.Now(),
	})

	effectiveIsolation := strings.TrimSpace(params.AgentIsolation)
	subSession, sessionWorkingDir, effectiveIsolation, err := c.prepareSubagentWorkspace(ctx, parentSession, subSession, effectiveIsolation)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}

	// Track worktree for cleanup after subagent completes.
	usedWorktree := effectiveIsolation == "worktree" && subSession.WorkspaceCWD != parentSession.WorkspaceCWD
	if usedWorktree {
		// Capture the parent work dir and worktree path for the deferred
		// merge-back. Patch mode is the default; branch mode (preserving
		// subagent commits) is opt-in via the per-task isolation override.
		parentWorkDir := parentSession.WorkspaceCWD
		if parentWorkDir == "" {
			parentWorkDir = c.cfg.WorkingDir()
		}
		worktreePath := subSession.WorkspaceCWD
		defer func() {
			mergeResult := c.cleanupWorktreeIfNeeded(context.Background(), parentWorkDir, worktreePath)
			c.applyMergeBackResult(&response, &err, mergeResult, params, subSession.ID)
		}()
	}

	model := params.Agent.Model()
	maxTokens := model.CatwalkCfg.DefaultMaxTokens
	if model.ModelCfg.MaxTokens != 0 {
		maxTokens = model.ModelCfg.MaxTokens
	}

	providerCfg, ok := c.cfg.Config().Providers.Get(model.ModelCfg.Provider)
	if !ok {
		return fantasy.ToolResponse{}, errModelProviderNotConfigured
	}

	resolvedAgentCfg, resolveErr := c.subagentConfig(params.SubagentTypeOrDefault())
	if resolveErr != nil {
		resolvedAgentCfg = config.Agent{ID: params.SubagentTypeOrDefault(), Description: params.SessionTitle, AllowedTools: nil}
	}

	parentPermissions, parentPermissionsErr := c.parentPermissionContext(ctx, session.CollaborationModeDefault, params.SessionID)
	if parentPermissionsErr != nil {
		return fantasy.ToolResponse{}, parentPermissionsErr
	}

	// Clear inherited runtime config after capturing the parent's effective
	// tools. Each subagent
	// must refresh its own models, tools, and system prompt; otherwise
	// concurrent child runs can observe the parent's runtime config and skip
	// their own initialization. This also prevents subagent permissions from
	// inheriting parent's temporary allowed tool restrictions in Plan/Review
	// modes.
	ctx = context.WithValue(ctx, sessionAgentRuntimeConfigContextKey{}, (*sessionAgentRuntimeConfig)(nil))
	ctx = context.WithValue(ctx, tools.SessionIDContextKey, subSession.ID)
	ctx = context.WithValue(ctx, tools.ToolCallIDContextKey, params.ToolCallID)
	ctx = context.WithValue(ctx, tools.WorkingDirContextKey, sessionWorkingDir)
	ctx = toolruntime.WithSessionID(ctx, subSession.ID)
	ctx = toolruntime.WithToolCallID(ctx, params.ToolCallID)
	ctx = withAgentPolicyContext(ctx, config.Agent{
		Memory:     params.AgentMemory,
		Isolation:  effectiveIsolation,
		Background: params.AgentBackground,
	})
	if params.DelegationMailbox != "" {
		ctx = toolruntime.WithDelegationMailbox(ctx, params.DelegationMailbox)
	}
	if params.IrcAgentID != "" {
		ctx = toolruntime.WithIrcAgentID(ctx, params.IrcAgentID)
	}

	// Inject worker identity for permission escalation.
	runtimeTask := subagentTask{Name: params.ToolCallID, Description: params.SessionTitle, Assignment: params.Prompt}
	runtime := buildSubagentRuntimeContext(
		params.SessionID,
		subSession.ID,
		params.ParentMessageID,
		params.ToolCallID,
		runtimeTask,
		resolvedAgentCfg,
		parentPermissions,
		parentPermissions.AllowedTools,
		effectiveIsolation,
		sessionWorkingDir,
		eventSink,
	)
	applySubagentRuntimeConfig(&runtime, c.cfg.Config().EffectiveSubagentRuntime())
	ctx = withSubagentRuntimeContext(ctx, runtime)

	if c.escalationBridge != nil {
		workerIdentity := permission.WorkerIdentity{
			AgentID:         subSession.ID,
			AgentName:       params.SessionTitle,
			AgentType:       "subagent",
			ParentSessionID: runtime.ParentSessionID,
			ChildSessionID:  runtime.ChildSessionID,
			TaskID:          runtime.TaskID,
			ProfileName:     runtime.AgentProfile.Name,
		}
		ctx = permission.WithWorkerIdentity(ctx, workerIdentity)
		ctx = permission.WithEscalationBridge(ctx, c.escalationBridge)
	}

	// Prepend a structured handoff summary of the coordinator's session so the
	// subagent has concrete context from the Research phase and does not need
	// to rediscover details the coordinator already gathered.
	// If PrecomputedContext was set by the batch path, use it directly to
	// avoid redundant computation.
	enrichedPrompt := params.Prompt
	if strings.TrimSpace(params.ExistingSessionID) == "" {
		if params.PrecomputedContext != "" {
			enrichedPrompt = params.PrecomputedContext + params.Prompt
		} else {
			handoff := c.buildSubagentHandoffSummary(ctx, params.SessionID)
			historyFile := c.writeParentHistoryFile(ctx, params.SessionID)
			defer c.cleanupParentHistoryFile(historyFile)
			if historyFile != "" {
				handoff += fmt.Sprintf(
					"\n<parent_history_file>\nFull conversation index: %s\nRead this file if you need context beyond what is provided above.\n</parent_history_file>\n",
					historyFile,
				)
			}
			enrichedPrompt = handoff + params.Prompt
		}
	}

	providerOptions := getProviderOptions(model, providerCfg, resolvedAgentCfg)
	temperature := model.ModelCfg.Temperature
	topP := model.ModelCfg.TopP
	topK := model.ModelCfg.TopK
	frequencyPenalty := model.ModelCfg.FrequencyPenalty
	presencePenalty := model.ModelCfg.PresencePenalty
	result, err := params.Agent.Run(ctx, SessionAgentCall{
		SessionID:        subSession.ID,
		Prompt:           enrichedPrompt,
		MaxOutputTokens:  maxTokens,
		ProviderOptions:  providerOptions,
		Temperature:      temperature,
		TopP:             topP,
		TopK:             topK,
		FrequencyPenalty: frequencyPenalty,
		PresencePenalty:  presencePenalty,
		NonInteractive:   true,
		OnProgress:       params.OnProgress,
	})
	if err != nil {
		_ = toolruntime.ReportFailure(ctx, "subagent_run", err)
		slog.Error("Sub-agent run failed", "error", err, "session", subSession.ID, "prompt", params.Prompt)
		content := c.subAgentErrorText(ctx, subSession.ID, err)
		eventSink.PublishSubagentEvent(ctx, SubagentEvent{Type: SubagentEventFailed, ParentSessionID: params.SessionID, ChildSessionID: subSession.ID, TaskID: runtime.TaskID, Message: params.SessionTitle, Status: "failed", Timestamp: time.Now()})
		if costErr := c.updateParentSessionCostDelta(ctx, subSession.ID, params.SessionID, previousChildCost); costErr != nil {
			return fantasy.ToolResponse{}, costErr
		}
		status := message.ToolResultSubtaskStatusFailed
		if ctx.Err() != nil {
			status = message.ToolResultSubtaskStatusCanceled
		}
		return withSubtaskToolResponseMetadata(
			fantasy.NewTextErrorResponse(content),
			params.ToolCallID,
			subSession.ID,
			params.ParentMessageID,
			status,
		), nil
	}

	if err := c.updateParentSessionCostDelta(ctx, subSession.ID, params.SessionID, previousChildCost); err != nil {
		return fantasy.ToolResponse{}, err
	}

	var (
		yieldResult message.ToolResultYield
		hasYield    bool
	)
	if !params.SkipStructuredFinishCheck {
		yieldResult, hasYield = c.ensureSubagentYield(ctx, params, subSession.ID, runtime, result, maxTokens, providerOptions, temperature, topP, topK, frequencyPenalty, presencePenalty)
	} else {
		yieldResult, hasYield = c.latestSubagentYield(ctx, subSession.ID)
	}
	content := c.subAgentResponseText(ctx, subSession.ID, result)
	if hasYield && strings.TrimSpace(yieldResult.Data) != "" {
		content = yieldResult.Data
	}
	if content == "" {
		slog.Warn("Sub-agent returned empty response", "session", subSession.ID, "prompt", params.Prompt)
		content = subAgentNoContentText(subSession.ID)
	}
	if parentSession.PermissionMode == session.PermissionModeAuto && !params.SkipHandoffReview {
		review, reviewErr := c.reviewHandoffText(ctx, parentSession, params.SessionTitle, content, params.Prompt, true)
		if reviewErr != nil {
			return withSubtaskToolResponseMetadata(
				fantasy.NewTextErrorResponse("Auto Mode blocked subagent handoff because the handoff review failed."),
				params.ToolCallID,
				subSession.ID,
				params.ParentMessageID,
				message.ToolResultSubtaskStatusFailed,
			), nil
		}
		if !review.AllowAuto {
			reason := strings.TrimSpace(review.Reason)
			if reason == "" {
				reason = "Auto Mode blocked subagent handoff."
			}
			return withSubtaskToolResponseMetadata(
				fantasy.NewTextErrorResponse(reason),
				params.ToolCallID,
				subSession.ID,
				params.ParentMessageID,
				message.ToolResultSubtaskStatusFailed,
			), nil
		}
	}
	status := message.ToolResultSubtaskStatusCompleted
	if hasYield && yieldResult.Status != "" {
		status = message.ToolResultSubtaskStatus(yieldResult.Status)
	}
	eventStatus := "completed"
	if status == message.ToolResultSubtaskStatusCompletedWithWarnings {
		eventStatus = "completed_with_warnings"
	} else if status == message.ToolResultSubtaskStatusBlocked {
		eventStatus = "blocked"
	}
	eventType := SubagentEventFinish
	if status == message.ToolResultSubtaskStatusBlocked {
		eventType = SubagentEventBlocked
	} else if status == message.ToolResultSubtaskStatusCanceled {
		eventType = SubagentEventCanceled
	} else if status == message.ToolResultSubtaskStatusFailed {
		eventType = SubagentEventFailed
	}
	eventSink.PublishSubagentEvent(ctx, SubagentEvent{Type: eventType, ParentSessionID: params.SessionID, ChildSessionID: subSession.ID, TaskID: runtime.TaskID, Message: params.SessionTitle, Status: eventStatus, Timestamp: time.Now()})

	response = withSubtaskToolResponseMetadata(
		fantasy.NewTextResponse(content),
		params.ToolCallID,
		subSession.ID,
		params.ParentMessageID,
		status,
	)
	if hasYield {
		response = withSubagentYieldToolResponseMetadata(response, yieldResult)
	}
	// On a clean completion (no error, non-failed status) keep the
	// SessionAgent live in childSessionAgents for a keep-alive window so a
	// follow-up agent tool call targeting ExistingSessionID can warm-revive
	// instead of rebuilding from disk. The lifecycle manager arms a TTL
	// timer that will park the entry when it expires.
	if status != message.ToolResultSubtaskStatusFailed &&
		status != message.ToolResultSubtaskStatusCanceled &&
		status != message.ToolResultSubtaskStatusBlocked &&
		c.lifecycle != nil && params.AgentID != "" {
		c.lifecycle.Adopt(subSession.ID, params.AgentID, defaultSubagentAdoptTTL)
		keepAlive = true
	}
	return response, nil
}

func withAgentPolicyContext(ctx context.Context, agentCfg config.Agent) context.Context {
	ctx = context.WithValue(ctx, tools.AgentMemoryContextKey, strings.TrimSpace(agentCfg.Memory))
	ctx = context.WithValue(ctx, tools.AgentIsolationContextKey, strings.TrimSpace(agentCfg.Isolation))
	if agentCfg.Background != nil {
		ctx = context.WithValue(ctx, tools.AgentBackgroundContextKey, *agentCfg.Background)
	}
	return ctx
}

func (c *coordinator) prepareSubagentWorkspace(ctx context.Context, parentSession, subSession session.Session, requestedIsolation string) (session.Session, string, string, error) {
	effectiveIsolation := strings.ToLower(strings.TrimSpace(requestedIsolation))
	if effectiveIsolation == "" {
		// Fall through to the configured global default before "session".
		// This makes Subagents.DefaultIsolation actually take effect when
		// no per-task/per-batch/agent-level isolation was requested,
		// instead of the previous dead override that mutated the runtime
		// context after the worktree had already been (not) created.
		if runtimeCfg := c.cfg.Config().EffectiveSubagentRuntime(); strings.TrimSpace(runtimeCfg.DefaultIsolation) != "" {
			if def := strings.ToLower(strings.TrimSpace(runtimeCfg.DefaultIsolation)); def != "none" {
				effectiveIsolation = def
			}
		}
		if effectiveIsolation == "" {
			effectiveIsolation = "session"
		}
	}

	sessionWorkingDir := strings.TrimSpace(parentSession.WorkspaceCWD)
	if sessionWorkingDir == "" {
		sessionWorkingDir = c.cfg.WorkingDir()
	}

	if effectiveIsolation != "worktree" {
		if strings.TrimSpace(subSession.WorkspaceCWD) == "" {
			subSession.WorkspaceCWD = sessionWorkingDir
			updatedSession, saveErr := c.sessions.Save(ctx, subSession)
			if saveErr != nil {
				return subSession, "", effectiveIsolation, fmt.Errorf("save subagent session workspace cwd: %w", saveErr)
			}
			subSession = updatedSession
		}
		return subSession, sessionWorkingDir, effectiveIsolation, nil
	}

	worktreeDir, err := c.createSubagentWorktreeDir(sessionWorkingDir, subSession.ID)
	if err != nil {
		slog.Warn("Worktree isolation unavailable, falling back to session isolation", "session", subSession.ID, "error", err)
		effectiveIsolation = "session"
		if strings.TrimSpace(subSession.WorkspaceCWD) == "" {
			subSession.WorkspaceCWD = sessionWorkingDir
			updatedSession, saveErr := c.sessions.Save(ctx, subSession)
			if saveErr != nil {
				return subSession, "", effectiveIsolation, fmt.Errorf("save subagent fallback workspace cwd: %w", saveErr)
			}
			subSession = updatedSession
		}
		return subSession, sessionWorkingDir, effectiveIsolation, nil
	}

	subSession.WorkspaceCWD = worktreeDir
	updatedSession, saveErr := c.sessions.Save(ctx, subSession)
	if saveErr != nil {
		return subSession, "", effectiveIsolation, fmt.Errorf("save subagent worktree workspace cwd: %w", saveErr)
	}
	return updatedSession, worktreeDir, effectiveIsolation, nil
}

func (c *coordinator) createSubagentWorktreeDir(baseDir, subSessionID string) (string, error) {
	baseDir = strings.TrimSpace(baseDir)
	if baseDir == "" {
		return "", fmt.Errorf("base directory is empty")
	}

	cmd := exec.Command("git", "-C", baseDir, "rev-parse", "--show-toplevel")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolve git root: %w", err)
	}
	gitRoot := strings.TrimSpace(string(output))
	if gitRoot == "" {
		return "", fmt.Errorf("git root is empty")
	}

	slug := strings.ReplaceAll(subSessionID, "$$", "-")
	slug = strings.ReplaceAll(slug, "/", "-")
	if len(slug) > 40 {
		slug = slug[:40]
	}
	branchName := fmt.Sprintf("crush-agent-%s", slug)
	worktreeRoot := c.subagentWorktreeRoot(gitRoot)
	worktreeDir := filepath.Join(worktreeRoot, branchName)
	if err := os.MkdirAll(worktreeRoot, 0o755); err != nil {
		return "", fmt.Errorf("create worktree root: %w", err)
	}

	if info, statErr := os.Stat(worktreeDir); statErr == nil && info.IsDir() {
		return worktreeDir, nil
	}

	addCmd := exec.Command("git", "-C", gitRoot, "worktree", "add", "-B", branchName, worktreeDir, "HEAD")
	addOutput, addErr := addCmd.CombinedOutput()
	if addErr != nil {
		return "", fmt.Errorf("create worktree: %w: %s", addErr, strings.TrimSpace(string(addOutput)))
	}
	return worktreeDir, nil
}

func (c *coordinator) subagentWorktreeRoot(gitRoot string) string {
	projectDataDir := ""
	if c != nil && c.cfg != nil {
		projectDataDir = strings.TrimSpace(c.cfg.ProjectDataDir())
	}
	if projectDataDir == "" {
		projectDataDir = config.ProjectDataDir(gitRoot)
	}
	return filepath.Join(projectDataDir, "worktrees")
}

func (c *coordinator) subagentWorktreeCleanupRoots(gitRoot string) []string {
	primary := c.subagentWorktreeRoot(gitRoot)
	legacy := filepath.Join(gitRoot, ".crush", "worktrees")
	if primary == legacy {
		return []string{primary}
	}
	return []string{primary, legacy}
}

// removeSubagentWorktree removes a worktree directory and its branch.
func (c *coordinator) removeSubagentWorktree(worktreeDir string) error {
	worktreeDir = strings.TrimSpace(worktreeDir)
	if worktreeDir == "" {
		return nil
	}

	if _, err := os.Stat(worktreeDir); os.IsNotExist(err) {
		return nil
	}

	// Use --git-common-dir to find the main repo root (works correctly in worktrees)
	// In a worktree, --show-toplevel returns the worktree's own root, not the main repo.
	cmd := exec.Command("git", "-C", worktreeDir, "rev-parse", "--git-common-dir")
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("resolve git common dir: %w", err)
	}
	gitCommonDir := strings.TrimSpace(string(output))
	if gitCommonDir == "" {
		return fmt.Errorf("git common dir is empty")
	}

	// Resolve relative path if needed.
	if !filepath.IsAbs(gitCommonDir) {
		gitCommonDir = filepath.Join(worktreeDir, gitCommonDir)
	}

	// Navigate from .git/worktrees/<name> to the main repo's .git, then to repo root.
	gitDir := filepath.Clean(gitCommonDir)
	if filepath.Base(filepath.Dir(gitDir)) == "worktrees" {
		gitDir = filepath.Dir(filepath.Dir(gitDir))
	}
	gitRoot := filepath.Dir(gitDir)

	branchName := filepath.Base(worktreeDir)

	removeCmd := exec.Command("git", "-C", gitRoot, "worktree", "remove", "--force", worktreeDir)
	if output, err := removeCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("remove worktree %s: %w, output: %s", worktreeDir, err, string(output))
	}

	branchCmd := exec.Command("git", "-C", gitRoot, "branch", "-D", branchName)
	if output, err := branchCmd.CombinedOutput(); err != nil {
		slog.Debug("Failed to delete worktree branch (may not exist)", "branch", branchName, "error", err, "output", string(output))
	}

	return nil
}

// hasWorktreeChanges checks if a worktree has uncommitted or committed changes
// relative to the parent HEAD. If the subagent committed inside its worktree,
// the working tree may be clean but the branch is still ahead of the parent.
func (c *coordinator) hasWorktreeChanges(parentWorkDir, worktreeDir string) (bool, error) {
	cmd := exec.Command("git", "-C", worktreeDir, "status", "--porcelain")
	output, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("check worktree status: %w", err)
	}
	if len(strings.TrimSpace(string(output))) > 0 {
		return true, nil
	}

	parentHead, err := gitRevParseHead(parentWorkDir)
	if err != nil {
		return false, fmt.Errorf("resolve parent HEAD: %w", err)
	}
	branchPoint, err := gitMergeBase(worktreeDir, "HEAD", parentHead)
	if err != nil {
		return false, fmt.Errorf("find branch point: %w", err)
	}
	diff, err := gitDiffStat(worktreeDir, branchPoint, "HEAD")
	if err != nil {
		return false, fmt.Errorf("diff worktree: %w", err)
	}
	return len(strings.TrimSpace(diff)) > 0, nil
}

// cleanupWorktreeIfNeeded merges a worktree's changes back into the parent's
// working tree (if it has changes) and then removes the worktree. If the
// merge-back fails, the worktree is preserved for manual resolution.
func (c *coordinator) cleanupWorktreeIfNeeded(ctx context.Context, parentWorkDir, worktreeDir string) MergeBackResult {
	worktreeDir = strings.TrimSpace(worktreeDir)
	if worktreeDir == "" {
		return MergeBackResult{Message: "worktree dir is empty, skipping cleanup"}
	}

	hasChanges, err := c.hasWorktreeChanges(parentWorkDir, worktreeDir)
	if err != nil {
		slog.Warn("Failed to check worktree changes, skipping cleanup", "path", worktreeDir, "error", err)
		return MergeBackResult{Message: fmt.Sprintf("failed to check worktree changes: %v", err)}
	}

	if !hasChanges {
		// No changes — just remove the worktree.
		if removeErr := c.removeSubagentWorktree(worktreeDir); removeErr != nil {
			slog.Warn("Failed to cleanup worktree", "path", worktreeDir, "error", removeErr)
		}
		return MergeBackResult{Success: true, Message: "worktree had no changes, removed"}
	}

	// Has changes — merge back into parent.
	result := c.mergeBackWorktree(ctx, parentWorkDir, worktreeDir)
	if result.Success {
		// Merge succeeded — remove the worktree.
		if removeErr := c.removeSubagentWorktree(worktreeDir); removeErr != nil {
			slog.Warn("Failed to remove worktree after successful merge", "path", worktreeDir, "error", removeErr)
		}
	} else {
		// Merge failed — preserve the worktree for manual resolution.
		slog.Warn("Merge-back failed, preserving worktree", "path", worktreeDir, "message", result.Message)
	}
	return result
}

// applyMergeBackResult merges the merge-back outcome into the subagent tool
// response. When the merge-back fails, the function turns the subagent's
// successful response into a tool error so the parent model and user are not
// left believing the changes landed when they did not.
func (c *coordinator) applyMergeBackResult(response *fantasy.ToolResponse, err *error, mergeResult MergeBackResult, params subAgentParams, childSessionID string) {
	if mergeResult.Message != "" {
		slog.Info("Subagent worktree merge-back", "success", mergeResult.Success, "message", mergeResult.Message)
	}
	if *err != nil {
		if !mergeResult.Success && mergeResult.Message != "" {
			*err = fmt.Errorf("subagent failed: %w; merge-back: %s", *err, mergeResult.Message)
		}
		return
	}
	if mergeResult.Success && mergeResult.ConflictFile == "" && mergeResult.SavedDiffPath == "" {
		return
	}
	if mergeResult.Message == "" {
		return
	}

	content := response.Content
	if content == "" {
		content = subAgentNoContentText(childSessionID)
	}
	mergedContent := fmt.Sprintf("%s\n\n<merge_back result=\"failure\">\n%s\n</merge_back>", content, mergeResult.Message)
	*response = withSubtaskToolResponseMetadata(
		fantasy.NewTextErrorResponse(mergedContent),
		params.ToolCallID,
		childSessionID,
		params.ParentMessageID,
		message.ToolResultSubtaskStatusFailed,
	)
}

// CleanupStaleWorktrees removes worktrees older than the cutoff duration.
func (c *coordinator) CleanupStaleWorktrees(ctx context.Context, cutoffDays int) error {
	workingDir := c.cfg.WorkingDir()

	cmd := exec.Command("git", "-C", workingDir, "rev-parse", "--show-toplevel")
	output, err := cmd.Output()
	if err != nil {
		return nil
	}
	gitRoot := strings.TrimSpace(string(output))

	for _, worktreesDir := range c.subagentWorktreeCleanupRoots(gitRoot) {
		if err := c.cleanupStaleWorktreesInDir(gitRoot, worktreesDir, cutoffDays); err != nil {
			return err
		}
	}
	return nil
}

func (c *coordinator) cleanupStaleWorktreesInDir(gitRoot, worktreesDir string, cutoffDays int) error {
	entries, err := os.ReadDir(worktreesDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read worktrees directory: %w", err)
	}

	cutoffTime := time.Now().AddDate(0, 0, -cutoffDays)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		worktreePath := filepath.Join(worktreesDir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}

		if info.ModTime().Before(cutoffTime) {
			hasChanges, chkErr := c.hasWorktreeChanges(gitRoot, worktreePath)
			if chkErr != nil {
				slog.Warn("Failed to check worktree changes, skipping cleanup", "path", worktreePath, "error", chkErr)
				continue
			}
			if !hasChanges {
				if err := c.removeSubagentWorktree(worktreePath); err != nil {
					slog.Warn("Failed to cleanup stale worktree", "path", worktreePath, "error", err)
				} else {
					slog.Debug("Cleaned up stale worktree", "path", worktreePath)
				}
			}
		}
	}

	return nil
}

func withSubtaskToolResponseMetadata(response fantasy.ToolResponse, parentToolCallID, childSessionID, parentMessageID string, status message.ToolResultSubtaskStatus) fantasy.ToolResponse {
	return withSubtaskToolResponseTaskRefMetadata(response, parentToolCallID, childSessionID, parentMessageID, "", status)
}

func withSubtaskToolResponseTaskRefMetadata(response fantasy.ToolResponse, parentToolCallID, childSessionID, parentMessageID, taskRef string, status message.ToolResultSubtaskStatus) fantasy.ToolResponse {
	response.Metadata = message.ToolResult{Metadata: response.Metadata}.WithSubtaskResult(message.ToolResultSubtaskResult{
		ChildSessionID:   childSessionID,
		ParentToolCallID: parentToolCallID,
		ParentMessageID:  parentMessageID,
		TaskRef:          taskRef,
		Status:           status,
	}).Metadata
	return response
}

func withSubagentYieldToolResponseMetadata(response fantasy.ToolResponse, yield message.ToolResultYield) fantasy.ToolResponse {
	response.Metadata = message.ToolResult{Metadata: response.Metadata}.WithYield(yield).Metadata
	return response
}

func subagentReducerMessages(results []subagentResult) []string {
	messages := make([]string, 0, len(results))
	for _, result := range results {
		label := strings.TrimSpace(result.Task.Description)
		if label == "" {
			label = result.Task.Name
		}
		content := compactText(result.Content)
		if content == "" {
			messages = append(messages, fmt.Sprintf("%s (%s)", label, result.Status))
			continue
		}
		content, truncated := ellipsizeText(content, subagentReducerMessageCharsLimit)
		entry := fmt.Sprintf("%s (%s): %s", label, result.Status, content)
		if truncated {
			entry += " [truncated]"
		}
		messages = append(messages, entry)
	}
	return messages
}

func subagentSessionDetailsForModel(results []subagentResult) string {
	lines := make([]string, 0, len(results))
	for _, result := range results {
		sessionID := strings.TrimSpace(result.ChildSessionID)
		if sessionID == "" {
			continue
		}
		label := strings.TrimSpace(result.Task.Description)
		if label == "" {
			label = result.Task.Name
		}
		identifier := sessionID
		if strings.TrimSpace(result.AgentID) != "" {
			identifier = fmt.Sprintf("%s (agent %s)", sessionID, strings.TrimSpace(result.AgentID))
		}
		taskRef := strings.TrimSpace(result.TaskRef)
		if taskRef != "" {
			lines = append(lines, fmt.Sprintf("- %s (%s): %s; task_ref=%s", label, result.Status, identifier, taskRef))
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s (%s): %s", label, result.Status, identifier))
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func subagentOutputDetailsForModel(results []subagentResult) string {
	// Use the full Content (yield results bypass truncation; non-yield
	// fallback is capped by subAgentResponseCharsLimit) rather than the 5k
	// UI preview, so the parent agent does not have to call subtask_result
	// for every parallel fan-out task. Budget is allocated fairly across
	// tasks that have content, then clamped by the per-task cap so a
	// single huge task cannot monopolize the aggregate.
	candidates := make([]int, 0, len(results))
	for i, result := range results {
		content := strings.TrimSpace(result.Content)
		if content == "" {
			content = strings.TrimSpace(result.Yield.Data)
		}
		if content == "" {
			content = strings.TrimSpace(result.Preview)
		}
		if content != "" {
			candidates = append(candidates, i)
		}
	}
	if len(candidates) == 0 {
		return ""
	}

	perTaskBudget := subagentOutputAggregateCharsLimit / len(candidates)
	if perTaskBudget > subagentOutputPerTaskCharsLimit {
		perTaskBudget = subagentOutputPerTaskCharsLimit
	}
	if perTaskBudget < subagentOutputPreviewCharsLimit {
		perTaskBudget = subagentOutputPreviewCharsLimit
	}

	lines := make([]string, 0, len(results))
	remaining := subagentOutputAggregateCharsLimit
	truncatedTail := false
	for _, result := range results {
		content := strings.TrimSpace(result.Content)
		if content == "" {
			content = strings.TrimSpace(result.Yield.Data)
		}
		if content == "" {
			content = strings.TrimSpace(result.Preview)
		}
		content = compactText(content)
		if content == "" {
			continue
		}

		label := strings.TrimSpace(result.Task.Description)
		if label == "" {
			label = result.Task.Name
		}

		budget := perTaskBudget
		if budget > remaining {
			budget = remaining
		}
		content, truncated := ellipsizeText(content, budget)
		line := fmt.Sprintf("- %s (%s): %s", label, result.Status, content)
		if truncated {
			taskRef := strings.TrimSpace(result.TaskRef)
			if taskRef != "" {
				line += fmt.Sprintf(" [truncated; full output: subtask://%s]", taskRef)
			} else {
				line += " [truncated; inspect child session for full details]"
			}
		}

		lineRunes := len([]rune(line))
		if remaining <= 0 {
			truncatedTail = true
			break
		}
		if lineRunes > remaining {
			clipped, _ := ellipsizeText(line, remaining)
			if clipped != "" {
				lines = append(lines, clipped)
			}
			truncatedTail = true
			remaining = 0
			break
		}

		lines = append(lines, line)
		remaining -= lineRunes
	}

	if len(lines) == 0 {
		return ""
	}
	if truncatedTail {
		lines = append(lines, "- … additional task output omitted to stay within context budget")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func (c *coordinator) subAgentErrorText(ctx context.Context, sessionID string, runErr error) string {
	if c.messages != nil {
		msgs, err := c.messages.List(ctx, sessionID)
		if err == nil {
			for i := len(msgs) - 1; i >= 0; i-- {
				msg := msgs[i]
				if msg.Role != message.Assistant || msg.IsSummaryMessage {
					continue
				}
				if finish := msg.FinishPart(); finish != nil && finish.Reason == message.FinishReasonError {
					switch {
					case strings.TrimSpace(finish.Details) != "":
						return strings.TrimSpace(finish.Details)
					case strings.TrimSpace(finish.Message) != "":
						return strings.TrimSpace(finish.Message)
					}
				}
			}
		} else {
			slog.Warn("Failed to load sub-agent messages for error fallback", "error", err, "session", sessionID)
		}
	}
	if runErr == nil {
		return "error generating response"
	}
	return strings.TrimSpace(runErr.Error())
}

// updateParentSessionCost accumulates the cost from a child session to its parent session.
func (c *coordinator) updateParentSessionCost(ctx context.Context, childSessionID, parentSessionID string) error {
	return c.updateParentSessionCostDelta(ctx, childSessionID, parentSessionID, 0)
}

func (c *coordinator) updateParentSessionCostDelta(ctx context.Context, childSessionID, parentSessionID string, previousChildCost float64) error {
	childSession, err := c.sessions.Get(ctx, childSessionID)
	if err != nil {
		return fmt.Errorf("get child session: %w", err)
	}

	parentSession, err := c.sessions.Get(ctx, parentSessionID)
	if err != nil {
		return fmt.Errorf("get parent session: %w", err)
	}

	delta := childSession.Cost - previousChildCost
	if delta <= 0 {
		return nil
	}
	parentSession.Cost += delta

	if _, err := c.sessions.Save(ctx, parentSession); err != nil {
		return fmt.Errorf("save parent session: %w", err)
	}

	return nil
}

const (
	// subagentHandoffMaxReasoningSteps caps how many recent assistant text
	// snippets are included in the handoff summary.
	subagentHandoffMaxReasoningSteps = 3
	// subagentHandoffMaxCharsPerStep caps each reasoning snippet.
	subagentHandoffMaxCharsPerStep = 500
	// subagentHandoffMaxToolActions caps the number of distinct tool call
	// summaries included in the handoff.
	subagentHandoffMaxToolActions = 5
	// subagentHandoffMaxUserChars caps the original user request snippet.
	subagentHandoffMaxUserChars = 300
	// subagentHandoffToolInputChars caps each tool call input summary.
	subagentHandoffToolInputChars = 80
)

// buildSubagentHandoffSummary builds a structured <parent_context> block from
// the parent session's messages. It includes three optional sections:
//   - <original_request>: the first user message (truncated)
//   - <recent_reasoning>: the last N assistant text snippets (thinking stripped)
//   - <key_actions>: summaries of recent tool calls (deduplicated by name)
//
// Returns "" when there is nothing meaningful to inject.
func (c *coordinator) buildSubagentHandoffSummary(ctx context.Context, parentSessionID string) string {
	if c.messages == nil {
		return ""
	}
	msgs, err := c.messages.List(ctx, parentSessionID)
	if err != nil || len(msgs) == 0 {
		return ""
	}

	// 1. Original request: first user message.
	originalRequest := extractOriginalRequest(msgs, subagentHandoffMaxUserChars)

	// 2. Recent reasoning: last N assistant text snippets (thinking stripped).
	snippets := extractRecentReasoning(msgs, subagentHandoffMaxReasoningSteps, subagentHandoffMaxCharsPerStep)

	// 3. Key actions: recent tool call summaries (deduplicated by name).
	actions := extractKeyActions(msgs, subagentHandoffMaxToolActions, subagentHandoffToolInputChars)

	if originalRequest == "" && len(snippets) == 0 && len(actions) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("<parent_context>\n")
	sb.WriteString("Context from the orchestrating agent's session. Use this to understand\n")
	sb.WriteString("what has already been discovered and done.\n")

	if originalRequest != "" {
		sb.WriteString("\n<original_request>\n")
		sb.WriteString(originalRequest)
		sb.WriteString("\n</original_request>\n")
	}

	if len(snippets) > 0 {
		sb.WriteString("\n<recent_reasoning>\n")
		for i, s := range snippets {
			fmt.Fprintf(&sb, "%d. %s\n", i+1, s)
		}
		sb.WriteString("</recent_reasoning>\n")
	}

	if len(actions) > 0 {
		sb.WriteString("\n<key_actions>\n")
		for _, a := range actions {
			fmt.Fprintf(&sb, "- %s\n", a)
		}
		sb.WriteString("</key_actions>\n")
	}

	sb.WriteString("</parent_context>\n\n")
	return sb.String()
}

// extractOriginalRequest returns the first user message text, truncated to
// maxChars. Returns "" if no user message is found.
func extractOriginalRequest(msgs []message.Message, maxChars int) string {
	for i := range msgs {
		if msgs[i].Role != message.User || msgs[i].IsSummaryMessage {
			continue
		}
		text := strings.TrimSpace(msgs[i].Content().Text)
		if text == "" {
			continue
		}
		runes := []rune(text)
		if len(runes) > maxChars {
			text = string(runes[:maxChars]) + "…"
		}
		return text
	}
	return ""
}

// extractRecentReasoning collects the last maxSteps assistant text snippets,
// stripping extended-thinking blocks and truncating each to maxChars.
// Results are returned in chronological order.
func extractRecentReasoning(msgs []message.Message, maxSteps, maxChars int) []string {
	var snippets []string
	for i := len(msgs) - 1; i >= 0 && len(snippets) < maxSteps; i-- {
		msg := msgs[i]
		if msg.Role != message.Assistant || msg.IsSummaryMessage {
			continue
		}
		text := strings.TrimSpace(msg.Content().Text)
		text = thinkTagRegex.ReplaceAllString(text, "")
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		runes := []rune(text)
		if len(runes) > maxChars {
			text = string(runes[:maxChars]) + "…"
		}
		snippets = append([]string{text}, snippets...) // maintain chronological order
	}
	return snippets
}

// extractKeyActions collects recent tool call summaries, deduplicated by tool
// name (keeping the most recent occurrence). Each summary is formatted as
// "tool_name(input_summary)" where input_summary is the first maxInputChars
// of the JSON input.
func extractKeyActions(msgs []message.Message, maxActions, maxInputChars int) []string {
	seen := make(map[string]int) // tool name → index in actions slice
	var actions []string
	// Iterate forward: later (newer) occurrences replace earlier ones via
	// the seen map, ensuring the most recent call per tool name is kept.
	for i := range msgs {
		msg := msgs[i]
		if msg.Role != message.Assistant || msg.IsSummaryMessage {
			continue
		}
		for _, tc := range msg.ToolCalls() {
			if tc.Name == "" {
				continue
			}
			summary := formatToolCallSummary(tc.Name, tc.Input, maxInputChars)
			if prevIdx, exists := seen[tc.Name]; exists {
				// Replace older entry with this newer occurrence.
				actions[prevIdx] = summary
			} else {
				seen[tc.Name] = len(actions)
				actions = append(actions, summary)
			}
		}
	}
	// Keep only the last maxActions entries (most recent).
	if len(actions) > maxActions {
		actions = actions[len(actions)-maxActions:]
	}
	return actions
}

// formatToolCallSummary produces a one-line summary of a tool call:
// "tool_name(input_summary)" where input is truncated to maxInputChars.
func formatToolCallSummary(name, input string, maxInputChars int) string {
	input = strings.TrimSpace(input)
	if input == "" {
		return name
	}
	// Compact JSON: remove newlines and excess whitespace.
	input = strings.ReplaceAll(input, "\n", " ")
	runes := []rune(input)
	if len(runes) > maxInputChars {
		input = string(runes[:maxInputChars]) + "…"
	}
	return fmt.Sprintf("%s(%s)", name, input)
}

// assembleSubagentPrompt combines auto-extracted parent context and
// user-provided shared context into a single <parent_context> block, then
// appends the task assignment. If both contexts are empty, it returns only
// the assignment.
func assembleSubagentPrompt(parentContext, sharedContext, assignment string) string {
	parentContext = strings.TrimSpace(parentContext)
	sharedContext = strings.TrimSpace(sharedContext)

	if parentContext == "" && sharedContext == "" {
		return assignment
	}

	var sb strings.Builder

	// If parentContext already contains a <parent_context> block, strip the
	// closing tag so we can merge shared context inside the same block.
	if parentContext != "" && strings.HasPrefix(parentContext, "<parent_context>") {
		trimmed := strings.TrimPrefix(parentContext, "<parent_context>")
		trimmed = strings.TrimSuffix(trimmed, "</parent_context>")
		sb.WriteString("<parent_context>")
		sb.WriteString(trimmed)
		if sharedContext != "" {
			sb.WriteString("\n<shared_context>\n")
			sb.WriteString(sharedContext)
			sb.WriteString("\n</shared_context>\n")
		}
		sb.WriteString("</parent_context>\n\n")
	} else if parentContext != "" {
		// parentContext is raw (not yet wrapped).
		sb.WriteString("<parent_context>\n")
		sb.WriteString(parentContext)
		if sharedContext != "" {
			sb.WriteString("\n\n<shared_context>\n")
			sb.WriteString(sharedContext)
			sb.WriteString("\n</shared_context>\n")
		}
		sb.WriteString("</parent_context>\n\n")
	} else {
		// Only sharedContext is present.
		sb.WriteString("<parent_context>\n<shared_context>\n")
		sb.WriteString(sharedContext)
		sb.WriteString("\n</shared_context>\n</parent_context>\n\n")
	}

	sb.WriteString(assignment)
	return sb.String()
}

const (
	// subagentHistoryMaxMessages caps messages included in the history index.
	subagentHistoryMaxMessages = 30
	// subagentHistoryMaxCharsPerMsg caps each message snippet in the index.
	subagentHistoryMaxCharsPerMsg = 200
	// subagentHistoryDirName is the subdirectory under ProjectDataDir for
	// temporary parent session history files.
	subagentHistoryDirName = "subagent-handoffs"
)

// writeParentHistoryFile exports a condensed index of the parent session's
// conversation as a lightweight Markdown file that subagents can read for
// deep context inspection. Returns "" on any error (graceful degradation).
func (c *coordinator) writeParentHistoryFile(ctx context.Context, parentSessionID string) string {
	if c.messages == nil {
		return ""
	}
	msgs, err := c.messages.List(ctx, parentSessionID)
	if err != nil || len(msgs) == 0 {
		return ""
	}

	projectDataDir := ""
	if c.cfg != nil {
		projectDataDir = strings.TrimSpace(c.cfg.ProjectDataDir())
	}
	if projectDataDir == "" {
		return ""
	}

	historyDir := filepath.Join(projectDataDir, subagentHistoryDirName)
	if err := os.MkdirAll(historyDir, 0o755); err != nil {
		slog.Warn("Failed to create subagent handoffs directory", "path", historyDir, "error", err)
		return ""
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "# Parent Session History\n")
	fmt.Fprintf(&sb, "Session: %s\n\n", parentSessionID)

	// Take the last N messages.
	start := 0
	if len(msgs) > subagentHistoryMaxMessages {
		start = len(msgs) - subagentHistoryMaxMessages
	}
	for i := start; i < len(msgs); i++ {
		msg := msgs[i]
		if msg.IsSummaryMessage {
			continue
		}
		line := formatHistoryLine(msg, subagentHistoryMaxCharsPerMsg)
		if line != "" {
			sb.WriteString(line)
			sb.WriteString("\n")
		}
	}

	historyPath := filepath.Join(historyDir, fmt.Sprintf("%s.md", parentSessionID))
	if err := os.WriteFile(historyPath, []byte(sb.String()), 0o644); err != nil {
		slog.Warn("Failed to write parent history file", "path", historyPath, "error", err)
		return ""
	}
	return historyPath
}

// formatHistoryLine produces a single-line summary of a message for the
// history index file.
func formatHistoryLine(msg message.Message, maxChars int) string {
	if msg.IsSummaryMessage {
		return ""
	}
	switch msg.Role {
	case message.User:
		text := strings.TrimSpace(msg.Content().Text)
		if text == "" {
			return ""
		}
		return formatHistoryEntry("User", text, maxChars)
	case message.Assistant:
		// Include text reasoning (thinking stripped).
		text := strings.TrimSpace(msg.Content().Text)
		text = thinkTagRegex.ReplaceAllString(text, "")
		text = strings.TrimSpace(text)
		line := ""
		if text != "" {
			line = formatHistoryEntry("Assistant", text, maxChars)
		}
		// Append tool call summaries.
		for _, tc := range msg.ToolCalls() {
			if tc.Name == "" {
				continue
			}
			summary := formatToolCallSummary(tc.Name, tc.Input, subagentHandoffToolInputChars)
			entry := fmt.Sprintf("[ToolCall] %s", summary)
			if line != "" {
				line += "\n" + entry
			} else {
				line = entry
			}
		}
		return line
	case message.Tool:
		for _, tr := range msg.ToolResults() {
			if tr.Name == "" {
				continue
			}
			content := strings.TrimSpace(tr.Content)
			if content == "" {
				return fmt.Sprintf("[ToolResult] %s: (empty)", tr.Name)
			}
			return formatHistoryEntry("ToolResult:"+tr.Name, content, maxChars)
		}
		return ""
	default:
		return ""
	}
}

// formatHistoryEntry formats a labeled history line with truncation.
func formatHistoryEntry(label, text string, maxChars int) string {
	runes := []rune(text)
	if len(runes) > maxChars {
		text = string(runes[:maxChars]) + "…"
	}
	return fmt.Sprintf("[%s] %s", label, text)
}

// cleanupParentHistoryFile removes a temporary parent history file.
func (c *coordinator) cleanupParentHistoryFile(filePath string) {
	if filePath == "" {
		return
	}
	if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
		slog.Warn("Failed to remove parent history file", "path", filePath, "error", err)
	}
}

func (c *coordinator) subAgentResponseText(ctx context.Context, sessionID string, result *fantasy.AgentResult) string {
	if result != nil && result.Response.Content != nil {
		if text := strings.TrimSpace(result.Response.Content.Text()); text != "" {
			return modelSafeSubAgentText(text, sessionID)
		}
	}

	if c.messages == nil {
		return ""
	}

	msgs, err := c.messages.List(ctx, sessionID)
	if err != nil {
		slog.Warn("Failed to load sub-agent messages for response fallback", "error", err, "session", sessionID)
		return ""
	}

	for i := len(msgs) - 1; i >= 0; i-- {
		msg := msgs[i]
		if msg.Role != message.Assistant || msg.IsSummaryMessage {
			continue
		}
		text := strings.TrimSpace(msg.Content().Text)
		if text == "" {
			if msg.FinishReason() == message.FinishReasonEndTurn {
				return ""
			}
			continue
		}
		return modelSafeSubAgentText(text, sessionID)
	}

	return ""
}

func subAgentNoContentText(sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return "Subagent completed with no textual response. Open the child session from this Agent tool call to inspect tool outputs and details."
	}
	return fmt.Sprintf("Subagent completed with no textual response. Open child session %s from this Agent tool call to inspect tool outputs and details.", sessionID)
}

// backgroundAgentLookup returns a lookup function for background agent status.
func (c *coordinator) backgroundAgentLookup() toolruntime.BackgroundAgentLookup {
	return func(agentAddress string) (status, content, childSessionID string, found bool) {
		resolvedID, ok := c.backgroundAgents.ResolveAddress(strings.TrimSpace(agentAddress))
		if !ok {
			return "", "", "", false
		}
		entry, ok := c.backgroundAgents.Get(resolvedID)
		if !ok {
			return "", "", "", false
		}
		return string(entry.Status), entry.Content, entry.ChildSessionID, true
	}
}

func (c *coordinator) backgroundAgentMessenger() toolruntime.BackgroundAgentMessenger {
	return func(ctx context.Context, agentAddress, prompt string) (string, bool, error) {
		resolvedID, ok := c.backgroundAgents.ResolveAddress(strings.TrimSpace(agentAddress))
		if !ok {
			return "", false, nil
		}
		entry, ok := c.backgroundAgents.Get(resolvedID)
		if !ok {
			return "", false, nil
		}
		depth, err := c.backgroundAgents.Enqueue(resolvedID, backgroundAgentCommand{
			Prompt:         strings.TrimSpace(prompt),
			SessionID:      tools.GetSessionFromContext(ctx),
			AgentMessageID: tools.GetMessageFromContext(ctx),
			ToolCallID:     tools.GetToolCallIDFromContext(ctx),
		})
		if err != nil {
			return "", true, err
		}
		if entry.Status == backgroundAgentStatusRunning || depth > 1 {
			return "queued", true, nil
		}
		return "started", true, nil
	}
}

func tryLoadMentalModels(ctx context.Context, retriever engine.Retriever, ttl time.Duration, maxWait time.Duration) {
	lr, ok := retriever.(engine.MentalModelsProvider)
	if !ok {
		return
	}

	if !lr.MentalModelsLoadedAt().IsZero() && time.Since(lr.MentalModelsLoadedAt()) < ttl {
		return
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := lr.LoadMentalModels(ctx); err != nil {
			slog.Debug("Failed to load Hindsight mental models", "error", err)
		}
	}()

	if maxWait > 0 {
		timer := time.NewTimer(maxWait)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			slog.Debug("Mental models load exceeded wait timeout, continuing in background", "timeout", maxWait)
		}
	}
}
