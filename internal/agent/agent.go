// Package agent is the core orchestration layer for Crush AI agents.
//
// It provides session-based AI agent functionality for managing
// conversations, tool execution, and message handling. It coordinates
// interactions between language models, messages, sessions, and tools while
// handling features like automatic summarization, queuing, and token
// management.
package agent

import (
	"cmp"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/bedrock"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/vercel"
	"charm.land/lipgloss/v2"

	"github.com/charmbracelet/crush/internal/agent/hyper"
	"github.com/charmbracelet/crush/internal/agent/notify"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/checkpoint"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/filetracker"
	"github.com/charmbracelet/crush/internal/hooks"
	"github.com/charmbracelet/crush/internal/memory/engine"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/oauth/copilot"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/plugin"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/stringext"
	"github.com/charmbracelet/crush/internal/version"
	"github.com/charmbracelet/x/exp/charmtone"
)

const (
	DefaultSessionName = "Untitled Session"

	// Constants for auto-summarization thresholds
	autoSummarizeReserveTokens = 20_000

	joinActiveRunMaxInjectedCalls  = 2
	joinActiveRunPromptCharsBudget = 1_600
)

var userAgent = fmt.Sprintf("Charm-Crush/%s (https://charm.land/crush)", version.Version)

//go:embed templates/title.md
var titlePrompt []byte

//go:embed templates/summary.md
var summaryPrompt []byte

// Used to remove <think> tags from generated titles.
var (
	thinkTagRegex        = regexp.MustCompile(`(?s)<think>.*?</think>`)
	titleWhitespaceRegex = regexp.MustCompile(`\s+`)
)

const autoResumePromptPrefix = "The previous session was interrupted because it got too long, the initial user request was: `"

type pendingExtraction struct {
	id     uint64
	cancel context.CancelFunc
}

type sessionCompactionTrigger string

const (
	sessionCompactionTriggerNone      sessionCompactionTrigger = ""
	sessionCompactionTriggerNormal    sessionCompactionTrigger = "normal_summarize"
	sessionCompactionTriggerRecover   sessionCompactionTrigger = "recover_summarize"
	sessionCompactionTriggerProactive sessionCompactionTrigger = "proactive_compact"
)

// MemoryPrefetch is an async memory recall handle. Modeled after Claude Code's
// approach: the coordinator starts prefetch once, the result is cached on
// settle, and consumers can poll readiness multiple times without blocking.
type MemoryPrefetch struct {
	// SettledAt is set (non-nil) when the prefetch completes.
	// Uses *time.Time so nil means "not settled" and the pointer value
	// distinguishes "settled with empty result" from "not settled".
	SettledAt *time.Time
	// Result caches the prefetch result after it settles.
	Result string
	// settledMu protects SettledAt and Result.
	settledMu sync.Mutex
}

// Settle marks the prefetch as complete and caches the result.
// Called by the coordinator prefetch goroutine.
func (m *MemoryPrefetch) Settle(result string) {
	m.settledMu.Lock()
	defer m.settledMu.Unlock()
	m.Result = result
	now := time.Now()
	m.SettledAt = &now
}

// GetSettled returns the cached result when prefetch has settled.
// settled=false means prefetch is not ready yet.
func (m *MemoryPrefetch) GetSettled() (result string, settled bool) {
	m.settledMu.Lock()
	defer m.settledMu.Unlock()
	if m.SettledAt == nil {
		return "", false
	}
	return m.Result, true
}

type SessionAgentCall struct {
	SessionID     string
	Prompt        string
	Purpose       plugin.ChatTransformPurpose
	InitiatorType string
	// JoinActiveRun allows this queued call to be injected into the active
	// run's next provider step instead of waiting for the current run to
	// finish.
	JoinActiveRun bool
	// BypassQueuePause allows this queued call to be processed even if the
	// queue is paused. Used for auto-resume calls after summarization.
	BypassQueuePause bool
	ProviderOptions  fantasy.ProviderOptions
	Attachments      []message.Attachment
	MaxOutputTokens  int64
	Temperature      *float64
	TopP             *float64
	TopK             *int64
	FrequencyPenalty *float64
	PresencePenalty  *float64
	NonInteractive   bool
	UserMessage      *message.Message
	// MemoryPrefetch is an async memory recall result. The coordinator starts
	// the prefetch goroutine, and the agent consumes it if ready. The result
	// is cached so retries can reuse it. Modeled after Claude Code's approach.
	MemoryPrefetch *MemoryPrefetch
	// OnProgress is called after each completed LLM step with accumulated
	// tool call count and the name of the last tool invoked.
	OnProgress func(toolUses int, lastTool string)
}

type SessionAgent interface {
	Run(context.Context, SessionAgentCall) (*fantasy.AgentResult, error)
	EstimateSessionPromptTokensForModel(context.Context, string, Model) (int64, error)
	SetModels(large Model, small Model)
	SetTools(tools []fantasy.AgentTool)
	SetSystemPrompt(systemPrompt string)
	SetSystemPromptPrefix(systemPromptPrefix string)
	Cancel(sessionID string)
	CancelAll()
	IsSessionBusy(sessionID string) bool
	IsBusy() bool
	QueuedPrompts(sessionID string) int
	QueuedPromptsList(sessionID string) []string
	RemoveQueuedPrompt(sessionID string, index int) bool
	ClearQueue(sessionID string)
	Summarize(context.Context, string, fantasy.ProviderOptions) error
	Model() Model
	PauseQueue(sessionID string)
	ResumeQueue(sessionID string)
	IsQueuePaused(sessionID string) bool
	PrioritizeQueuedPrompt(sessionID string, index int) bool
	RespondAsBackground(ctx context.Context, from, message string) (string, error)
}

type Model struct {
	Model      fantasy.LanguageModel
	CatwalkCfg catwalk.Model
	ModelCfg   config.SelectedModel
}

// MemoryEngineHooks bundles lifecycle callbacks that the coordinator
// provides to the agent when the memory engine is enabled. All hooks
// are optional (nil when engine is disabled).
type MemoryEngineHooks struct {
	// OnBeforeCompaction is called before session summarization/compaction.
	// The returned string, when non-empty, is a Markdown-formatted "rescue"
	// payload that the agent injects into the compaction prompt so durable
	// memories survive summarization.
	OnBeforeCompaction func(ctx context.Context, sessionID string) string
	// AfterTurnIdle is called after each successful LLM turn.
	AfterTurnIdle func(ctx context.Context, sessionID string)
	// OnSessionClosed is called when a session is closed/deleted.
	OnSessionClosed func(ctx context.Context, sessionID string)
}

type deferredToolRuntime interface {
	activateDeferredToolsForSession(sessionID string, toolNames []string) []string
	activatedDeferredToolsForSession(sessionID string) map[string]struct{}
	isDeferredTool(name string) bool
}

type sessionAgent struct {
	largeModel         *csync.Value[Model]
	smallModel         *csync.Value[Model]
	systemPromptPrefix *csync.Value[string]
	systemPrompt       *csync.Value[string]
	workingDir         string
	tools              *csync.Slice[fantasy.AgentTool]
	agentFactory       func(model fantasy.LanguageModel, opts ...fantasy.AgentOption) fantasy.Agent

	refreshCallConfig    func(context.Context) (sessionAgentRuntimeConfig, error)
	deferredToolRuntime  deferredToolRuntime
	isSubAgent           bool
	sessions             session.Service
	messages             message.Service
	backgroundModel      *backgroundModel
	reviewToolResult     func(context.Context, string, message.ToolResult, session.PermissionMode) (message.ToolResult, error)
	disableAutoSummarize bool
	isYolo               bool
	notify               pubsub.Publisher[notify.Notification]
	hookManager          *hooks.Manager
	pluginRuntime        *plugin.Runtime
	filetracker          filetracker.Service
	checkpoint           checkpoint.Service
	retryDelayFunc       func(attempt int, serverRetryAfter time.Duration) time.Duration
	retryWaitFunc        func(context.Context, time.Duration) error

	queueMu        sync.Mutex
	messageQueue   *csync.Map[string, []SessionAgentCall]
	activeRequests *csync.Map[string, context.CancelFunc]
	pausedQueues   *csync.Map[string, bool]

	extractionMu         sync.Mutex
	pendingExtractions   map[string][]pendingExtraction
	nextExtractionID     uint64
	sessionMemoryEnabled bool
	memoryEngineEnabled  bool

	// memoryEngineEventStore provides direct EventStore access when the memory
	// engine is enabled. Used for Working Memory read/write operations.
	memoryEngineEventStore engine.EventStore

	// memoryEngineHooks holds lifecycle callbacks provided by the coordinator
	// when the memory engine is enabled. Nil when engine is disabled.
	memoryEngineHooks *MemoryEngineHooks
}

type SessionAgentOptions struct {
	LargeModel             Model
	SmallModel             Model
	SystemPromptPrefix     string
	SystemPrompt           string
	WorkingDir             string
	AgentFactory           func(model fantasy.LanguageModel, opts ...fantasy.AgentOption) fantasy.Agent
	RefreshCallConfig      func(context.Context) (sessionAgentRuntimeConfig, error)
	DeferredToolRuntime    deferredToolRuntime
	IsSubAgent             bool
	DisableAutoSummarize   bool
	IsYolo                 bool
	Sessions               session.Service
	Messages               message.Service
	BackgroundModel        *backgroundModel
	ReviewToolResult       func(context.Context, string, message.ToolResult, session.PermissionMode) (message.ToolResult, error)
	Tools                  []fantasy.AgentTool
	Notify                 pubsub.Publisher[notify.Notification]
	HookManager            *hooks.Manager
	PluginRuntime          *plugin.Runtime
	Filetracker            filetracker.Service
	Checkpoint             checkpoint.Service
	RetryDelayFunc         func(attempt int, serverRetryAfter time.Duration) time.Duration
	EnableSessionMemory    bool
	MemoryEngineEnabled    bool
	MemoryEngineEventStore engine.EventStore
	MemoryEngineHooks      *MemoryEngineHooks
	RetryWaitFunc          func(context.Context, time.Duration) error
}

type sessionAgentRuntimeConfig struct {
	ProviderOptions    fantasy.ProviderOptions
	MaxOutputTokens    int64
	Temperature        *float64
	TopP               *float64
	TopK               *int64
	FrequencyPenalty   *float64
	PresencePenalty    *float64
	SystemPrompt       *string
	SystemPromptPrefix *string
	CollaborationMode  session.CollaborationMode
	PermissionMode     session.PermissionMode
	AllowedToolNames   []string
	Tools              []fantasy.AgentTool
}

type sessionAgentRuntimeConfigContextKey struct{}

func NewSessionAgent(
	opts SessionAgentOptions,
) SessionAgent {
	agentFactory := opts.AgentFactory
	if agentFactory == nil {
		agentFactory = fantasy.NewAgent
	}
	retryDelayFunc := opts.RetryDelayFunc
	if retryDelayFunc == nil {
		retryDelayFunc = retryDelay
	}
	retryWaitFunc := opts.RetryWaitFunc
	if retryWaitFunc == nil {
		retryWaitFunc = waitForRetryDelay
	}
	return &sessionAgent{
		largeModel:             csync.NewValue(opts.LargeModel),
		smallModel:             csync.NewValue(opts.SmallModel),
		systemPromptPrefix:     csync.NewValue(opts.SystemPromptPrefix),
		systemPrompt:           csync.NewValue(opts.SystemPrompt),
		workingDir:             opts.WorkingDir,
		agentFactory:           agentFactory,
		refreshCallConfig:      opts.RefreshCallConfig,
		deferredToolRuntime:    opts.DeferredToolRuntime,
		isSubAgent:             opts.IsSubAgent,
		sessions:               opts.Sessions,
		messages:               opts.Messages,
		backgroundModel:        opts.BackgroundModel,
		reviewToolResult:       opts.ReviewToolResult,
		disableAutoSummarize:   opts.DisableAutoSummarize,
		tools:                  csync.NewSliceFrom(opts.Tools),
		isYolo:                 opts.IsYolo,
		notify:                 opts.Notify,
		hookManager:            opts.HookManager,
		pluginRuntime:          opts.PluginRuntime,
		filetracker:            opts.Filetracker,
		checkpoint:             opts.Checkpoint,
		retryDelayFunc:         retryDelayFunc,
		retryWaitFunc:          retryWaitFunc,
		messageQueue:           csync.NewMap[string, []SessionAgentCall](),
		activeRequests:         csync.NewMap[string, context.CancelFunc](),
		pausedQueues:           csync.NewMap[string, bool](),
		pendingExtractions:     make(map[string][]pendingExtraction),
		sessionMemoryEnabled:   opts.EnableSessionMemory,
		memoryEngineEnabled:    opts.MemoryEngineEnabled,
		memoryEngineEventStore: opts.MemoryEngineEventStore,
		memoryEngineHooks:      opts.MemoryEngineHooks,
	}
}

func (a *sessionAgent) plugins() *plugin.Runtime {
	if a != nil && a.pluginRuntime != nil {
		return a.pluginRuntime
	}
	return plugin.DefaultRuntime()
}

func (a *sessionAgent) Run(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	start := time.Now()
	defer func() {
		slog.Debug("[PERF] sessionAgent.Run total", "duration", time.Since(start), "session_id", call.SessionID)
	}()

	if call.InitiatorType != "" {
		ctx = copilot.ContextWithInitiatorType(ctx, call.InitiatorType)
	} else if a.isSubAgent {
		ctx = copilot.ContextWithInitiatorType(ctx, copilot.InitiatorAgent)
	}

	// isUserInitiatedRequest is true only for the very first step of a real user
	// prompt. All tool-call continuations, auto-resume prompts, sub-agent
	// requests, and any call with an explicit InitiatorAgent type are free
	// (X-Initiator: agent).
	isUserInitiatedRequest := call.InitiatorType == copilot.InitiatorUser ||
		(call.InitiatorType == "" && !a.isSubAgent)
	firstRequestStep := true

	if call.Prompt == "" && !message.ContainsTextAttachment(call.Attachments) {
		return nil, ErrEmptyPrompt
	}
	if call.SessionID == "" {
		return nil, ErrSessionMissing
	}

	// Queue the message if busy
	if a.IsSessionBusy(call.SessionID) {
		if call.UserMessage != nil && call.UserMessage.ID != "" && a.messages != nil {
			if err := a.messages.Delete(ctx, call.UserMessage.ID); err != nil {
				slog.Warn("Failed to remove queued user message", "session_id", call.SessionID, "message_id", call.UserMessage.ID, "error", err)
			}
			call.UserMessage = nil
		}
		a.enqueueQueuedCall(call.SessionID, call)
		return nil, nil
	}

	// Add the session to the context and mark as busy immediately to prevent concurrent re-entry.
	ctx = context.WithValue(ctx, tools.SessionIDContextKey, call.SessionID)
	ctx = context.WithValue(ctx, tools.SessionServiceContextKey, a.sessions)
	ctx = context.WithValue(ctx, tools.MessageServiceContextKey, a.messages)

	genCtx, cancel := context.WithCancel(ctx)
	a.activeRequests.Set(call.SessionID, cancel)
	defer a.activeRequests.Del(call.SessionID)
	defer cancel()

	if a.hookManager != nil {
		a.hookManager.RunSessionStart(genCtx, call.SessionID)
		defer a.hookManager.RunSessionEnd(genCtx, call.SessionID)
	}

	if a.hookManager != nil && call.Prompt != "" {
		a.hookManager.RunUserPromptSubmit(genCtx, call.SessionID, call.Prompt)
	}

	if a.checkpoint != nil && !a.isSubAgent {
		if _, cpErr := a.checkpoint.CreateCheckpoint(genCtx, call.SessionID, ""); cpErr != nil {
			slog.Warn("Failed to create checkpoint", "error", cpErr, "session_id", call.SessionID)
		}
	}

	runtimeConfig, err := a.refreshCallConfigIfNeeded(genCtx, &call)
	if err != nil {
		return nil, err
	}
	slog.Debug("[PERF] sessionAgent: initial setup done", "duration", time.Since(start), "session_id", call.SessionID)

	// Copy mutable fields under lock to avoid races with SetTools/SetModels.
	agentTools := a.tools.Copy()
	largeModel := a.largeModel.Get()
	systemPrompt := a.systemPrompt.Get()
	promptPrefix := a.systemPromptPrefix.Get()
	if runtimeConfig != nil {
		if runtimeConfig.SystemPrompt != nil {
			systemPrompt = *runtimeConfig.SystemPrompt
		}
		if runtimeConfig.SystemPromptPrefix != nil {
			promptPrefix = *runtimeConfig.SystemPromptPrefix
		}
		if len(runtimeConfig.Tools) > 0 {
			agentTools = append([]fantasy.AgentTool(nil), runtimeConfig.Tools...)
		}
		if len(runtimeConfig.AllowedToolNames) > 0 {
			agentTools = filterToolsByNames(agentTools, runtimeConfig.AllowedToolNames)
		}
	}
	var instructions strings.Builder

	for _, server := range mcp.GetStates() {
		if server.State != mcp.StateConnected {
			continue
		}
		if s := server.Client.InitializeResult().Instructions; s != "" {
			instructions.WriteString(s)
			instructions.WriteString("\n\n")
		}
	}

	if s := instructions.String(); s != "" {
		systemPrompt += "\n\n<mcp-instructions>\n" + s + "\n</mcp-instructions>"
	}

	// Check if memory prefetch is ready (non-blocking). If settled, use cached
	// result. This approach mirrors Claude Code's design: the result is cached
	// after settlement, so retries get the same data without re-running.
	// Memory is now injected as user message (see below), not in system prompt,
	// to preserve prompt cache.
	prefetchedRecallReady := false
	promptMemoryInjected := false // guards one-time injection of prefetched memory into call.Prompt
	prefetchNotReadyLogged := false
	if !a.isSubAgent && call.MemoryPrefetch != nil {
		if result, settled := call.MemoryPrefetch.GetSettled(); settled {
			if result != "" {
				prefetchedRecallReady = true
				slog.Debug("[PERF] sessionAgent: prefetched memory recall ready", "session_id", call.SessionID)
			}
		} else {
			prefetchNotReadyLogged = true
			slog.Debug("[PERF] sessionAgent: memory prefetch not ready, will retry on next step", "session_id", call.SessionID)
		}
	}

	providerCtx := defaultProviderContext()
	requestPurpose := call.Purpose
	if requestPurpose == "" {
		requestPurpose = plugin.ChatTransformPurposeRequest
	}

	var preflightSummarized bool
	sessionLock := sync.Mutex{}
	currentSession, err := a.sessions.Get(genCtx, call.SessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to get session: %w", err)
	}

	msgs, err := a.getSessionMessages(genCtx, currentSession)
	if err != nil {
		return nil, fmt.Errorf("failed to get session messages: %w", err)
	}
	msgs = excludeCurrentUserMessage(msgs, call.UserMessage)
	slog.Debug("[PERF] sessionAgent: got session messages", "duration", time.Since(start), "count", len(msgs), "session_id", call.SessionID)
	promptPrefix = buildDelegationPromptPrefix(promptPrefix, agentTools, a.isSubAgent)
	if len(msgs) > 0 {
		// Prune old tool results before sending to plugins. This is a
		// pure in-memory operation that clears oversized tool output from
		// older messages, preventing the stdin/stdout JSON payload from
		// exceeding plugin buffer limits on session restore.
		msgs = builtinPruneToolResults(msgs)
	}
	slog.Debug("[PERF] sessionAgent: restored session context", "duration", time.Since(start), "session_id", call.SessionID)

	preflightState, err := a.buildChatRequestState(genCtx, chatRequestStateInput{
		SessionID:      call.SessionID,
		Agent:          "session",
		Model:          largeModel,
		Provider:       providerCtx,
		Purpose:        plugin.ChatTransformPurposePreflightEstimate,
		RequestPurpose: requestPurpose,
		Messages:       msgs,
		Message:        transientUserMessage(call.SessionID, call.Prompt, call.Attachments),
		Attachments:    call.Attachments,
		SystemPrompt:   systemPrompt,
		PromptPrefix:   promptPrefix,
		PermissionMode: currentSession.PermissionMode,
	})
	if err != nil {
		return nil, err
	}
	slog.Debug("[PERF] sessionAgent: preflight estimate done", "duration", time.Since(start), "session_id", call.SessionID)
	if !a.disableAutoSummarize && len(msgs) > 0 {
		// Estimate input tokens only. The shouldAutoSummarize function handles
		// output token reservation internally, so we don't need to add maxOutputTokens here.
		// This prevents double-counting the output reservation for large context models.
		estimatedInput := a.estimateSessionPromptTokens(preflightState.History, call.Prompt, call.Attachments, agentTools, preflightState.SystemPrompt, preflightState.PromptPrefix)
		if !preflightState.EstimateReduced {
			estimatedInput = max(estimatedInput, currentSession.LastInputTokens())
		}
		trigger := proactiveCompactionTrigger(largeModel, estimatedInput, call.MaxOutputTokens)
		budget := promptTokenBudgetForModel(largeModel, call.MaxOutputTokens)
		slog.Info("Auto-summarize preflight decision",
			"session_id", call.SessionID,
			"model", largeModel.ModelCfg.Model,
			"provider", largeModel.ModelCfg.Provider,
			"estimated_input_tokens", estimatedInput,
			"last_prompt_tokens", currentSession.LastPromptTokens,
			"context_window", budget.ContextWindow,
			"input_limit", budget.InputLimit,
			"max_output_tokens", budget.MaxOutputTokens,
			"reserved_input_tokens", budget.ReservedInputTokens,
			"usable_input_tokens", budget.UsableInputTokens,
			"uses_explicit_input_limit", budget.UsesExplicitInputLimit,
			"estimate_reduced", preflightState.EstimateReduced,
			"trigger", trigger,
		)
		if trigger != sessionCompactionTriggerNone {
			if truncErr := a.truncateOversizedToolResults(genCtx, call.SessionID); truncErr != nil {
				slog.Warn("Failed to truncate oversized tool results before preflight summarization", "error", truncErr, "session_id", call.SessionID)
			}
			summarizeCtx := context.WithValue(genCtx, internalCompactionKey{}, true)
			summarizeErr := a.Summarize(withSessionCompactingPurpose(copilot.ContextWithInitiatorType(summarizeCtx, copilot.InitiatorAgent), trigger.Purpose()), call.SessionID, call.ProviderOptions)
			if summarizeErr != nil {
				return nil, summarizeErr
			}
			preflightSummarized = true
			currentSession, err = a.sessions.Get(genCtx, call.SessionID)
			if err != nil {
				return nil, fmt.Errorf("failed to reload session after summarization: %w", err)
			}
			msgs, err = a.getSessionMessages(genCtx, currentSession)
			if err != nil {
				return nil, fmt.Errorf("failed to reload session messages after summarization: %w", err)
			}
			msgs = excludeCurrentUserMessage(msgs, call.UserMessage)
		}
	}
	slog.Debug("[PERF] sessionAgent: auto summarize check done", "duration", time.Since(start), "session_id", call.SessionID)

	var wg sync.WaitGroup
	if !call.NonInteractive && shouldGenerateSessionTitle(currentSession.Title) {
		titlePrompt := titlePromptFromCallOrHistory(call.Prompt, msgs)
		if titlePrompt != "" {
			titleCtx := copilot.ContextWithInitiatorType(genCtx, copilot.InitiatorAgent)
			wg.Go(func() {
				a.generateTitle(titleCtx, call.SessionID, titlePrompt, &sessionLock)
			})
		}
	}
	defer wg.Wait()

	// Add the user message to the session.
	var userMessage message.Message
	if call.UserMessage != nil {
		userMessage = *call.UserMessage
	} else {
		var err error
		userMessage, err = a.createUserMessage(genCtx, call)
		if err != nil {
			return nil, err
		}
	}
	slog.Debug("[PERF] sessionAgent: user message created (animation starts here)", "duration", time.Since(start), "session_id", call.SessionID)

	requestState, err := a.buildChatRequestState(genCtx, chatRequestStateInput{
		SessionID:      call.SessionID,
		Agent:          "session",
		Model:          largeModel,
		Provider:       providerCtx,
		Purpose:        requestPurpose,
		RequestPurpose: requestPurpose,
		Messages:       msgs,
		Message:        userMessage,
		Attachments:    call.Attachments,
		SystemPrompt:   systemPrompt,
		PromptPrefix:   promptPrefix,
		PermissionMode: currentSession.PermissionMode,
	})
	if err != nil {
		return nil, err
	}
	slog.Debug("[PERF] sessionAgent: buildChatRequestState done", "duration", time.Since(start), "session_id", call.SessionID)
	if len(agentTools) > 0 {
		// Add Anthropic caching to the last tool.
		agentTools[len(agentTools)-1].SetProviderOptions(a.getCacheControlOptions())
	}
	agent := a.agentFactory(
		retryableStreamModel{largeModel.Model},
		fantasy.WithSystemPrompt(requestState.SystemPrompt),
		fantasy.WithTools(agentTools...),
		fantasy.WithUserAgent(userAgent),
	)

	startTime := time.Now()
	a.eventPromptSent(call.SessionID)

	var shouldSummarize bool
	var compactionTrigger sessionCompactionTrigger
	var contextWindowExceeded bool
	var currentAssistant *message.Message
	var currentStepToolMessageIDs []string
	var currentStepToolResultChars int
	var allRunMessageIDs []string
	var estimatedPromptTokens int64
	var completedStepsThisRun int
	var runToolUses int
	var runLastTool string
	var emptyStreamRetryAttempt int
	// stripRedactedThinking is flipped on when a proxy rejects the
	// Anthropic `redacted_thinking` content block, so subsequent
	// prepareStep invocations strip those blocks from the history.
	var stripRedactedThinking bool
	// inStepCompactionBase holds the compacted messages produced by the
	// ChatMessagesTransform plugin during a PrepareStep call. On each
	// subsequent step we prepend this base instead of the full fantasy-
	// agent history, so the plugin's compaction is effectively persistent
	// within the run. inStepCompactionOffset records how many messages
	// options.Messages contained at the time of compaction; messages at
	// or after that index are the "new" messages appended since.
	var inStepCompactionBase []fantasy.Message
	var inStepCompactionOffset int
	runStream := func(providerOptions fantasy.ProviderOptions, billFirstStepAsUser bool) (*fantasy.AgentResult, error) {
		prefetchedRecallInjected := prefetchedRecallReady
		currentAssistant = nil
		currentStepToolMessageIDs = nil
		currentStepToolResultChars = 0
		allRunMessageIDs = nil
		estimatedPromptTokens = 0
		shouldSummarize = false
		inStepCompactionBase = nil
		inStepCompactionOffset = 0
		completedStepsThisRun = 0
		runToolUses = 0
		runLastTool = ""
		firstRequestStep = billFirstStepAsUser

		if err := a.plugins().TriggerChatBeforeRequest(genCtx, plugin.ChatBeforeRequestInput{
			SessionID: call.SessionID,
			Agent:     "session",
			Model: plugin.ModelInfo{
				ProviderID: largeModel.ModelCfg.Provider,
				ModelID:    largeModel.ModelCfg.Model,
			},
			Provider: providerCtx,
			Message:  userMessage,
		}); err != nil {
			return nil, markNonRetriableError(err)
		}

		var maxOutputTokens *int64
		if call.MaxOutputTokens > 0 {
			maxOutputTokens = &call.MaxOutputTokens
		}

		initialMessages := requestState.History
		if isAnthropicStyleProtocolProvider(largeModel) {
			var sanitized bool
			var sanitizedCount int
			initialMessages, sanitized, sanitizedCount = sanitizeAnthropicToolCallIDsInMessages(initialMessages)
			if sanitized {
				slog.Warn("Sanitized Anthropic-compatible tool call IDs before initial provider request",
					"session_id", call.SessionID,
					"model", largeModel.ModelCfg.Model,
					"provider", largeModel.ModelCfg.Provider,
					"sanitized_count", sanitizedCount,
				)
			}
		}
		if stripRedactedThinking {
			initialMessages, _ = stripRedactedThinkingParts(initialMessages)
		}
		// Inject memory for first request if ready.
		// Mirror Claude Code's attachment-merge approach: memory content is merged into
		// the user prompt (as an extra text block), NOT prepended as a separate message.
		// Prepending would change the message prefix and invalidate prompt cache.
		// By appending to the prompt, the system message prefix stays stable.
		// promptMemoryInjected is declared in the outer Run scope (not inside runStream)
		// so that retry calls to runStream do NOT re-append the same memory content.
		if prefetchedRecallReady && !promptMemoryInjected && call.MemoryPrefetch != nil {
			if result, settled := call.MemoryPrefetch.GetSettled(); settled && result != "" {
				call.Prompt += "\n\n" + FormatAutoRecallMessage(result)
				promptMemoryInjected = true
			}
		}

		var stepMessages []fantasy.Message
		result, err := agent.Stream(genCtx, fantasy.AgentStreamCall{
			Prompt:           message.PromptWithTextAttachments(call.Prompt, call.Attachments),
			Files:            requestState.Files,
			Messages:         initialMessages,
			ProviderOptions:  providerOptions,
			MaxOutputTokens:  maxOutputTokens,
			TopP:             call.TopP,
			Temperature:      call.Temperature,
			PresencePenalty:  call.PresencePenalty,
			TopK:             call.TopK,
			FrequencyPenalty: call.FrequencyPenalty,
			PrepareStep: func(callContext context.Context, options fantasy.PrepareStepFunctionOptions) (_ context.Context, prepared fantasy.PrepareStepResult, err error) {
				// Explicitly tag every LLM request with the correct X-Initiator value
				// so GitHub Copilot billing is correct regardless of how the fantasy
				// framework propagates the outer context. Only the first step of a
				// real user-initiated request is billable; tool-call loops,
				// sub-agent steps, and continuations are always free.
				if isUserInitiatedRequest && firstRequestStep {
					callContext = copilot.ContextWithInitiatorType(callContext, copilot.InitiatorUser)
				} else {
					callContext = copilot.ContextWithInitiatorType(callContext, copilot.InitiatorAgent)
				}
				firstRequestStep = false

				stepRuntimeConfig := runtimeConfig
				if a.refreshCallConfig != nil {
					refreshed, refreshErr := a.refreshCallConfig(callContext)
					if refreshErr != nil {
						slog.Warn("Failed to refresh runtime config for step", "error", refreshErr, "session_id", call.SessionID)
					} else {
						stepRuntimeConfig = &refreshed
					}
				}

				prepared.Tools = a.tools.Copy()
				if stepRuntimeConfig != nil && len(stepRuntimeConfig.Tools) > 0 {
					prepared.Tools = append([]fantasy.AgentTool(nil), stepRuntimeConfig.Tools...)
				}
				if stepRuntimeConfig != nil && len(stepRuntimeConfig.AllowedToolNames) > 0 {
					prepared.Tools = filterToolsByNames(prepared.Tools, stepRuntimeConfig.AllowedToolNames)
				}
				// Add Anthropic caching to the last tool.
				if len(prepared.Tools) > 0 {
					prepared.Tools[len(prepared.Tools)-1].SetProviderOptions(a.getCacheControlOptions())
				}

				// If the plugin performed a significant in-step compaction on a
				// previous step, reconstruct prepared.Messages from the compacted
				// base plus only the messages added since that compaction. This
				// makes the plugin's compaction persistent across steps within a
				// single run — without it the fantasy agent rebuilds the full
				// initialPrompt+responseMessages slice on every iteration,
				// causing the plugin to re-compact every step indefinitely.
				if inStepCompactionBase != nil && inStepCompactionOffset <= len(options.Messages) {
					newMsgs := options.Messages[inStepCompactionOffset:]
					prepared.Messages = append(append([]fantasy.Message{}, inStepCompactionBase...), newMsgs...)
				} else {
					prepared.Messages = options.Messages
				}
				for i := range prepared.Messages {
					prepared.Messages[i].ProviderOptions = nil
				}
				if stripRedactedThinking {
					prepared.Messages, _ = stripRedactedThinkingParts(prepared.Messages)
				}

				if !a.isSubAgent && call.MemoryPrefetch != nil && !prefetchedRecallInjected {
					if result, settled := call.MemoryPrefetch.GetSettled(); settled {
						prefetchedRecallInjected = true
						if result != "" {
							// Mirror Claude Code's approach: memory content is merged into
							// the last user message (which carries tool_results) as extra text,
							// NOT prepended as a separate message. Prepending would change
							// the message prefix and invalidate established prompt cache.
							// By appending to an existing user message, the system prompt
							// and conversation history prefix remain cache-stable.
							memoryContent := FormatAutoRecallMessage(result)
							injected := false
							for i := len(prepared.Messages) - 1; i >= 0; i-- {
								if prepared.Messages[i].Role == fantasy.MessageRoleUser {
									memoryPart := fantasy.TextPart{Text: "\n\n" + memoryContent}
									// Insert before the first tool result block so text content
									// precedes tool results, matching the Anthropic API format.
									insertIdx := len(prepared.Messages[i].Content)
									for j, c := range prepared.Messages[i].Content {
										if c.GetType() == fantasy.ContentTypeToolResult {
											insertIdx = j
											break
										}
									}
									prepared.Messages[i].Content = append(prepared.Messages[i].Content[:insertIdx], append([]fantasy.MessagePart{memoryPart}, prepared.Messages[i].Content[insertIdx:]...)...)
									injected = true
									break
								}
							}
							// Fallback: if no user message exists to merge into (e.g. empty
							// history), append a new user message. This keeps the system
							// prompt prefix stable because the new message is at the tail,
							// not at the head of the array.
							if !injected {
								prepared.Messages = append(prepared.Messages, fantasy.NewUserMessage(memoryContent))
							}
							slog.Debug("[PERF] sessionAgent: injected settled memory prefetch into last user message", "session_id", call.SessionID)
						}
					} else if !prefetchNotReadyLogged {
						prefetchNotReadyLogged = true
						slog.Debug("[PERF] sessionAgent: memory prefetch not ready, will retry on next step", "session_id", call.SessionID)
					}
				}

				// Save the auto_recall content in case transform removes it.
				var autoRecallContent string
				if prefetchedRecallInjected && call.MemoryPrefetch != nil {
					if result, settled := call.MemoryPrefetch.GetSettled(); settled && result != "" {
						autoRecallContent = result
					}
				}

				// Prune old tool results before sending to plugins in each step.
				// Only attempt the expensive fantasy↔internal conversion when the
				// message count suggests pruning might be worthwhile.
				if len(prepared.Messages) > builtinPruneRecentUserTurns*3 {
					internalForPrune := message.FromFantasyMessages(prepared.Messages)
					pruned := builtinPruneToolResults(internalForPrune)
					// builtinPruneToolResults returns the same slice pointer when
					// nothing was pruned — only reconvert if it actually changed.
					if len(pruned) > 0 && &pruned[0] != &internalForPrune[0] {
						prepared.Messages, _ = a.preparePrompt(pruned)
					}
				}

				// Trigger messages.transform plugin on every step (including tool result steps).
				// This allows plugins like morph_compact to compress messages after tool calls.
				if len(prepared.Messages) > 0 {
					originalTokens := a.estimateSessionPromptTokens(
						prepared.Messages,
						call.Prompt,
						call.Attachments,
						prepared.Tools,
						requestState.SystemPrompt,
						requestState.PromptPrefix,
					)
					internalMsgs := message.FromFantasyMessages(prepared.Messages)
					transformedMsgs, transformErr := a.plugins().TriggerChatMessagesTransform(callContext, plugin.ChatMessagesTransformInput{
						SessionID:      call.SessionID,
						Agent:          "session",
						Model:          agentModelInfo(largeModel),
						Provider:       providerCtx,
						Purpose:        requestPurpose,
						RequestPurpose: requestPurpose,
						Usage:          usageSnapshotFromMessages(internalMsgs, originalTokens),
					}, plugin.ChatMessagesTransformOutput{Messages: internalMsgs})
					if transformErr != nil {
						slog.Warn("Failed to transform messages in PrepareStep", "error", transformErr, "session_id", call.SessionID)
					} else if len(transformedMsgs.Messages) > 0 {
						// Convert back to fantasy messages.
						prepared.Messages, _ = a.preparePrompt(transformedMsgs.Messages)
						if stripRedactedThinking {
							prepared.Messages, _ = stripRedactedThinkingParts(prepared.Messages)
						}

						// Re-inject auto_recall if transform removed it.
						// Mirror Claude Code's approach: merge into the last user message
						// instead of prepending, to preserve prompt cache stability.
						if autoRecallContent != "" && !hasAutoRecallInMessages(prepared.Messages) {
							memoryContent := FormatAutoRecallMessage(autoRecallContent)
							injected := false
							for i := len(prepared.Messages) - 1; i >= 0; i-- {
								if prepared.Messages[i].Role == fantasy.MessageRoleUser {
									prepared.Messages[i].Content = append(prepared.Messages[i].Content, fantasy.TextPart{Text: "\n\n" + memoryContent})
									injected = true
									break
								}
							}
							if !injected {
								prepared.Messages = append(prepared.Messages, fantasy.NewUserMessage(memoryContent))
							}
							slog.Debug("[PERF] sessionAgent: re-injected auto_recall into last user message after transform", "session_id", call.SessionID)
						}

						newTokens := a.estimateSessionPromptTokens(
							prepared.Messages,
							call.Prompt,
							call.Attachments,
							prepared.Tools,
							requestState.SystemPrompt,
							requestState.PromptPrefix,
						)
						// If morph compression reduced tokens significantly, update the local
						// session copy so follow-up auto-summarize checks use the reduced
						// prompt estimate for this in-flight request.
						if newTokens < originalTokens {
							slog.Debug("Messages transformed (compressed) in PrepareStep", "original_tokens", originalTokens, "new_tokens", newTokens, "session_id", call.SessionID)
							sessionLock.Lock()
							if newTokens < currentSession.LastPromptTokens {
								currentSession.LastPromptTokens = newTokens
							}
							sessionLock.Unlock()
							// When the plugin reduces tokens by more than 40%, treat it as a
							// real compaction. Persist the result as the new base so future
							// steps continue from the compacted state instead of the full
							// fantasy-agent history (which rebuilds every step).
							if newTokens*10 < originalTokens*6 {
								inStepCompactionBase = prepared.Messages
								inStepCompactionOffset = len(options.Messages)
								slog.Debug("Per-step compaction base updated",
									"base_tokens", newTokens,
									"offset", inStepCompactionOffset,
									"session_id", call.SessionID,
								)
							}
						}
					}
				}

				queuedCalls := a.takeJoinActiveRunCalls(call.SessionID)
				remainingJoinBudget := joinActiveRunPromptCharsBudget

				type selectedCall struct {
					index int
					call  SessionAgentCall
				}
				var selected []selectedCall
				// Dedupe queued prompts that exactly match the active call
				// prompt or another already-selected queued prompt. Pressing
				// "继续" / "continue" multiple times in quick succession would
				// otherwise inject N identical text blocks into one Anthropic
				// user turn, which is pure noise to the model and on some
				// providers (Anthropic with thinking enabled, several
				// Anthropic-compatible proxies) causes the assistant to treat
				// the turn as a redundant continuation and end_turn early —
				// the "莫名其妙中断" symptom. Drop duplicates outright instead
				// of re-enqueuing them, otherwise they'd just be reinjected on
				// the next step.
				seenPrompts := make(map[string]struct{})
				if mainPrompt := strings.TrimSpace(call.Prompt); mainPrompt != "" {
					seenPrompts[mainPrompt] = struct{}{}
				}
				for i := len(queuedCalls) - 1; i >= 0; i-- {
					queued := queuedCalls[i]
					if len(selected) >= joinActiveRunMaxInjectedCalls || remainingJoinBudget <= 0 {
						a.enqueueQueuedCall(call.SessionID, queued)
						continue
					}
					prompt := strings.TrimSpace(queued.Prompt)
					if prompt == "" {
						a.enqueueQueuedCall(call.SessionID, queued)
						continue
					}
					if _, dup := seenPrompts[prompt]; dup {
						// Identical to an already-handled prompt this turn;
						// drop silently rather than re-enqueue.
						slog.Debug("dropping duplicate queued prompt for join-active-run",
							"session_id", call.SessionID, "prompt_runes", len([]rune(prompt)))
						continue
					}
					promptRunes := []rune(prompt)
					if len(promptRunes) > remainingJoinBudget {
						if remainingJoinBudget <= 1 {
							a.enqueueQueuedCall(call.SessionID, queued)
							continue
						}
						prompt = string(promptRunes[:remainingJoinBudget-1]) + "…"
					}
					queued.Prompt = prompt
					seenPrompts[prompt] = struct{}{}
					selected = append(selected, selectedCall{index: i, call: queued})
					remainingJoinBudget -= len([]rune(prompt))
				}

				for s := len(selected) - 1; s >= 0; s-- {
					userMessage, createErr := a.createUserMessage(callContext, selected[s].call)
					if createErr != nil {
						return callContext, prepared, createErr
					}
					prepared.Messages = append(prepared.Messages, userMessage.ToAIMessage()...)
				}

				// Defensive structural merge: collapse any consecutive
				// same-role fantasy messages into a single message before
				// handing off to provider serialization. Even though
				// fantasy's Anthropic provider already groups consecutive
				// user/tool messages into one wire-level user turn, this
				// guarantees the invariant locally and removes pathological
				// repeated text blocks (e.g. ["继续","继续","继续"]) that
				// look like noise to the model and can cause early end_turn
				// on Anthropic with thinking enabled.
				prepared.Messages = mergeConsecutiveSameRoleFantasyMessages(prepared.Messages)

				prepared.Messages = a.workaroundProviderMediaLimitations(prepared.Messages, largeModel)
				if !largeModel.CatwalkCfg.SupportsImages {
					prepared.Messages = stripImagePartsFromFantasyMessages(prepared.Messages)
				}

				lastSystemRoleInx := 0
				systemMessageUpdated := false
				for i, msg := range prepared.Messages {
					// Only add cache control to the last message.
					if msg.Role == fantasy.MessageRoleSystem {
						lastSystemRoleInx = i
					} else if !systemMessageUpdated {
						prepared.Messages[lastSystemRoleInx].ProviderOptions = a.getCacheControlOptions()
						systemMessageUpdated = true
					}
					// Than add cache control to the last 2 messages.
					if i > len(prepared.Messages)-3 {
						prepared.Messages[i].ProviderOptions = a.getCacheControlOptions()
					}
				}

				if requestState.PromptPrefix != "" {
					prepared.Messages = append([]fantasy.Message{fantasy.NewSystemMessage(requestState.PromptPrefix)}, prepared.Messages...)
				}
				if isAnthropicStyleProtocolProvider(largeModel) {
					var sanitized bool
					var sanitizedCount int
					prepared.Messages, sanitized, sanitizedCount = sanitizeAnthropicToolCallIDsInMessages(prepared.Messages)
					if sanitized {
						slog.Warn("Sanitized Anthropic-compatible tool call IDs before provider request",
							"session_id", call.SessionID,
							"model", largeModel.ModelCfg.Model,
							"provider", largeModel.ModelCfg.Provider,
							"sanitized_count", sanitizedCount,
						)
					}
				}

				var assistantMsg message.Message
				assistantMsg, err = a.messages.Create(callContext, call.SessionID, message.CreateMessageParams{
					Role:                   message.Assistant,
					Parts:                  []message.ContentPart{},
					Model:                  largeModel.ModelCfg.Model,
					Provider:               largeModel.ModelCfg.Provider,
					ActivatedDeferredTools: a.currentActivatedDeferredTools(call.SessionID),
				})
				if err != nil {
					return callContext, prepared, err
				}
				callContext = context.WithValue(callContext, tools.MessageIDContextKey, assistantMsg.ID)
				callContext = context.WithValue(callContext, tools.SupportsImagesContextKey, largeModel.CatwalkCfg.SupportsImages)
				callContext = context.WithValue(callContext, tools.ModelNameContextKey, largeModel.CatwalkCfg.Name)
				callContext = context.WithValue(callContext, tools.SessionServiceContextKey, a.sessions)
				callContext = context.WithValue(callContext, tools.MessageServiceContextKey, a.messages)
				currentAssistant = &assistantMsg
				currentStepToolMessageIDs = nil
				currentStepToolResultChars = 0
				allRunMessageIDs = append(allRunMessageIDs, assistantMsg.ID)

				// For follow-up steps (e.g., tool result steps), prepared.Messages already
				// contains the user message from the initial request, so we should not
				// double-count by passing call.Prompt/attachments. For the initial step,
				// prepared.Messages does not yet contain the user prompt (it's added by
				// the fantasy framework), so we need call.Prompt/attachments for estimation.
				// We check whether options.Messages (from the fantasy framework, before any
				// local modifications like auto_recall, PromptPrefix, or queued calls) has
				// grown beyond initialMessages to determine if the current user message has
				// been added. This correctly handles retries and distinguishes from follow-up
				// steps without being affected by local message additions.
				hasCurrentUserMessage := len(options.Messages) > len(initialMessages)
				promptForEstimate := call.Prompt
				attachmentsForEstimate := call.Attachments
				if hasCurrentUserMessage {
					promptForEstimate = ""
					attachmentsForEstimate = nil
				}

				estimatedPromptTokens = a.estimateSessionPromptTokens(
					prepared.Messages,
					promptForEstimate,
					attachmentsForEstimate,
					prepared.Tools,
					requestState.SystemPrompt,
					"",
				)
				promptBudget := promptTokenBudgetForModel(largeModel, call.MaxOutputTokens)
				wouldAutoSummarize := promptBudget.UsableInputTokens > 0 && estimatedPromptTokens >= promptBudget.UsableInputTokens
				slog.Debug("Prepared provider prompt usage estimate",
					"session_id", call.SessionID,
					"model", largeModel.ModelCfg.Model,
					"provider", largeModel.ModelCfg.Provider,
					"estimated_prompt_tokens", estimatedPromptTokens,
					"context_window", promptBudget.ContextWindow,
					"input_limit", promptBudget.InputLimit,
					"max_output_tokens", promptBudget.MaxOutputTokens,
					"reserved_input_tokens", promptBudget.ReservedInputTokens,
					"usable_input_tokens", promptBudget.UsableInputTokens,
					"would_auto_summarize", wouldAutoSummarize,
					"prepared_messages", len(prepared.Messages),
					"prepared_tools", len(prepared.Tools),
					"has_current_user_message", hasCurrentUserMessage,
					"prompt_for_estimate_chars", len([]rune(promptForEstimate)),
					"attachments_for_estimate", len(attachmentsForEstimate),
				)
				stepMessages = cloneFantasyMessages(prepared.Messages)
				if estimatedPromptTokens > 0 {
					currentAssistant.SetUsage(message.Usage{InputTokens: estimatedPromptTokens})
					if updateUsageErr := a.messages.Update(callContext, *currentAssistant); updateUsageErr != nil {
						return callContext, prepared, updateUsageErr
					}
				}
				return callContext, prepared, err
			},
			OnReasoningStart: func(id string, reasoning fantasy.ReasoningContent) error {
				currentAssistant.AppendReasoningContent(reasoning.Text)
				return a.messages.Update(genCtx, *currentAssistant)
			},
			OnReasoningDelta: func(id string, text string) error {
				currentAssistant.AppendReasoningContent(text)
				return a.messages.Update(genCtx, *currentAssistant)
			},
			OnReasoningEnd: func(id string, reasoning fantasy.ReasoningContent) error {
				// handle anthropic signature
				if anthropicData, ok := reasoning.ProviderMetadata[anthropic.Name]; ok {
					if reasoning, ok := anthropicData.(*anthropic.ReasoningOptionMetadata); ok {
						currentAssistant.AppendReasoningSignature(reasoning.Signature)
					}
				}
				if googleData, ok := reasoning.ProviderMetadata[google.Name]; ok {
					if reasoning, ok := googleData.(*google.ReasoningMetadata); ok {
						currentAssistant.AppendThoughtSignature(reasoning.Signature, reasoning.ToolID)
					}
				}
				if openaiData, ok := reasoning.ProviderMetadata[openai.Name]; ok {
					if reasoning, ok := openaiData.(*openai.ResponsesReasoningMetadata); ok {
						currentAssistant.SetReasoningResponsesData(reasoning)
					}
				}
				currentAssistant.FinishThinking()
				return a.messages.Update(genCtx, *currentAssistant)
			},
			OnTextDelta: func(id string, text string) error {
				// Strip leading newline from initial text content. This is is
				// particularly important in non-interactive mode where leading
				// newlines are very visible.
				if len(currentAssistant.Parts) == 0 {
					text = strings.TrimPrefix(text, "\n")
				}

				currentAssistant.AppendContent(text)
				return a.messages.Update(genCtx, *currentAssistant)
			},
			OnToolInputStart: func(id string, toolName string) error {
				currentAssistant.FinishThinking()
				toolCall := message.ToolCall{
					ID:               id,
					Name:             toolName,
					ProviderExecuted: false,
					Finished:         false,
				}
				currentAssistant.AddToolCall(toolCall)
				if stripTextualToolCallProtocolFromAssistant(currentAssistant) {
					slog.Warn("Removed textual tool-call protocol from assistant content after tool input started",
						"session_id", currentAssistant.SessionID,
						"message_id", currentAssistant.ID,
						"model", currentAssistant.Model,
						"provider", currentAssistant.Provider,
						"tool_call_id", id,
						"tool_name", toolName,
					)
				}
				return a.messages.Update(ctx, *currentAssistant)
			},
			OnRetry: func(providerErr *fantasy.ProviderError, delay time.Duration) {
				slog.Info("Retrying after network error", "error", providerErr.Error(), "delay", delay)
				if currentAssistant == nil {
					return
				}
				if err := a.resetRetriedStep(ctx, currentAssistant, currentStepToolMessageIDs); err != nil {
					slog.Warn("Failed to reset step state before retry", "error", err, "session_id", currentAssistant.SessionID, "message_id", currentAssistant.ID)
					return
				}
				currentStepToolMessageIDs = nil
				currentStepToolResultChars = 0
			},
			OnToolCall: func(tc fantasy.ToolCallContent) error {
				currentAssistant.FinishThinking()
				toolCall := message.ToolCall{
					ID:               tc.ToolCallID,
					Name:             tc.ToolName,
					Input:            tc.Input,
					ProviderExecuted: false,
					Finished:         true,
				}
				currentAssistant.AddToolCall(toolCall)
				if stripTextualToolCallProtocolFromAssistant(currentAssistant) {
					slog.Warn("Removed textual tool-call protocol from assistant content after structured tool call",
						"session_id", currentAssistant.SessionID,
						"message_id", currentAssistant.ID,
						"model", currentAssistant.Model,
						"provider", currentAssistant.Provider,
						"tool_call_id", tc.ToolCallID,
						"tool_name", tc.ToolName,
					)
				}
				runToolUses++
				runLastTool = tc.ToolName
				return a.messages.Update(ctx, *currentAssistant)
			},
			OnToolResult: func(result fantasy.ToolResultContent) error {
				toolResult := a.convertToToolResult(result)
				if toolResult.Name == tools.ToolSearchToolName {
					if state, ok := deferredToolStateFromToolSearchResult(toolResult.Content); ok {
						toolResult = toolResult.WithDeferredToolState(state)
					}
				}
				toolResult.Content = redactSecrets(toolResult.Content)
				toolResult, additionalMedia := a.extractAdditionalMCPMedia(toolResult)
				if runtimeConfig != nil {
					toolResult = a.applyToolResultReview(genCtx, currentAssistant.SessionID, toolResult, runtimeConfig.PermissionMode)
				}
				toolResult = a.enforceStepToolResultBudget(currentAssistant.SessionID, toolResult, &currentStepToolResultChars)
				if truncatedResult, truncated := a.truncateToolResult(currentAssistant.SessionID, toolResult); truncated {
					toolResult = truncatedResult
				}
				toolMsg, createMsgErr := a.messages.Create(ctx, currentAssistant.SessionID, message.CreateMessageParams{
					Role:                   message.Tool,
					Parts:                  []message.ContentPart{toolResult},
					ActivatedDeferredTools: a.currentActivatedDeferredTools(currentAssistant.SessionID),
				})
				if createMsgErr != nil {
					return createMsgErr
				}
				currentStepToolMessageIDs = append(currentStepToolMessageIDs, toolMsg.ID)
				allRunMessageIDs = append(allRunMessageIDs, toolMsg.ID)

				if len(additionalMedia) > 0 {
					parts := make([]message.ContentPart, 0, len(additionalMedia)+1)
					parts = append(parts, message.TextContent{Text: "Additional media content from the tool result:"})
					for _, mediaPart := range additionalMedia {
						parts = append(parts, mediaPart)
					}
					additionalMsg, additionalErr := a.messages.Create(ctx, currentAssistant.SessionID, message.CreateMessageParams{
						Role:                   message.User,
						Parts:                  parts,
						ActivatedDeferredTools: a.currentActivatedDeferredTools(currentAssistant.SessionID),
					})
					if additionalErr != nil {
						return additionalErr
					}
					currentStepToolMessageIDs = append(currentStepToolMessageIDs, additionalMsg.ID)
					allRunMessageIDs = append(allRunMessageIDs, additionalMsg.ID)
				}
				return nil
			},
			OnStepFinish: func(stepResult fantasy.StepResult) error {
				if len(currentAssistant.ToolCalls()) > 0 && stripTextualToolCallProtocolFromAssistant(currentAssistant) {
					slog.Warn("Removed textual tool-call protocol from assistant content at step finish",
						"session_id", currentAssistant.SessionID,
						"message_id", currentAssistant.ID,
						"model", currentAssistant.Model,
						"provider", currentAssistant.Provider,
						"tool_calls_count", len(currentAssistant.ToolCalls()),
						"finish_reason", stepResult.FinishReason,
					)
				}
				finishReason := message.FinishReasonUnknown
				switch stepResult.FinishReason {
				case fantasy.FinishReasonLength:
					finishReason = message.FinishReasonMaxTokens
				case fantasy.FinishReasonStop:
					finishReason = message.FinishReasonEndTurn
				case fantasy.FinishReasonToolCalls:
					finishReason = message.FinishReasonToolUse
				}
				currentAssistant.AddFinish(finishReason, "", "")
				sessionLock.Lock()
				defer sessionLock.Unlock()

				updatedSession, getSessionErr := a.sessions.Get(ctx, call.SessionID)
				if getSessionErr != nil {
					return getSessionErr
				}
				usage := stepResult.Usage
				estimated := false
				var currAssistant message.Message
				if currentAssistant != nil {
					currAssistant = *currentAssistant
				}
				if fallbackUsage, ok := fallbackStepUsage(stepMessages, stepResult, currAssistant); ok {
					usage = fallbackUsage
					estimated = true
				}
				a.updateSessionUsage(largeModel, &updatedSession, usage, a.openrouterCost(stepResult.ProviderMetadata), estimatedPromptTokens, estimated)
				_, sessionErr := a.sessions.Save(ctx, updatedSession)
				if sessionErr != nil {
					return sessionErr
				}
				completedStepsThisRun++
				currentSession = updatedSession
				normalizedUsage := normalizedMessageUsage(usage, usageProvider(largeModel), estimatedPromptTokens)
				slog.Debug("Updated assistant usage from provider response",
					"session_id", call.SessionID,
					"message_id", currentAssistant.ID,
					"model", largeModel.ModelCfg.Model,
					"provider", largeModel.ModelCfg.Provider,
					"provider_usage_provider", usageProvider(largeModel),
					"provider_input_tokens", stepResult.Usage.InputTokens,
					"provider_output_tokens", stepResult.Usage.OutputTokens,
					"provider_reasoning_tokens", stepResult.Usage.ReasoningTokens,
					"provider_cache_read_tokens", stepResult.Usage.CacheReadTokens,
					"provider_cache_creation_tokens", stepResult.Usage.CacheCreationTokens,
					"estimated_prompt_tokens", estimatedPromptTokens,
					"normalized_prompt_tokens", normalizedUsage.PromptTokens(),
					"normalized_output_tokens", normalizedUsage.OutputTokens,
					"normalized_reasoning_tokens", normalizedUsage.ReasoningTokens,
					"normalized_total_tokens", normalizedUsage.PromptTokens()+normalizedUsage.OutputTokens,
				)
				currentAssistant.SetUsage(normalizedUsage)
				updateErr := a.messages.Update(genCtx, *currentAssistant)
				if call.OnProgress != nil {
					call.OnProgress(runToolUses, runLastTool)
				}
				return updateErr
			},
			StopWhen: []fantasy.StopCondition{
				func(_ []fantasy.StepResult) bool {
					projectedPromptTokens, estimateReduced, estimateErr := a.estimateNextStepPromptTokens(genCtx, call.SessionID, agentTools, systemPrompt, promptPrefix, largeModel, providerCtx, requestPurpose)
					if estimateErr != nil {
						slog.Warn("Failed to estimate next-step prompt tokens", "error", estimateErr, "session_id", call.SessionID)
						// Fallback: use the higher of LastInputTokens or the current step's estimatedPromptTokens.
						// estimatedPromptTokens is set during PrepareStep and reflects the actual messages
						// that will be sent to the LLM, making it more accurate than LastInputTokens.
						fallbackTokens := currentSession.LastInputTokens()
						if estimatedPromptTokens > fallbackTokens {
							fallbackTokens = estimatedPromptTokens
							slog.Info("Using current step's estimatedPromptTokens as fallback", "estimatedPromptTokens", estimatedPromptTokens, "lastInputTokens", currentSession.LastInputTokens(), "session_id", call.SessionID)
						} else {
							slog.Info("Using LastInputTokens as fallback", "lastInputTokens", fallbackTokens, "session_id", call.SessionID)
						}
						projectedPromptTokens = fallbackTokens
					}
					if !preflightSummarized && !estimateReduced {
						projectedPromptTokens = max(projectedPromptTokens, currentSession.LastInputTokens())
					}
					// Pass input-only estimate to shouldAutoSummarize. The function
					// handles output token reservation internally to avoid
					// double-counting. Skip when preflight already summarized to
					// prevent double summarization in the same Run call.
					// Context-window-exceeded errors are handled separately and
					// will still trigger recovery summarization.
					shouldStepSummarize := shouldAutoSummarize(largeModel, projectedPromptTokens, call.MaxOutputTokens)
					budget := promptTokenBudgetForModel(largeModel, call.MaxOutputTokens)
					slog.Debug("Auto-summarize step decision",
						"session_id", call.SessionID,
						"model", largeModel.ModelCfg.Model,
						"provider", largeModel.ModelCfg.Provider,
						"projected_prompt_tokens", projectedPromptTokens,
						"last_prompt_tokens", currentSession.LastPromptTokens,
						"estimated_prompt_tokens", estimatedPromptTokens,
						"context_window", budget.ContextWindow,
						"input_limit", budget.InputLimit,
						"max_output_tokens", budget.MaxOutputTokens,
						"reserved_input_tokens", budget.ReservedInputTokens,
						"usable_input_tokens", budget.UsableInputTokens,
						"uses_explicit_input_limit", budget.UsesExplicitInputLimit,
						"estimate_reduced", estimateReduced,
						"estimate_error", estimateErr != nil,
						"should_summarize", shouldStepSummarize,
						"preflight_summarized", preflightSummarized,
						"auto_summarize_disabled", a.disableAutoSummarize,
					)
					if shouldStepSummarize && !a.disableAutoSummarize && !preflightSummarized {
						shouldSummarize = true
						return true
					}
					return false
				},
				func(steps []fantasy.StepResult) bool {
					return hasRepeatedToolCalls(steps, loopDetectionWindowSize, loopDetectionMaxRepeats)
				},
			},
		})
		if err == nil {
			hydrateAgentResultFromAssistantMessage(result, currentAssistant)
		}
		if hookErr := a.plugins().TriggerChatAfterResponse(genCtx, plugin.ChatAfterResponseInput{
			SessionID: call.SessionID,
			Agent:     "session",
			Model: plugin.ModelInfo{
				ProviderID: largeModel.ModelCfg.Provider,
				ModelID:    largeModel.ModelCfg.Model,
			},
			Purpose: requestPurpose,
			Result:  result,
			Error:   err,
		}); hookErr != nil {
			if err != nil {
				return nil, fmt.Errorf("stream error: %w; hook error: %w", err, markNonRetriableError(hookErr))
			}
			return nil, markNonRetriableError(hookErr)
		}
		return result, err
	}

	providerOptions := call.ProviderOptions
	var result *fantasy.AgentResult
	var retryAttempt int
	var textualToolProtocolRetryAttempt int
	var textualToolProtocolRecoveries int
	textualToolProtocolRecoveryCounts := make(map[string]int)
	for {
		result, err = runStream(providerOptions, retryAttempt == 0)

		if err == nil {
			textualToolCalls := parseTextualToolCallsFromAssistant(currentAssistant)
			if len(textualToolCalls) > 0 && textualToolProtocolRecoveries >= maxTextualToolProtocolRecoveries {
				if cleanupErr := a.cleanupFailedAttempt(ctx, currentAssistant, currentStepToolMessageIDs); cleanupErr != nil {
					return nil, cleanupErr
				}
				err = fmt.Errorf("model emitted textual tool-call protocol instead of structured tool calls after repeated recovery")
				break
			}
			if repeatedKey, repeatedCount, repeated := recordTextualToolCallRecoveries(textualToolCalls, textualToolProtocolRecoveryCounts); repeated {
				if cleanupErr := a.cleanupFailedAttempt(ctx, currentAssistant, currentStepToolMessageIDs); cleanupErr != nil {
					return nil, cleanupErr
				}
				err = fmt.Errorf("model repeatedly emitted the same textual tool-call protocol instead of structured tool calls")
				slog.Warn("Stopping repeated textual tool-call recovery",
					"session_id", call.SessionID,
					"model", largeModel.ModelCfg.Model,
					"provider", largeModel.ModelCfg.Provider,
					"repeated_key", repeatedKey,
					"repeated_count", repeatedCount,
				)
				break
			}
			recoveredToolMessageIDs, recoveredToolUses, recoveredLastTool, recovered, recoverErr := a.recoverTextualToolCallProtocol(
				ctx,
				genCtx,
				currentAssistant,
				textualToolCalls,
				agentTools,
				runtimeConfig,
				largeModel,
				&currentStepToolResultChars,
			)
			if recoverErr != nil {
				err = recoverErr
				break
			}
			if recovered {
				textualToolProtocolRecoveries++
				currentStepToolMessageIDs = append(currentStepToolMessageIDs, recoveredToolMessageIDs...)
				runToolUses += recoveredToolUses
				if recoveredLastTool != "" {
					runLastTool = recoveredLastTool
				}
				currentSession, err = a.sessions.Get(ctx, call.SessionID)
				if err != nil {
					return nil, fmt.Errorf("failed to reload session after textual tool-call recovery: %w", err)
				}
				retryMsgs, getMsgsErr := a.getSessionMessages(ctx, currentSession)
				if getMsgsErr != nil {
					return nil, getMsgsErr
				}
				retryMsgs = excludeCurrentUserMessage(retryMsgs, call.UserMessage)
				retryState, buildErr := a.buildChatRequestState(genCtx, chatRequestStateInput{
					SessionID:      call.SessionID,
					Agent:          "session",
					Model:          largeModel,
					Provider:       providerCtx,
					Purpose:        requestPurpose,
					RequestPurpose: requestPurpose,
					Messages:       retryMsgs,
					Message:        userMessage,
					Attachments:    call.Attachments,
					SystemPrompt:   systemPrompt,
					PromptPrefix:   promptPrefix,
					PermissionMode: currentSession.PermissionMode,
				})
				if buildErr != nil {
					return nil, buildErr
				}
				requestState = retryState
				currentAssistant = nil
				currentStepToolMessageIDs = nil
				currentStepToolResultChars = 0
				continue
			}
		}

		if err == nil && shouldRetryForTextualToolCallProtocol(currentAssistant) {
			if cleanupErr := a.cleanupFailedAttempt(ctx, currentAssistant, currentStepToolMessageIDs); cleanupErr != nil {
				return nil, cleanupErr
			}
			currentAssistant = nil
			currentStepToolMessageIDs = nil
			currentStepToolResultChars = 0
			if completedStepsThisRun > 0 {
				retryMsgs, getMsgsErr := a.getSessionMessages(ctx, currentSession)
				if getMsgsErr != nil {
					return nil, getMsgsErr
				}
				retryMsgs = excludeCurrentUserMessage(retryMsgs, call.UserMessage)
				retryState, buildErr := a.buildChatRequestState(genCtx, chatRequestStateInput{
					SessionID:      call.SessionID,
					Agent:          "session",
					Model:          largeModel,
					Provider:       providerCtx,
					Purpose:        requestPurpose,
					RequestPurpose: requestPurpose,
					Messages:       retryMsgs,
					Message:        userMessage,
					Attachments:    call.Attachments,
					SystemPrompt:   systemPrompt,
					PromptPrefix:   promptPrefix,
					PermissionMode: currentSession.PermissionMode,
				})
				if buildErr != nil {
					return nil, buildErr
				}
				requestState = retryState
			}
			if textualToolProtocolRetryAttempt >= maxTextualToolProtocolRetries {
				err = fmt.Errorf("model emitted textual tool-call protocol instead of structured tool calls")
				break
			}
			textualToolProtocolRetryAttempt++
			slog.Warn("Retrying after model emitted textual tool-call protocol",
				"session_id", call.SessionID,
				"model", largeModel.ModelCfg.Model,
				"provider", largeModel.ModelCfg.Provider,
				"attempt", textualToolProtocolRetryAttempt,
				"max_attempts", maxTextualToolProtocolRetries,
			)
			continue
		}

		if err == nil && shouldRetryForEmptyStreamResponse(currentAssistant) {
			if cleanupErr := a.cleanupFailedAttempt(ctx, currentAssistant, currentStepToolMessageIDs); cleanupErr != nil {
				return nil, cleanupErr
			}
			currentAssistant = nil
			currentStepToolMessageIDs = nil
			currentStepToolResultChars = 0
			if completedStepsThisRun > 0 {
				retryMsgs, getMsgsErr := a.getSessionMessages(ctx, currentSession)
				if getMsgsErr != nil {
					return nil, getMsgsErr
				}
				retryMsgs = excludeCurrentUserMessage(retryMsgs, call.UserMessage)
				retryState, buildErr := a.buildChatRequestState(genCtx, chatRequestStateInput{
					SessionID:      call.SessionID,
					Agent:          "session",
					Model:          largeModel,
					Provider:       providerCtx,
					Purpose:        requestPurpose,
					RequestPurpose: requestPurpose,
					Messages:       retryMsgs,
					Message:        userMessage,
					Attachments:    call.Attachments,
					SystemPrompt:   systemPrompt,
					PromptPrefix:   promptPrefix,
					PermissionMode: currentSession.PermissionMode,
				})
				if buildErr != nil {
					return nil, buildErr
				}
				requestState = retryState
			}
			if emptyStreamRetryAttempt >= maxRetriableAttempts {
				err = fmt.Errorf("received empty response stream after %d retries; the upstream model may be temporarily unavailable", maxRetriableAttempts)
				break
			}
			emptyStreamRetryAttempt++
			delay := a.retryDelayFunc(emptyStreamRetryAttempt, 0)
			slog.Warn("Retrying after empty response stream",
				"session_id", call.SessionID,
				"model", largeModel.ModelCfg.Model,
				"provider", largeModel.ModelCfg.Provider,
				"attempt", emptyStreamRetryAttempt,
				"max_attempts", maxRetriableAttempts,
				"delay", delay,
			)
			retryText := fmt.Sprintf(
				"Service temporarily unavailable. Retrying in %d seconds... (attempt %d/%d)",
				int(delay.Seconds()), emptyStreamRetryAttempt, maxRetriableAttempts,
			)
			retryMsg, retryMsgErr := a.messages.Create(genCtx, call.SessionID, message.CreateMessageParams{
				Role: message.Assistant,
				Parts: []message.ContentPart{
					message.TextContent{Text: retryText},
				},
				Model:    largeModel.ModelCfg.Model,
				Provider: largeModel.ModelCfg.Provider,
			})
			waitErr := a.retryWaitFunc(genCtx, delay)
			if waitErr != nil {
				if retryMsgErr == nil {
					_ = a.messages.Delete(genCtx, retryMsg.ID)
				}
				return nil, waitErr
			}
			if retryMsgErr == nil {
				_ = a.messages.Delete(genCtx, retryMsg.ID)
			}
			continue
		}

		if err != nil && isRetriableError(err) && !a.disableAutoSummarize {
			observedPromptTokens := max(currentSession.LastInputTokens(), estimatedPromptTokens)
			if shouldAutoSummarize(largeModel, observedPromptTokens, call.MaxOutputTokens) {
				slog.Warn("Near context limit during transient failure; forcing summarization to recover",
					"error", err,
					"session_id", call.SessionID,
					"model", largeModel.ModelCfg.Model,
					"provider", largeModel.ModelCfg.Provider,
					"observed_prompt_tokens", observedPromptTokens,
				)
				if truncErr := a.truncateOversizedToolResults(ctx, call.SessionID); truncErr != nil {
					slog.Warn("Failed to truncate oversized tool results before retry summarization", "error", truncErr, "session_id", call.SessionID)
				}
				if currentAssistant != nil {
					currentAssistant.FinishThinking()
					currentAssistant.AddFinish(
						message.FinishReasonError,
						"Context limit reached",
						"The conversation history is near this model's context window limit and the request is repeatedly failing. Auto-summarizing the session to continue the task…",
					)
					if updateErr := a.messages.Update(ctx, *currentAssistant); updateErr != nil {
						slog.Warn("Failed to update assistant message before retry summarization", "error", updateErr, "session_id", call.SessionID)
					}
				}
				compactionTrigger = sessionCompactionTriggerRecover
				shouldSummarize = true
				err = nil
				result = &fantasy.AgentResult{}
				break
			}
		}

		if err != nil && (isContextWindowExceededError(err) || isContextLengthError(err)) {
			break
		}

		if err != nil && isRetriableError(err) && retryAttempt < maxRetriableAttempts {
			if completedStepsThisRun > 0 {
				// Steps already completed — only clean up the current
				// incomplete step to avoid re-executing tool side
				// effects. Completed steps' messages stay in the DB.
				if cleanupErr := a.cleanupFailedAttempt(ctx, currentAssistant, currentStepToolMessageIDs); cleanupErr != nil {
					slog.Warn("Failed to clean up incomplete step during retry",
						"error", cleanupErr, "session_id", call.SessionID)
					break
				}
				// Re-fetch history from DB so the retry includes the
				// completed steps' messages.
				retryMsgs, getMsgsErr := a.getSessionMessages(ctx, currentSession)
				if getMsgsErr != nil {
					slog.Warn("Failed to re-fetch messages for retry",
						"error", getMsgsErr, "session_id", call.SessionID)
					break
				}
				retryMsgs = excludeCurrentUserMessage(retryMsgs, call.UserMessage)
				retryState, buildErr := a.buildChatRequestState(genCtx, chatRequestStateInput{
					SessionID:      call.SessionID,
					Agent:          "session",
					Model:          largeModel,
					Provider:       providerCtx,
					Purpose:        requestPurpose,
					RequestPurpose: requestPurpose,
					Messages:       retryMsgs,
					Message:        userMessage,
					Attachments:    call.Attachments,
					SystemPrompt:   systemPrompt,
					PromptPrefix:   promptPrefix,
					PermissionMode: currentSession.PermissionMode,
				})
				if buildErr != nil {
					slog.Warn("Failed to rebuild request state for retry",
						"error", buildErr, "session_id", call.SessionID)
					break
				}
				requestState = retryState
				// Reset retry budget — completed steps prove the
				// previous attempt made progress, so this is a new
				// transient failure that deserves full retries.
				retryAttempt = 0
				emptyStreamRetryAttempt = 0
			} else {
				// No steps completed — clean up all messages so the
				// retry starts from a clean slate.
				for _, id := range allRunMessageIDs {
					if delErr := a.messages.Delete(ctx, id); delErr != nil {
						slog.Warn("Failed to delete message during retry cleanup",
							"error", delErr, "message_id", id)
					}
				}
			}
			retryAttempt++
			delay := a.retryDelayFunc(retryAttempt, retryAfterFromError(err))
			slog.Warn("Retrying after transient error",
				"error", err,
				"attempt", retryAttempt,
				"delay", delay,
				"completed_steps", completedStepsThisRun,
				"session_id", call.SessionID,
				"model", largeModel.ModelCfg.Model,
				"provider", largeModel.ModelCfg.Provider,
			)

			// Show a temporary message in the chat so the user knows
			// a retry is in progress and how long it will take.
			retryText := fmt.Sprintf(
				"Service temporarily unavailable. Retrying in %d seconds... (attempt %d/%d)",
				int(delay.Seconds()), retryAttempt, maxRetriableAttempts,
			)
			retryMsg, retryMsgErr := a.messages.Create(genCtx, call.SessionID, message.CreateMessageParams{
				Role: message.Assistant,
				Parts: []message.ContentPart{
					message.TextContent{Text: retryText},
				},
				Model:    largeModel.ModelCfg.Model,
				Provider: largeModel.ModelCfg.Provider,
			})

			waitErr := a.retryWaitFunc(genCtx, delay)
			if waitErr != nil {
				// Clean up the retry message before returning.
				if retryMsgErr == nil {
					_ = a.messages.Delete(genCtx, retryMsg.ID)
				}
				return nil, waitErr
			}

			// Remove the temporary retry message before the next attempt.
			if retryMsgErr == nil {
				_ = a.messages.Delete(genCtx, retryMsg.ID)
			}
			continue
		}
		break
	}

	if shouldRetryWithoutAnthropicThinking(err, providerOptions) {
		slog.Warn(
			"Retrying request without Anthropic thinking after provider rejected unsigned reasoning content",
			"session_id", call.SessionID,
			"model", largeModel.ModelCfg.Model,
			"provider", largeModel.ModelCfg.Provider,
			"completed_steps", completedStepsThisRun,
		)
		if cleanupErr := a.cleanupFailedAttempt(ctx, currentAssistant, currentStepToolMessageIDs); cleanupErr != nil {
			return nil, cleanupErr
		}
		currentAssistant = nil
		currentStepToolMessageIDs = nil
		if completedStepsThisRun > 0 {
			retryMsgs, getMsgsErr := a.getSessionMessages(ctx, currentSession)
			if getMsgsErr != nil {
				return nil, getMsgsErr
			}
			retryMsgs = excludeCurrentUserMessage(retryMsgs, call.UserMessage)
			retryState, buildErr := a.buildChatRequestState(genCtx, chatRequestStateInput{
				SessionID:      call.SessionID,
				Agent:          "session",
				Model:          largeModel,
				Provider:       providerCtx,
				Purpose:        requestPurpose,
				RequestPurpose: requestPurpose,
				Messages:       retryMsgs,
				Message:        userMessage,
				Attachments:    call.Attachments,
				SystemPrompt:   systemPrompt,
				PromptPrefix:   promptPrefix,
				PermissionMode: currentSession.PermissionMode,
			})
			if buildErr != nil {
				return nil, buildErr
			}
			requestState = retryState
		}
		providerOptions, _ = disableAnthropicThinking(providerOptions)
		result, err = runStream(providerOptions, false)
	}

	if shouldRetryWithoutRedactedThinking(err) {
		slog.Warn(
			"Retrying request after proxy rejected Anthropic redacted_thinking blocks",
			"session_id", call.SessionID,
			"model", largeModel.ModelCfg.Model,
			"provider", largeModel.ModelCfg.Provider,
			"completed_steps", completedStepsThisRun,
			"error", err,
		)
		if cleanupErr := a.cleanupFailedAttempt(ctx, currentAssistant, currentStepToolMessageIDs); cleanupErr != nil {
			return nil, cleanupErr
		}
		currentAssistant = nil
		currentStepToolMessageIDs = nil
		if completedStepsThisRun > 0 {
			retryMsgs, getMsgsErr := a.getSessionMessages(ctx, currentSession)
			if getMsgsErr != nil {
				return nil, getMsgsErr
			}
			retryMsgs = excludeCurrentUserMessage(retryMsgs, call.UserMessage)
			retryState, buildErr := a.buildChatRequestState(genCtx, chatRequestStateInput{
				SessionID:      call.SessionID,
				Agent:          "session",
				Model:          largeModel,
				Provider:       providerCtx,
				Purpose:        requestPurpose,
				RequestPurpose: requestPurpose,
				Messages:       retryMsgs,
				Message:        userMessage,
				Attachments:    call.Attachments,
				SystemPrompt:   systemPrompt,
				PromptPrefix:   promptPrefix,
				PermissionMode: currentSession.PermissionMode,
			})
			if buildErr != nil {
				return nil, buildErr
			}
			requestState = retryState
		}
		stripRedactedThinking = true
		result, err = runStream(providerOptions, false)
	}

	alreadyRecoveringFromContextWindow := strings.HasPrefix(call.Prompt, contextWindowResumePromptPrefix)
	contextWindowErr := isContextWindowExceededError(err) || isContextLengthError(err)
	if contextWindowErr {
		estimatedInput := max(currentSession.LastInputTokens(), estimatedPromptTokens)
		if estimatedInput > currentSession.LastPromptTokens {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cleanupCancel()
			updatedSession, getSessionErr := a.sessions.Get(cleanupCtx, call.SessionID)
			if getSessionErr != nil {
				slog.Warn("Failed to load session for context-window token update", "error", getSessionErr, "session_id", call.SessionID)
			} else if estimatedInput > updatedSession.LastPromptTokens {
				updatedSession.LastPromptTokens = estimatedInput
				sessionLock.Lock()
				if _, saveSessionErr := a.sessions.Save(cleanupCtx, updatedSession); saveSessionErr != nil {
					slog.Warn("Failed to persist context-window token estimate", "error", saveSessionErr, "session_id", call.SessionID)
				} else {
					currentSession = updatedSession
				}
				sessionLock.Unlock()
			}
		}
	}
	if contextWindowErr && !a.disableAutoSummarize && !alreadyRecoveringFromContextWindow {
		slog.Warn("Context window exceeded; forcing summarization to recover",
			"session_id", call.SessionID,
			"model", largeModel.ModelCfg.Model,
			"provider", largeModel.ModelCfg.Provider,
			"completed_steps", completedStepsThisRun,
		)
		if truncErr := a.truncateOversizedToolResults(ctx, call.SessionID); truncErr != nil {
			slog.Warn("Failed to truncate oversized tool results", "error", truncErr)
		}
		if currentAssistant != nil {
			currentAssistant.FinishThinking()
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cleanupCancel()
			msgs, listErr := a.messages.List(cleanupCtx, currentAssistant.SessionID)
			if listErr != nil {
				return nil, listErr
			}
			for _, tc := range currentAssistant.ToolCalls() {
				if !tc.Finished {
					tc.Finished = true
					tc.Input = "{}"
					currentAssistant.AddToolCall(tc)
				}
				found := false
				for _, msg := range msgs {
					if msg.Role != message.Tool {
						continue
					}
					for _, tr := range msg.ToolResults() {
						if tr.ToolCallID == tc.ID {
							found = true
							break
						}
					}
					if found {
						break
					}
				}
				if found {
					continue
				}
				toolResult := message.ToolResult{
					ToolCallID: tc.ID,
					Name:       tc.Name,
					Content:    "There was an error while executing the tool",
					IsError:    true,
				}
				if subtaskStatus, ok := syntheticSubtaskStatusForTool(tc.Name, false, false); ok {
					toolResult = toolResult.WithSubtaskResult(message.ToolResultSubtaskResult{
						ParentToolCallID: tc.ID,
						Status:           subtaskStatus,
					})
				}
				if _, createErr := a.messages.Create(cleanupCtx, currentAssistant.SessionID, message.CreateMessageParams{
					Role: message.Tool,
					Parts: []message.ContentPart{
						toolResult,
					},
				}); createErr != nil {
					return nil, createErr
				}
			}
			if cw := EffectiveContextWindow(largeModel.CatwalkCfg); cw > 0 {
				currentAssistant.SetUsage(message.Usage{InputTokens: cw})
			}
			currentAssistant.AddFinish(
				message.FinishReasonError,
				"Context limit reached",
				"The conversation history reached this model's context window limit. Auto-summarizing the session to continue the task…",
			)
			if updateErr := a.messages.Update(cleanupCtx, *currentAssistant); updateErr != nil {
				slog.Warn("Failed to update failed assistant message after context-window error", "error", updateErr)
			}
		}
		contextWindowExceeded = true
		compactionTrigger = sessionCompactionTriggerRecover
		shouldSummarize = true
		err = nil
		result = &fantasy.AgentResult{}
	} else if contextWindowErr && alreadyRecoveringFromContextWindow {
		slog.Warn("Context window exceeded again after recover attempt; returning provider error",
			"session_id", call.SessionID,
			"model", largeModel.ModelCfg.Model,
			"provider", largeModel.ModelCfg.Provider,
		)
	}

	a.eventPromptResponded(call.SessionID, time.Since(startTime).Truncate(time.Second))

	if err != nil {
		isCancelErr := errors.Is(err, context.Canceled)
		isPermissionErr := permission.IsPermissionError(err)
		permissionErr, hasPermissionErr := permission.AsPermissionError(err)
		failureKind := "error"
		if isCancelErr {
			failureKind = "canceled"
		} else if isPermissionErr {
			failureKind = "permission_denied"
		}
		logArgs := []any{
			"session_id", call.SessionID,
			"error", err,
			"failure_kind", failureKind,
			"completed_steps", completedStepsThisRun,
			"runToolUses", runToolUses,
			"runLastTool", runLastTool,
			"retryAttempt", retryAttempt,
			"model", largeModel.ModelCfg.Model,
			"provider", largeModel.ModelCfg.Provider,
		}
		if isCancelErr {
			slog.Warn("Agent run interrupted", logArgs...)
		} else {
			slog.Error("Agent run failed", logArgs...)
		}
		if currentAssistant == nil {
			return result, err
		}
		// Ensure we finish thinking on error to close the reasoning state.
		currentAssistant.FinishThinking()
		toolCalls := currentAssistant.ToolCalls()
		// Use a detached context for cleanup DB operations. Both ctx and
		// genCtx may be cancelled (e.g. ACP session/cancel cancels the
		// parent runCtx which propagates to both). We must still persist
		// tool-result messages so the conversation history stays valid.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		msgs, createErr := a.messages.List(cleanupCtx, currentAssistant.SessionID)
		if createErr != nil {
			return nil, createErr
		}
		for _, tc := range toolCalls {
			if !tc.Finished {
				tc.Finished = true
				tc.Input = "{}"
				currentAssistant.AddToolCall(tc)
				updateErr := a.messages.Update(cleanupCtx, *currentAssistant)
				if updateErr != nil {
					return nil, updateErr
				}
			}

			found := false
			for _, msg := range msgs {
				if msg.Role == message.Tool {
					for _, tr := range msg.ToolResults() {
						if tr.ToolCallID == tc.ID {
							found = true
							break
						}
					}
				}
				if found {
					break
				}
			}
			if found {
				continue
			}
			content := "There was an error while executing the tool"
			if isCancelErr {
				content = "Error: user cancelled assistant tool calling"
			} else if isPermissionErr {
				if hasPermissionErr && permissionErr.Kind == permission.PermissionErrorKindPolicyDenied {
					content = cmp.Or(permissionErr.Message, "Permission blocked by safety policy")
				} else {
					content = "User denied permission"
				}
			}
			toolResult := message.ToolResult{
				ToolCallID: tc.ID,
				Name:       tc.Name,
				Content:    content,
				IsError:    true,
			}
			if subtaskStatus, ok := syntheticSubtaskStatusForTool(tc.Name, isCancelErr, isPermissionErr); ok {
				toolResult = toolResult.WithSubtaskResult(message.ToolResultSubtaskResult{
					ParentToolCallID: tc.ID,
					Status:           subtaskStatus,
				})
			}
			_, createErr = a.messages.Create(cleanupCtx, currentAssistant.SessionID, message.CreateMessageParams{
				Role: message.Tool,
				Parts: []message.ContentPart{
					toolResult,
				},
			})
			if createErr != nil {
				return nil, createErr
			}
		}
		var fantasyErr *fantasy.Error
		var providerErr *fantasy.ProviderError
		const defaultTitle = "Provider Error"
		linkStyle := lipgloss.NewStyle().Foreground(charmtone.Guac).Underline(true)
		if isCancelErr {
			currentAssistant.AddFinish(message.FinishReasonCanceled, "User canceled request", "")
		} else if isPermissionErr {
			if hasPermissionErr && permissionErr.Kind == permission.PermissionErrorKindPolicyDenied {
				currentAssistant.AddFinish(
					message.FinishReasonPermissionDenied,
					"Permission blocked",
					permissionErr.Details,
				)
			} else {
				currentAssistant.AddFinish(message.FinishReasonPermissionDenied, "User denied permission", "")
			}
		} else if errors.Is(err, hyper.ErrNoCredits) {
			url := hyper.BaseURL()
			link := linkStyle.Hyperlink(url, "id=hyper").Render(url)
			currentAssistant.AddFinish(message.FinishReasonError, "No credits", "You're out of credits. Add more at "+link)
		} else if errors.As(err, &providerErr) {
			if providerErr.Message == "The requested model is not supported." {
				url := "https://github.com/settings/copilot/features"
				link := linkStyle.Hyperlink(url, "id=copilot").Render(url)
				currentAssistant.AddFinish(
					message.FinishReasonError,
					"Copilot model not enabled",
					withRetryFailureDetails(
						fmt.Sprintf("%q is not enabled in Copilot. Go to the following page to enable it. Then, wait 5 minutes before trying again. %s", largeModel.CatwalkCfg.Name, link),
						retryAttempt,
					),
				)
			} else {
				currentAssistant.AddFinish(
					message.FinishReasonError,
					cmp.Or(stringext.Capitalize(providerErr.Title), defaultTitle),
					withRetryFailureDetails(providerErr.Message, retryAttempt),
				)
			}
		} else if errors.As(err, &fantasyErr) {
			currentAssistant.AddFinish(
				message.FinishReasonError,
				cmp.Or(stringext.Capitalize(fantasyErr.Title), defaultTitle),
				withRetryFailureDetails(fantasyErr.Message, retryAttempt),
			)
		} else {
			currentAssistant.AddFinish(
				message.FinishReasonError,
				defaultTitle,
				withRetryFailureDetails(err.Error(), retryAttempt),
			)
		}
		// Use the detached cleanup context to ensure the assistant message
		// (with its finish reason) is always persisted.
		updateErr := a.messages.Update(cleanupCtx, *currentAssistant)
		if updateErr != nil {
			return nil, updateErr
		}
		return nil, err
	}

	// Send notification that agent has finished its turn (skip for
	// nested/non-interactive sessions).
	// NOTE: This is done after checking for summarization and queued messages
	// to avoid sending a spurious "agent finished" notification when the agent
	// is about to continue working.
	if a.hookManager != nil && !shouldSummarize {
		a.hookManager.RunStop(ctx, call.SessionID)
	}

	queuedMessages, ok := a.messageQueue.Get(call.SessionID)
	hasQueuedMessages := ok && len(queuedMessages) > 0
	if !call.NonInteractive && a.notify != nil && !shouldSummarize && !hasQueuedMessages {
		a.notify.Publish(pubsub.CreatedEvent, notify.Notification{
			SessionID:    call.SessionID,
			SessionTitle: currentSession.Title,
			Type:         notify.TypeAgentFinished,
		})
	}

	if shouldSummarize {
		a.activeRequests.Del(call.SessionID)
		if compactionTrigger == sessionCompactionTriggerNone {
			compactionTrigger = sessionCompactionTriggerNormal
		}
		if a.memoryEngineHooks != nil && a.memoryEngineHooks.OnBeforeCompaction != nil {
			rescuePayload := a.memoryEngineHooks.OnBeforeCompaction(context.Background(), call.SessionID)
			if rescuePayload != "" {
				genCtx = withCompactionRescue(genCtx, rescuePayload)
			}
		}
		if summarizeErr := a.Summarize(withSessionCompactingPurpose(copilot.ContextWithInitiatorType(genCtx, copilot.InitiatorAgent), compactionTrigger.Purpose()), call.SessionID, call.ProviderOptions); summarizeErr != nil {
			return nil, summarizeErr
		}
		hasPendingToolCalls := currentAssistant != nil && len(currentAssistant.ToolCalls()) > 0
		shouldAutoResume := hasPendingToolCalls || compactionTrigger == sessionCompactionTriggerRecover
		if shouldAutoResume {
			resumePrefix := autoResumePromptPrefix
			if contextWindowExceeded {
				resumePrefix = contextWindowResumePromptPrefix
			}
			// If call.Prompt already has a resume prefix, extract the original prompt
			// to avoid nested prefixes (e.g., from a prior auto-resume).
			originalPrompt := call.Prompt
			for _, prefix := range []string{autoResumePromptPrefix, contextWindowResumePromptPrefix} {
				if strings.HasPrefix(call.Prompt, prefix) {
					trimmed := strings.TrimPrefix(call.Prompt, prefix)
					// Remove the trailing backtick added by the resume prefix wrapper.
					// Use LastIndex to find the wrapper's closing backtick, which correctly
					// handles user prompts that may themselves end with backticks.
					if end := strings.LastIndex(trimmed, "`"); end >= 0 {
						originalPrompt = strings.TrimSpace(trimmed[:end])
					} else {
						originalPrompt = trimmed
					}
					break
				}
			}
			call.Prompt = fmt.Sprintf(resumePrefix+"%s`", originalPrompt)
			resumedUserMessage := userMessage
			call.UserMessage = &resumedUserMessage
			if compactionTrigger == sessionCompactionTriggerRecover {
				call.Purpose = plugin.ChatTransformPurposeRecover
			}
			call.InitiatorType = copilot.InitiatorAgent
			call.BypassQueuePause = true
			a.enqueueQueuedCall(call.SessionID, call)
		}
	}

	// Release active request before processing queued messages.
	a.activeRequests.Del(call.SessionID)
	cancel()
	wg.Wait()

	if a.QueuedPrompts(call.SessionID) == 0 {
		return result, err
	}
	// Don't auto-process the next queued message while the queue is paused,
	// unless the queued call has BypassQueuePause set (auto-resume calls).
	if a.IsQueuePaused(call.SessionID) {
		// Peek at the next queued message to check if it can bypass pause.
		a.queueMu.Lock()
		queuedCalls, ok := a.messageQueue.Get(call.SessionID)
		if !ok || len(queuedCalls) == 0 {
			a.queueMu.Unlock()
			return result, err
		}
		nextCall := queuedCalls[0]
		a.queueMu.Unlock()
		if !nextCall.BypassQueuePause {
			return result, err
		}
	}
	// There are queued messages restart the loop.
	firstQueuedMessage, ok := a.popNextQueuedCall(call.SessionID)
	if !ok {
		return result, err
	}
	ctx = context.WithValue(ctx, sessionAgentRuntimeConfigContextKey{}, (*sessionAgentRuntimeConfig)(nil))
	return a.Run(ctx, firstQueuedMessage)
}

func syntheticSubtaskStatusForTool(toolName string, isCancelErr, isPermissionErr bool) (message.ToolResultSubtaskStatus, bool) {
	switch toolName {
	case AgentToolName, tools.AgenticFetchToolName:
		if isCancelErr || isPermissionErr {
			return message.ToolResultSubtaskStatusCanceled, true
		}
		return message.ToolResultSubtaskStatusFailed, true
	default:
		return "", false
	}
}

func hydrateAgentResultFromAssistantMessage(result *fantasy.AgentResult, assistant *message.Message) {
	if result == nil || assistant == nil {
		return
	}

	text := assistant.Content().Text
	if strings.TrimSpace(text) == "" {
		return
	}

	if result.Response.Content.Text() != "" {
		return
	}

	textPart := fantasy.TextContent{Text: text}
	result.Response.Content = append(fantasy.ResponseContent{textPart}, result.Response.Content...)

	if len(result.Steps) == 0 {
		return
	}
	last := &result.Steps[len(result.Steps)-1]
	if last.Content.Text() == "" {
		last.Content = append(fantasy.ResponseContent{textPart}, last.Content...)
	}
}

// isContextLengthError checks if the error is due to context length exceeding the model's limit.
func isContextLengthError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	// Check for common context length error patterns from various providers.
	// These patterns are specifically chosen to avoid matching rate limits or other errors.
	contextLengthIndicators := []string{
		"context window",
		"context length",
		"maximum context length",
		"range of input length",
		"context_too_long",
		"context window exceeded",
		"input length exceeds",
		"input token count",
		"prompt is too long",
		"prompt too long",
		"token limit exceeded",
		"exceeded model token limit",
		"reduce the length of the messages",
		"exceeds the available context size",
		"greater than the context length",
		"too large for model",
		"request body too large",
		"request entity too large",
	}
	lowerErr := strings.ToLower(errStr)
	for _, indicator := range contextLengthIndicators {
		if strings.Contains(lowerErr, indicator) {
			return true
		}
	}
	return false
}

// truncateMessagesToFit truncates messages to fit within the specified token limit.
// It keeps the most recent messages and removes older ones until the estimated
// token count is below the limit.
// Note: System messages are excluded from the result. The caller is responsible
// for adding appropriate system messages (e.g., via PrepareStep).
func truncateMessagesToFit(msgs []fantasy.Message, maxTokens int64) []fantasy.Message {
	if len(msgs) == 0 {
		return msgs
	}

	// Always keep at least the last 2 messages (user request + assistant response).
	minMessagesToKeep := 2
	if len(msgs) <= minMessagesToKeep {
		return msgs
	}

	// Calculate tokens for all messages.
	type msgInfo struct {
		index  int
		tokens int64
	}

	msgInfos := make([]msgInfo, len(msgs))
	var totalTokens int64
	for i, msg := range msgs {
		tokens := estimateSingleMessageTokens(msg)
		msgInfos[i] = msgInfo{index: i, tokens: tokens}
		totalTokens += tokens
	}

	// If already under limit, return as-is.
	if totalTokens <= maxTokens {
		return msgs
	}

	// Skip system messages at the beginning (they will be re-added by the caller's PrepareStep).
	startIdx := 0
	for startIdx < len(msgs) && msgs[startIdx].Role == fantasy.MessageRoleSystem {
		startIdx++
	}

	// Subtract tokens for messages we already skipped (system messages).
	for i := 0; i < startIdx; i++ {
		totalTokens -= msgInfos[i].tokens
	}

	// Store original token count before removal.
	originalTokens := totalTokens

	// Remove from the oldest non-system message first.
	for totalTokens > maxTokens && startIdx < len(msgs)-minMessagesToKeep {
		totalTokens -= msgInfos[startIdx].tokens
		startIdx++
	}

	slog.Info("Truncated messages for summarization",
		"original_count", len(msgs),
		"new_count", len(msgs)-startIdx,
		"removed_count", startIdx,
		"original_tokens", originalTokens,
		"new_tokens", totalTokens)

	return msgs[startIdx:]
}

func summaryHistoryTokenBudget(model Model, maxOutputTokens int64, prompt string, systemPrompt string, systemPromptPrefix string) int64 {
	budget := promptTokenBudgetForModel(model, maxOutputTokens)
	if budget.UsableInputTokens <= 0 {
		return 0
	}
	overheadTokens := estimateStringTokens(prompt) +
		estimateStringTokens(systemPrompt) +
		estimateStringTokens(systemPromptPrefix)
	return max(0, budget.UsableInputTokens-overheadTokens)
}

func (a *sessionAgent) applyToolResultReview(ctx context.Context, sessionID string, toolResult message.ToolResult, permissionMode session.PermissionMode) message.ToolResult {
	if a.reviewToolResult == nil {
		return toolResult
	}

	reviewed, err := a.reviewToolResult(ctx, sessionID, toolResult, permissionMode)
	if err != nil {
		slog.Warn("Failed to review tool result for Auto Mode", "error", err, "tool_name", toolResult.Name, "session_id", sessionID)
	}
	return reviewed
}

// estimateSingleMessageTokens estimates tokens for a single fantasy.Message.
func estimateSingleMessageTokens(msg fantasy.Message) int64 {
	return estimateMessageContentTokens(msg.Content)
}

type internalCompactionKey struct{}

func isInternalCompaction(ctx context.Context) bool {
	v, _ := ctx.Value(internalCompactionKey{}).(bool)
	return v
}

func (a *sessionAgent) Summarize(ctx context.Context, sessionID string, opts fantasy.ProviderOptions) error {
	if !isInternalCompaction(ctx) && a.IsSessionBusy(sessionID) {
		return ErrSessionBusy
	}
	if a.refreshCallConfig != nil {
		runtimeConfig, err := a.refreshCallConfig(ctx)
		if err != nil {
			return err
		}
		if runtimeConfig.ProviderOptions != nil {
			opts = runtimeConfig.ProviderOptions
		}
	}

	// Copy mutable fields under lock to avoid races with SetModels.
	summaryModel := a.largeModel.Get()
	systemPromptPrefix := a.systemPromptPrefix.Get()
	// 选择摘要模型：优先使用 background model，否则回退到 large model
	if a.backgroundModel != nil {
		summaryModel = a.backgroundModel.model
		// 为 background model 重新计算 provider options
		opts = getProviderOptions(summaryModel, a.backgroundModel.provider)
		// 切换为 background model provider 的 system prompt prefix
		systemPromptPrefix = a.backgroundModel.provider.SystemPromptPrefix
	}
	providerCtx := defaultProviderContext()
	compactingPurpose := sessionCompactingPurposeFromContext(ctx)

	currentSession, err := a.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to get session: %w", err)
	}
	if truncErr := a.truncateOversizedToolResults(ctx, sessionID); truncErr != nil {
		slog.Warn("Failed to truncate oversized tool results before summarization", "error", truncErr, "session_id", sessionID)
	}
	msgs, err := a.getSessionMessages(ctx, currentSession)
	if err != nil {
		return err
	}
	if len(msgs) == 0 {
		// Nothing to summarize.
		return nil
	}

	if a.hookManager != nil {
		a.hookManager.RunPreCompact(ctx, sessionID)
	}

	// Prune old tool results before sending to plugins during
	// summarization. Without this, the full unpruned payload sent
	// to transforms can exceed plugin buffer limits on large sessions.
	msgs = builtinPruneToolResults(msgs)

	// Filter out non-text content if the summary model doesn't support images
	if !summaryModel.CatwalkCfg.SupportsImages {
		originalCount := message.CountNonTextContent(msgs)
		msgs = message.FilterNonTextContent(msgs)
		filteredCount := message.CountNonTextContent(msgs)
		itemsRemoved := originalCount - filteredCount
		if itemsRemoved > 0 {
			slog.Info("Filtered non-text content for summarization",
				"session_id", sessionID,
				"model", summaryModel.ModelCfg.Model,
				"items_removed", itemsRemoved)
		}
	}

	if shouldReactiveCompactMessages(compactingPurpose) {
		reactiveCompacted, reactiveErr := a.reactiveCompactSessionMessages(ctx, sessionID, summaryModel, providerCtx, msgs)
		if reactiveErr != nil {
			return reactiveErr
		}
		if len(reactiveCompacted) > 0 {
			msgs = reactiveCompacted
		}
	}
	if shouldAutoCompactMessages(compactingPurpose, msgs) {
		autoCompacted, autoCompactErr := a.autoCompactSessionMessages(ctx, sessionID, summaryModel, providerCtx, msgs)
		if autoCompactErr != nil {
			return autoCompactErr
		}
		if len(autoCompacted) > 0 {
			msgs = autoCompacted
		}
		postCompacted, postCompactErr := a.postCompactSessionMessages(ctx, sessionID, summaryModel, providerCtx, msgs)
		if postCompactErr != nil {
			return postCompactErr
		}
		if len(postCompacted) > 0 {
			msgs = postCompacted
		}
	}

	transformedMsgs, err := a.transformSessionMessages(ctx, chatRequestStateInput{
		SessionID:             sessionID,
		Agent:                 "session",
		Model:                 summaryModel,
		Provider:              providerCtx,
		Purpose:               compactingPurpose,
		RequestPurpose:        compactingPurpose,
		Messages:              msgs,
		Message:               message.Message{SessionID: sessionID, Role: message.User},
		EstimatedPromptTokens: a.estimatePromptForMessages(msgs),
	})
	if err != nil {
		// If the plugin transform fails (e.g., morph-compact API error or
		// timeout), fall back to the original messages instead of aborting
		// summarization entirely. This ensures the built-in summarization
		// can still produce a summary as a safety net.
		slog.Warn("Plugin transform failed during summarization, proceeding with original messages",
			"error", err, "session_id", sessionID)
		transformedMsgs = msgs
	}
	aiMsgs, _ := a.preparePrompt(transformedMsgs)
	compactUsage := usageSnapshotFromMessages(msgs, a.estimatePromptForMessages(msgs))
	compacting, err := a.plugins().TriggerSessionCompacting(ctx, plugin.SessionCompactingInput{
		SessionID: sessionID,
		Agent:     "session",
		Model:     agentModelInfo(summaryModel),
		Purpose:   compactingPurpose,
		Usage:     compactUsage,
	}, plugin.SessionCompactingOutput{})
	if err != nil {
		return err
	}

	if a.filetracker != nil {
		fileContext := a.buildRecentFileContext(ctx, sessionID, int64(summaryModel.CatwalkCfg.ContextWindow))
		compacting.Context = append(compacting.Context, fileContext...)
	}

	// Inject memory rescue (top durable events from MemoryEngine) when the
	// caller attached one via withCompactionRescue. The rescue block must
	// appear at the top of the additional context so the summarizer copies
	// or paraphrases the events into the new summary.
	if rescue := compactionRescueFromContext(ctx); rescue != "" {
		compacting.Context = append([]string{rescue}, compacting.Context...)
	}

	genCtx, cancel := context.WithCancel(ctx)
	genCtx = copilot.ContextWithInitiatorType(genCtx, copilot.InitiatorAgent)
	if !isInternalCompaction(ctx) {
		a.activeRequests.Set(sessionID, cancel)
		defer a.activeRequests.Del(sessionID)
	}
	defer cancel()

	agent := a.agentFactory(retryableStreamModel{summaryModel.Model},
		fantasy.WithSystemPrompt(string(summaryPrompt)),
		fantasy.WithUserAgent(userAgent),
	)
	summaryMessage, err := a.messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:             message.Assistant,
		Model:            summaryModel.ModelCfg.Model,
		Provider:         summaryModel.ModelCfg.Provider,
		IsSummaryMessage: true,
	})
	if err != nil {
		return err
	}

	summaryPromptText := buildSessionCompactingPrompt(currentSession.Todos, compacting.Context, compacting.Prompt)

	// Check whether history alone would leave enough room for the summary
	// prompt, system prompt, and output reserve. This uses the same effective
	// prompt budget as normal auto-summarization, including max_prompt_tokens.
	contextWindow := EffectiveContextWindow(summaryModel.CatwalkCfg)
	maxAllowedTokens := summaryHistoryTokenBudget(
		summaryModel,
		0,
		summaryPromptText,
		string(summaryPrompt),
		systemPromptPrefix,
	)
	if maxAllowedTokens > 0 {
		estimatedTokens := estimatePromptTokens(aiMsgs, nil)
		if estimatedTokens > maxAllowedTokens {
			slog.Warn("Messages exceed context window, truncating before summarization",
				"estimated_tokens", estimatedTokens,
				"max_allowed", maxAllowedTokens,
				"context_window", contextWindow,
				"session_id", sessionID)
			aiMsgs = truncateMessagesToFit(aiMsgs, maxAllowedTokens)
		}
	}

	summarizeEstimatedPromptTokens := a.estimateSessionPromptTokens(
		aiMsgs,
		summaryPromptText,
		nil,
		nil,
		string(summaryPrompt),
		systemPromptPrefix,
	)
	if summarizeEstimatedPromptTokens > 0 {
		summaryMessage.SetUsage(message.Usage{InputTokens: summarizeEstimatedPromptTokens})
		if updateErr := a.messages.Update(genCtx, summaryMessage); updateErr != nil {
			return updateErr
		}
	}

	summaryStream := func() (*fantasy.AgentResult, error) {
		return agent.Stream(genCtx, fantasy.AgentStreamCall{
			Prompt:          summaryPromptText,
			Messages:        aiMsgs,
			ProviderOptions: opts,
			PrepareStep: func(callContext context.Context, options fantasy.PrepareStepFunctionOptions) (_ context.Context, prepared fantasy.PrepareStepResult, err error) {
				callContext = copilot.ContextWithInitiatorType(callContext, copilot.InitiatorAgent)
				prepared.Messages = options.Messages
				if systemPromptPrefix != "" {
					prepared.Messages = append([]fantasy.Message{fantasy.NewSystemMessage(systemPromptPrefix)}, prepared.Messages...)
				}
				return callContext, prepared, nil
			},
			OnReasoningDelta: func(id string, text string) error {
				summaryMessage.AppendReasoningContent(text)
				return a.messages.Update(genCtx, summaryMessage)
			},
			OnReasoningEnd: func(id string, reasoning fantasy.ReasoningContent) error {
				if anthropicData, ok := reasoning.ProviderMetadata["anthropic"]; ok {
					if signature, ok := anthropicData.(*anthropic.ReasoningOptionMetadata); ok && signature.Signature != "" {
						summaryMessage.AppendReasoningSignature(signature.Signature)
					}
				}
				summaryMessage.FinishThinking()
				return a.messages.Update(genCtx, summaryMessage)
			},
			OnTextDelta: func(id, text string) error {
				summaryMessage.AppendContent(text)
				return a.messages.Update(genCtx, summaryMessage)
			},
		})
	}

	var (
		resp                    *fantasy.AgentResult
		summaryRetry            int
		summaryThinkingDisabled bool
		summaryRedactedStripped bool
		forceTruncated          bool
	)
	resetSummaryMessage := func() error {
		summaryMessage.Parts = nil
		if resetErr := a.messages.Update(genCtx, summaryMessage); resetErr != nil {
			slog.Warn("Failed to reset summary message before retry", "error", resetErr, "session_id", sessionID, "message_id", summaryMessage.ID)
			return resetErr
		}
		return nil
	}
	for {
		resp, err = summaryStream()
		if err == nil {
			break
		}

		if !summaryThinkingDisabled && shouldRetryWithoutAnthropicThinking(err, opts) {
			slog.Warn(
				"Retrying summarization without Anthropic thinking after provider rejected unsigned reasoning content",
				"session_id", sessionID,
				"model", summaryModel.ModelCfg.Model,
				"provider", summaryModel.ModelCfg.Provider,
				"error", err,
			)
			var changed bool
			opts, changed = disableAnthropicThinking(opts)
			if changed {
				summaryThinkingDisabled = true
				if resetErr := resetSummaryMessage(); resetErr != nil {
					return resetErr
				}
				continue
			}
		}

		if !summaryRedactedStripped && shouldRetryWithoutRedactedThinking(err) {
			slog.Warn(
				"Retrying summarization after proxy rejected Anthropic redacted_thinking blocks",
				"session_id", sessionID,
				"model", summaryModel.ModelCfg.Model,
				"provider", summaryModel.ModelCfg.Provider,
				"error", err,
			)
			var changed bool
			aiMsgs, changed = stripRedactedThinkingParts(aiMsgs)
			if changed {
				summaryRedactedStripped = true
				if resetErr := resetSummaryMessage(); resetErr != nil {
					return resetErr
				}
				continue
			}
		}

		isCancelErr := errors.Is(err, context.Canceled)
		isContextLengthErr := isContextLengthError(err)

		// On context length error, try one more time with aggressive
		// truncation before giving up. This handles the case where
		// plugins (e.g., morph-compact) produce messages that are still
		// too large, or where the initial token estimation was too
		// optimistic compared to the provider's actual limit.
		if isContextLengthErr && !forceTruncated {
			forceTruncated = true
			aggressiveMax := contextWindow / 4
			if aggressiveMax < 2000 {
				aggressiveMax = 2000
			}
			slog.Warn("Retrying summarization with aggressive truncation after context length error",
				"session_id", sessionID,
				"aggressive_max_tokens", aggressiveMax,
				"context_window", contextWindow,
				"original_error", err)
			aiMsgs = truncateMessagesToFit(aiMsgs, aggressiveMax)
			if resetErr := resetSummaryMessage(); resetErr != nil {
				return resetErr
			}
			continue
		}

		if isCancelErr || isContextLengthErr {
			deleteErr := a.messages.Delete(ctx, summaryMessage.ID)
			if isContextLengthErr {
				if deleteErr != nil {
					slog.Warn("Failed to delete summary message after context length error", "error", deleteErr, "session_id", sessionID, "message_id", summaryMessage.ID)
				}
				return fmt.Errorf("context too long for summarization: %w", err)
			}
			return deleteErr
		}

		if !isRetriableError(err) || summaryRetry >= maxRetriableAttempts {
			summaryMessage.FinishThinking()
			summaryMessage.AddFinish(
				message.FinishReasonError,
				"Summarization failed",
				withRetryFailureDetails(err.Error(), summaryRetry),
			)
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
			updateErr := a.messages.Update(cleanupCtx, summaryMessage)
			cleanupCancel()
			if updateErr != nil {
				slog.Warn("Failed to persist summary failure message", "error", updateErr, "session_id", sessionID, "message_id", summaryMessage.ID)
				if deleteErr := a.messages.Delete(context.Background(), summaryMessage.ID); deleteErr != nil {
					slog.Warn("Failed to delete summary message after persist failure", "error", deleteErr, "session_id", sessionID, "message_id", summaryMessage.ID)
				}
			}
			return err
		}

		summaryRetry++
		delay := a.retryDelayFunc(summaryRetry, retryAfterFromError(err))
		slog.Warn("Retrying summarization after transient error",
			"error", err,
			"attempt", summaryRetry,
			"delay", delay,
			"session_id", sessionID,
		)
		if resetErr := resetSummaryMessage(); resetErr != nil {
			return resetErr
		}
		if waitErr := a.retryWaitFunc(genCtx, delay); waitErr != nil {
			return waitErr
		}
	}

	summaryMessage.SetUsage(normalizedMessageUsage(resp.TotalUsage, usageProvider(summaryModel), summarizeEstimatedPromptTokens))
	summaryMessage.AddFinish(message.FinishReasonEndTurn, "", "")
	err = a.messages.Update(genCtx, summaryMessage)
	if err != nil {
		return err
	}

	var openrouterCost *float64
	for _, step := range resp.Steps {
		stepCost := a.openrouterCost(step.ProviderMetadata)
		if stepCost != nil {
			newCost := *stepCost
			if openrouterCost != nil {
				newCost += *openrouterCost
			}
			openrouterCost = &newCost
		}
	}

	a.updateSessionUsage(summaryModel, &currentSession, resp.TotalUsage, openrouterCost, summarizeEstimatedPromptTokens, false)

	currentSession.SummaryMessageID = summaryMessage.ID
	_, err = a.sessions.Save(genCtx, currentSession)
	if err == nil && a.hookManager != nil {
		a.hookManager.RunPostCompact(ctx, sessionID)
	}
	// Compact 后生成 session working memory，供后续 turn 快速恢复上下文。
	if err == nil && a.enableSessionMemory() {
		a.asyncUpdateSessionMemory(ctx, sessionID)
	}
	return err
}

func (a *sessionAgent) RespondAsBackground(ctx context.Context, from, message string) (string, error) {
	prompt := fmt.Sprintf("<irc>\nYou received an IRC message from agent `%s`.\n\nReply briefly and directly using the conversation context already available to you. Do **not** call any tools. The reply you write is delivered back to `%s` as your answer.\n\nMessage:\n%s\n</irc>", from, from, message)

	bgModel := a.largeModel.Get()
	systemPrompt := a.systemPrompt.Get()
	var providerOpts fantasy.ProviderOptions
	if a.backgroundModel != nil {
		bgModel = a.backgroundModel.model
		providerOpts = getProviderOptions(bgModel, a.backgroundModel.provider)
	} else {
		providerOpts = getProviderOptions(bgModel, config.ProviderConfig{})
	}

	noThink := false
	bgModel.ModelCfg.Think = &noThink

	agent := fantasy.NewAgent(
		bgModel.Model,
		fantasy.WithSystemPrompt(systemPrompt),
		fantasy.WithMaxOutputTokens(512),
		fantasy.WithUserAgent("crush-irc-bg"),
	)

	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	resp, err := agent.Generate(
		callCtx,
		fantasy.AgentCall{
			Prompt:          prompt,
			ProviderOptions: providerOpts,
			PrepareStep: func(callCtx context.Context, options fantasy.PrepareStepFunctionOptions) (_ context.Context, prepared fantasy.PrepareStepResult, err error) {
				prepared.Messages = options.Messages
				return callCtx, prepared, nil
			},
		},
	)
	if err != nil {
		return "", fmt.Errorf("irc background response failed: %w", err)
	}
	if resp == nil {
		return "", nil
	}
	reply := resp.Response.Content.Text()
	if len([]rune(reply)) > 500 {
		reply = string([]rune(reply)[:500]) + "…"
	}
	return reply, nil
}

func (a *sessionAgent) getCacheControlOptions() fantasy.ProviderOptions {
	if t, _ := strconv.ParseBool(os.Getenv("CRUSH_DISABLE_ANTHROPIC_CACHE")); t {
		return fantasy.ProviderOptions{}
	}
	return fantasy.ProviderOptions{
		anthropic.Name: &anthropic.ProviderCacheControlOptions{
			CacheControl: anthropic.CacheControl{Type: "ephemeral"},
		},
		bedrock.Name: &anthropic.ProviderCacheControlOptions{
			CacheControl: anthropic.CacheControl{Type: "ephemeral"},
		},
		vercel.Name: &anthropic.ProviderCacheControlOptions{
			CacheControl: anthropic.CacheControl{Type: "ephemeral"},
		},
	}
}

func (a *sessionAgent) createUserMessage(ctx context.Context, call SessionAgentCall) (message.Message, error) {
	parts := []message.ContentPart{message.TextContent{Text: call.Prompt}}
	var attachmentParts []message.ContentPart
	for _, attachment := range call.Attachments {
		attachmentParts = append(attachmentParts, message.BinaryContent{Path: attachment.FilePath, MIMEType: attachment.MimeType, Data: attachment.Content})
	}
	parts = append(parts, attachmentParts...)
	msg, err := a.messages.Create(ctx, call.SessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: parts,
	})
	if err != nil {
		return message.Message{}, fmt.Errorf("failed to create user message: %w", err)
	}
	return msg, nil
}

const canceledPromptBranchSystemNote = "Previous assistant work before this point was canceled before completion. Ignore unfinished tool activity from that attempt. Treat the next user message as the active instruction, and use earlier user messages only as background context."

func syntheticCanceledPromptBoundary(index int) message.Message {
	return message.Message{
		ID:   fmt.Sprintf("synthetic-canceled-prompt-boundary-%d", index),
		Role: message.System,
		Parts: []message.ContentPart{
			message.TextContent{Text: canceledPromptBranchSystemNote},
		},
	}
}

func trimCanceledPromptBranches(msgs []message.Message) []message.Message {
	hasLaterUser := false
	laterUserIndex := -1
	keep := make([]bool, len(msgs))
	insertBoundaryBefore := make(map[int]bool)
	removed := false
	for i := range keep {
		keep[i] = true
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		msg := msgs[i]
		if msg.Role == message.User {
			hasLaterUser = true
			laterUserIndex = i
			continue
		}
		if !hasLaterUser {
			continue
		}
		if msg.Role != message.Assistant {
			continue
		}
		// Drop both explicitly canceled assistant turns and orphaned
		// assistant turns that never received a Finish part (e.g. the
		// process was interrupted before cleanup could persist the
		// finish reason). Leaving these in place sends a partially-
		// streamed assistant turn — often missing its signed thinking
		// block — back to strict thinking-mode proxies, which reject the
		// request with errors such as "content[].thinking in the
		// thinking mode must be passed back to the API".
		finishReason := msg.FinishReason()
		if finishReason != message.FinishReasonCanceled && finishReason != "" {
			continue
		}
		if keep[i] {
			keep[i] = false
			removed = true
		}
		if laterUserIndex >= 0 {
			insertBoundaryBefore[laterUserIndex] = true
		}
		for j := i - 1; j >= 0; j-- {
			if msgs[j].Role != message.Assistant && msgs[j].Role != message.Tool {
				break
			}
			if keep[j] {
				keep[j] = false
				removed = true
			}
		}
	}
	if !removed {
		return msgs
	}
	filtered := make([]message.Message, 0, len(msgs))
	for i, msg := range msgs {
		if insertBoundaryBefore[i] {
			filtered = append(filtered, syntheticCanceledPromptBoundary(i))
		}
		if !keep[i] {
			continue
		}
		filtered = append(filtered, msg)
	}
	return filtered
}

func (a *sessionAgent) restoreDeferredToolProtocolState(sessionID string, msgs []message.Message) []string {
	if a == nil || a.deferredToolRuntime == nil || sessionID == "" {
		return nil
	}
	activated := make([]string, 0)
	seen := make(map[string]struct{})
	for _, m := range msgs {
		for _, name := range m.ActivatedDeferredTools {
			trimmed := strings.TrimSpace(name)
			if trimmed == "" {
				continue
			}
			if _, ok := seen[trimmed]; ok {
				continue
			}
			seen[trimmed] = struct{}{}
			activated = append(activated, trimmed)
		}
		if m.Role != message.Tool {
			continue
		}
		for _, tr := range m.ToolResults() {
			if state, ok := tr.DeferredToolState(); ok {
				for _, name := range state.ActivatedTools {
					trimmed := strings.TrimSpace(name)
					if trimmed == "" {
						continue
					}
					if _, ok := seen[trimmed]; ok {
						continue
					}
					seen[trimmed] = struct{}{}
					activated = append(activated, trimmed)
				}
			}
		}
	}
	if len(activated) == 0 {
		return nil
	}
	return a.deferredToolRuntime.activateDeferredToolsForSession(sessionID, activated)
}

func (a *sessionAgent) currentActivatedDeferredTools(sessionID string) []string {
	if a == nil || a.deferredToolRuntime == nil || sessionID == "" {
		return nil
	}
	set := a.deferredToolRuntime.activatedDeferredToolsForSession(sessionID)
	if len(set) == 0 {
		return nil
	}
	result := make([]string, 0, len(set))
	for name := range set {
		result = append(result, name)
	}
	slices.Sort(result)
	return result
}

func (a *sessionAgent) deferredToolProtocolState(sessionID string, recoveredTool string, recoveryAction string) message.ToolResultDeferredToolState {
	return message.ToolResultDeferredToolState{
		ActivatedTools: a.currentActivatedDeferredTools(sessionID),
		RecoveredTool:  strings.TrimSpace(recoveredTool),
		RecoveryAction: strings.TrimSpace(recoveryAction),
	}
}

func protocolStateFromRecoveryMetadata(recoveredToolFallback string, metadata string) message.ToolResultDeferredToolState {
	var payload struct {
		RecoveredBy       string   `json:"recovered_by"`
		RecoveryAction    string   `json:"recovery_action"`
		FallbackTool      string   `json:"fallback_tool"`
		FallbackToolQuery string   `json:"fallback_tool_query"`
		RecoveredParams   []string `json:"recovered_parameters"`
		Tool              string   `json:"tool"`
	}
	if strings.TrimSpace(metadata) == "" {
		return message.ToolResultDeferredToolState{}
	}
	if err := json.Unmarshal([]byte(metadata), &payload); err != nil {
		return message.ToolResultDeferredToolState{}
	}
	if strings.TrimSpace(payload.RecoveredBy) == "" && strings.TrimSpace(payload.RecoveryAction) == "" {
		return message.ToolResultDeferredToolState{}
	}
	recoveredTool := strings.TrimSpace(payload.Tool)
	if recoveredTool == "" {
		recoveredTool = strings.TrimSpace(recoveredToolFallback)
	}
	return message.ToolResultDeferredToolState{
		ActivatedTools:      nil,
		RecoveredTool:       recoveredTool,
		RecoveryAction:      strings.TrimSpace(payload.RecoveryAction),
		FallbackTool:        strings.TrimSpace(payload.FallbackTool),
		FallbackToolQuery:   strings.TrimSpace(payload.FallbackToolQuery),
		RecoveredParameters: payload.RecoveredParams,
	}
}

func (a *sessionAgent) preparePrompt(msgs []message.Message, attachments ...message.Attachment) ([]fantasy.Message, []fantasy.FilePart) {
	var history []fantasy.Message
	msgs = trimCanceledPromptBranches(msgs)
	if len(msgs) > 0 {
		a.restoreDeferredToolProtocolState(msgs[0].SessionID, msgs)
	}

	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		if len(m.Parts) == 0 {
			continue
		}
		// Assistant message without content or tool calls (cancelled before it
		// returned anything).
		if m.Role == message.Assistant && len(m.ToolCalls()) == 0 && m.Content().Text == "" && m.ReasoningContent().String() == "" {
			continue
		}

		if m.Role == message.Tool {
			toolResults := m.ToolResults()
			if len(toolResults) == 0 {
				continue
			}

			toolCallIDs := previousAssistantToolCallIDs(msgs, i)
			filteredToolResults := make([]message.ToolResult, 0, len(toolResults))
			for _, tr := range toolResults {
				if toolCallIDs[tr.ToolCallID] {
					filteredToolResults = append(filteredToolResults, tr)
					continue
				}
				slog.Warn("Dropping orphaned tool_result without matching previous assistant tool_call",
					"tool_call_id", tr.ToolCallID,
					"tool_name", tr.Name,
				)
			}
			if len(filteredToolResults) == 0 {
				continue
			}
			filtered := m
			filtered.Parts = nil
			filtered.SetToolResults(filteredToolResults)
			history = append(history, filtered.ToAIMessage()...)
			continue
		}

		history = append(history, m.ToAIMessage()...)

		if m.Role == message.Assistant {
			history = appendMissingToolResults(history, m, nextToolResultIDs(msgs, i))
		}
	}

	var files []fantasy.FilePart
	for _, attachment := range attachments {
		if attachment.IsText() {
			continue
		}
		files = append(files, fantasy.FilePart{
			Filename:  attachment.FileName,
			Data:      attachment.Content,
			MediaType: attachment.MimeType,
		})
	}

	return history, files
}

func previousAssistantToolCallIDs(msgs []message.Message, toolIndex int) map[string]bool {
	toolCallIDs := make(map[string]bool)
	for i := toolIndex - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role == message.Tool {
			continue
		}
		if m.Role != message.Assistant {
			break
		}
		for _, tc := range m.ToolCalls() {
			toolCallIDs[tc.ID] = true
		}
		break
	}
	return toolCallIDs
}

func nextToolResultIDs(msgs []message.Message, assistantIndex int) map[string]bool {
	toolResultIDs := make(map[string]bool)
	for i := assistantIndex + 1; i < len(msgs); i++ {
		m := msgs[i]
		if m.Role != message.Tool {
			break
		}
		for _, tr := range m.ToolResults() {
			toolResultIDs[tr.ToolCallID] = true
		}
	}
	return toolResultIDs
}

func appendMissingToolResults(history []fantasy.Message, m message.Message, toolResultIDs map[string]bool) []fantasy.Message {
	var missingParts []fantasy.MessagePart
	for _, tc := range m.ToolCalls() {
		if toolResultIDs[tc.ID] {
			continue
		}
		slog.Warn("Injecting synthetic tool_result for orphaned tool_use",
			"tool_call_id", tc.ID, "tool_name", tc.Name)
		missingOutput := fantasy.ToolResultOutputContentError{
			Error: fmt.Errorf("tool execution was interrupted"),
		}
		missingPart := fantasy.ToolResultPart{
			ToolCallID: tc.ID,
			Output:     missingOutput,
		}
		missingParts = append(missingParts, missingPart)
	}
	if len(missingParts) == 0 {
		return history
	}
	return append(history, fantasy.Message{
		Role:    fantasy.MessageRoleTool,
		Content: missingParts,
	})
}

func (a *sessionAgent) getSessionMessages(ctx context.Context, session session.Session) ([]message.Message, error) {
	start := time.Now()
	msgs, err := a.messages.List(ctx, session.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to list messages: %w", err)
	}
	slog.Debug("[PERF] getSessionMessages: messages.List done", "duration", time.Since(start), "session_id", session.ID, "count", len(msgs))

	if session.SummaryMessageID != "" {
		summaryMsgIndex := -1
		for i, msg := range msgs {
			if msg.ID == session.SummaryMessageID {
				summaryMsgIndex = i
				break
			}
		}
		if summaryMsgIndex != -1 {
			msgs = msgs[summaryMsgIndex:]
			msgs[0].Role = message.User
		}
	}
	return filterAutoModePromptMessages(msgs, session.PermissionMode), nil
}

func excludeCurrentUserMessage(msgs []message.Message, userMessage *message.Message) []message.Message {
	if userMessage == nil || userMessage.ID == "" || len(msgs) == 0 {
		return msgs
	}

	filtered := make([]message.Message, 0, len(msgs)-1)
	removed := false
	for _, msg := range msgs {
		if !removed && msg.ID == userMessage.ID {
			removed = true
			continue
		}
		filtered = append(filtered, msg)
	}
	if !removed {
		return msgs
	}
	return filtered
}
