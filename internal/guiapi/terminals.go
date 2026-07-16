package guiapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/charmbracelet/crush/internal/acp"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/guimetrics"
	"github.com/charmbracelet/crush/internal/idempotency"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/terminal"
)

const (
	errorTerminalNotFound = "CRUSH_TERMINAL_NOT_FOUND"
	errorPermissionDenied = "CRUSH_PERMISSION_DENIED"
	errorTerminalFailed   = "CRUSH_TERMINAL_FAILED"
	maxTerminalInputBytes = 1024 * 1024
)

type terminalOpenParams struct {
	SessionID       string            `json:"sessionId"`
	Command         string            `json:"command"`
	Args            []string          `json:"args,omitempty"`
	CWD             string            `json:"cwd,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	Cols            int               `json:"cols,omitempty"`
	Rows            int               `json:"rows,omitempty"`
	ClientRequestID string            `json:"clientRequestId"`
}

type terminalInputParams struct {
	SessionID       string `json:"sessionId"`
	TerminalID      string `json:"terminalId"`
	Text            string `json:"text,omitempty"`
	Bytes           string `json:"bytes,omitempty"`
	ClientRequestID string `json:"clientRequestId"`
}

type terminalResizeParams struct {
	SessionID       string `json:"sessionId"`
	TerminalID      string `json:"terminalId"`
	Cols            int    `json:"cols"`
	Rows            int    `json:"rows"`
	ClientRequestID string `json:"clientRequestId"`
}

type terminalKillParams struct {
	SessionID       string `json:"sessionId"`
	TerminalID      string `json:"terminalId"`
	Signal          string `json:"signal,omitempty"`
	ClientRequestID string `json:"clientRequestId"`
}

type terminalSnapshotParams struct {
	SessionID   string `json:"sessionId"`
	TerminalID  string `json:"terminalId"`
	AfterOffset uint64 `json:"afterOffset,omitempty"`
}

type terminalResult struct {
	TerminalID string `json:"terminalId"`
	SessionID  string `json:"sessionId"`
	State      string `json:"state"`
	Cols       int    `json:"cols"`
	Rows       int    `json:"rows"`
	Offset     uint64 `json:"offset"`
	CreatedAt  int64  `json:"createdAt"`
	ExitedAt   int64  `json:"exitedAt,omitempty"`
	ExitCode   *int   `json:"exitCode,omitempty"`
	Signal     string `json:"signal,omitempty"`
}

type terminalSnapshotResult struct {
	terminalResult
	StartOffset uint64 `json:"startOffset"`
	EndOffset   uint64 `json:"endOffset"`
	Truncated   bool   `json:"truncated"`
	More        bool   `json:"more"`
	Data        []byte `json:"data"`
}

type terminalInputResult struct {
	TerminalID string `json:"terminalId"`
	Written    int    `json:"written"`
}

type terminalKillResult struct {
	Acknowledged bool           `json:"acknowledged"`
	Terminal     terminalResult `json:"terminal"`
}

func (s *Service) SetTerminalServices(manager *terminal.Manager, permissions permission.Service) {
	s.mu.Lock()
	s.terminals = manager
	s.permissions = permissions
	s.mu.Unlock()
}

func (s *Service) registerTerminalHandlers() {
	s.routes["crush/terminal/open"] = route{feature: FeatureTerminal, handler: s.handleTerminalOpen}
	s.routes["crush/terminal/input"] = route{feature: FeatureTerminal, handler: s.handleTerminalInput}
	s.routes["crush/terminal/resize"] = route{feature: FeatureTerminal, handler: s.handleTerminalResize}
	s.routes["crush/terminal/kill"] = route{feature: FeatureTerminal, handler: s.handleTerminalKill}
	s.routes["crush/terminal/snapshot"] = route{feature: FeatureTerminal, handler: s.handleTerminalSnapshot}
}

func (s *Service) handleTerminalOpen(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params terminalOpenParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParams(err)
	}
	if params.SessionID == "" || params.Command == "" || params.ClientRequestID == "" {
		return nil, invalidParams(errors.New("sessionId, command, and clientRequestId are required"))
	}
	return s.executeTerminalMutation(ctx, "crush/terminal/open", params.SessionID, params.ClientRequestID, params, func() (any, *acp.RPCError) {
		s.mu.RLock()
		sessions := s.sessions
		s.mu.RUnlock()
		if sessions == nil {
			return nil, sourceUnavailable("session source is unavailable")
		}
		sess, err := sessions.Get(context.WithoutCancel(ctx), params.SessionID)
		if err != nil {
			return nil, sessionSourceError(params.SessionID, err)
		}
		cwd := params.CWD
		if cwd == "" {
			cwd = sess.WorkspaceCWD
		}
		request := terminal.OpenRequest{
			ClientID: s.blobOwnerID(), SessionID: params.SessionID, Command: params.Command,
			Args: params.Args, CWD: cwd, Env: params.Env, Cols: params.Cols, Rows: params.Rows,
		}
		if err := terminal.ValidateOpenRequest(&request); err != nil {
			return nil, terminalError(err)
		}
		if rpcErr := s.requestTerminalPermission(ctx, params.SessionID, params.ClientRequestID, request.CWD,
			fmt.Sprintf("Execute command: %s", params.Command), terminalBashPermissionParams(request.Command, request.Args, request.CWD)); rpcErr != nil {
			return nil, rpcErr
		}
		manager := s.terminalManager()
		if manager == nil {
			return nil, sourceUnavailable("terminal service is unavailable")
		}
		metadata, err := manager.Open(context.WithoutCancel(ctx), request)
		s.recordTerminalRetained(ctx)
		if err != nil {
			return nil, terminalError(err)
		}
		return projectTerminal(metadata), nil
	})
}

func (s *Service) handleTerminalInput(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params terminalInputParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParams(err)
	}
	if params.SessionID == "" || params.TerminalID == "" || params.ClientRequestID == "" || params.Text != "" && params.Bytes != "" {
		return nil, invalidParams(errors.New("sessionId, terminalId, clientRequestId, and exactly one input encoding are required"))
	}
	data := []byte(params.Text)
	if params.Bytes != "" {
		if base64.StdEncoding.DecodedLen(len(params.Bytes)) > maxTerminalInputBytes {
			return nil, protocolError(-32036, errorPayloadTooLarge, nil)
		}
		var err error
		data, err = base64.StdEncoding.DecodeString(params.Bytes)
		if err != nil {
			return nil, invalidParams(errors.New("bytes must be base64"))
		}
	}
	if len(data) == 0 {
		return nil, invalidParams(errors.New("terminal input is empty"))
	}
	if len(data) > maxTerminalInputBytes {
		return nil, protocolError(-32036, errorPayloadTooLarge, nil)
	}
	defer clear(data)
	return s.executeTerminalMutation(ctx, "crush/terminal/input", params.SessionID, params.ClientRequestID, params, func() (any, *acp.RPCError) {
		_, rpcErr := s.authorizeTerminalOperation(ctx, params.SessionID, params.TerminalID, params.ClientRequestID, "Send input to terminal")
		if rpcErr != nil {
			return nil, rpcErr
		}
		manager := s.terminalManager()
		written, err := manager.Input(context.WithoutCancel(ctx), s.blobOwnerID(), params.SessionID, params.TerminalID, data)
		s.recordTerminalRetained(ctx)
		if err != nil {
			return nil, terminalError(err)
		}
		return terminalInputResult{TerminalID: params.TerminalID, Written: written}, nil
	})
}

func (s *Service) handleTerminalResize(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params terminalResizeParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParams(err)
	}
	if params.TerminalID == "" {
		return nil, invalidParams(errors.New("terminalId is required"))
	}
	return s.executeTerminalMutation(ctx, "crush/terminal/resize", params.SessionID, params.ClientRequestID, params, func() (any, *acp.RPCError) {
		manager := s.terminalManager()
		if manager == nil {
			return nil, sourceUnavailable("terminal service is unavailable")
		}
		metadata, err := manager.Resize(s.blobOwnerID(), params.SessionID, params.TerminalID, params.Cols, params.Rows)
		if err != nil {
			return nil, terminalError(err)
		}
		return projectTerminal(metadata), nil
	})
}

func (s *Service) handleTerminalKill(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params terminalKillParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParams(err)
	}
	if params.TerminalID == "" {
		return nil, invalidParams(errors.New("terminalId is required"))
	}
	return s.executeTerminalMutation(ctx, "crush/terminal/kill", params.SessionID, params.ClientRequestID, params, func() (any, *acp.RPCError) {
		metadata, rpcErr := s.authorizeTerminalOperation(ctx, params.SessionID, params.TerminalID, params.ClientRequestID, "Terminate terminal")
		if rpcErr != nil {
			return nil, rpcErr
		}
		manager := s.terminalManager()
		if err := manager.Kill(s.blobOwnerID(), params.SessionID, params.TerminalID, params.Signal); err != nil {
			return nil, terminalError(err)
		}
		return terminalKillResult{Acknowledged: true, Terminal: projectTerminal(metadata)}, nil
	})
}

func (s *Service) handleTerminalSnapshot(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params terminalSnapshotParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParams(err)
	}
	if params.SessionID == "" || params.TerminalID == "" {
		return nil, invalidParams(errors.New("sessionId and terminalId are required"))
	}
	manager := s.terminalManager()
	if manager == nil {
		return nil, sourceUnavailable("terminal service is unavailable")
	}
	value, err := manager.Snapshot(s.blobOwnerID(), params.SessionID, params.TerminalID, params.AfterOffset)
	s.recordTerminalRetained(ctx)
	if err != nil {
		return nil, terminalError(err)
	}
	return terminalSnapshotResult{
		terminalResult: projectTerminal(value.Metadata), StartOffset: value.StartOffset,
		EndOffset: value.EndOffset, Truncated: value.Truncated, More: value.More, Data: value.Data,
	}, nil
}

func (s *Service) authorizeTerminalOperation(ctx context.Context, sessionID, terminalID, requestID, description string) (terminal.Metadata, *acp.RPCError) {
	manager := s.terminalManager()
	if manager == nil {
		return terminal.Metadata{}, sourceUnavailable("terminal service is unavailable")
	}
	metadata, err := manager.Get(s.blobOwnerID(), sessionID, terminalID)
	if err != nil {
		return terminal.Metadata{}, terminalError(err)
	}
	if rpcErr := s.requestTerminalPermission(ctx, sessionID, requestID, metadata.CWD, description, terminalBashPermissionParams(metadata.Command, metadata.Args, metadata.CWD)); rpcErr != nil {
		return terminal.Metadata{}, rpcErr
	}
	return metadata, nil
}

func (s *Service) requestTerminalPermission(ctx context.Context, sessionID, requestID, cwd, description string, params any) *acp.RPCError {
	s.mu.RLock()
	permissions := s.permissions
	s.mu.RUnlock()
	if permissions == nil {
		return sourceUnavailable("permission service is unavailable")
	}
	granted, err := permissions.Request(ctx, permission.CreatePermissionRequest{
		SessionID: sessionID, AuthoritySessionID: sessionID, ToolCallID: requestID,
		ToolName: "bash", Action: "execute", Description: description, Path: cwd, Params: params,
	})
	if err != nil || !granted {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return &acp.RPCError{Code: -32031, Message: errorDeadlineExceeded, Data: ErrorData{Code: errorDeadlineExceeded, Retryable: true}}
		}
		return protocolError(-32041, errorPermissionDenied, nil)
	}
	return nil
}

func (s *Service) executeTerminalMutation(ctx context.Context, method, sessionID, requestID string, payload any, fn func() (any, *acp.RPCError)) (any, *acp.RPCError) {
	if sessionID == "" || requestID == "" {
		return nil, invalidParams(errors.New("sessionId and clientRequestId are required"))
	}
	s.mu.RLock()
	store := s.terminalReplay
	s.mu.RUnlock()
	if store == nil {
		return nil, sourceUnavailable("terminal idempotency service is unavailable")
	}
	outcome, err := store.Execute(ctx, method+"\x00"+sessionID, requestID, payload, func() idempotency.Outcome {
		value, rpcErr := fn()
		return idempotency.Outcome{Value: value, Failure: rpcErr}
	})
	if err != nil {
		if errors.Is(err, idempotency.ErrConflict) {
			return nil, protocolError(-32030, errorIdempotencyConflict, map[string]any{"clientRequestId": requestID})
		}
		if errors.Is(err, idempotency.ErrCapacity) {
			return nil, &acp.RPCError{Code: -32032, Message: errorSessionBusy, Data: ErrorData{Code: errorSessionBusy, Retryable: true}}
		}
		return nil, invalidParams(errors.New("clientRequestId must be a UUID"))
	}
	if rpcErr, _ := outcome.Failure.(*acp.RPCError); rpcErr != nil {
		return nil, rpcErr
	}
	return outcome.Value, nil
}

func (s *Service) terminalManager() *terminal.Manager {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.terminals
}

func projectTerminal(value terminal.Metadata) terminalResult {
	result := terminalResult{
		TerminalID: value.ID, SessionID: value.SessionID, State: string(value.State),
		Cols: value.Cols, Rows: value.Rows, Offset: value.Offset, CreatedAt: value.CreatedAt.UnixMilli(),
		Signal: value.Signal,
	}
	if value.HasExit {
		exitCode := value.ExitCode
		result.ExitCode = &exitCode
		result.ExitedAt = value.ExitedAt.UnixMilli()
	}
	return result
}

func terminalError(err error) *acp.RPCError {
	switch {
	case errors.Is(err, terminal.ErrNotFound), errors.Is(err, terminal.ErrOwnerMismatch):
		return protocolError(-32042, errorTerminalNotFound, nil)
	case errors.Is(err, terminal.ErrInvalidInput), errors.Is(err, terminal.ErrInvalidOffset):
		return invalidParams(errors.New("invalid terminal request"))
	case errors.Is(err, terminal.ErrCapacity):
		return &acp.RPCError{Code: -32032, Message: errorSessionBusy, Data: ErrorData{Code: errorSessionBusy, Retryable: true}}
	case errors.Is(err, terminal.ErrNotRunning):
		return protocolError(-32043, errorTerminalFailed, map[string]any{"reason": "not_running"})
	default:
		return &acp.RPCError{Code: acp.CodeInternalError, Message: errorTerminalFailed, Data: ErrorData{Code: errorTerminalFailed, Retryable: true}}
	}
}

func terminalBashPermissionParams(command string, args []string, cwd string) tools.BashPermissionsParams {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, quotePermissionArg(command))
	for _, arg := range args {
		parts = append(parts, quotePermissionArg(arg))
	}
	return tools.BashPermissionsParams{
		Description: "Interactive terminal", Command: strings.Join(parts, " "),
		WorkingDir: cwd, RunInBackground: true,
	}
}

func quotePermissionArg(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func (s *Service) recordTerminalRetained(ctx context.Context) {
	manager := s.terminalManager()
	if manager == nil {
		return
	}
	guimetrics.FromContext(ctx).SetGauge(guimetrics.TerminalRetainedBytes, manager.RetainedBytes(), guimetrics.Labels{Kind: "terminal"})
}
