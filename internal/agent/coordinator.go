package agent

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
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
	"github.com/charmbracelet/crush/internal/agent/hyper"
	"github.com/charmbracelet/crush/internal/agent/mailbox"
	"github.com/charmbracelet/crush/internal/agent/notify"
	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/agent/reducer"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/checkpoint"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/filetracker"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/hooks"
	"github.com/charmbracelet/crush/internal/httpext"
	"github.com/charmbracelet/crush/internal/log"
	"github.com/charmbracelet/crush/internal/lsp"
	"github.com/charmbracelet/crush/internal/memory/engine"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/oauth/copilot"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/plugin"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/timeline"
	"github.com/charmbracelet/crush/internal/toolruntime"
	"github.com/charmbracelet/crush/internal/userinput"
	"golang.org/x/sync/errgroup"

	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/azure"
	"charm.land/fantasy/providers/bedrock"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/providers/openrouter"
	"charm.land/fantasy/providers/vercel"
	openaisdk "github.com/charmbracelet/openai-go/option"
	"github.com/qjebbs/go-jsons"
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
	notify        pubsub.Publisher[notify.Notification]
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

	// memoryEngine is the event-sourced memory pipeline orchestrator.
	memoryEngine *engine.Engine

	// agentRegistry tracks all running agents for IRC peer discovery.
	agentRegistry *AgentRegistry

	// mainAgentID is the ID of the main (coder) agent in the registry.
	mainAgentID string

	// transcriptTurnCounts tracks turn counts per session for transcript backend.
	transcriptTurnCounts   map[string]int
	transcriptTurnCountsMu sync.Mutex
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
	notify pubsub.Publisher[notify.Notification],
	toolRuntime toolruntime.Service,
	timeline timeline.Service,
	pluginRuntime *plugin.Runtime,
	memoryEngine *engine.Engine,
) (Coordinator, error) {
	hookMgr, err := hooks.NewManager(cfg.Config().Hooks)
	if err != nil {
		slog.Warn("Failed to initialize hook manager, hooks will be disabled", "error", err)
		hookMgr = nil
	} else {
		slog.Debug("Hook manager initialized", "hooks_count", len(cfg.Config().Hooks))
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
	}
	if c.pluginRuntime == nil {
		c.pluginRuntime = plugin.DefaultRuntime()
	}
	if memoryEngine != nil {
		c.SetMemoryEngine(memoryEngine)
	}
	agentCfg, ok := cfg.Config().Agents[config.AgentCoder]
	if !ok {
		return nil, errCoderAgentNotConfigured
	}

	// TODO: make this dynamic when we support multiple agents
	prompt, err := promptForAgent(agentCfg, false, prompt.WithWorkingDir(c.cfg.WorkingDir()))
	if err != nil {
		return nil, err
	}

	agent, err := c.buildAgent(ctx, prompt, agentCfg, false)
	if err != nil {
		return nil, err
	}
	c.currentAgent = agent
	c.agents[config.AgentCoder] = agent

	c.agentRegistry = GlobalAgentRegistry()
	c.mainAgentID = "0-Main"
	c.agentRegistry.Register(AgentRef{
		ID:          c.mainAgentID,
		DisplayName: "Main",
		Kind:        AgentKindMain,
		Status:      AgentStatusIdle,
		Agent:       agent,
	})

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

// SetMemoryEngine attaches the memory engine to the coordinator.
// If the engine is enabled, the episodic memory extractor and consolidator
// are wired automatically for the local backend only. The hindsight backend
// delegates extraction and consolidation to the remote Hindsight service, so
// running them locally would be redundant and wasteful.
func (c *coordinator) SetMemoryEngine(eng *engine.Engine) {
	c.memoryEngine = eng
	if eng != nil && eng.Enabled() {
		if eng.Backend() != "hindsight" {
			c.wireMemoryExtractor(eng)
			c.wireMemoryConsolidator(eng)
		}
	}
}

// MemoryEngine returns the attached memory engine, if any.
func (c *coordinator) MemoryEngine() *engine.Engine {
	return c.memoryEngine
}

// memoryEngineEventStore returns the engine's EventStore, or nil if the
// engine is not configured or disabled.
func (c *coordinator) memoryEngineEventStore() engine.EventStore {
	if c.memoryEngine == nil || !c.memoryEngine.Enabled() {
		return nil
	}
	return c.memoryEngine.EventStore()
}

// memoryEngineRetriever returns the engine's Retriever, or nil if the
// engine is not configured or disabled.
func (c *coordinator) memoryEngineRetriever() engine.Retriever {
	if c.memoryEngine == nil || !c.memoryEngine.Enabled() {
		return nil
	}
	return c.memoryEngine.Retriever()
}

// transcriptAfterTurn handles transcript window retention for the transcript backend.
// It increments the turn counter and retains the transcript window every N turns.
func (c *coordinator) transcriptAfterTurn(ctx context.Context, sessionID string) {
	if c.memoryEngine == nil || c.memoryEngine.TranscriptRetainer() == nil {
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
	retainer := c.memoryEngine.TranscriptRetainer()
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

// onSessionClosed cleans up coordinator and memory-engine state for a session.
func (c *coordinator) onSessionClosed(ctx context.Context, sessionID string) {
	c.clearTranscriptTurnCountForSession(sessionID)
	if c.memoryEngine != nil {
		if err := c.memoryEngine.OnSessionClosed(ctx, sessionID); err != nil {
			slog.Warn("Memory engine OnSessionClosed failed", "error", err, "session_id", sessionID)
		}
	}
}

// memoryEngineHooks returns lifecycle callbacks for the session agent
// when the memory engine is enabled. Returns nil when the engine is
// disabled or not configured.
func (c *coordinator) memoryEngineHooks() *MemoryEngineHooks {
	if c.memoryEngine == nil || !c.memoryEngine.Enabled() {
		return nil
	}
	return &MemoryEngineHooks{
		OnBeforeCompaction: func(ctx context.Context, sessionID string) string {
			if c.memoryEngine.Backend() == "hindsight" {
				c.transcriptAfterTurn(ctx, sessionID)
				return ""
			}
			if err := c.memoryEngine.OnBeforeCompaction(ctx, sessionID); err != nil {
				slog.Warn("Memory engine OnBeforeCompaction failed", "error", err, "session_id", sessionID)
			}
			// Build compaction rescue payload to inject into summary prompt.
			memCfg := c.compactionRecallConfig()
			if memCfg == nil || !memCfg.IsEnabled() {
				return ""
			}
			rescue, rescueErr := c.memoryEngine.PrepareCompactionRescue(ctx, sessionID, engine.CompactionRescueOptions{
				TopK:        memCfg.GetTopK(),
				MaxBytes:    memCfg.GetMaxBytes(),
				UseReranker: memCfg.GetUseRerank(),
			})
			if rescueErr != nil {
				slog.Warn("Compaction rescue preparation failed", "error", rescueErr, "session_id", sessionID)
				return ""
			}
			if rescue == nil {
				return ""
			}
			return rescue.Rendered
		},
		OnSessionClosed: func(ctx context.Context, sessionID string) {
			c.onSessionClosed(ctx, sessionID)
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
			for _, line := range strings.Split(content, "\n") {
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

// buildRecentConversation extracts the last N user/assistant message turns
// from the session history to provide context for short-query expansion.
// This mirrors claude-code's approach of enriching terse prompts like
// "continue" or "fix it" with surrounding conversation context.
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

	supportsImages, supportsImagesErr := c.resolveCoderModelSupportsImages()
	if supportsImagesErr != nil {
		slog.Warn("Failed to resolve model image support; keeping original attachments", "error", supportsImagesErr, "session_id", sessionID)
		supportsImages = true
	}
	filteredAttachments := filterAttachmentsForModelSupport(attachments, supportsImages)
	parts := []message.ContentPart{message.TextContent{Text: prompt}}
	for _, attachment := range filteredAttachments {
		parts = append(parts, message.BinaryContent{Path: attachment.FilePath, MIMEType: attachment.MimeType, Data: attachment.Content})
	}
	userMessage, err := c.messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: parts,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create user message: %w", err)
	}
	slog.Debug("[PERF] coordinator: created user message", "duration", time.Since(start), "session_id", sessionID)

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
			if c.memoryEngine != nil && c.memoryEngine.Enabled() {
				retriever := c.memoryEngine.Retriever()
				recall = buildAutoRecallBlock(prefetchCtx, retriever, sessionID)
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

	run := func() (*fantasy.AgentResult, error) {
		slog.Debug("[PERF] coordinator: starting sessionAgent.Run", "duration", time.Since(start), "session_id", sessionID)
		return c.currentAgent.Run(ctx, SessionAgentCall{
			SessionID:        sessionID,
			Prompt:           prompt,
			Attachments:      filteredAttachments,
			MaxOutputTokens:  maxTokens,
			ProviderOptions:  runtimeConfig.ProviderOptions,
			Temperature:      runtimeConfig.Temperature,
			TopP:             runtimeConfig.TopP,
			TopK:             runtimeConfig.TopK,
			FrequencyPenalty: runtimeConfig.FrequencyPenalty,
			PresencePenalty:  runtimeConfig.PresencePenalty,
			UserMessage:      &userMessage,
			MemoryPrefetch:   memoryPrefetch,
		})
	}
	// Call engine OnSessionCreated for first-turn initialization.
	if c.memoryEngine != nil && c.memoryEngine.Enabled() {
		c.memoryEngine.OnSessionCreated(ctx, sessionID)
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
		if c.memoryEngine != nil && c.memoryEngine.Enabled() {
			if c.memoryEngine.Backend() == "hindsight" {
				c.transcriptAfterTurn(ctx, sessionID)
			} else {
				slog.Debug("Memory engine enabled, calling AfterTurnIdle")
				if err := c.memoryEngine.AfterTurnIdle(context.Background(), sessionID, nil); err != nil {
					slog.Warn("Memory engine AfterTurnIdle failed", "error", err, "session_id", sessionID)
				}
			}
		}
	}

	return result, originalErr
}

func getProviderOptions(model Model, providerCfg config.ProviderConfig, agentCfg ...config.Agent) fantasy.ProviderOptions {
	options := fantasy.ProviderOptions{}

	cfgOpts := []byte("{}")
	providerCfgOpts := []byte("{}")
	catwalkOpts := []byte("{}")

	if model.CatwalkCfg.Options.ProviderOptions != nil {
		data, err := json.Marshal(model.CatwalkCfg.Options.ProviderOptions)
		if err == nil {
			catwalkOpts = data
		}
	}

	if providerCfg.ProviderOptions != nil {
		data, err := json.Marshal(providerCfg.ProviderOptions)
		if err == nil {
			providerCfgOpts = data
		}
	}

	if model.ModelCfg.ProviderOptions != nil {
		data, err := json.Marshal(model.ModelCfg.ProviderOptions)
		if err == nil {
			cfgOpts = data
		}
	}

	readers := []io.Reader{
		bytes.NewReader(catwalkOpts),
		bytes.NewReader(providerCfgOpts),
		bytes.NewReader(cfgOpts),
	}

	got, err := jsons.Merge(readers)
	if err != nil {
		slog.Error("Could not merge call config", "err", err)
		return options
	}

	mergedOptions := make(map[string]any)

	err = json.Unmarshal([]byte(got), &mergedOptions)
	if err != nil {
		slog.Error("Could not create config for call", "err", err)
		return options
	}

	providerType := providerCfg.Type
	if providerType == "hyper" {
		if strings.Contains(model.CatwalkCfg.ID, "claude") {
			providerType = anthropic.Name
		} else if strings.Contains(model.CatwalkCfg.ID, "gpt") {
			providerType = openai.Name
		} else if strings.Contains(model.CatwalkCfg.ID, "gemini") {
			providerType = google.Name
		} else {
			providerType = openaicompat.Name
		}
	}

	// Reasoning effort: use agent config if set, then user selection,
	// then fall back to model's default.
	reasoningEffort := model.CatwalkCfg.DefaultReasoningEffort
	for _, a := range agentCfg {
		if strings.TrimSpace(a.ReasoningEffort) != "" {
			reasoningEffort = a.ReasoningEffort
			break
		}
	}

	switch providerType {
	case openai.Name, azure.Name:
		thinkingDisabled := model.ModelCfg.Think != nil && !*model.ModelCfg.Think
		if thinkingDisabled {
			// Explicitly disabled: clear any reasoning params from provider config too.
			delete(mergedOptions, "reasoning_effort")
		} else {
			_, hasReasoningEffort := mergedOptions["reasoning_effort"]
			if !hasReasoningEffort && model.CatwalkCfg.CanReason {
				// Default: enable reasoning for models that support it.
				if reasoningEffort != "" {
					mergedOptions["reasoning_effort"] = reasoningEffort
				} else {
					mergedOptions["reasoning_effort"] = "high"
				}
			}
		}
		if openai.IsResponsesModel(model.CatwalkCfg.ID) {
			if thinkingDisabled {
				// Clear Responses API reasoning params from provider config.
				delete(mergedOptions, "reasoning_summary")
				delete(mergedOptions, "include")
			} else if openai.IsResponsesReasoningModel(model.CatwalkCfg.ID) {
				_, hasSummary := mergedOptions["reasoning_summary"]
				if !hasSummary {
					mergedOptions["reasoning_summary"] = "auto"
				}
				_, hasInclude := mergedOptions["include"]
				if !hasInclude {
					mergedOptions["include"] = []openai.IncludeType{openai.IncludeReasoningEncryptedContent}
				}
			}
			parsed, err := openai.ParseResponsesOptions(mergedOptions)
			if err == nil {
				options[openai.Name] = parsed
			}
		} else {
			parsed, err := openai.ParseOptions(mergedOptions)
			if err == nil {
				options[openai.Name] = parsed
			}
		}
	case anthropic.Name:
		// Map reasoning effort to Anthropic parameters.
		//
		// Claude 4.6+ (claude-sonnet-4.6, claude-opus-4.6, claude-opus-4-7, etc.)
		// supports the "effort" parameter which enables adaptive thinking. The
		// fantasy SDK converts effort → thinking: {type: "adaptive"} automatically.
		//
		// Older Claude models use the legacy thinking: {type: "enabled", budget_tokens}.
		//
		// Default behavior: if the model supports reasoning (CanReason), enable thinking
		// by default. Users can override via Think=false to disable.
		thinkingDisabled := model.ModelCfg.Think != nil && !*model.ModelCfg.Think
		if thinkingDisabled {
			// Explicitly disabled: clear any thinking params from provider config too.
			delete(mergedOptions, "effort")
			delete(mergedOptions, "thinking")
		} else {
			_, hasEffort := mergedOptions["effort"]
			_, hasThinking := mergedOptions["thinking"]
			if !hasEffort && !hasThinking && model.CatwalkCfg.CanReason {
				isClaude46 := requiresAdaptiveThinking(model.CatwalkCfg.ID)
				switch {
				case reasoningEffort != "":
					if isClaude46 {
						// Claude 4.6+: use effort parameter (adaptive thinking)
						mergedOptions["effort"] = reasoningEffort
					} else {
						// Older Claude: use budget_tokens
						budgetTokens := effortToBudgetTokens(reasoningEffort)
						mergedOptions["thinking"] = map[string]any{
							"type":          "enabled",
							"budget_tokens": budgetTokens,
						}
					}
				default:
					// Default: model supports reasoning, enable thinking with high effort.
					if isClaude46 {
						mergedOptions["effort"] = "high"
					} else {
						mergedOptions["thinking"] = map[string]any{
							"type":          "enabled",
							"budget_tokens": effortToBudgetTokens("high"),
						}
					}
				}
			}
		}
		parsed, err := anthropic.ParseOptions(mergedOptions)
		if err == nil {
			options[anthropic.Name] = parsed
		}

	case openrouter.Name:
		thinkingDisabled := model.ModelCfg.Think != nil && !*model.ModelCfg.Think
		if thinkingDisabled {
			delete(mergedOptions, "reasoning")
		} else {
			_, hasReasoning := mergedOptions["reasoning"]
			if !hasReasoning && model.CatwalkCfg.CanReason {
				// Default: enable reasoning for models that support it.
				if reasoningEffort != "" {
					mergedOptions["reasoning"] = map[string]any{
						"enabled": true,
						"effort":  reasoningEffort,
					}
				} else {
					mergedOptions["reasoning"] = map[string]any{
						"enabled": true,
						"effort":  "high",
					}
				}
			}
		}
		parsed, err := openrouter.ParseOptions(mergedOptions)
		if err == nil {
			options[openrouter.Name] = parsed
		}
	case vercel.Name:
		thinkingDisabled := model.ModelCfg.Think != nil && !*model.ModelCfg.Think
		if thinkingDisabled {
			delete(mergedOptions, "reasoning")
		} else {
			_, hasReasoning := mergedOptions["reasoning"]
			if !hasReasoning && model.CatwalkCfg.CanReason {
				// Default: enable reasoning for models that support it.
				if reasoningEffort != "" {
					mergedOptions["reasoning"] = map[string]any{
						"enabled": true,
						"effort":  reasoningEffort,
					}
				} else {
					mergedOptions["reasoning"] = map[string]any{
						"enabled": true,
						"effort":  "high",
					}
				}
			}
		}
		parsed, err := vercel.ParseOptions(mergedOptions)
		if err == nil {
			options[vercel.Name] = parsed
		}
	case google.Name:
		thinkingDisabled := model.ModelCfg.Think != nil && !*model.ModelCfg.Think
		if thinkingDisabled {
			delete(mergedOptions, "thinking_config")
		} else {
			_, hasThinkingConfig := mergedOptions["thinking_config"]
			if !hasThinkingConfig && model.CatwalkCfg.CanReason {
				// Default: enable thinking for models that support it.
				if reasoningEffort != "" {
					mergedOptions["thinking_config"] = map[string]any{
						"thinking_level":   reasoningEffort,
						"include_thoughts": true,
					}
				} else {
					mergedOptions["thinking_config"] = map[string]any{
						"thinking_level":   "high",
						"include_thoughts": true,
					}
				}
			}
		}
		parsed, err := google.ParseOptions(mergedOptions)
		if err == nil {
			options[google.Name] = parsed
		}
	case openaicompat.Name:
		thinkingDisabled := model.ModelCfg.Think != nil && !*model.ModelCfg.Think
		if thinkingDisabled {
			delete(mergedOptions, "reasoning_effort")
		} else {
			_, hasReasoningEffort := mergedOptions["reasoning_effort"]
			if !hasReasoningEffort && model.CatwalkCfg.CanReason {
				// Default: enable reasoning for models that support it.
				if reasoningEffort != "" {
					mergedOptions["reasoning_effort"] = reasoningEffort
				} else {
					mergedOptions["reasoning_effort"] = "high"
				}
			}
		}
		parsed, err := openaicompat.ParseOptions(mergedOptions)
		if err == nil {
			options[openaicompat.Name] = parsed
		}
	}

	return options
}

func effortToBudgetTokens(effort string) int {
	// Budget tokens chosen to produce the correct reasoning_effort when translated by Copilot API
	budgetMap := map[string]int{
		"low":    2048,  // Will map to "low" in Copilot (1024 <= budget < 8192)
		"medium": 12288, // Will map to "medium" in Copilot (8192 <= budget < 24576)
		"high":   28672, // Will map to "high" in Copilot (24576 <= budget < 32768)
		"max":    49152, // Will map to "xhigh" in Copilot (>= 32768)
	}

	budget, ok := budgetMap[effort]
	if !ok {
		budget = 12288 // default to medium
	}

	return budget
}

// requiresAdaptiveThinking returns true for Claude models version 4.6 and above which
// require the "effort" parameter (adaptive thinking) instead of the legacy
// thinking: {type: "enabled", budget_tokens: N}.
func requiresAdaptiveThinking(modelID string) bool {
	id := strings.ToLower(modelID)

	// For provider-prefixed model IDs (e.g., "anthropic/claude-sonnet-4.6"),
	// extract the base model ID by finding the last occurrence of "claude-"
	baseID := id
	if idx := strings.LastIndex(id, "claude-"); idx != -1 && idx > 0 {
		baseID = id[idx:]
	}

	// Match patterns like claude-{variant}-4.N or claude-{variant}-4-N where N >= 6
	for _, variant := range []string{"sonnet", "opus", "haiku"} {
		prefix := "claude-" + variant + "-4"
		// Check for prefix match (e.g., claude-sonnet-4.6)
		if strings.HasPrefix(baseID, prefix+".") {
			minor := baseID[len(prefix)+1:]
			if n, err := parseLeadingInt(minor); err == nil && n >= 6 {
				return true
			}
		}
		// Check for prefix match with dash (e.g., claude-sonnet-4-6)
		if strings.HasPrefix(baseID, prefix+"-") {
			minor := baseID[len(prefix)+1:]
			if n, err := parseLeadingInt(minor); err == nil && n >= 6 {
				return true
			}
		}
	}
	return false
}

// parseLeadingInt parses the leading integer from a string (stops at first non-digit).
func parseLeadingInt(s string) (int, error) {
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, fmt.Errorf("no digits")
	}
	n := 0
	for i := 0; i < end; i++ {
		n = n*10 + int(s[i]-'0')
	}
	return n, nil
}

func mergeCallOptions(model Model, cfg config.ProviderConfig) (fantasy.ProviderOptions, *float64, *float64, *int64, *float64, *float64) {
	modelOptions := getProviderOptions(model, cfg)
	temp := cmp.Or(model.ModelCfg.Temperature, model.CatwalkCfg.Options.Temperature)
	topP := cmp.Or(model.ModelCfg.TopP, model.CatwalkCfg.Options.TopP)
	topK := cmp.Or(model.ModelCfg.TopK, model.CatwalkCfg.Options.TopK)
	freqPenalty := cmp.Or(model.ModelCfg.FrequencyPenalty, model.CatwalkCfg.Options.FrequencyPenalty)
	presPenalty := cmp.Or(model.ModelCfg.PresencePenalty, model.CatwalkCfg.Options.PresencePenalty)
	return modelOptions, temp, topP, topK, freqPenalty, presPenalty
}

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

func effectiveMaxOutputTokens(model Model) (int64, bool) {
	maxTokens := model.CatwalkCfg.DefaultMaxTokens
	if model.ModelCfg.MaxTokens == 0 {
		return maxTokens, false
	}
	if model.CatwalkCfg.DefaultMaxTokens > 0 && model.ModelCfg.MaxTokens > model.CatwalkCfg.DefaultMaxTokens*2 {
		return model.CatwalkCfg.DefaultMaxTokens, true
	}
	return model.ModelCfg.MaxTokens, false
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
		RefreshCallConfig: func(callCtx context.Context) (sessionAgentRuntimeConfig, error) {
			return c.refreshSessionAgentRuntimeConfig(callCtx, result, prompt, agent, isSubAgent)
		},
		DeferredToolRuntime:  c,
		IsSubAgent:           isSubAgent,
		DisableAutoSummarize: c.cfg.Config().Options.DisableAutoSummarize,
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
		// Session working memory is only useful for the local backend.
		// The hindsight backend retrieves memories from the remote service, so
		// generating local working memory there is pure cost with no benefit.
		EnableSessionMemory:    c.memoryEngine != nil && c.memoryEngine.Enabled() && c.memoryEngine.Backend() != "hindsight",
		MemoryEngineEnabled:    c.memoryEngine != nil && c.memoryEngine.Enabled(),
		MemoryEngineEventStore: c.memoryEngineEventStore(),
		MemoryEngineHooks:      c.memoryEngineHooks(),
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
	currentAgent.SetModels(inferenceModel, small)

	mode, err := c.collaborationModeForContext(ctx)
	if err != nil {
		return sessionAgentRuntimeConfig{}, err
	}
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

	allowedToolNames := make([]string, len(toolSet))
	for i, tool := range toolSet {
		allowedToolNames[i] = tool.Info().Name
	}

	return sessionAgentRuntimeConfig{
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
		AllowedToolNames:   allowedToolNames,
		Tools:              append([]fantasy.AgentTool(nil), toolSet...),
	}, nil
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

	var largeCatwalkModel *catwalk.Model
	var smallCatwalkModel *catwalk.Model

	for i := range largeProviderCfg.Models {
		if largeProviderCfg.Models[i].ID == largeModelCfg.Model {
			largeCatwalkModel = &largeProviderCfg.Models[i]
			break
		}
	}
	for i := range smallProviderCfg.Models {
		if smallProviderCfg.Models[i].ID == smallModelCfg.Model {
			smallCatwalkModel = &smallProviderCfg.Models[i]
			break
		}
	}

	if largeCatwalkModel == nil {
		return Model{}, Model{}, errLargeModelNotFound
	}

	if largeModelCfg.ContextWindow > 0 {
		largeCatwalkModel.ContextWindow = largeModelCfg.ContextWindow
	}
	if largeModelCfg.MaxPromptTokens > 0 {
		if largeCatwalkModel.Options.ProviderOptions == nil {
			largeCatwalkModel.Options.ProviderOptions = map[string]any{}
		}
		largeCatwalkModel.Options.ProviderOptions["max_prompt_tokens"] = largeModelCfg.MaxPromptTokens
	}

	if smallCatwalkModel == nil {
		return Model{}, Model{}, errSmallModelNotFound
	}

	if smallModelCfg.ContextWindow > 0 {
		smallCatwalkModel.ContextWindow = smallModelCfg.ContextWindow
	}
	if smallModelCfg.MaxPromptTokens > 0 {
		if smallCatwalkModel.Options.ProviderOptions == nil {
			smallCatwalkModel.Options.ProviderOptions = map[string]any{}
		}
		smallCatwalkModel.Options.ProviderOptions["max_prompt_tokens"] = smallModelCfg.MaxPromptTokens
	}

	largeThinkingDisabled := largeModelCfg.Think != nil && !*largeModelCfg.Think
	largeProvider, err := c.buildProvider(largeProviderCfg, *largeCatwalkModel, isSubAgent, largeThinkingDisabled)
	if err != nil {
		return Model{}, Model{}, err
	}

	smallThinkingDisabled := smallModelCfg.Think != nil && !*smallModelCfg.Think
	smallProvider, err := c.buildProvider(smallProviderCfg, *smallCatwalkModel, true, smallThinkingDisabled)
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
			CatwalkCfg: *largeCatwalkModel,
			ModelCfg:   largeModelCfg,
		}, Model{
			Model:      smallModel,
			CatwalkCfg: *smallCatwalkModel,
			ModelCfg:   smallModelCfg,
		}, nil
}

func (c *coordinator) buildAnthropicProvider(baseURL, apiKey string, headers map[string]string, providerID string, useCopilotClient, isSubAgent bool) (fantasy.Provider, error) {
	var opts []anthropic.Option

	switch {
	case strings.HasPrefix(apiKey, "Bearer "):
		// NOTE: Prevent the SDK from picking up the API key from env.
		os.Setenv("ANTHROPIC_API_KEY", "")
		headers["Authorization"] = apiKey
	case providerID == string(catwalk.InferenceProviderMiniMax) || providerID == string(catwalk.InferenceProviderMiniMaxChina):
		// NOTE: Prevent the SDK from picking up the API key from env.
		os.Setenv("ANTHROPIC_API_KEY", "")
		headers["Authorization"] = "Bearer " + apiKey
	case apiKey != "":
		// X-Api-Key header
		opts = append(opts, anthropic.WithAPIKey(apiKey))
	}

	if len(headers) > 0 {
		opts = append(opts, anthropic.WithHeaders(headers))
	}

	if baseURL != "" {
		opts = append(opts, anthropic.WithBaseURL(baseURL))
	}

	// Set HTTP client based on provider and debug mode.
	var httpClient *http.Client
	if useCopilotClient {
		httpClient = copilot.NewClient(isSubAgent, c.cfg.Config().Options.Debug)
	} else if c.cfg.Config().Options.Debug {
		httpClient = log.NewHTTPClient()
	}
	// Always wrap so that requests without an explicit `thinking` field have
	// `thinking:{type:"disabled"}` injected. This is a no-op for upstream
	// Anthropic (where omitting thinking already means disabled) but is
	// required for Anthropic-compatible proxies (DeepSeek's /anthropic
	// endpoint, etc.) whose default is ON. It also makes the in-flight
	// "retry without thinking" path actually disable thinking on the wire
	// when the SDK has nilled the typed Thinking option.
	wrapped := httpext.WrapAnthropicDisableThinkingHTTPClient(httpClient)
	opts = append(opts, anthropic.WithHTTPClient(httpext.WrapActivityTrackingHTTPClient(wrapped)))

	return anthropic.New(opts...)
}

func (c *coordinator) buildOpenaiProvider(baseURL, apiKey string, headers map[string]string, copilotService, useCopilotClient, isSubAgent, responsesWebSocket bool) (fantasy.Provider, error) {
	opts := []openai.Option{
		openai.WithAPIKey(apiKey),
		openai.WithUseResponsesAPI(),
	}

	// Set HTTP client based on provider and debug mode.
	var httpClient *http.Client
	if useCopilotClient {
		httpClient = copilot.NewClient(isSubAgent, c.cfg.Config().Options.Debug)
	} else if copilotService {
		// Use billing client for Copilot service.
		httpClient = copilot.NewBillingClient(copilotService, c.cfg.Config().Options.Debug)
	} else if c.cfg.Config().Options.Debug {
		httpClient = log.NewHTTPClient()
	}
	opts = append(opts, openai.WithHTTPClient(wrapOpenAIStreamingHTTPClient(httpClient, responsesWebSocket)))

	if len(headers) > 0 {
		opts = append(opts, openai.WithHeaders(headers))
	}
	if baseURL != "" {
		opts = append(opts, openai.WithBaseURL(baseURL))
	}
	return openai.New(opts...)
}

func wrapOpenAIStreamingHTTPClient(httpClient *http.Client, responsesWebSocket bool) *http.Client {
	if responsesWebSocket {
		httpClient = httpext.WrapOpenAIResponsesWebSocketHTTPClient(httpClient)
	}
	return httpext.WrapActivityTrackingHTTPClient(httpClient)
}

func (c *coordinator) buildOpenrouterProvider(_, apiKey string, headers map[string]string) (fantasy.Provider, error) {
	opts := []openrouter.Option{
		openrouter.WithAPIKey(apiKey),
	}
	var httpClient *http.Client
	if c.cfg.Config().Options.Debug {
		httpClient = log.NewHTTPClient()
	}
	opts = append(opts, openrouter.WithHTTPClient(httpext.WrapActivityTrackingHTTPClient(httpClient)))
	if len(headers) > 0 {
		opts = append(opts, openrouter.WithHeaders(headers))
	}
	return openrouter.New(opts...)
}

func (c *coordinator) buildVercelProvider(_, apiKey string, headers map[string]string) (fantasy.Provider, error) {
	opts := []vercel.Option{
		vercel.WithAPIKey(apiKey),
	}
	var httpClient *http.Client
	if c.cfg.Config().Options.Debug {
		httpClient = log.NewHTTPClient()
	}
	opts = append(opts, vercel.WithHTTPClient(httpext.WrapActivityTrackingHTTPClient(httpClient)))
	if len(headers) > 0 {
		opts = append(opts, vercel.WithHeaders(headers))
	}
	return vercel.New(opts...)
}

func (c *coordinator) buildOpenaiCompatProvider(
	baseURL, apiKey string,
	headers map[string]string,
	extraBody map[string]any,
	providerID string,
	useCopilotClient bool,
	isSubAgent bool,
	copilotService bool,
	responsesWebSocket bool,
) (fantasy.Provider, error) {
	opts := []openaicompat.Option{
		openaicompat.WithBaseURL(baseURL),
		openaicompat.WithAPIKey(apiKey),
	}

	// Set HTTP client based on provider and debug mode.
	var httpClient *http.Client
	if providerID == string(catwalk.InferenceProviderCopilot) || useCopilotClient {
		if providerID == string(catwalk.InferenceProviderCopilot) {
			opts = append(opts, openaicompat.WithUseResponsesAPI())
		}
		// Copilot client already applies reasoning field normalization internally.
		httpClient = copilot.NewClient(isSubAgent, c.cfg.Config().Options.Debug)
	} else if copilotService {
		// Use billing client for Copilot-compatible providers, wrapped with
		// reasoning field normalization.
		billingClient := copilot.NewBillingClient(copilotService, c.cfg.Config().Options.Debug)
		httpClient = &http.Client{
			Transport: copilot.NewReasoningNormalizingTransport(billingClient.Transport),
		}
	} else {
		// For all other openai-compat providers, apply reasoning field
		// normalization so that models returning "reasoning" or
		// "reasoning_text" are transparently mapped to "reasoning_content".
		var inner http.RoundTripper
		if c.cfg.Config().Options.Debug {
			inner = log.NewHTTPClient().Transport
		}
		httpClient = &http.Client{
			Transport: copilot.NewReasoningNormalizingTransport(inner),
		}
	}
	opts = append(opts, openaicompat.WithHTTPClient(wrapOpenAIStreamingHTTPClient(httpClient, responsesWebSocket)))

	if len(headers) > 0 {
		opts = append(opts, openaicompat.WithHeaders(headers))
	}

	for extraKey, extraValue := range extraBody {
		opts = append(opts, openaicompat.WithSDKOptions(openaisdk.WithJSONSet(extraKey, extraValue)))
	}

	return openaicompat.New(opts...)
}

func (c *coordinator) buildAzureProvider(baseURL, apiKey string, headers map[string]string, options map[string]string) (fantasy.Provider, error) {
	opts := []azure.Option{
		azure.WithBaseURL(baseURL),
		azure.WithAPIKey(apiKey),
		azure.WithUseResponsesAPI(),
	}
	var httpClient *http.Client
	if c.cfg.Config().Options.Debug {
		httpClient = log.NewHTTPClient()
	}
	opts = append(opts, azure.WithHTTPClient(httpext.WrapActivityTrackingHTTPClient(httpClient)))
	if options == nil {
		options = make(map[string]string)
	}
	if apiVersion, ok := options["apiVersion"]; ok {
		opts = append(opts, azure.WithAPIVersion(apiVersion))
	}
	if len(headers) > 0 {
		opts = append(opts, azure.WithHeaders(headers))
	}

	return azure.New(opts...)
}

func (c *coordinator) buildBedrockProvider(apiKey string, headers map[string]string) (fantasy.Provider, error) {
	var opts []bedrock.Option
	var httpClient *http.Client
	if c.cfg.Config().Options.Debug {
		httpClient = log.NewHTTPClient()
	}
	opts = append(opts, bedrock.WithHTTPClient(httpext.WrapActivityTrackingHTTPClient(httpClient)))
	if len(headers) > 0 {
		opts = append(opts, bedrock.WithHeaders(headers))
	}
	switch {
	case apiKey != "":
		opts = append(opts, bedrock.WithAPIKey(apiKey))
	case os.Getenv("AWS_BEARER_TOKEN_BEDROCK") != "":
		opts = append(opts, bedrock.WithAPIKey(os.Getenv("AWS_BEARER_TOKEN_BEDROCK")))
	default:
		// Skip, let the SDK do authentication.
	}
	return bedrock.New(opts...)
}

func (c *coordinator) buildGoogleProvider(baseURL, apiKey string, headers map[string]string) (fantasy.Provider, error) {
	opts := []google.Option{
		google.WithBaseURL(baseURL),
		google.WithGeminiAPIKey(apiKey),
	}
	var httpClient *http.Client
	if c.cfg.Config().Options.Debug {
		httpClient = log.NewHTTPClient()
	}
	opts = append(opts, google.WithHTTPClient(httpext.WrapActivityTrackingHTTPClient(httpClient)))
	if len(headers) > 0 {
		opts = append(opts, google.WithHeaders(headers))
	}
	return google.New(opts...)
}

func (c *coordinator) buildGoogleVertexProvider(headers map[string]string, options map[string]string) (fantasy.Provider, error) {
	opts := []google.Option{}
	var httpClient *http.Client
	if c.cfg.Config().Options.Debug {
		httpClient = log.NewHTTPClient()
	}
	opts = append(opts, google.WithHTTPClient(httpext.WrapActivityTrackingHTTPClient(httpClient)))
	if len(headers) > 0 {
		opts = append(opts, google.WithHeaders(headers))
	}

	project := options["project"]
	location := options["location"]

	opts = append(opts, google.WithVertex(project, location))

	return google.New(opts...)
}

func (c *coordinator) buildHyperProvider(baseURL, apiKey string) (fantasy.Provider, error) {
	opts := []hyper.Option{
		hyper.WithAPIKey(apiKey),
	}
	if baseURL != "" {
		opts = append(opts, hyper.WithBaseURL(baseURL))
	}
	var httpClient *http.Client
	if c.cfg.Config().Options.Debug {
		httpClient = log.NewHTTPClient()
	}
	opts = append(opts, hyper.WithHTTPClient(httpext.WrapActivityTrackingHTTPClient(httpClient)))
	return hyper.New(opts...)
}

func isAnthropicThinking(model catwalk.Model) bool {
	// When model.CanReason is true, thinking is enabled by default unless the
	// user explicitly disables it (Think=false). Callers that need to respect the
	// explicit-disable case must also check thinkingDisabled separately.
	if model.CanReason {
		return true
	}

	opts, err := anthropic.ParseOptions(model.Options.ProviderOptions)
	return err == nil && opts.Thinking != nil
}

func (c *coordinator) buildProvider(providerCfg config.ProviderConfig, model catwalk.Model, isSubAgent bool, thinkingDisabled bool) (fantasy.Provider, error) {
	headers := maps.Clone(providerCfg.ExtraHeaders)
	if headers == nil {
		headers = make(map[string]string)
	}

	// handle special headers for anthropic
	if providerCfg.Type == anthropic.Name && isAnthropicThinking(model) && !thinkingDisabled {
		if v, ok := headers["anthropic-beta"]; ok {
			headers["anthropic-beta"] = v + ",interleaved-thinking-2025-05-14"
		} else {
			headers["anthropic-beta"] = "interleaved-thinking-2025-05-14"
		}
	}

	apiKey, _ := c.cfg.Resolve(providerCfg.APIKey)
	apiKey = config.DecryptAPIKeyIfNeeded(apiKey)
	baseURL, _ := c.cfg.Resolve(providerCfg.BaseURL)

	switch providerCfg.Type {
	case openai.Name:
		return c.buildOpenaiProvider(baseURL, apiKey, headers, providerCfg.CopilotService, providerCfg.UseCopilotClient, isSubAgent, providerCfg.ResponsesWebSocket)
	case anthropic.Name:
		return c.buildAnthropicProvider(baseURL, apiKey, headers, providerCfg.ID, providerCfg.UseCopilotClient, isSubAgent)
	case openrouter.Name:
		return c.buildOpenrouterProvider(baseURL, apiKey, headers)
	case vercel.Name:
		return c.buildVercelProvider(baseURL, apiKey, headers)
	case azure.Name:
		return c.buildAzureProvider(baseURL, apiKey, headers, providerCfg.ExtraParams)
	case bedrock.Name:
		return c.buildBedrockProvider(apiKey, headers)
	case google.Name:
		return c.buildGoogleProvider(baseURL, apiKey, headers)
	case "google-vertex":
		return c.buildGoogleVertexProvider(headers, providerCfg.ExtraParams)
	case openaicompat.Name:
		if providerCfg.ID == string(catwalk.InferenceProviderZAI) {
			if providerCfg.ExtraBody == nil {
				providerCfg.ExtraBody = map[string]any{}
			}
			providerCfg.ExtraBody["tool_stream"] = true
		}
		return c.buildOpenaiCompatProvider(
			baseURL,
			apiKey,
			headers,
			providerCfg.ExtraBody,
			providerCfg.ID,
			providerCfg.UseCopilotClient,
			isSubAgent,
			providerCfg.CopilotService,
			providerCfg.ResponsesWebSocket,
		)
	case hyper.Name:
		return c.buildHyperProvider(baseURL, apiKey)
	default:
		return nil, fmt.Errorf("provider type not supported: %q", providerCfg.Type)
	}
}

func isExactoSupported(modelID string) bool {
	supportedModels := []string{
		"moonshotai/kimi-k2-0905",
		"deepseek/deepseek-v3.1-terminus",
		"z-ai/glm-4.6",
		"openai/gpt-oss-120b",
		"qwen/qwen3-coder",
	}
	return slices.Contains(supportedModels, modelID)
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
}

func (c *coordinator) RemoveQueuedPrompt(sessionID string, index int) bool {
	return c.currentAgent.RemoveQueuedPrompt(sessionID, index)
}

func (c *coordinator) ClearQueue(sessionID string) {
	c.currentAgent.ClearQueue(sessionID)
}

func (c *coordinator) PauseQueue(sessionID string) {
	c.currentAgent.PauseQueue(sessionID)
}

func (c *coordinator) ResumeQueue(sessionID string) {
	c.currentAgent.ResumeQueue(sessionID)
}

func (c *coordinator) IsQueuePaused(sessionID string) bool {
	return c.currentAgent.IsQueuePaused(sessionID)
}

func (c *coordinator) PrioritizeQueuedPrompt(sessionID string, index int) bool {
	return c.currentAgent.PrioritizeQueuedPrompt(sessionID, index)
}

func (c *coordinator) IsBusy() bool {
	return c.currentAgent.IsBusy()
}

func (c *coordinator) IsSessionBusy(sessionID string) bool {
	return c.currentAgent.IsSessionBusy(sessionID)
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
	providerCfg, ok := c.cfg.Config().Providers.Get(modelCfg.Provider)
	if !ok {
		return false, errModelProviderNotConfigured
	}
	for i := range providerCfg.Models {
		if providerCfg.Models[i].ID == modelCfg.Model {
			return providerCfg.Models[i].SupportsImages, nil
		}
	}
	return false, fmt.Errorf("model %q not found in provider config", modelCfg.Model)
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
	promptBuilder, err := promptForAgent(agentCfg, false, prompt.WithWorkingDir(workingDir))
	if err != nil {
		return sessionAgentRuntimeConfig{}, err
	}

	return c.refreshSessionAgentRuntimeConfig(ctx, c.currentAgent, promptBuilder, agentCfg, false)
}

func (c *coordinator) RefreshTools(ctx context.Context) error {
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
	if sessionID == "" || len(toolNames) == 0 {
		return nil
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

func (c *coordinator) QueuedPrompts(sessionID string) int {
	return c.currentAgent.QueuedPrompts(sessionID)
}

func (c *coordinator) QueuedPromptsList(sessionID string) []string {
	return c.currentAgent.QueuedPromptsList(sessionID)
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
			return candidate, nil
		}
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
}

type subagentBatchParams struct {
	SessionID       string
	AgentMessageID  string
	ToolCallID      string
	Tasks           []subagentTask
	Context         string
	RunInBackground bool
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

type subagentReducerInput struct {
	TaskResult   reducer.TaskResult
	ChildSession message.ToolResultReducerChildSession
	Result       subagentResult
	SideEffects  SideEffectSummary
	Runtime      SubagentRuntimeContext
	HasRuntime   bool
	AttemptCount int
}

type subAgentFactory func(context.Context, string) (SessionAgent, config.Agent, error)

func (c *coordinator) runSubAgent(ctx context.Context, params subAgentParams) (fantasy.ToolResponse, error) {
	return c.runSubAgentDirect(ctx, params)
}

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

	baseContext := strings.TrimSpace(params.Context)

	// Prepare each task synchronously: build the subagent, register it,
	// drain mailbox messages, and assemble the runtime parameters.
	for i, t := range params.Tasks {
		prepared[i].Task = t
		prepared[i].TaskRef = taskRefs[t.Name]

		subAgent, agentCfg, buildErr := c.buildSubAgentForType(ctx, t.SubagentType)
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

		agentID := fmt.Sprintf("%s::%s", c.mainAgentID, t.Name)
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
		if baseContext != "" {
			prompt = fmt.Sprintf("Shared context:\n%s\n\n---\n\n%s", baseContext, prompt)
		}

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
			Agent:             subAgent,
			SessionID:         params.SessionID,
			AgentMessageID:    params.AgentMessageID,
			ParentMessageID:   params.AgentMessageID,
			ToolCallID:        taskToolCallID,
			Prompt:            prompt,
			SessionTitle:      formatSubagentSessionTitle(description, subagentType),
			SubagentType:      subagentType,
			DelegationMailbox: params.ToolCallID,
			AgentMemory:       agentCfg.Memory,
			AgentIsolation:    agentCfg.Isolation,
			AgentBackground:   agentCfg.Background,
			SkipHandoffReview: true,
			IrcAgentID:        agentID,
		}
	}

	// Build the executor and run all prepared tasks in parallel.
	runtimeCfg := c.cfg.Config().EffectiveSubagentRuntime()
	runner := func(ctx context.Context, p subAgentParams) (fantasy.ToolResponse, error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error("Subagent task panicked", "tool_call_id", p.ToolCallID, "panic", recovered)
			}
		}()
		if c.hookManager != nil {
			c.hookManager.RunSubagentStart(ctx, p.ToolCallID, p.SubagentType, p.SessionID)
			defer c.hookManager.RunSubagentStop(context.Background(), p.ToolCallID, p.SubagentType, p.SessionID)
		}
		return c.runSubAgent(ctx, p)
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
				c.agentRegistry.SetStatus(prepared[i].AgentID, AgentStatusCompleted)
			case message.ToolResultSubtaskStatusCanceled,
				message.ToolResultSubtaskStatusFailed,
				message.ToolResultSubtaskStatusBlocked:
				c.agentRegistry.SetStatus(prepared[i].AgentID, AgentStatusAborted)
			}
			c.agentRegistry.Unregister(prepared[i].AgentID)
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

func subagentSemaphoreForAgent(agentCfg config.Agent, runtimeCfg config.SubagentRuntimeConfig, semaphores map[string]chan struct{}, semMu *sync.Mutex) chan struct{} {
	limit := runtimeCfg.MaxConcurrency
	if agentCfg.TaskGovernance != nil {
		if agentLimit := agentCfg.TaskGovernance.MaxConcurrentLimit(); agentLimit > 0 {
			limit = agentLimit
		}
	}
	if limit <= 0 {
		return nil
	}

	agentID := strings.TrimSpace(agentCfg.ID)
	if agentID == "" {
		agentID = config.AgentGeneral
	}
	if agentCfg.TaskGovernance == nil || agentCfg.TaskGovernance.MaxConcurrentLimit() == 0 {
		agentID = "__global__"
	}

	semMu.Lock()
	defer semMu.Unlock()
	if semaphore, ok := semaphores[agentID]; ok {
		return semaphore
	}
	semaphore := make(chan struct{}, limit)
	semaphores[agentID] = semaphore
	return semaphore
}

func acquireSubagentSemaphore(ctx context.Context, semaphore chan struct{}) (func(), error) {
	if semaphore == nil {
		return func() {}, nil
	}
	select {
	case semaphore <- struct{}{}:
		return func() { <-semaphore }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
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

func (c *coordinator) sideEffectSummary(ctx context.Context, result subagentResult) SideEffectSummary {
	filesTouched := slices.Clone(result.FilesTouched)
	if len(filesTouched) > 1 {
		slices.Sort(filesTouched)
		filesTouched = slices.Compact(filesTouched)
	}
	summary := SideEffectSummary{
		FilesTouched: filesTouched,
	}
	if len(filesTouched) > 0 {
		summary.MutatingTools = append(summary.MutatingTools, tools.EditToolName)
	}
	if strings.TrimSpace(result.AgentID) != "" || result.Status == message.ToolResultSubtaskStatusRunning {
		summary.SpawnedBackground = true
	}
	if c == nil || c.messages == nil || strings.TrimSpace(result.ChildSessionID) == "" {
		return summary
	}
	msgs, err := c.messages.List(ctx, result.ChildSessionID)
	if err != nil {
		return summary
	}
	for _, msg := range msgs {
		if msg.Role != message.Tool {
			continue
		}
		for _, toolResult := range msg.ToolResults() {
			switch toolResult.Name {
			case tools.EditToolName, tools.WriteToolName, tools.DownloadToolName:
				summary.MutatingTools = append(summary.MutatingTools, toolResult.Name)
			case tools.BashToolName:
				if !toolResult.IsError {
					summary.MutatingTools = append(summary.MutatingTools, toolResult.Name)
				}
			}
		}
	}
	if len(summary.MutatingTools) > 1 {
		slices.Sort(summary.MutatingTools)
		summary.MutatingTools = slices.Compact(summary.MutatingTools)
	}
	if len(summary.FilesTouched) == 0 && c.filetracker != nil && strings.TrimSpace(result.ChildSessionID) != "" {
		if files, err := c.filetracker.ListReadFiles(ctx, result.ChildSessionID); err == nil {
			summary.FilesTouched = append(summary.FilesTouched, files...)
		}
	}
	return summary
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
	case tools.BashToolName, tools.JobOutputToolName, tools.JobWaitToolName, tools.JobKillToolName:
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
	shouldRetry := policy == MissingFinishRetryThenWarn || policy == MissingFinishRetryThenFail
	if shouldRetry {
		for range 2 {
			_, runErr := params.Agent.Run(ctx, SessionAgentCall{
				SessionID:        childSessionID,
				Prompt:           "Call yield exactly once now. Summarize only the work already completed. Do not start new work unless needed to determine final status.",
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

func (c *coordinator) runSubAgentDirect(ctx context.Context, params subAgentParams) (fantasy.ToolResponse, error) {
	untrackSubAgent := c.trackActiveSubAgent(params.SessionID, params.Agent)
	defer untrackSubAgent()

	parentSession, err := c.sessions.Get(ctx, params.SessionID)
	if err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("get parent session: %w", err)
	}
	if parentSession.PermissionMode == session.PermissionModeAuto && !params.SkipHandoffReview {
		review, reviewErr := c.reviewHandoffText(ctx, parentSession, params.SessionTitle, params.Prompt)
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

	c.childSessionAgents.Store(subSession.ID, params.Agent)
	defer c.childSessionAgents.Delete(subSession.ID)

	if params.SessionSetup != nil {
		params.SessionSetup(subSession.ID)
	}

	eventSink := coordinatorSubagentEventSink{timeline: c.timeline}

	effectiveIsolation := strings.TrimSpace(params.AgentIsolation)
	subSession, sessionWorkingDir, effectiveIsolation, err := c.prepareSubagentWorkspace(ctx, parentSession, subSession, effectiveIsolation)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}

	// Track worktree for cleanup after subagent completes.
	usedWorktree := effectiveIsolation == "worktree" && subSession.WorkspaceCWD != parentSession.WorkspaceCWD
	if usedWorktree {
		defer func() {
			c.cleanupWorktreeIfNeeded(subSession.WorkspaceCWD)
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
	parentPermissions, parentPermissionsErr := c.parentPermissionContext(ctx, parentSession.CollaborationMode, params.SessionID)
	if parentPermissionsErr != nil {
		return fantasy.ToolResponse{}, parentPermissionsErr
	}

	// Clear any inherited runtime config from the parent agent before running
	// the subagent. Each subagent must refresh its own models, tools, and
	// system prompt; otherwise concurrent child runs can observe the parent's
	// runtime config and skip their own initialization.
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
	if runtimeCfg := c.cfg.Config().EffectiveSubagentRuntime(); strings.TrimSpace(runtimeCfg.DefaultIsolation) != "" && strings.TrimSpace(params.AgentIsolation) == "" {
		runtime.Isolation.Kind = subagentIsolationKind(runtimeCfg.DefaultIsolation)
	}
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

	// Prepend a brief summary of the coordinator's recent reasoning so the
	// subagent has concrete context from the Research phase and does not need
	// to rediscover details the coordinator already gathered.
	enrichedPrompt := params.Prompt
	if strings.TrimSpace(params.ExistingSessionID) == "" {
		enrichedPrompt = c.buildSubagentContextPrefix(ctx, params.SessionID) + params.Prompt
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
		review, reviewErr := c.reviewHandoffText(ctx, parentSession, params.SessionTitle, content)
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

	response := withSubtaskToolResponseMetadata(
		fantasy.NewTextResponse(content),
		params.ToolCallID,
		subSession.ID,
		params.ParentMessageID,
		status,
	)
	if hasYield {
		response = withSubagentYieldToolResponseMetadata(response, yieldResult)
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
		effectiveIsolation = "session"
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

// hasWorktreeChanges checks if a worktree has uncommitted changes.
func (c *coordinator) hasWorktreeChanges(worktreeDir string) (bool, error) {
	cmd := exec.Command("git", "-C", worktreeDir, "status", "--porcelain")
	output, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("check worktree status: %w", err)
	}
	return len(strings.TrimSpace(string(output))) > 0, nil
}

// cleanupWorktreeIfNeeded removes a worktree if it has no changes.
func (c *coordinator) cleanupWorktreeIfNeeded(worktreeDir string) {
	worktreeDir = strings.TrimSpace(worktreeDir)
	if worktreeDir == "" {
		return
	}

	hasChanges, err := c.hasWorktreeChanges(worktreeDir)
	if err != nil {
		slog.Warn("Failed to check worktree changes, skipping cleanup", "path", worktreeDir, "error", err)
		return
	}

	if hasChanges {
		slog.Debug("Worktree has changes, preserving", "path", worktreeDir)
		return
	}

	if err := c.removeSubagentWorktree(worktreeDir); err != nil {
		slog.Warn("Failed to cleanup worktree", "path", worktreeDir, "error", err)
	}
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
		if err := c.cleanupStaleWorktreesInDir(worktreesDir, cutoffDays); err != nil {
			return err
		}
	}
	return nil
}

func (c *coordinator) cleanupStaleWorktreesInDir(worktreesDir string, cutoffDays int) error {
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
			hasChanges, chkErr := c.hasWorktreeChanges(worktreePath)
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
	// subagentContextInjectMaxMessages caps how many of the coordinator's recent
	// text messages are prepended to a subagent's prompt as parent context.
	subagentContextInjectMaxMessages = 3
	// subagentContextInjectMaxCharsPerMsg caps each injected snippet so that the
	// combined overhead stays well under a typical context window.
	subagentContextInjectMaxCharsPerMsg = 500
)

// buildSubagentContextPrefix collects the coordinator's most recent text
// reasoning from the parent session and formats it as a <parent_context> block.
// This gives subagents concrete information the coordinator gathered during the
// Research phase without requiring them to rediscover it from scratch.
// Returns "" when there is nothing meaningful to inject.
func (c *coordinator) buildSubagentContextPrefix(ctx context.Context, parentSessionID string) string {
	if c.messages == nil {
		return ""
	}
	msgs, err := c.messages.List(ctx, parentSessionID)
	if err != nil || len(msgs) == 0 {
		return ""
	}

	var snippets []string
	for i := len(msgs) - 1; i >= 0 && len(snippets) < subagentContextInjectMaxMessages; i-- {
		msg := msgs[i]
		if msg.Role != message.Assistant || msg.IsSummaryMessage {
			continue
		}
		text := strings.TrimSpace(msg.Content().Text)
		// Strip extended-thinking blocks — they contain internal reasoning that
		// is not useful to share with a subagent.
		text = thinkTagRegex.ReplaceAllString(text, "")
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		runes := []rune(text)
		if len(runes) > subagentContextInjectMaxCharsPerMsg {
			text = string(runes[:subagentContextInjectMaxCharsPerMsg]) + "…"
		}
		snippets = append([]string{text}, snippets...) // maintain chronological order
	}

	if len(snippets) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("<parent_context>\n")
	sb.WriteString("Context gathered by the orchestrating agent before this task was delegated:\n\n")
	for i, s := range snippets {
		fmt.Fprintf(&sb, "%d. %s\n\n", i+1, s)
	}
	sb.WriteString("</parent_context>\n\n")
	return sb.String()
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

// runBackgroundTaskNode launches a single task as a background agent.
// It returns the agent ID immediately while the task executes asynchronously.
func (c *coordinator) runBackgroundTaskNode(
	ctx context.Context,
	params subagentBatchParams,
	task subagentTask,
	subAgent SessionAgent,
	agentCfg config.Agent,
	description string,
	subagentType string,
	semaphore chan struct{},
) string {
	taskToolCallID := fmt.Sprintf("%s::%s", params.ToolCallID, task.Name)
	var childSessionID string
	var agentID string

	agentName := fmt.Sprintf("%s-%s", task.Name, generateAgentID())

	runner := func(ctx context.Context, command backgroundAgentCommand) backgroundAgentRunResult {
		releaseSemaphore, acquireErr := acquireSubagentSemaphore(ctx, semaphore)
		if acquireErr != nil {
			return backgroundAgentRunResult{
				Status:  backgroundAgentStatusCanceled,
				Content: strings.TrimSpace(acquireErr.Error()),
			}
		}
		defer releaseSemaphore()

		attemptCtx := ctx
		timeoutCancel := func() {}
		if agentCfg.TaskGovernance != nil {
			if timeout := agentCfg.TaskGovernance.Timeout(); timeout > 0 {
				attemptCtx, timeoutCancel = context.WithTimeout(attemptCtx, timeout)
			}
		}
		cancel := timeoutCancel

		if c.hookManager != nil {
			c.hookManager.RunSubagentStart(attemptCtx, agentID, subagentType, params.SessionID)
		}

		runParams := subAgentParams{
			Agent:             subAgent,
			SessionID:         params.SessionID,
			ExistingSessionID: childSessionID,
			AgentMessageID:    cmp.Or(command.AgentMessageID, params.AgentMessageID),
			ParentMessageID:   cmp.Or(command.AgentMessageID, params.AgentMessageID),
			ToolCallID:        cmp.Or(command.ToolCallID, taskToolCallID),
			Prompt:            strings.TrimSpace(command.Prompt),
			SessionTitle:      formatSubagentSessionTitle(description, subagentType),
			SubagentType:      subagentType,
			DelegationMailbox: params.ToolCallID,
			AgentMemory:       agentCfg.Memory,
			AgentIsolation:    agentCfg.Isolation,
			AgentBackground:   agentCfg.Background,
			SkipHandoffReview: true,
		}
		response, runErr := c.runSubAgent(attemptCtx, runParams)
		cancel()

		if c.hookManager != nil {
			c.hookManager.RunSubagentStop(context.Background(), agentID, subagentType, params.SessionID)
		}

		if runErr != nil {
			slog.Error("Background agent task failed", "agent_id", agentID, "task_name", task.Name, "error", runErr)
			return backgroundAgentRunResult{
				Status:         backgroundAgentStatusFailed,
				ChildSessionID: childSessionID,
				Content:        runErr.Error(),
			}
		}

		content := response.Content
		if response.Metadata != "" {
			if sub, ok := message.ParseToolResultSubtaskResult(response.Metadata); ok && sub.ChildSessionID != "" {
				childSessionID = sub.ChildSessionID
			}
			if yield, ok := message.ParseToolResultYield(response.Metadata); ok {
				c.backgroundAgents.UpdateArtifacts(agentID, yield.Data, nil, nil)
				if strings.TrimSpace(content) == "" && strings.TrimSpace(yield.Data) != "" {
					content = strings.TrimSpace(yield.Data)
				}
			}
		}
		if content == "" {
			content = "Background agent completed with no output."
		}

		status := backgroundAgentStatusCompleted
		if subtask, ok := message.ParseToolResultSubtaskResult(response.Metadata); ok {
			switch subtask.Status {
			case message.ToolResultSubtaskStatusFailed, message.ToolResultSubtaskStatusBlocked:
				status = backgroundAgentStatusFailed
			case message.ToolResultSubtaskStatusCanceled:
				status = backgroundAgentStatusCanceled
			}
		}
		return backgroundAgentRunResult{
			Status:         status,
			ChildSessionID: childSessionID,
			Content:        content,
		}
	}

	agentID = c.backgroundAgents.RegisterNamed(agentName, subagentType, description, runner)
	c.backgroundAgents.SetParentSession(agentID, params.SessionID)

	_, enqueueErr := c.backgroundAgents.Enqueue(agentID, backgroundAgentCommand{
		Prompt:         strings.TrimSpace(task.Assignment),
		SessionID:      params.SessionID,
		AgentMessageID: params.AgentMessageID,
		ToolCallID:     taskToolCallID,
	})
	if enqueueErr != nil {
		slog.Error("Background agent enqueue failed", "agent_id", agentID, "task_name", task.Name, "error", enqueueErr)
		c.backgroundAgents.Fail(agentID, enqueueErr.Error())
	}
	return agentID
}
