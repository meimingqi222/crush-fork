package guiapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/charmbracelet/crush/internal/acp"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/guimetrics"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/sessionevent"
	"github.com/charmbracelet/crush/internal/terminal"
)

const (
	errorSessionNotFound = "CRUSH_SESSION_NOT_FOUND"
	errorSnapshotFailed  = "CRUSH_SNAPSHOT_FAILED"
)

// SnapshotSource builds a bounded session projection.
type SnapshotSource interface {
	Snapshot(context.Context, string) (sessionevent.Snapshot, error)
}

type snapshotParams struct {
	SessionID string `json:"sessionId"`
}

// CoordinatorSnapshotSource adapts current Agent runtime state without making
// the sessionevent package depend on the Agent implementation.
type CoordinatorSnapshotSource struct {
	coordinator coordinatorSnapshotReader
	terminals   interface {
		ListSession(string) []terminal.Metadata
	}
	mcp interface {
		SnapshotResources(string) []sessionevent.ResourceSummary
	}
}

func (s *CoordinatorSnapshotSource) SetMCPSource(source interface {
	SnapshotResources(string) []sessionevent.ResourceSummary
},
) {
	s.mcp = source
}

func (s *CoordinatorSnapshotSource) SetTerminalSource(source interface {
	ListSession(string) []terminal.Metadata
},
) {
	s.terminals = source
}

type coordinatorSnapshotReader interface {
	IsSessionBusy(string) bool
	QueuedPrompts(string) int
	IsQueuePaused(string) bool
	ModelForSession(string) (agent.Model, bool)
}

func NewCoordinatorSnapshotSource(coordinator coordinatorSnapshotReader) *CoordinatorSnapshotSource {
	return &CoordinatorSnapshotSource{coordinator: coordinator}
}

func (s *CoordinatorSnapshotSource) SnapshotRuntime(sessionID string) sessionevent.RuntimeSnapshot {
	if s == nil || s.coordinator == nil {
		return sessionevent.RuntimeSnapshot{}
	}
	projection := sessionevent.RuntimeSnapshot{
		Busy:        s.coordinator.IsSessionBusy(sessionID),
		QueueCount:  s.coordinator.QueuedPrompts(sessionID),
		QueuePaused: s.coordinator.IsQueuePaused(sessionID),
	}
	if model, ok := s.coordinator.ModelForSession(sessionID); ok {
		projection.Model = model.ModelCfg.Model
		projection.Provider = model.ModelCfg.Provider
	}
	if resolver, ok := s.coordinator.(interface {
		EffectiveInference(context.Context, string) (session.EffectiveInference, error)
	}); ok {
		if effective, err := resolver.EffectiveInference(context.Background(), sessionID); err == nil {
			projection.Inference = effective
			projection.Model = effective.Model
			projection.Provider = effective.Provider
		}
	}
	if s.terminals != nil {
		for _, item := range s.terminals.ListSession(sessionID) {
			projection.Terminals = append(projection.Terminals, sessionevent.ResourceSummary{
				ID: item.ID, Status: string(item.State),
			})
		}
	}
	if s.mcp != nil {
		projection.MCPServers = s.mcp.SnapshotResources(sessionID)
	}
	return projection
}

func (s *Service) registerSnapshotHandler() {
	s.routes["crush/session/snapshot"] = route{feature: FeatureSessionSync, handler: s.handleSnapshot}
}

func (s *Service) handleSnapshot(ctx context.Context, raw json.RawMessage) (result any, rpcErr *acp.RPCError) {
	var params snapshotParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParams(err)
	}
	if params.SessionID == "" {
		return nil, invalidParams(errors.New("sessionId is required"))
	}
	snapshot, rpcErr := s.buildSnapshot(ctx, params.SessionID)
	if rpcErr != nil {
		return nil, rpcErr
	}
	return snapshot, nil
}

func (s *Service) buildSnapshot(ctx context.Context, sessionID string) (sessionevent.Snapshot, *acp.RPCError) {
	s.mu.RLock()
	source := s.snapshots
	s.mu.RUnlock()
	if source == nil {
		return sessionevent.Snapshot{}, &acp.RPCError{Code: acp.CodeInternalError, Message: "session snapshot service is unavailable"}
	}
	snapshot, err := source.Snapshot(ctx, sessionID)
	outcome := "success"
	if err != nil {
		outcome = "error"
	}
	guimetrics.FromContext(ctx).Add(guimetrics.GUISnapshotTotal, 1, guimetrics.Labels{Outcome: outcome})
	if err == nil {
		return snapshot, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return sessionevent.Snapshot{}, protocolError(-32021, errorSessionNotFound, map[string]any{"sessionId": sessionID})
	}
	return sessionevent.Snapshot{}, &acp.RPCError{
		Code:    acp.CodeInternalError,
		Message: errorSnapshotFailed,
		Data: ErrorData{
			Code:      errorSnapshotFailed,
			Retryable: true,
		},
	}
}
