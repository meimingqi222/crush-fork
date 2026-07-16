package guiapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/crush/internal/acp"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/sessionevent"
)

const (
	maxSessionTitleBytes       = 256
	errorForkBoundaryInvalid   = "CRUSH_FORK_BOUNDARY_INVALID"
	errorSessionMutationFailed = "CRUSH_SESSION_MUTATION_FAILED"
)

type SessionMutationService interface {
	Get(context.Context, string) (session.Session, error)
	Rename(context.Context, string, string) error
	SetArchived(context.Context, string, bool) (session.Session, error)
	SetPinned(context.Context, string, bool) (session.Session, error)
	Delete(context.Context, string) error
	Fork(context.Context, string, string) (session.Session, error)
	UpdateInference(context.Context, string, uint64, session.InferenceOverrides) (session.Session, error)
}

type InferenceResolver interface {
	ValidateInferenceOverrides(context.Context, string, session.InferenceOverrides) error
	EffectiveInference(context.Context, string) (session.EffectiveInference, error)
}

type SessionRuntimeCloser interface {
	CloseSession(string)
}

type sessionIDParams struct {
	SessionID string `json:"sessionId"`
}

type sessionGetResult struct {
	Session         sessionevent.SessionSummary  `json:"session"`
	Status          string                       `json:"status"`
	ActiveTurn      *sessionevent.TurnSummary    `json:"activeTurn,omitempty"`
	Queue           sessionevent.QueueSummary    `json:"queue"`
	EffectiveConfig sessionevent.InferenceConfig `json:"effectiveConfig"`
	LatestSequence  uint64                       `json:"latestSequence"`
	SessionRevision uint64                       `json:"sessionRevision"`
}

type sessionRenameParams struct {
	SessionID       string `json:"sessionId"`
	Title           string `json:"title"`
	ClientRequestID string `json:"clientRequestId"`
}

type sessionArchiveParams struct {
	SessionID       string `json:"sessionId"`
	Archived        bool   `json:"archived"`
	ClientRequestID string `json:"clientRequestId"`
}

type sessionDeleteParams struct {
	SessionID       string `json:"sessionId"`
	ClientRequestID string `json:"clientRequestId"`
}

type sessionForkParams struct {
	SessionID       string `json:"sessionId"`
	MessageID       string `json:"messageId,omitempty"`
	ClientRequestID string `json:"clientRequestId"`
}

type sessionPinParams struct {
	SessionID       string `json:"sessionId"`
	Pinned          bool   `json:"pinned"`
	ClientRequestID string `json:"clientRequestId"`
}

type sessionInferenceGetParams struct {
	SessionID string `json:"sessionId"`
}

type sessionInferenceUpdateParams struct {
	SessionID        string                     `json:"sessionId"`
	ExpectedRevision uint64                     `json:"expectedRevision"`
	Overrides        session.InferenceOverrides `json:"overrides"`
	ClientRequestID  string                     `json:"clientRequestId"`
}

type sessionInferenceResult struct {
	Revision  uint64                     `json:"revision"`
	Overrides session.InferenceOverrides `json:"overrides"`
	Effective session.EffectiveInference `json:"effective"`
}

type sessionDeleteResult struct {
	SessionID string `json:"sessionId"`
	Deleted   bool   `json:"deleted"`
}

type sessionForkResult struct {
	SessionID string                `json:"sessionId"`
	Snapshot  sessionevent.Snapshot `json:"snapshot"`
}

func (s *Service) SetSessionMutationServices(sessions SessionMutationService, runtime SessionRuntimeCloser) {
	s.mu.Lock()
	s.sessionMutations = sessions
	s.sessionRuntime = runtime
	s.mu.Unlock()
}

func (s *Service) SetInferenceResolver(resolver InferenceResolver) {
	s.mu.Lock()
	s.inference = resolver
	s.mu.Unlock()
}

func (s *Service) registerSessionMutationHandlers() {
	s.routes["crush/session/get"] = route{feature: FeatureSessionSync, handler: s.handleSessionGet}
	s.routes["crush/session/rename"] = route{feature: FeatureSessionControl, handler: s.handleSessionRename}
	s.routes["crush/session/archive"] = route{feature: FeatureSessionControl, handler: s.handleSessionArchive}
	s.routes["crush/session/delete"] = route{feature: FeatureSessionControl, handler: s.handleSessionDelete}
	s.routes["crush/session/fork"] = route{feature: FeatureSessionControl, handler: s.handleSessionFork}
	s.routes["crush/session/pin"] = route{feature: FeatureSessionControl, handler: s.handleSessionPin}
	s.routes["crush/session/config/get"] = route{feature: FeatureSessionSync, handler: s.handleSessionInferenceGet}
	s.routes["crush/session/config/update"] = route{feature: FeatureSessionControl, handler: s.handleSessionInferenceUpdate}
}

func (s *Service) handleSessionInferenceGet(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params sessionInferenceGetParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParams(err)
	}
	service, _, rpcErr := s.sessionMutationSources()
	if rpcErr != nil {
		return nil, rpcErr
	}
	current, err := service.Get(ctx, params.SessionID)
	if err != nil {
		return nil, sessionMutationError(params.SessionID, err)
	}
	effective, rpcErr := s.resolveEffectiveInference(ctx, params.SessionID)
	if rpcErr != nil {
		return nil, rpcErr
	}
	return sessionInferenceResult{Revision: current.InferenceRevision, Overrides: current.Inference, Effective: effective}, nil
}

func (s *Service) handleSessionInferenceUpdate(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params sessionInferenceUpdateParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParams(err)
	}
	opCtx := context.WithoutCancel(ctx)
	return s.executeMutation(ctx, "crush/session/config/update", params.SessionID, params.ClientRequestID, params, func() (any, *acp.RPCError) {
		service, _, rpcErr := s.sessionMutationSources()
		if rpcErr != nil {
			return nil, rpcErr
		}
		if rpcErr := s.validateInference(opCtx, params.SessionID, params.Overrides); rpcErr != nil {
			return nil, rpcErr
		}
		updated, err := service.UpdateInference(opCtx, params.SessionID, params.ExpectedRevision, params.Overrides)
		if errors.Is(err, session.ErrInferenceConflict) {
			return nil, protocolError(-32034, errorRevisionConflict, map[string]any{"expectedRevision": params.ExpectedRevision})
		}
		if err != nil {
			return nil, sessionMutationError(params.SessionID, err)
		}
		s.publishSessionUpdated(updated)
		effective, rpcErr := s.resolveEffectiveInference(opCtx, params.SessionID)
		if rpcErr != nil {
			return nil, rpcErr
		}
		return sessionInferenceResult{Revision: updated.InferenceRevision, Overrides: updated.Inference, Effective: effective}, nil
	})
}

func (s *Service) handleSessionGet(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params sessionIDParams
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
	return sessionGetResult{
		Session: snapshot.Session, Status: snapshot.Status, ActiveTurn: snapshot.ActiveTurn,
		Queue: snapshot.Queue, EffectiveConfig: snapshot.EffectiveConfig,
		LatestSequence: snapshot.LatestSequence, SessionRevision: snapshot.SessionRevision,
	}, nil
}

func (s *Service) handleSessionRename(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params sessionRenameParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParams(err)
	}
	params.Title = strings.TrimSpace(params.Title)
	if params.Title == "" || len(params.Title) > maxSessionTitleBytes || !utf8.ValidString(params.Title) {
		return nil, invalidParams(errors.New("title must be valid UTF-8 between 1 and 256 bytes"))
	}
	opCtx := context.WithoutCancel(ctx)
	return s.mutateSession(ctx, "crush/session/rename", params.SessionID, params.ClientRequestID, params, func(service SessionMutationService) (session.Session, *acp.RPCError) {
		if err := service.Rename(opCtx, params.SessionID, params.Title); err != nil {
			return session.Session{}, sessionMutationError(params.SessionID, err)
		}
		updated, err := service.Get(opCtx, params.SessionID)
		if err != nil {
			return session.Session{}, sessionMutationError(params.SessionID, err)
		}
		return updated, nil
	})
}

func (s *Service) handleSessionArchive(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params sessionArchiveParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParams(err)
	}
	opCtx := context.WithoutCancel(ctx)
	return s.mutateSession(ctx, "crush/session/archive", params.SessionID, params.ClientRequestID, params, func(service SessionMutationService) (session.Session, *acp.RPCError) {
		updated, err := service.SetArchived(opCtx, params.SessionID, params.Archived)
		if err != nil {
			return session.Session{}, sessionMutationError(params.SessionID, err)
		}
		return updated, nil
	})
}

func (s *Service) handleSessionPin(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params sessionPinParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParams(err)
	}
	opCtx := context.WithoutCancel(ctx)
	return s.mutateSession(ctx, "crush/session/pin", params.SessionID, params.ClientRequestID, params, func(service SessionMutationService) (session.Session, *acp.RPCError) {
		updated, err := service.SetPinned(opCtx, params.SessionID, params.Pinned)
		if err != nil {
			return session.Session{}, sessionMutationError(params.SessionID, err)
		}
		return updated, nil
	})
}

func (s *Service) handleSessionDelete(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params sessionDeleteParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParams(err)
	}
	opCtx := context.WithoutCancel(ctx)
	return s.executeMutation(ctx, "crush/session/delete", params.SessionID, params.ClientRequestID, params, func() (any, *acp.RPCError) {
		service, runtime, rpcErr := s.sessionMutationSources()
		if rpcErr != nil {
			return nil, rpcErr
		}
		if _, err := service.Get(opCtx, params.SessionID); err != nil {
			return nil, sessionMutationError(params.SessionID, err)
		}
		if runtime != nil {
			runtime.CloseSession(params.SessionID)
		}
		s.releaseSessionClientFS(params.SessionID)
		s.releaseSessionBlobs(params.SessionID)
		if err := service.Delete(opCtx, params.SessionID); err != nil {
			return nil, sessionMutationError(params.SessionID, err)
		}
		return sessionDeleteResult{SessionID: params.SessionID, Deleted: true}, nil
	})
}

func (s *Service) handleSessionFork(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params sessionForkParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParams(err)
	}
	opCtx := context.WithoutCancel(ctx)
	return s.executeMutation(ctx, "crush/session/fork", params.SessionID, params.ClientRequestID, params, func() (any, *acp.RPCError) {
		service, _, rpcErr := s.sessionMutationSources()
		if rpcErr != nil {
			return nil, rpcErr
		}
		if _, err := service.Get(opCtx, params.SessionID); err != nil {
			return nil, sessionMutationError(params.SessionID, err)
		}
		forked, err := service.Fork(opCtx, params.SessionID, params.MessageID)
		if err != nil {
			if errors.Is(err, session.ErrForkBoundaryNotFound) {
				return nil, protocolError(-32038, errorForkBoundaryInvalid, nil)
			}
			return nil, sessionMutationError(params.SessionID, err)
		}
		snapshot, snapshotErr := s.buildSnapshot(opCtx, forked.ID)
		if snapshotErr != nil {
			return nil, snapshotErr
		}
		return sessionForkResult{SessionID: forked.ID, Snapshot: snapshot}, nil
	})
}

func (s *Service) mutateSession(
	ctx context.Context,
	method, sessionID, requestID string,
	payload any,
	fn func(SessionMutationService) (session.Session, *acp.RPCError),
) (any, *acp.RPCError) {
	return s.executeMutation(ctx, method, sessionID, requestID, payload, func() (any, *acp.RPCError) {
		service, _, rpcErr := s.sessionMutationSources()
		if rpcErr != nil {
			return nil, rpcErr
		}
		if _, err := service.Get(context.WithoutCancel(ctx), sessionID); err != nil {
			return nil, sessionMutationError(sessionID, err)
		}
		updated, rpcErr := fn(service)
		if rpcErr != nil {
			return nil, rpcErr
		}
		s.publishSessionUpdated(updated)
		return sessionSummary(updated), nil
	})
}

func (s *Service) sessionMutationSources() (SessionMutationService, SessionRuntimeCloser, *acp.RPCError) {
	s.mu.RLock()
	service := s.sessionMutations
	runtime := s.sessionRuntime
	s.mu.RUnlock()
	if service == nil {
		return nil, nil, sourceUnavailable("session mutation service is unavailable")
	}
	return service, runtime, nil
}

func (s *Service) validateInference(ctx context.Context, sessionID string, overrides session.InferenceOverrides) *acp.RPCError {
	s.mu.RLock()
	resolver := s.inference
	sessions := s.sessions
	s.mu.RUnlock()
	if sessions != nil {
		if _, err := sessions.Get(ctx, sessionID); err != nil {
			return sessionSourceError(sessionID, err)
		}
	}
	if resolver == nil {
		return sourceUnavailable("session inference resolver is unavailable")
	}
	if err := resolver.ValidateInferenceOverrides(ctx, sessionID, overrides); err != nil {
		return invalidParams(errors.New("invalid inference overrides"))
	}
	return nil
}

func (s *Service) resolveEffectiveInference(ctx context.Context, sessionID string) (session.EffectiveInference, *acp.RPCError) {
	s.mu.RLock()
	resolver := s.inference
	s.mu.RUnlock()
	if resolver == nil {
		return session.EffectiveInference{}, sourceUnavailable("session inference resolver is unavailable")
	}
	value, err := resolver.EffectiveInference(ctx, sessionID)
	if err != nil {
		return session.EffectiveInference{}, sessionMutationError(sessionID, err)
	}
	return value, nil
}

func (s *Service) publishSessionUpdated(updated session.Session) {
	if s.events == nil {
		return
	}
	_, _ = s.events.Publish(updated.ID, sessionevent.NewEvent{
		Kind: sessionevent.KindSessionUpdated, Delivery: sessionevent.DeliveryLatest,
		CoalesceKey: "session:" + updated.ID, Payload: sessionSummary(updated),
	})
}

func sessionSummary(value session.Session) sessionevent.SessionSummary {
	return sessionevent.SessionSummary{
		ID: value.ID, ParentSessionID: value.ParentSessionID, Kind: string(value.Kind),
		Title: value.Title, WorkspaceCWD: value.WorkspaceCWD,
		CollaborationMode: string(value.CollaborationMode), PermissionMode: string(value.PermissionMode),
		MessageCount: value.MessageCount, PromptTokens: value.PromptTokens,
		CompletionTokens: value.CompletionTokens, Archived: value.Archived, Pinned: value.Pinned,
		CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
}

func sessionMutationError(sessionID string, err error) *acp.RPCError {
	if errors.Is(err, sql.ErrNoRows) {
		return protocolError(-32021, errorSessionNotFound, map[string]any{"sessionId": sessionID})
	}
	return &acp.RPCError{Code: acp.CodeInternalError, Message: errorSessionMutationFailed, Data: ErrorData{Code: errorSessionMutationFailed, Retryable: true}}
}
