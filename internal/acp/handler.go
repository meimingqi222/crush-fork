package acp

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent"
	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/clientfs"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/guimetrics"
	"github.com/charmbracelet/crush/internal/mcplifecycle"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/timeline"
	"github.com/charmbracelet/crush/internal/toolruntime"
	"github.com/charmbracelet/crush/internal/version"
	"github.com/google/uuid"
)

// cancelEntry wraps a cancel function for safe concurrent prompt handling.
// It allows the deferred cleanup to verify it's still the active entry
// before deleting from the map.
type cancelEntry struct {
	cancel context.CancelFunc
}

// Handler dispatches incoming ACP requests to the correct methods.
type Handler struct {
	app    App
	server *Server // set after server is constructed (circular reference resolved via setter)

	mu               sync.RWMutex
	cancels          map[string]*cancelEntry
	sessionCWD       map[string]string
	activeToolParams map[string]any
	clientCaps       ClientCapabilities

	mcpOwner string

	experimental ExperimentalExtension
}

// ExperimentalExtension augments initialize without adding private methods to
// the standard ACP Handler.
type ExperimentalExtension interface {
	ExperimentalCapabilities() map[string]any
	NegotiateExperimental(map[string]json.RawMessage) *RPCError
}

// MCPLifecycle is the shared asynchronous root-session MCP capability service.
type MCPLifecycle interface {
	ReplaceAsync(string, string, []mcplifecycle.ServerConfig) error
	Access(string) *mcplifecycle.Access
	CloseOwner(context.Context, string)
}

// App is the subset of app.App the ACP handler needs.
type App interface {
	GetSessions() session.Service
	GetMessages() message.Service
	GetCoordinator() agent.Coordinator
	GetConfig() *config.ConfigStore
	GetPermissions() permission.Service
	GetToolRuntime() toolruntime.Service
	GetTimeline() timeline.Service
	GetMCPLifecycle() MCPLifecycle
}

// NewHandler constructs a Handler backed by the given App.
func NewHandler(app App) *Handler {
	return &Handler{
		app:              app,
		cancels:          make(map[string]*cancelEntry),
		sessionCWD:       make(map[string]string),
		activeToolParams: make(map[string]any),
		mcpOwner:         uuid.NewString(),
	}
}

func (h *Handler) setToolParams(toolCallID string, params any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.activeToolParams == nil {
		h.activeToolParams = make(map[string]any)
	}
	h.activeToolParams[toolCallID] = params
}

func (h *Handler) getToolParams(toolCallID string) any {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.activeToolParams == nil {
		return nil
	}
	return h.activeToolParams[toolCallID]
}

func (h *Handler) deleteToolParams(toolCallID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.activeToolParams != nil {
		delete(h.activeToolParams, toolCallID)
	}
}

// SetServer wires the server reference so the handler can send notifications
// and outgoing calls.
func (h *Handler) SetServer(s *Server) {
	h.server = s
}

// SetExperimentalExtension installs connection-scoped capability negotiation.
// It must be called before Serve starts.
func (h *Handler) SetExperimentalExtension(extension ExperimentalExtension) {
	h.experimental = extension
}

// ClientCapabilities returns the capabilities advertised by the connected
// client during initialize. Returns the zero value before initialize is
// called. Smaller features (e.g. image-block fallbacks) can consult these to
// decide whether to route through the client FS.
func (h *Handler) ClientCapabilities() ClientCapabilities {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.clientCaps
}

func (h *Handler) setClientCapabilities(caps ClientCapabilities) {
	h.mu.Lock()
	h.clientCaps = caps
	h.mu.Unlock()
}

// replaceSessionMCP schedules root-session MCP replacement without waiting for
// transport startup. The shared service owns generation, tombstones and access.
func (h *Handler) replaceSessionMCP(sessionID string, servers []MCPServerConfig) error {
	lifecycle := h.app.GetMCPLifecycle()
	if lifecycle == nil {
		return nil
	}
	configs := make([]mcplifecycle.ServerConfig, 0, len(servers))
	for _, server := range servers {
		configs = append(configs, mcplifecycle.ServerConfig{
			Name: server.Name, Config: acpMCPConfigToConfig(server),
		})
	}
	return lifecycle.ReplaceAsync(h.mcpOwner, sessionID, configs)
}

func acpMCPConfigToConfig(s MCPServerConfig) config.MCPConfig {
	typ := config.MCPType(strings.ToLower(strings.TrimSpace(s.Type)))
	if typ == "" {
		typ = config.MCPStdio
	}
	env := make(map[string]string, len(s.Env))
	for _, e := range s.Env {
		env[e.Name] = e.Value
	}
	headers := make(map[string]string, len(s.Headers))
	for _, header := range s.Headers {
		headers[header.Name] = header.Value
	}
	return config.MCPConfig{
		Command: s.Command,
		Args:    s.Args,
		Env:     env,
		Type:    typ,
		URL:     s.URL,
		Headers: headers,
	}
}

// Close cancels prompts and revokes every MCP session owned by this connection.
func (h *Handler) Close(ctx context.Context) {
	h.mu.Lock()
	for _, entry := range h.cancels {
		entry.cancel()
	}
	clear(h.cancels)
	activePromptCount := len(h.cancels)
	h.mu.Unlock()
	recordActivePromptCount(ctx, activePromptCount)
	if lifecycle := h.app.GetMCPLifecycle(); lifecycle != nil {
		lifecycle.CloseOwner(ctx, h.mcpOwner)
	}
}

func recordActivePromptCount(ctx context.Context, count int) {
	guimetrics.FromContext(ctx).SetGauge(
		guimetrics.ActivePromptCount,
		int64(count),
		guimetrics.Labels{},
	)
}

// Handle dispatches an incoming request.
func (h *Handler) Handle(ctx context.Context, req *Request) (result any, rpcErr *RPCError) {
	started := time.Now()
	method := metricMethod(req.Method)
	defer func() {
		outcome := "success"
		if rpcErr != nil {
			outcome = "error"
		}
		labels := guimetrics.Labels{Method: method, Outcome: outcome, Transport: TransportName(ctx)}
		recorder := guimetrics.FromContext(ctx)
		recorder.ObserveDuration(guimetrics.ACPRequestDuration, time.Since(started), labels)
		if method == "session/load" {
			recorder.ObserveDuration(guimetrics.SessionLoadDuration, time.Since(started), labels)
		}
	}()

	switch req.Method {
	case "initialize":
		return h.handleInitialize(ctx, req)
	case "session/new":
		return h.handleSessionNew(ctx, req)
	case "session/load":
		return h.handleSessionLoad(ctx, req)
	case "session/list":
		return h.handleSessionList(ctx, req)
	case "session/prompt":
		return h.handleSessionPrompt(ctx, req)
	case "session/cancel":
		return h.handleSessionCancel(ctx, req)
	case "session/set_config_option":
		return h.handleSetConfigOption(ctx, req)
	case "session/set_mode":
		return h.handleSetMode(ctx, req)
	default:
		return nil, &RPCError{Code: CodeMethodNotFound, Message: fmt.Sprintf("method not found: %s", req.Method)}
	}
}

func metricMethod(method string) string {
	switch method {
	case "initialize", "session/new", "session/load", "session/list",
		"session/prompt", "session/cancel", "session/set_config_option",
		"session/set_mode":
		return method
	default:
		return "other"
	}
}

// handleInitialize processes the initialize handshake.
func (h *Handler) handleInitialize(_ context.Context, req *Request) (any, *RPCError) {
	var params InitializeParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()}
	}

	slog.Info("ACP: client connected", "client", params.ClientInfo.Name, "version", params.ClientInfo.Version)

	// Store the client's capabilities so other features (e.g. client-FS
	// indirection for unsaved buffers) can consult them later without a
	// deep refactor now.
	h.setClientCapabilities(params.ClientCapabilities)
	if h.experimental != nil {
		if rpcErr := h.experimental.NegotiateExperimental(params.Experimental); rpcErr != nil {
			return nil, rpcErr
		}
	}

	result := InitializeResult{
		ProtocolVersion: ProtocolVersion,
		AgentCapabilities: AgentCapabilities{
			LoadSession: true,
			PromptCapabilities: &PromptCapabilities{
				Image:           true,
				EmbeddedContext: true,
			},
			MCP: &MCPCapabilities{
				HTTP: true,
				SSE:  true,
			},
			SessionCapabilities: &SessionCapabilities{
				List: &struct{}{},
			},
		},
		AgentInfo: AgentInfo{
			Name:    "crush",
			Title:   "Crush",
			Version: version.Version,
		},
		AuthMethods: []string{},
	}
	if h.experimental != nil {
		result.Experimental = h.experimental.ExperimentalCapabilities()
	}
	return result, nil
}

// handleSessionNew creates a new session.
func (h *Handler) handleSessionNew(ctx context.Context, req *Request) (any, *RPCError) {
	var params SessionNewParams
	if req.Params != nil {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()}
		}
	}

	sess, err := h.app.GetSessions().Create(ctx, agent.DefaultSessionName)
	if err != nil {
		return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("failed to create session: %v", err)}
	}

	requestedCWD := normalizeOptionalSessionCWD(params.CWD)
	sess, err = h.persistSessionCWD(ctx, sess, requestedCWD)
	if err != nil {
		return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("failed to persist session cwd: %v", err)}
	}

	// Connect any client-supplied session-scoped MCP servers.
	if err := h.replaceSessionMCP(sess.ID, params.MCPServers); err != nil {
		return nil, &RPCError{Code: CodeInternalError, Message: "failed to schedule session MCP lifecycle"}
	}

	// Use the internal session ID as the ACP session ID for simplicity.
	slog.Info("ACP: created session", "session_id", sess.ID)
	return SessionNewResult{
		SessionID:     sess.ID,
		ConfigOptions: h.buildConfigOptions(sess.ID),
		Modes:         h.buildModes(sess.ID),
	}, nil
}

// handleSessionLoad loads an existing session.
func (h *Handler) handleSessionLoad(ctx context.Context, req *Request) (any, *RPCError) {
	var params SessionLoadParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()}
	}

	sess, err := h.app.GetSessions().Get(ctx, params.SessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, &RPCError{Code: CodeResourceNotFound, Message: fmt.Sprintf("session not found: %s", params.SessionID)}
		}
		return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("failed to get session: %v", err)}
	}

	// Replay history as session/update notifications before responding so
	// clients can deterministically rebuild transcript state during load.
	requestedCWD := normalizeOptionalSessionCWD(params.CWD)
	sess, err = h.persistSessionCWD(ctx, sess, requestedCWD)
	if err != nil {
		return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("failed to persist session cwd: %v", err)}
	}

	// Connect any client-supplied session-scoped MCP servers.
	if err := h.replaceSessionMCP(sess.ID, params.MCPServers); err != nil {
		return nil, &RPCError{Code: CodeInternalError, Message: "failed to schedule session MCP lifecycle"}
	}

	h.replayHistory(ctx, sess.ID)

	slog.Info("ACP: loaded session", "session_id", sess.ID)
	return SessionLoadResult{
		ConfigOptions: h.buildConfigOptions(sess.ID),
		Modes:         h.buildModes(sess.ID),
	}, nil
}

// replayHistory replays all messages as session/update notifications.
func (h *Handler) replayHistory(ctx context.Context, sessionID string) {
	msgs, err := h.app.GetMessages().List(ctx, sessionID)
	if err != nil {
		slog.Warn("ACP: failed to list messages for replay", "session_id", sessionID, "err", err)
		return
	}
	for _, msg := range msgs {
		switch msg.Role {
		case message.User:
			if content := msg.Content().Text; content != "" {
				h.sendUpdateSyncWithContext(ctx, sessionID, SessionUpdate{
					SessionUpdate: SessionUpdateUserMessageChunk,
					Content:       TextBlock(content),
				})
			}
			for _, bc := range msg.BinaryContent() {
				h.sendUpdateSyncWithContext(ctx, sessionID, SessionUpdate{
					SessionUpdate: SessionUpdateUserMessageChunk,
					Content:       BinaryContent(bc.MIMEType, base64.StdEncoding.EncodeToString(bc.Data)),
				})
			}
			for _, iuc := range msg.ImageURLContent() {
				h.sendUpdateSyncWithContext(ctx, sessionID, SessionUpdate{
					SessionUpdate: SessionUpdateUserMessageChunk,
					Content:       ResourceContent(iuc.URL, mimeTypeForResourceURL(iuc.URL), iuc.Detail),
				})
			}
		case message.Tool:
			for _, tr := range msg.ToolResults() {
				h.sendUpdateSyncWithContext(ctx, sessionID, h.sessionUpdateFromToolResult(tr, sessionID, sessionID, nil))
			}
		case message.Assistant:
			if content := msg.Content().Text; content != "" {
				h.sendUpdateSyncWithContext(ctx, sessionID, SessionUpdate{
					SessionUpdate: SessionUpdateAgentMessageChunk,
					Content:       TextBlock(content),
				})
			}
			for _, bc := range msg.BinaryContent() {
				h.sendUpdateSyncWithContext(ctx, sessionID, SessionUpdate{
					SessionUpdate: SessionUpdateAgentMessageChunk,
					Content:       BinaryContent(bc.MIMEType, base64.StdEncoding.EncodeToString(bc.Data)),
				})
			}
			for _, iuc := range msg.ImageURLContent() {
				h.sendUpdateSyncWithContext(ctx, sessionID, SessionUpdate{
					SessionUpdate: SessionUpdateAgentMessageChunk,
					Content:       ResourceContent(iuc.URL, mimeTypeForResourceURL(iuc.URL), iuc.Detail),
				})
			}
			for _, tc := range msg.ToolCalls() {
				update, _ := h.buildToolCallUpdate(tc, sessionID, sessionID, nil)
				h.sendUpdateSyncWithContext(ctx, sessionID, update)
			}
		}
	}
}

// handleSessionPrompt runs a prompt turn and streams updates back.
func (h *Handler) handleSessionPrompt(ctx context.Context, req *Request) (any, *RPCError) {
	var params PromptParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()}
	}

	if params.SessionID == "" {
		return nil, &RPCError{Code: CodeInvalidParams, Message: "sessionId is required"}
	}

	// Build prompt text and attachments from content blocks.
	promptText, attachments, err := extractPromptContent(params.Prompt)
	if err != nil {
		return nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()}
	}
	if promptText == "" && len(attachments) == 0 {
		return nil, &RPCError{Code: CodeInvalidParams, Message: "prompt is empty"}
	}

	// Subscribe to messages and sessions before running to capture streaming updates.
	// Use a dedicated child context so the subscription is cleaned up when
	// this function returns, preventing a slow memory leak.
	subCtx, subCancel := context.WithCancel(ctx)
	defer subCancel()
	msgSub := h.app.GetMessages().Subscribe(subCtx)
	sessionSub := h.app.GetSessions().Subscribe(subCtx)
	runtimeSub := h.app.GetToolRuntime().Subscribe(subCtx)
	timelineSub := h.app.GetTimeline().Subscribe(subCtx)

	// Wrap context with cancellation so session/cancel can stop the run.
	runCtx, cancel := context.WithCancel(ctx)
	if lifecycle := h.app.GetMCPLifecycle(); lifecycle != nil {
		runCtx = agenttools.WithMCPServerAccess(runCtx, lifecycle.Access(params.SessionID))
	}
	if provider, ok := h.experimental.(interface {
		ClientFSForSession(string, string) *clientfs.Scope
	}); ok {
		if sess, getErr := h.app.GetSessions().Get(runCtx, params.SessionID); getErr == nil {
			workspace := sess.WorkspaceCWD
			if workspace == "" && h.app.GetConfig() != nil {
				workspace = h.app.GetConfig().WorkingDir()
			}
			runCtx = clientfs.WithScope(runCtx, provider.ClientFSForSession(params.SessionID, workspace))
		}
	}
	entry := &cancelEntry{cancel: cancel}
	h.mu.Lock()
	// Cancel any previous prompt for this session before overwriting.
	if oldEntry, ok := h.cancels[params.SessionID]; ok {
		oldEntry.cancel()
	}
	h.cancels[params.SessionID] = entry
	activePromptCount := len(h.cancels)
	h.mu.Unlock()
	recordActivePromptCount(ctx, activePromptCount)
	defer func() {
		cancel()
		h.mu.Lock()
		// Only delete if this entry is still the active one.
		// This prevents a finishing prompt from removing another prompt's
		// cancel entry when multiple prompts run concurrently for the same session.
		if h.cancels[params.SessionID] == entry {
			delete(h.cancels, params.SessionID)
		}
		activePromptCount := len(h.cancels)
		h.mu.Unlock()
		recordActivePromptCount(ctx, activePromptCount)
	}()

	// Track the last-known text length per message for streaming.
	readBytes := make(map[string]int)
	runtimeSnapshotHashes := make(map[string][32]byte)
	trackedSessionIDs := map[string]struct{}{params.SessionID: {}}
	rejectedSessionIDs := make(map[string]struct{})
	prefixCache := make(map[string]string)
	defer func() {
		// Sweep any tool-call parameters observed this turn so cancelled
		// or errored runs do not leak activeToolParams entries.
		for key := range readBytes {
			if !strings.Contains(key, ":tc:") || strings.HasSuffix(key, ":done") {
				continue
			}
			tcID := key[strings.LastIndex(key, ":tc:")+4:]
			h.deleteToolParams(tcID)
		}
	}()

	// Run the agent in a goroutine and stream message events.
	type runResult struct {
		result *fantasy.AgentResult
		err    error
	}
	done := make(chan runResult, 1)

	go func() {
		result, err := h.app.GetCoordinator().Run(runCtx, params.SessionID, promptText, attachments...)
		done <- runResult{result, err}
	}()

	stopReason := StopReasonEndTurn

loop:
	for {
		select {
		case r := <-done:
			if r.err != nil {
				if isContextError(r.err) {
					stopReason = StopReasonCancelled
				} else {
					return nil, &RPCError{Code: CodeInternalError, Message: r.err.Error()}
				}
			} else if r.result != nil {
				stopReason = stopReasonFromAgentResult(r.result)
			}
			// Drain any remaining subscription events before returning so that
			// trailing stream chunks are not lost.
			for {
				drained := false
				select {
				case event := <-msgSub:
					drained = true
					if h.shouldForwardSessionEvent(subCtx, params.SessionID, event.Payload.SessionID, trackedSessionIDs, rejectedSessionIDs) {
						h.handleMessageEvent(event.Payload, params.SessionID, readBytes, prefixCache, func(update SessionUpdate) {
							h.sendUpdateSyncWithContext(subCtx, params.SessionID, update)
						})
					}
				default:
				}

				select {
				case event := <-sessionSub:
					drained = true
					if event.Payload.ID == params.SessionID {
						h.handleSessionEvent(event, func(update SessionUpdate) {
							h.sendUpdateSyncWithContext(subCtx, params.SessionID, update)
						})
						continue
					}
					if event.Payload.ParentSessionID == params.SessionID {
						trackedSessionIDs[event.Payload.ID] = struct{}{}
					}
				default:
				}

				select {
				case event := <-runtimeSub:
					drained = true
					if h.shouldForwardSessionEvent(subCtx, params.SessionID, event.Payload.SessionID, trackedSessionIDs, rejectedSessionIDs) {
						h.handleToolRuntimeEvent(event, params.SessionID, runtimeSnapshotHashes, prefixCache, func(update SessionUpdate) {
							h.sendUpdateSyncWithContext(subCtx, params.SessionID, update)
						})
					}
				default:
				}

				select {
				case event := <-timelineSub:
					drained = true
					if h.shouldForwardSessionEvent(subCtx, params.SessionID, event.Payload.SessionID, trackedSessionIDs, rejectedSessionIDs) {
						h.handleTimelineEvent(event, func(update SessionUpdate) {
							h.sendUpdateSyncWithContext(subCtx, params.SessionID, update)
						})
					}
				default:
				}

				if !drained {
					break loop
				}
			}

		case event := <-msgSub:
			msg := event.Payload
			if !h.shouldForwardSessionEvent(subCtx, params.SessionID, msg.SessionID, trackedSessionIDs, rejectedSessionIDs) {
				continue
			}
			h.handleMessageEvent(msg, params.SessionID, readBytes, prefixCache, func(update SessionUpdate) {
				h.sendUpdateSyncWithContext(subCtx, params.SessionID, update)
			})

		case event := <-sessionSub:
			if event.Payload.ID == params.SessionID {
				h.handleSessionEvent(event, func(update SessionUpdate) {
					h.sendUpdateSyncWithContext(subCtx, params.SessionID, update)
				})
				continue
			}
			if event.Payload.ParentSessionID == params.SessionID {
				trackedSessionIDs[event.Payload.ID] = struct{}{}
			}

		case event := <-runtimeSub:
			if !h.shouldForwardSessionEvent(subCtx, params.SessionID, event.Payload.SessionID, trackedSessionIDs, rejectedSessionIDs) {
				continue
			}
			h.handleToolRuntimeEvent(event, params.SessionID, runtimeSnapshotHashes, prefixCache, func(update SessionUpdate) {
				h.sendUpdateSyncWithContext(subCtx, params.SessionID, update)
			})

		case event := <-timelineSub:
			if !h.shouldForwardSessionEvent(subCtx, params.SessionID, event.Payload.SessionID, trackedSessionIDs, rejectedSessionIDs) {
				continue
			}
			h.handleTimelineEvent(event, func(update SessionUpdate) {
				h.sendUpdateSyncWithContext(subCtx, params.SessionID, update)
			})

		case <-ctx.Done():
			stopReason = StopReasonCancelled
			break loop
		}
	}
	return PromptResult{StopReason: stopReason}, nil
}

// handleMessageEvent converts a message update into session/update notifications.
func (h *Handler) handleMessageEvent(msg message.Message, parentSessionID string, readBytes map[string]int, prefixCache map[string]string, send func(SessionUpdate)) {
	switch msg.Role {
	case message.Assistant:
		if msg.SessionID == parentSessionID {
			// Stream text content as agent_message_chunk.
			content := msg.Content().Text
			prev := readBytes[msg.ID]
			if len(content) > prev {
				chunk := content[prev:]
				readBytes[msg.ID] = len(content)
				send(SessionUpdate{
					SessionUpdate: SessionUpdateAgentMessageChunk,
					Content:       TextBlock(chunk),
				})
			}

			// Stream reasoning/thinking.
			thinking := msg.ReasoningContent().Thinking
			prevThink := readBytes[msg.ID+":think"]
			if len(thinking) > prevThink {
				chunk := thinking[prevThink:]
				readBytes[msg.ID+":think"] = len(thinking)
				send(SessionUpdate{
					SessionUpdate: SessionUpdateAgentThoughtChunk,
					Content:       TextBlock(chunk),
				})
			}

			// Emit binary (image/audio) and resource (URL reference) content.
			for i, bc := range msg.BinaryContent() {
				key := msg.ID + ":binary:" + strconv.Itoa(i)
				if seen := readBytes[key]; seen == 0 {
					readBytes[key] = max(len(bc.Data), 1)
					send(SessionUpdate{
						SessionUpdate: SessionUpdateAgentMessageChunk,
						Content:       BinaryContent(bc.MIMEType, base64.StdEncoding.EncodeToString(bc.Data)),
					})
				}
			}
			for i, iuc := range msg.ImageURLContent() {
				key := msg.ID + ":resource:" + strconv.Itoa(i)
				if seen := readBytes[key]; seen == 0 {
					readBytes[key] = 1
					send(SessionUpdate{
						SessionUpdate: SessionUpdateAgentMessageChunk,
						Content:       ResourceContent(iuc.URL, mimeTypeForResourceURL(iuc.URL), iuc.Detail),
					})
				}
			}
		}

		// Emit tool call events.
		for _, tc := range msg.ToolCalls() {
			key := msg.ID + ":tc:" + tc.ID
			update, inputParams := h.buildToolCallUpdate(tc, msg.SessionID, parentSessionID, prefixCache)
			h.setToolParams(tc.ID, inputParams)

			if _, seen := readBytes[key]; !seen {
				readBytes[key] = 1
				send(update)
			} else if tc.Finished {
				finishedKey := msg.ID + ":tc:" + tc.ID + ":done"
				if _, done := readBytes[finishedKey]; !done {
					readBytes[finishedKey] = 1
					update.SessionUpdate = SessionUpdateToolCallUpdate
					send(update)
				}
			}
		}
	case message.Tool:
		for _, tr := range msg.ToolResults() {
			key := msg.ID + ":tr:" + tr.ToolCallID
			if _, seen := readBytes[key]; !seen {
				readBytes[key] = 1
				send(h.sessionUpdateFromToolResult(tr, msg.SessionID, parentSessionID, prefixCache))
			}
			// Bridge todos tool results to ACP plan updates so IDEs that
			// render agent plans stay in sync with crush's todo state.
			// Best-effort: failures to parse must not disturb the normal
			// tool-call updates.
			if tr.Name == agenttools.TodosToolName {
				if update, ok := planUpdateFromTodosResult(tr); ok {
					send(update)
				}
			}
		}
	}
}

// planUpdateFromTodosResult translates a todos tool result's metadata into an
// ACP SessionUpdatePlan notification. Returns ok=false when the metadata is
// absent or has no todo entries.
func planUpdateFromTodosResult(tr message.ToolResult) (SessionUpdate, bool) {
	if strings.TrimSpace(tr.Metadata) == "" {
		return SessionUpdate{}, false
	}
	var meta agenttools.TodosResponseMetadata
	if err := json.Unmarshal([]byte(tr.Metadata), &meta); err != nil {
		return SessionUpdate{}, false
	}
	entries := make([]PlanEntry, 0, len(meta.Todos))
	for _, todo := range meta.Todos {
		entries = append(entries, PlanEntry{
			Content:  todo.Content,
			Priority: "medium",
			Status:   string(todo.Status),
		})
	}
	return SessionUpdate{
		SessionUpdate: SessionUpdatePlan,
		Entries:       &entries,
	}, true
}

func (h *Handler) buildToolCallUpdate(tc message.ToolCall, sessionID, parentSessionID string, prefixCache map[string]string) (SessionUpdate, any) {
	var inputParams any
	if tc.Input != "" {
		_ = json.Unmarshal([]byte(tc.Input), &inputParams)
	}
	if inputParams == nil {
		inputParams = tc.Input
	}
	prefix := h.getSubagentPrefix(context.Background(), sessionID, parentSessionID, prefixCache)
	parentTCID := ""
	if sessionID != parentSessionID {
		parentTCID = cleanParentToolCallID(sessionID)
	}
	status := ToolCallStatusInProgress
	if tc.Finished {
		status = ToolCallStatusCompleted
	}
	return SessionUpdate{
		SessionUpdate:    SessionUpdateToolCall,
		ToolCallID:       tc.ID,
		Title:            prefix + GetBeautifulTitle(tc.Name, "", inputParams),
		Kind:             GetToolKind(tc.Name),
		Status:           status,
		RawInput:         inputParams,
		ParentToolCallID: parentTCID,
	}, inputParams
}

func (h *Handler) handleToolRuntimeEvent(event pubsub.Event[toolruntime.State], parentSessionID string, snapshotHashes map[string][32]byte, prefixCache map[string]string, send func(SessionUpdate)) {
	if event.Type == pubsub.DeletedEvent {
		return
	}

	state := event.Payload
	if background, _ := state.ClientMetadata["background"].(bool); background {
		return
	}

	if state.Status == toolruntime.StatusRunning {
		snapshot := strings.TrimSpace(state.SnapshotText)
		if snapshot == "" {
			return
		}

		hash := sha256.Sum256([]byte(snapshot))
		if prev, ok := snapshotHashes[state.ToolCallID]; ok && prev == hash {
			return
		}
		snapshotHashes[state.ToolCallID] = hash

		inputParams := h.getToolParams(state.ToolCallID)
		prefix := h.getSubagentPrefix(context.Background(), state.SessionID, parentSessionID, prefixCache)
		parentTCID := ""
		if state.SessionID != parentSessionID {
			parentTCID = cleanParentToolCallID(state.SessionID)
		}

		send(SessionUpdate{
			SessionUpdate:    SessionUpdateToolCallUpdate,
			ToolCallID:       state.ToolCallID,
			Title:            prefix + GetBeautifulTitle(state.ToolName, "", inputParams),
			Kind:             GetToolKind(state.ToolName),
			Status:           ToolCallStatusInProgress,
			Content:          []any{ContentItem(TextBlock(snapshot))},
			ClientMetadata:   state.ClientMetadata,
			DurationMs:       state.DurationMs,
			ParentToolCallID: parentTCID,
			Locations:        h.getToolLocations(state.ToolName, inputParams),
		})
		return
	}

	if state.Status != toolruntime.StatusCompleted && state.Status != toolruntime.StatusFailed && state.Status != toolruntime.StatusCanceled {
		return
	}
	delete(snapshotHashes, state.ToolCallID)
	// Do not delete activeToolParams here, as sessionUpdateFromToolResult
	// will need these parameters to generate the final beautiful title.

	status := ToolCallStatusCompleted
	switch state.Status {
	case toolruntime.StatusFailed:
		status = ToolCallStatusFailed
	case toolruntime.StatusCanceled:
		status = ToolCallStatusCanceled
	}

	inputParams := h.getToolParams(state.ToolCallID)
	prefix := h.getSubagentPrefix(context.Background(), state.SessionID, parentSessionID, prefixCache)
	parentTCID := ""
	if state.SessionID != parentSessionID {
		parentTCID = cleanParentToolCallID(state.SessionID)
	}

	send(SessionUpdate{
		SessionUpdate:    SessionUpdateToolCallUpdate,
		ToolCallID:       state.ToolCallID,
		Title:            prefix + GetBeautifulTitle(state.ToolName, "", inputParams),
		Kind:             GetToolKind(state.ToolName),
		Status:           status,
		ClientMetadata:   state.ClientMetadata,
		DurationMs:       state.DurationMs,
		ParentToolCallID: parentTCID,
		Locations:        h.getToolLocations(state.ToolName, inputParams),
	})
}

// handleSessionEvent converts a session update into session/update notifications.
func (h *Handler) handleSessionEvent(event pubsub.Event[session.Session], send func(SessionUpdate)) {
	sess := event.Payload
	send(SessionUpdate{
		SessionUpdate: SessionUpdateSessionInfoUpdate,
		Title:         sess.Title,
		UpdatedAt:     time.Unix(sess.UpdatedAt, 0).UTC().Format(time.RFC3339),
	})
}

func (h *Handler) handleTimelineEvent(event pubsub.Event[timeline.Event], send func(SessionUpdate)) {
	if event.Type == pubsub.DeletedEvent {
		return
	}
	send(SessionUpdate{
		SessionUpdate: SessionUpdateTimelineEvent,
		TimelineEvent: timelineEventPayload(event.Payload),
	})
}

func (h *Handler) sessionUpdateFromToolResult(tr message.ToolResult, sessionID, parentSessionID string, prefixCache map[string]string) SessionUpdate {
	status := ToolCallStatusCompleted
	subtaskResult, hasSubtaskResult := tr.SubtaskResult()
	if hasSubtaskResult {
		switch subtaskResult.Status {
		case message.ToolResultSubtaskStatusFailed:
			status = ToolCallStatusFailed
		case message.ToolResultSubtaskStatusCanceled:
			status = ToolCallStatusCanceled
		}
	} else if tr.IsError {
		status = ToolCallStatusFailed
	}

	inputParams := h.getToolParams(tr.ToolCallID)
	prefix := h.getSubagentPrefix(context.Background(), sessionID, parentSessionID, prefixCache)
	parentTCID := ""
	if sessionID != parentSessionID {
		parentTCID = cleanParentToolCallID(sessionID)
	}

	toolKind := GetToolKind(tr.Name)
	// Per the ACP spec, tool_call_update content is a ToolCallContent[] array.
	// Edit/write tools produce a text entry plus an optional diff entry;
	// everything else is wrapped as a single "content" item.
	var content any
	if toolKind == ToolKindEdit {
		if diffContent := h.getToolDiffContent(tr.Name, inputParams, tr.Content); diffContent != nil {
			content = diffContent
		}
	}
	if content == nil {
		content = []any{ContentItem(TextBlock(tr.Content))}
	}

	update := SessionUpdate{
		SessionUpdate:    SessionUpdateToolCallUpdate,
		ToolCallID:       tr.ToolCallID,
		Title:            prefix + GetBeautifulTitle(tr.Name, "", inputParams),
		Kind:             toolKind,
		Status:           status,
		RawOutput:        tr.Content,
		Content:          content,
		ParentToolCallID: parentTCID,
		Locations:        h.getToolLocations(tr.Name, inputParams),
		ClientMetadata:   clientFSClientMetadata(tr.Metadata),
	}
	h.deleteToolParams(tr.ToolCallID)
	if hasSubtaskResult {
		update.ChildSessionID = subtaskResult.ChildSessionID
		update.ParentToolCallID = subtaskResult.ParentToolCallID
		update.SubtaskResult = &SubtaskResult{Status: string(subtaskResult.Status), ParentMessageID: subtaskResult.ParentMessageID}
	}
	if reducer, ok := tr.Reducer(); ok {
		update.Reducer = &Reducer{
			Summary:     reducer.Summary,
			Artifacts:   reducer.Artifacts,
			Risks:       reducer.Risks,
			NextActions: reducer.NextActions,
			Confidence:  reducer.Confidence,
			MailboxID:   reducer.MailboxID,
			Messages:    reducer.Messages,
		}
	}
	return update
}

func clientFSClientMetadata(raw string) map[string]any {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	type fileMetadata struct {
		Path      string `json:"path"`
		FilePath  string `json:"file_path"`
		SourceURI string `json:"source_uri"`
		Revision  string `json:"revision"`
	}
	var values []fileMetadata
	if strings.HasPrefix(raw, "[") {
		if json.Unmarshal([]byte(raw), &values) != nil {
			return nil
		}
	} else {
		var value fileMetadata
		if json.Unmarshal([]byte(raw), &value) != nil {
			return nil
		}
		values = []fileMetadata{value}
	}
	if len(values) > 50 {
		values = values[:50]
	}
	files := make([]map[string]string, 0, len(values))
	for _, value := range values {
		if value.SourceURI == "" || value.Revision == "" {
			continue
		}
		path := value.Path
		if path == "" {
			path = value.FilePath
		}
		files = append(files, map[string]string{
			"path": path, "sourceUri": value.SourceURI, "revision": value.Revision,
		})
	}
	if len(files) == 0 {
		return nil
	}
	return map[string]any{"clientFS": map[string]any{"files": files}}
}

// handleSessionCancel cancels a running prompt turn.
func (h *Handler) handleSessionCancel(_ context.Context, req *Request) (any, *RPCError) {
	var params SessionCancelParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()}
	}

	h.mu.RLock()
	entry, ok := h.cancels[params.SessionID]
	h.mu.RUnlock()

	if ok {
		entry.cancel()
		slog.Info("ACP: cancelled session", "session_id", params.SessionID)
	}
	return struct{}{}, nil
}

// sendUpdate dispatches a session/update notification to the connected client.
func (h *Handler) sendUpdate(sessionID string, update SessionUpdate) {
	h.sendUpdateWithContext(context.Background(), sessionID, update)
}

func (h *Handler) sendUpdateWithContext(ctx context.Context, sessionID string, update SessionUpdate) {
	if h.server == nil {
		return
	}
	h.server.Notify(ctx, "session/update", SessionUpdateNotification{
		SessionID: sessionID,
		Update:    update,
	})
}

func (h *Handler) sendUpdateSyncWithContext(ctx context.Context, sessionID string, update SessionUpdate) {
	if h.server == nil {
		return
	}
	if err := h.server.NotifySync(ctx, "session/update", SessionUpdateNotification{
		SessionID: sessionID,
		Update:    update,
	}); err != nil {
		slog.Warn("ACP: failed to write session update", "session_id", sessionID, "err", err)
	}
}

func (h *Handler) shouldForwardSessionEvent(ctx context.Context, parentSessionID string, candidateSessionID string, trackedSessionIDs, rejectedSessionIDs map[string]struct{}) bool {
	if candidateSessionID == parentSessionID {
		trackedSessionIDs[candidateSessionID] = struct{}{}
		return true
	}
	if _, ok := trackedSessionIDs[candidateSessionID]; ok {
		return true
	}
	if candidateSessionID == "" {
		return false
	}
	if rejectedSessionIDs != nil {
		if _, ok := rejectedSessionIDs[candidateSessionID]; ok {
			return false
		}
	}
	candidate, err := h.app.GetSessions().Get(ctx, candidateSessionID)
	if err != nil {
		if rejectedSessionIDs != nil {
			rejectedSessionIDs[candidateSessionID] = struct{}{}
		}
		return false
	}
	if candidate.ParentSessionID != parentSessionID {
		if rejectedSessionIDs != nil {
			rejectedSessionIDs[candidateSessionID] = struct{}{}
		}
		return false
	}
	trackedSessionIDs[candidateSessionID] = struct{}{}
	return true
}

// extractPromptContent joins text blocks and inlines resource blocks into a
// prompt string, and extracts image/audio blocks into crush attachments.
func extractPromptContent(blocks []ContentBlock) (string, []message.Attachment, error) {
	var sb strings.Builder
	var attachments []message.Attachment
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				if sb.Len() > 0 {
					sb.WriteByte('\n')
				}
				sb.WriteString(b.Text)
			}
		case "resource":
			if b.Resource == nil {
				return "", nil, errors.New("resource block is missing resource")
			}
			if b.Resource.URI == "" {
				return "", nil, errors.New("resource block is missing uri")
			}
			if b.Resource.Text == "" && b.Resource.Blob == "" {
				return "", nil, errors.New("resource block is missing text or blob")
			}
			if b.Resource.Text != "" && b.Resource.Blob != "" {
				return "", nil, errors.New("resource block cannot contain both text and blob")
			}
			if b.Resource.Blob != "" {
				data, err := base64.StdEncoding.DecodeString(b.Resource.Blob)
				if err != nil {
					return "", nil, fmt.Errorf("invalid resource blob: %w", err)
				}
				mimeType := b.Resource.MIMEType
				if mimeType == "" {
					mimeType = "application/octet-stream"
				}
				attachments = append(attachments, message.Attachment{
					FileName: b.Resource.URI,
					MimeType: mimeType,
					Content:  data,
				})
				continue
			}
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString("```resource\n")
			sb.WriteString(b.Resource.URI)
			if b.Resource.Text != "" {
				sb.WriteByte('\n')
				sb.WriteString(b.Resource.Text)
			}
			sb.WriteString("\n```")
		case "image", "audio":
			if b.Data == "" {
				return "", nil, fmt.Errorf("%s block is missing data", b.Type)
			}
			if b.MIMEType == "" {
				return "", nil, fmt.Errorf("%s block is missing mimeType", b.Type)
			}
			data, err := base64.StdEncoding.DecodeString(b.Data)
			if err != nil {
				return "", nil, fmt.Errorf("invalid %s block: %w", b.Type, err)
			}
			attachments = append(attachments, message.Attachment{
				FileName: b.Type + "-" + strconv.Itoa(len(attachments)),
				MimeType: b.MIMEType,
				Content:  data,
			})
		default:
			return "", nil, fmt.Errorf("unsupported content block type: %s", b.Type)
		}
	}
	return sb.String(), attachments, nil
}

// mimeTypeForResourceURL infers the MIME type of an external resource from
// its URL, returning "" when it cannot be determined.
func mimeTypeForResourceURL(uri string) string {
	if mimeType := mime.TypeByExtension(filepath.Ext(uri)); mimeType != "" {
		return mimeType
	}
	u, err := url.Parse(uri)
	if err == nil && u != nil {
		if mimeType := mime.TypeByExtension(filepath.Ext(u.Path)); mimeType != "" {
			return mimeType
		}
	}
	return ""
}

// stopReasonFromAgentResult maps the final run finish reason to an ACP
// StopReason.
func stopReasonFromAgentResult(result *fantasy.AgentResult) StopReason {
	if result == nil {
		return StopReasonEndTurn
	}
	switch result.Response.FinishReason {
	case fantasy.FinishReasonLength:
		return StopReasonMaxTokens
	case fantasy.FinishReasonContentFilter:
		return StopReasonRefusal
	case fantasy.FinishReasonMaxTurnRequests:
		return StopReasonMaxTurnRequests
	default:
		return StopReasonEndTurn
	}
}

// isContextError returns true if the error is a context cancellation, deadline,
// or the agent's own request-cancelled sentinel.
func isContextError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if errors.Is(err, agent.ErrRequestCancelled) {
		return true
	}
	return false
}

func (h *Handler) currentModeForSession(sessionID string) string {
	if strings.TrimSpace(sessionID) == "" {
		return "default"
	}
	sess, err := h.app.GetSessions().Get(context.Background(), sessionID)
	if err != nil {
		return "default"
	}
	return session.ModeStateFromSession(sess).CurrentModeID()
}

func (h *Handler) setSessionCWD(sessionID, cwd string) {
	h.mu.Lock()
	h.sessionCWD[sessionID] = cwd
	h.mu.Unlock()
}

func normalizeOptionalSessionCWD(cwd string) string {
	if strings.TrimSpace(cwd) == "" {
		return ""
	}
	return normalizeSessionCWD(cwd)
}

func normalizeSessionCWD(cwd string) string {
	if cwd == "" {
		cwd = "."
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return cwd
	}
	return abs
}

func (h *Handler) persistSessionCWD(ctx context.Context, sess session.Session, cwd string) (session.Session, error) {
	if cwd != "" && cwd != sess.WorkspaceCWD {
		sess.WorkspaceCWD = cwd
		saved, err := h.app.GetSessions().Save(ctx, sess)
		if err != nil {
			return session.Session{}, err
		}
		sess = saved
	}
	h.setSessionCWD(sess.ID, normalizeOptionalSessionCWD(sess.WorkspaceCWD))
	return sess, nil
}

func (h *Handler) sessionCWDForSession(sess session.Session, fallbackCWD string) string {
	h.mu.RLock()
	cwd := h.sessionCWD[sess.ID]
	h.mu.RUnlock()
	if stored := normalizeOptionalSessionCWD(sess.WorkspaceCWD); stored != "" {
		return stored
	}
	if cwd != "" {
		return cwd
	}
	return normalizeSessionCWD(fallbackCWD)
}

func (h *Handler) buildModes(sessionID string) *SessionModeState {
	return &SessionModeState{
		CurrentModeID: h.currentModeForSession(sessionID),
		AvailableModes: []SessionMode{
			{
				ID:          "default",
				Name:        "Default",
				Description: "Ask for permission when required",
			},
			{
				ID:          "auto",
				Name:        "Auto",
				Description: "Guarded autonomy with manual fallback",
			},
			{
				ID:          "yolo",
				Name:        "YOLO",
				Description: "Auto-approve tool permissions in this session",
			},
		},
	}
}

// applyPermissionMode performs the shared mode-transition logic used by both
// session/set_mode and session/set_config_option{configId: "mode"}. It
// validates the mode, clears persistent permissions when entering auto,
// persists the new permission mode, and emits an auto-mode exit prompt message
// when applicable. Callers are responsible for sending the
// current_mode_update / config_option_update notifications and for shaping
// their own response, since the two handlers differ in those respects.
func (h *Handler) applyPermissionMode(ctx context.Context, sessionID, modeID string) (*session.ModeTransition, *RPCError) {
	if modeID == "auto" {
		h.app.GetPermissions().ClearPersistentPermissions(sessionID)
	}
	current, err := h.app.GetSessions().Get(ctx, sessionID)
	if err != nil {
		return nil, &RPCError{Code: CodeInternalError, Message: err.Error()}
	}
	transition := session.NewPermissionModeTransition(current, session.NormalizePermissionMode(modeID))
	if _, err := h.app.GetSessions().UpdatePermissionMode(ctx, sessionID, transition.Current.PermissionMode); err != nil {
		return nil, &RPCError{Code: CodeInternalError, Message: err.Error()}
	}
	if transition.ExitedAutoMode() {
		if _, err := h.app.GetMessages().Create(ctx, sessionID, message.NewAutoModePromptMessage(message.AutoModePromptTypeExit)); err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: err.Error()}
		}
	}
	return &transition, nil
}

func (h *Handler) handleSetMode(ctx context.Context, req *Request) (any, *RPCError) {
	var params SetModeParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()}
	}
	if params.ModeID != "default" && params.ModeID != "auto" && params.ModeID != "yolo" {
		return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid modeId: %s", params.ModeID)}
	}
	transition, rpcErr := h.applyPermissionMode(ctx, params.SessionID, params.ModeID)
	if rpcErr != nil {
		return nil, rpcErr
	}
	h.sendUpdate(params.SessionID, SessionUpdate{
		SessionUpdate: SessionUpdateCurrentModeUpdate,
		CurrentModeID: transition.Current.CurrentModeID(),
	})
	h.sendUpdate(params.SessionID, SessionUpdate{
		SessionUpdate: SessionUpdateConfigOptionUpdate,
		ConfigOptions: h.buildConfigOptions(params.SessionID),
	})
	return struct{}{}, nil
}

// buildConfigOptions builds the config options for the session.
// It returns available models that the user can select from.
func (h *Handler) buildConfigOptions(sessionID string) []ConfigOption {
	cfg := h.app.GetConfig()
	if cfg == nil {
		return nil
	}

	var options []ConfigOptionVariant
	seenModels := make(map[string]bool)

	// Add enabled providers' models.
	for _, provider := range cfg.Config().EnabledProviders() {
		for _, model := range provider.Models {
			key := provider.ID + ":" + model.ID
			if seenModels[key] {
				continue
			}
			seenModels[key] = true

			name := model.Name
			if name == "" {
				name = model.ID
			}
			options = append(options, ConfigOptionVariant{
				Value:       key,
				Name:        name,
				Description: provider.Name,
			})
		}
	}

	// Determine current models.
	currentLarge := cfg.Config().Models[config.SelectedModelTypeLarge]
	currentValue := ""
	if currentLarge.Provider != "" && currentLarge.Model != "" {
		currentValue = currentLarge.Provider + ":" + currentLarge.Model
	}

	currentSmall := cfg.Config().Models[config.SelectedModelTypeSmall]
	currentSmallValue := ""
	if currentSmall.Provider != "" && currentSmall.Model != "" {
		currentSmallValue = currentSmall.Provider + ":" + currentSmall.Model
	}

	if len(options) == 0 {
		return nil
	}

	return []ConfigOption{
		{
			ID:           "model_large",
			Name:         "Large Model",
			Category:     "model",
			Type:         "select",
			CurrentValue: currentValue,
			Options:      options,
		},
		{
			ID:           "model_small",
			Name:         "Small Model",
			Category:     "model",
			Type:         "select",
			CurrentValue: currentSmallValue,
			Options:      options,
		},
		{
			ID:           "mode",
			Name:         "Permission Mode",
			Category:     "mode",
			Type:         "select",
			CurrentValue: h.currentModeForSession(sessionID),
			Options: []ConfigOptionVariant{
				{Value: "default", Name: "Default", Description: "Ask for permission when required"},
				{Value: "auto", Name: "Auto", Description: "Guarded autonomy with manual fallback"},
				{Value: "yolo", Name: "YOLO", Description: "Auto-approve tool permissions in this session"},
			},
		},
	}
}

// handleSessionList lists existing sessions.
func (h *Handler) handleSessionList(ctx context.Context, req *Request) (any, *RPCError) {
	var params SessionListParams
	if req.Params != nil {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()}
		}
	}

	sessions, err := h.app.GetSessions().List(ctx)
	if err != nil {
		return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("failed to list sessions: %v", err)}
	}

	entries := make([]SessionListEntry, 0, len(sessions))
	for _, s := range sessions {
		// Skip task/sub-agent sessions (those with a parent).
		if s.ParentSessionID != "" {
			continue
		}
		entry := SessionListEntry{
			SessionID: s.ID,
			CWD:       h.sessionCWDForSession(s, params.CWD),
			Title:     s.Title,
		}
		if s.UpdatedAt != 0 {
			entry.UpdatedAt = time.Unix(s.UpdatedAt, 0).UTC().Format(time.RFC3339)
		}
		entries = append(entries, entry)
	}

	return SessionListResult{Sessions: entries}, nil
}

// handleSetConfigOption handles the session/set_config_option request.
func (h *Handler) handleSetConfigOption(ctx context.Context, req *Request) (any, *RPCError) {
	var params SetConfigOptionParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()}
	}

	if params.ConfigID != "model_large" && params.ConfigID != "model_small" && params.ConfigID != "mode" {
		return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("unknown config option: %s", params.ConfigID)}
	}

	cfg := h.app.GetConfig()
	if cfg == nil {
		return nil, &RPCError{Code: CodeInternalError, Message: "config not available"}
	}

	if params.ConfigID == "mode" {
		if params.Value != "default" && params.Value != "auto" && params.Value != "yolo" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid mode option value: %s", params.Value)}
		}
		transition, rpcErr := h.applyPermissionMode(ctx, params.SessionID, params.Value)
		if rpcErr != nil {
			return nil, rpcErr
		}

		updated := h.buildConfigOptions(params.SessionID)
		h.sendUpdate(params.SessionID, SessionUpdate{
			SessionUpdate: SessionUpdateConfigOptionUpdate,
			ConfigOptions: updated,
		})
		h.sendUpdate(params.SessionID, SessionUpdate{
			SessionUpdate: SessionUpdateCurrentModeUpdate,
			CurrentModeID: transition.Current.CurrentModeID(),
		})
		return SetConfigOptionResult{ConfigOptions: updated}, nil
	}

	// Validate requested value against advertised model options.
	valid := false
	for _, opt := range h.buildConfigOptions(params.SessionID) {
		if opt.ID != params.ConfigID {
			continue
		}
		for _, candidate := range opt.Options {
			if candidate.Value == params.Value {
				valid = true
				break
			}
		}
		break
	}
	if !valid {
		return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid model option value: %s", params.Value)}
	}

	// Parse the value (format: "provider:model").
	parts := strings.SplitN(params.Value, ":", 2)
	if len(parts) != 2 {
		return nil, &RPCError{Code: CodeInvalidParams, Message: "invalid model value format, expected 'provider:model'"}
	}
	providerID, modelID := parts[0], parts[1]
	selectedModel := config.SelectedModel{Provider: providerID, Model: modelID}

	var modelType config.SelectedModelType
	if params.ConfigID == "model_large" {
		modelType = config.SelectedModelTypeLarge
	} else {
		modelType = config.SelectedModelTypeSmall
	}

	if err := h.app.GetCoordinator().PrepareModelSwitch(ctx, params.SessionID, modelType, selectedModel); err != nil {
		return nil, &RPCError{Code: CodeInternalError, Message: err.Error()}
	}

	if params.ConfigID == "model_large" {
		largeCurrent := cfg.Config().Models[config.SelectedModelTypeLarge]
		newLarge := config.SelectedModel{
			Provider:         selectedModel.Provider,
			Model:            selectedModel.Model,
			MaxTokens:        largeCurrent.MaxTokens,
			Temperature:      largeCurrent.Temperature,
			TopP:             largeCurrent.TopP,
			TopK:             largeCurrent.TopK,
			FrequencyPenalty: largeCurrent.FrequencyPenalty,
			PresencePenalty:  largeCurrent.PresencePenalty,
		}
		if err := cfg.UpdatePreferredModel(config.ScopeWorkspace, config.SelectedModelTypeLarge, newLarge); err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("failed to update large model: %v", err)}
		}
	} else {
		smallCurrent := cfg.Config().Models[config.SelectedModelTypeSmall]
		newSmall := config.SelectedModel{
			Provider:         selectedModel.Provider,
			Model:            selectedModel.Model,
			MaxTokens:        smallCurrent.MaxTokens,
			Temperature:      smallCurrent.Temperature,
			TopP:             smallCurrent.TopP,
			TopK:             smallCurrent.TopK,
			FrequencyPenalty: smallCurrent.FrequencyPenalty,
			PresencePenalty:  smallCurrent.PresencePenalty,
		}
		if err := cfg.UpdatePreferredModel(config.ScopeWorkspace, config.SelectedModelTypeSmall, newSmall); err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("failed to update small model: %v", err)}
		}
	}

	// Refresh the agent's model.
	if err := h.app.GetCoordinator().UpdateModels(ctx); err != nil {
		return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("failed to refresh agent models: %v", err)}
	}

	updatedConfigOptions := h.buildConfigOptions(params.SessionID)
	// Also notify the client in case its UI only listens to session/update.
	h.sendUpdate(params.SessionID, SessionUpdate{
		SessionUpdate: SessionUpdateConfigOptionUpdate,
		ConfigOptions: updatedConfigOptions,
	})

	return SetConfigOptionResult{
		ConfigOptions: updatedConfigOptions,
	}, nil
}

// Compile-time check that pubsub.Event[message.Message] is the event type used
// in the message subscription loop.
var _ pubsub.Event[message.Message]

func (h *Handler) getSubagentPrefix(ctx context.Context, sessionID, parentSessionID string, cache map[string]string) string {
	if sessionID == "" || sessionID == parentSessionID {
		return ""
	}
	if cache != nil {
		if prefix, ok := cache[sessionID]; ok {
			return prefix
		}
	}
	sess, err := h.app.GetSessions().Get(ctx, sessionID)
	if err != nil {
		return ""
	}
	var prefix string
	// Extract subagent name from title like "Explore task (@explore subagent)"
	if idx := strings.Index(sess.Title, "(@"); idx != -1 {
		sub := sess.Title[idx+2:]
		if endIdx := strings.Index(sub, " "); endIdx != -1 {
			prefix = "[" + sub[:endIdx] + "] "
		}
	}
	if prefix == "" {
		// Fallback to title.
		title := sess.Title
		if len(title) > 15 {
			title = title[:12] + "..."
		}
		prefix = "[" + title + "] "
	}
	if cache != nil {
		cache[sessionID] = prefix
	}
	return prefix
}

func (h *Handler) getToolLocations(toolName string, params any) []Location {
	if params == nil {
		return nil
	}
	path := extractParam(params, "TargetFile", "file_path", "filePath", "filepath", "path", "src", "Source", "SearchPath", "search_path")
	if path != "" {
		return []Location{{Path: path}}
	}
	return nil
}

// getToolDiffContent builds a ToolCallContent[] array for edit/write tool
// results. The first entry is always the text output wrapped as a "content"
// item; when the tool produced a diff, a "diff" entry is appended.
func (h *Handler) getToolDiffContent(toolName string, params any, toolOutput string) []any {
	if params == nil {
		return nil
	}

	path := extractParam(params, "file_path", "path")
	if path == "" {
		return nil
	}

	blocks := []any{ContentItem(TextBlock(toolOutput))}

	if toolName == "edit" {
		oldText := extractParam(params, "old_string")
		newText := extractParam(params, "new_string")
		if oldText != "" || newText != "" {
			blocks = append(blocks, DiffBlock{
				Type:    "diff",
				Path:    path,
				OldText: oldText,
				NewText: newText,
			})
			return blocks
		}
	}

	if toolName == "write" {
		newText := extractParam(params, "content")
		if newText != "" {
			blocks = append(blocks, DiffBlock{
				Type:    "diff",
				Path:    path,
				OldText: "",
				NewText: newText,
			})
			return blocks
		}
	}

	return nil
}

func cleanParentToolCallID(sessionID string) string {
	// Only strip the prefix before "$$" if present (e.g. "messageID$$toolCallID")
	// We MUST preserve the "::" task suffix because the coordinator publishes
	// the delegation ToolCall in the parent session with the "::" suffix
	// to distinguish multiple parallel subagents in the IDE UI.
	if idx := strings.Index(sessionID, "$$"); idx != -1 {
		return sessionID[idx+2:]
	}
	return sessionID
}
