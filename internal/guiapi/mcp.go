package guiapi

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/charmbracelet/crush/internal/acp"
	"github.com/charmbracelet/crush/internal/idempotency"
	"github.com/charmbracelet/crush/internal/mcplifecycle"
)

const maxMCPServers = 256

type mcpSessionParams struct {
	SessionID string `json:"sessionId"`
}

type mcpServerParams struct {
	SessionID string `json:"sessionId"`
	ServerID  string `json:"serverId"`
}

type mcpMutationParams struct {
	SessionID       string `json:"sessionId"`
	ServerID        string `json:"serverId"`
	ClientRequestID string `json:"clientRequestId"`
}

type mcpLogsParams struct {
	SessionID     string `json:"sessionId"`
	ServerID      string `json:"serverId"`
	AfterSequence uint64 `json:"afterSequence,omitempty"`
	Limit         int    `json:"limit,omitempty"`
}

type mcpListResult struct {
	Revision uint64            `json:"revision"`
	Servers  []mcpServerResult `json:"servers"`
}

type mcpServerResult struct {
	ServerID  string `json:"serverId"`
	Name      string `json:"name"`
	Scope     string `json:"scope"`
	Status    string `json:"status"`
	Tools     int    `json:"tools"`
	Prompts   int    `json:"prompts"`
	Resources int    `json:"resources"`
	Revision  uint64 `json:"revision"`
	ErrorCode string `json:"errorCode,omitempty"`
	UpdatedAt int64  `json:"updatedAt"`
}

type mcpLogsResult struct {
	Entries        []mcpLogResult `json:"entries"`
	LatestSequence uint64         `json:"latestSequence"`
	Truncated      bool           `json:"truncated"`
}

type mcpLogResult struct {
	Sequence  uint64 `json:"sequence"`
	Timestamp int64  `json:"timestamp"`
	Level     string `json:"level"`
	Logger    string `json:"logger,omitempty"`
	Message   string `json:"message"`
}

func (s *Service) registerMCPHandlers() {
	s.routes["crush/mcp/list"] = route{feature: FeatureMCPControl, handler: s.handleMCPList}
	s.routes["crush/mcp/status"] = route{feature: FeatureMCPControl, handler: s.handleMCPStatus}
	s.routes["crush/mcp/reconnect"] = route{feature: FeatureMCPControl, handler: s.handleMCPReconnect}
	s.routes["crush/mcp/disable"] = route{feature: FeatureMCPControl, handler: s.handleMCPDisable}
	s.routes["crush/mcp/logs"] = route{feature: FeatureMCPControl, handler: s.handleMCPLogs}
}

func (s *Service) handleMCPList(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params mcpSessionParams
	if err := json.Unmarshal(raw, &params); err != nil || params.SessionID == "" {
		return nil, invalidParams(errors.New("sessionId is required"))
	}
	service, rpcErr := s.mcpService(ctx, params.SessionID)
	if rpcErr != nil {
		return nil, rpcErr
	}
	servers, revision, err := service.List(params.SessionID)
	if err != nil {
		return nil, mcpError(err)
	}
	if len(servers) > maxMCPServers {
		return nil, mcpError(mcplifecycle.ErrCapacity)
	}
	result := make([]mcpServerResult, len(servers))
	for i := range servers {
		result[i] = projectMCPServer(servers[i])
	}
	return mcpListResult{Revision: revision, Servers: result}, nil
}

func (s *Service) handleMCPStatus(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params mcpServerParams
	if err := json.Unmarshal(raw, &params); err != nil || params.SessionID == "" || params.ServerID == "" {
		return nil, invalidParams(errors.New("sessionId and serverId are required"))
	}
	service, rpcErr := s.mcpService(ctx, params.SessionID)
	if rpcErr != nil {
		return nil, rpcErr
	}
	server, err := service.Status(params.SessionID, params.ServerID)
	if err != nil {
		return nil, mcpError(err)
	}
	return projectMCPServer(server), nil
}

func (s *Service) handleMCPReconnect(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params mcpMutationParams
	if err := json.Unmarshal(raw, &params); err != nil || params.SessionID == "" || params.ServerID == "" || params.ClientRequestID == "" {
		return nil, invalidParams(errors.New("sessionId, serverId and clientRequestId are required"))
	}
	return s.executeMCPMutation(ctx, "mcp/reconnect", params, func(service *mcplifecycle.Service) (any, *acp.RPCError) {
		server, err := service.ReconnectAsync(params.SessionID, params.ServerID)
		if err != nil {
			return nil, mcpError(err)
		}
		return projectMCPServer(server), nil
	})
}

func (s *Service) handleMCPDisable(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params mcpMutationParams
	if err := json.Unmarshal(raw, &params); err != nil || params.SessionID == "" || params.ServerID == "" || params.ClientRequestID == "" {
		return nil, invalidParams(errors.New("sessionId, serverId and clientRequestId are required"))
	}
	return s.executeMCPMutation(ctx, "mcp/disable", params, func(service *mcplifecycle.Service) (any, *acp.RPCError) {
		server, err := service.DisableAsync(params.SessionID, params.ServerID)
		if err != nil {
			return nil, mcpError(err)
		}
		return projectMCPServer(server), nil
	})
}

func (s *Service) handleMCPLogs(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params mcpLogsParams
	if err := json.Unmarshal(raw, &params); err != nil || params.SessionID == "" || params.ServerID == "" {
		return nil, invalidParams(errors.New("sessionId and serverId are required"))
	}
	if params.Limit < 0 || params.Limit > 1000 {
		return nil, invalidParams(errors.New("limit must be between 0 and 1000"))
	}
	service, rpcErr := s.mcpService(ctx, params.SessionID)
	if rpcErr != nil {
		return nil, rpcErr
	}
	page, err := service.Logs(params.SessionID, params.ServerID, params.AfterSequence, params.Limit)
	if err != nil {
		return nil, mcpError(err)
	}
	entries := make([]mcpLogResult, len(page.Entries))
	for i, entry := range page.Entries {
		entries[i] = mcpLogResult{
			Sequence: entry.Sequence, Timestamp: entry.Timestamp.UnixMilli(),
			Level: entry.Level, Logger: entry.Logger, Message: entry.Message,
		}
	}
	return mcpLogsResult{Entries: entries, LatestSequence: page.LatestSequence, Truncated: page.Truncated}, nil
}

func (s *Service) executeMCPMutation(
	ctx context.Context,
	method string,
	params mcpMutationParams,
	fn func(*mcplifecycle.Service) (any, *acp.RPCError),
) (any, *acp.RPCError) {
	service, rpcErr := s.mcpService(ctx, params.SessionID)
	if rpcErr != nil {
		return nil, rpcErr
	}
	s.mu.RLock()
	replay := s.mcpReplay
	s.mu.RUnlock()
	if replay == nil {
		return nil, sourceUnavailable("MCP control service is unavailable")
	}
	outcome, err := replay.Execute(ctx, method+"\x00"+params.SessionID, params.ClientRequestID, params, func() idempotency.Outcome {
		value, failure := fn(service)
		return idempotency.Outcome{Value: value, Failure: failure}
	})
	if err != nil {
		if errors.Is(err, idempotency.ErrConflict) {
			return nil, protocolError(-32030, errorIdempotencyConflict, map[string]any{"clientRequestId": params.ClientRequestID})
		}
		if errors.Is(err, idempotency.ErrCapacity) {
			return nil, mcpError(mcplifecycle.ErrCapacity)
		}
		return nil, invalidParams(errors.New("clientRequestId must be a UUID"))
	}
	if failure, _ := outcome.Failure.(*acp.RPCError); failure != nil {
		return nil, failure
	}
	return outcome.Value, nil
}

func (s *Service) mcpService(ctx context.Context, sessionID string) (*mcplifecycle.Service, *acp.RPCError) {
	s.mu.RLock()
	service, sessions, closed := s.mcpLifecycle, s.sessions, s.closed
	s.mu.RUnlock()
	if closed || service == nil {
		return nil, sourceUnavailable("MCP control service is unavailable")
	}
	if sessions != nil {
		if _, err := sessions.Get(ctx, sessionID); err != nil {
			return nil, sessionSourceError(sessionID, err)
		}
	}
	return service, nil
}

func projectMCPServer(server mcplifecycle.Server) mcpServerResult {
	updatedAt := server.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Unix(0, 0)
	}
	return mcpServerResult{
		ServerID: server.ID, Name: server.Name, Scope: string(server.Scope), Status: string(server.Status),
		Tools: server.Counts.Tools, Prompts: server.Counts.Prompts, Resources: server.Counts.Resources,
		Revision: server.Revision, ErrorCode: server.ErrorCode, UpdatedAt: updatedAt.UnixMilli(),
	}
}

func mcpError(err error) *acp.RPCError {
	switch {
	case errors.Is(err, mcplifecycle.ErrNotFound):
		return protocolError(-32060, "CRUSH_MCP_NOT_FOUND", nil)
	case errors.Is(err, mcplifecycle.ErrCapacity):
		return &acp.RPCError{Code: -32061, Message: "CRUSH_MCP_CAPACITY", Data: ErrorData{Code: "CRUSH_MCP_CAPACITY", Retryable: true}}
	default:
		return &acp.RPCError{Code: -32062, Message: "CRUSH_MCP_CONTROL_FAILED", Data: ErrorData{Code: "CRUSH_MCP_CONTROL_FAILED", Retryable: true}}
	}
}
