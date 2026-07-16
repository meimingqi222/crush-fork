package guiapi

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/charmbracelet/crush/internal/acp"
	"github.com/charmbracelet/crush/internal/idempotency"
	"github.com/charmbracelet/crush/internal/providerauth"
)

const (
	providerAuthNotification = "crush/provider/auth_event"
	maxProviderResults       = 512
	maxProviderModelResults  = 4096
)

type providerListResult struct {
	Providers []providerResult `json:"providers"`
}

type providerResult struct {
	ProviderID    string   `json:"providerId"`
	Name          string   `json:"name"`
	Type          string   `json:"type,omitempty"`
	AuthMethods   []string `json:"authMethods"`
	Configured    bool     `json:"configured"`
	Authenticated bool     `json:"authenticated"`
	Disabled      bool     `json:"disabled"`
	ModelCount    int      `json:"modelCount"`
}

type providerParams struct {
	ProviderID string `json:"providerId"`
}

type providerModelsResult struct {
	Models []providerModelResult `json:"models"`
}

type providerModelResult struct {
	ProviderID             string   `json:"providerId"`
	ModelID                string   `json:"modelId"`
	Name                   string   `json:"name"`
	ContextWindow          int64    `json:"contextWindow"`
	MaxOutputTokens        int64    `json:"maxOutputTokens"`
	CanReason              bool     `json:"canReason"`
	ReasoningLevels        []string `json:"reasoningLevels,omitempty"`
	DefaultReasoningEffort string   `json:"defaultReasoningEffort,omitempty"`
	SupportsImages         bool     `json:"supportsImages"`
}

type providerAuthStatusResult struct {
	ProviderID    string `json:"providerId"`
	Authenticated bool   `json:"authenticated"`
}

type providerLoginParams struct {
	ProviderID      string `json:"providerId"`
	AuthMethod      string `json:"authMethod"`
	APIKey          string `json:"apiKey,omitempty"`
	ClientRequestID string `json:"clientRequestId"`
}

type providerLoginResult struct {
	LoginID    string `json:"loginId"`
	ProviderID string `json:"providerId"`
	Status     string `json:"status"`
}

type providerLoginCancelParams struct {
	LoginID         string `json:"loginId"`
	ClientRequestID string `json:"clientRequestId"`
}

type providerLoginCancelResult struct {
	LoginID string `json:"loginId"`
	Status  string `json:"status"`
}

type providerLogoutParams struct {
	ProviderID      string `json:"providerId"`
	ClientRequestID string `json:"clientRequestId"`
}

type providerLogoutResult struct {
	ProviderID    string `json:"providerId"`
	Authenticated bool   `json:"authenticated"`
}

type providerAuthEvent struct {
	LoginID         string `json:"loginId"`
	ProviderID      string `json:"providerId"`
	Status          string `json:"status"`
	VerificationURI string `json:"verificationUri,omitempty"`
	UserCode        string `json:"userCode,omitempty"`
	ExpiresAt       int64  `json:"expiresAt,omitempty"`
	ErrorCode       string `json:"errorCode,omitempty"`
	Message         string `json:"message,omitempty"`
}

func (s *Service) registerProviderHandlers() {
	s.routes["crush/provider/list"] = route{feature: FeatureProviderAuth, handler: s.handleProviderList}
	s.routes["crush/provider/models"] = route{feature: FeatureProviderAuth, handler: s.handleProviderModels}
	s.routes["crush/provider/auth_status"] = route{feature: FeatureProviderAuth, handler: s.handleProviderAuthStatus}
	s.routes["crush/provider/login"] = route{feature: FeatureProviderAuth, handler: s.handleProviderLogin}
	s.routes["crush/provider/login_cancel"] = route{feature: FeatureProviderAuth, handler: s.handleProviderLoginCancel}
	s.routes["crush/provider/logout"] = route{feature: FeatureProviderAuth, handler: s.handleProviderLogout}
}

func (s *Service) handleProviderList(context.Context, json.RawMessage) (any, *acp.RPCError) {
	manager, _, _, rpcErr := s.providerServices()
	if rpcErr != nil {
		return nil, rpcErr
	}
	providers := manager.Providers()
	if len(providers) > maxProviderResults {
		return nil, providerError(providerauth.ErrCapacity)
	}
	result := make([]providerResult, 0, len(providers))
	for _, provider := range providers {
		authMethods := make([]string, len(provider.AuthMethods))
		for i, method := range provider.AuthMethods {
			authMethods[i] = string(method)
		}
		result = append(result, providerResult{
			ProviderID: provider.ID, Name: provider.Name, Type: provider.Type,
			AuthMethods: authMethods, Configured: provider.Configured,
			Authenticated: provider.Authenticated, Disabled: provider.Disabled,
			ModelCount: provider.ModelCount,
		})
	}
	return providerListResult{Providers: result}, nil
}

func (s *Service) handleProviderModels(_ context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params providerParams
	if err := json.Unmarshal(raw, &params); err != nil || params.ProviderID == "" {
		return nil, invalidParams(errors.New("providerId is required"))
	}
	manager, _, _, rpcErr := s.providerServices()
	if rpcErr != nil {
		return nil, rpcErr
	}
	models, err := manager.Models(params.ProviderID)
	if err != nil {
		return nil, providerError(err)
	}
	if len(models) > maxProviderModelResults {
		return nil, providerError(providerauth.ErrCapacity)
	}
	result := make([]providerModelResult, 0, len(models))
	for _, model := range models {
		result = append(result, providerModelResult{
			ProviderID: model.ProviderID, ModelID: model.ID, Name: model.Name,
			ContextWindow: model.ContextWindow, MaxOutputTokens: model.MaxOutputTokens,
			CanReason: model.CanReason, ReasoningLevels: model.ReasoningLevels,
			DefaultReasoningEffort: model.DefaultReasoningEffort, SupportsImages: model.SupportsImages,
		})
	}
	return providerModelsResult{Models: result}, nil
}

func (s *Service) handleProviderAuthStatus(_ context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params providerParams
	if err := json.Unmarshal(raw, &params); err != nil || params.ProviderID == "" {
		return nil, invalidParams(errors.New("providerId is required"))
	}
	manager, _, _, rpcErr := s.providerServices()
	if rpcErr != nil {
		return nil, rpcErr
	}
	status, err := manager.AuthStatus(params.ProviderID)
	if err != nil {
		return nil, providerError(err)
	}
	return providerAuthStatusResult{ProviderID: status.ProviderID, Authenticated: status.Authenticated}, nil
}

func (s *Service) handleProviderLogin(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params providerLoginParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParams(errors.New("invalid provider login request"))
	}
	if params.ProviderID == "" || params.AuthMethod == "" || params.ClientRequestID == "" {
		return nil, invalidParams(errors.New("providerId, authMethod and clientRequestId are required"))
	}
	manager, replay, owner, rpcErr := s.providerServices()
	if rpcErr != nil {
		return nil, rpcErr
	}
	outcome, err := replay.Execute(ctx, "provider/login", params.ClientRequestID, params, func() idempotency.Outcome {
		loginID, prepareErr := manager.PrepareLogin(owner, params.ProviderID, providerauth.AuthMethod(params.AuthMethod), params.APIKey, s.providerEventSink())
		if prepareErr != nil {
			return idempotency.Outcome{Failure: providerError(prepareErr)}
		}
		return idempotency.Outcome{Value: providerLoginResult{
			LoginID: loginID, ProviderID: params.ProviderID, Status: string(providerauth.StatusStarting),
		}}
	})
	if err != nil {
		return nil, providerReplayError(params.ClientRequestID, err)
	}
	if failure, _ := outcome.Failure.(*acp.RPCError); failure != nil {
		return nil, failure
	}
	result, ok := outcome.Value.(providerLoginResult)
	if !ok {
		return nil, providerError(providerauth.ErrClosed)
	}
	return &responseLifecycle{
		result: result,
		after: func(writeErr error) {
			if writeErr != nil {
				manager.AbortPrepared(owner, result.LoginID)
				return
			}
			_ = manager.Start(owner, result.LoginID)
		},
	}, nil
}

func (s *Service) handleProviderLoginCancel(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params providerLoginCancelParams
	if err := json.Unmarshal(raw, &params); err != nil || params.LoginID == "" || params.ClientRequestID == "" {
		return nil, invalidParams(errors.New("loginId and clientRequestId are required"))
	}
	manager, replay, owner, rpcErr := s.providerServices()
	if rpcErr != nil {
		return nil, rpcErr
	}
	outcome, err := replay.Execute(ctx, "provider/login_cancel", params.ClientRequestID, params, func() idempotency.Outcome {
		if cancelErr := manager.CanCancel(owner, params.LoginID); cancelErr != nil {
			return idempotency.Outcome{Failure: providerError(cancelErr)}
		}
		return idempotency.Outcome{Value: providerLoginCancelResult{LoginID: params.LoginID, Status: "cancelling"}}
	})
	if err != nil {
		return nil, providerReplayError(params.ClientRequestID, err)
	}
	if failure, _ := outcome.Failure.(*acp.RPCError); failure != nil {
		return nil, failure
	}
	result, ok := outcome.Value.(providerLoginCancelResult)
	if !ok {
		return nil, providerError(providerauth.ErrClosed)
	}
	return &responseLifecycle{
		result: result,
		after: func(writeErr error) {
			if writeErr == nil {
				_ = manager.Cancel(owner, result.LoginID)
			}
		},
	}, nil
}

func (s *Service) handleProviderLogout(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params providerLogoutParams
	if err := json.Unmarshal(raw, &params); err != nil || params.ProviderID == "" || params.ClientRequestID == "" {
		return nil, invalidParams(errors.New("providerId and clientRequestId are required"))
	}
	manager, replay, _, rpcErr := s.providerServices()
	if rpcErr != nil {
		return nil, rpcErr
	}
	outcome, err := replay.Execute(ctx, "provider/logout", params.ClientRequestID, params, func() idempotency.Outcome {
		if logoutErr := manager.Logout(params.ProviderID); logoutErr != nil {
			return idempotency.Outcome{Failure: providerError(logoutErr)}
		}
		return idempotency.Outcome{Value: providerLogoutResult{ProviderID: params.ProviderID}}
	})
	if err != nil {
		return nil, providerReplayError(params.ClientRequestID, err)
	}
	if failure, _ := outcome.Failure.(*acp.RPCError); failure != nil {
		return nil, failure
	}
	return outcome.Value, nil
}

func (s *Service) providerServices() (*providerauth.Manager, *idempotency.Store, string, *acp.RPCError) {
	s.mu.RLock()
	manager, replay, owner, closed := s.providerAuth, s.providerReplay, s.blobOwner, s.closed
	s.mu.RUnlock()
	if closed || manager == nil || replay == nil {
		return nil, nil, "", sourceUnavailable("provider authentication service is unavailable")
	}
	return manager, replay, owner, nil
}

func (s *Service) providerEventSink() providerauth.EventSink {
	return func(ctx context.Context, event providerauth.Event) error {
		s.mu.RLock()
		writer := s.writer
		_, selected := s.features[FeatureProviderAuth]
		available := !s.closed && s.negotiated && selected
		s.mu.RUnlock()
		if writer == nil || !available {
			return errors.New("provider event transport is unavailable")
		}
		wire := providerAuthEvent{
			LoginID: event.LoginID, ProviderID: event.ProviderID, Status: string(event.Status),
			VerificationURI: event.VerificationURI, UserCode: event.UserCode,
			ErrorCode: event.ErrorCode, Message: event.Message,
		}
		if !event.ExpiresAt.IsZero() {
			wire.ExpiresAt = event.ExpiresAt.UnixMilli()
		}
		return writer.NotifySync(ctx, providerAuthNotification, wire)
	}
}

func providerReplayError(requestID string, err error) *acp.RPCError {
	switch {
	case errors.Is(err, idempotency.ErrConflict):
		return protocolError(-32030, errorIdempotencyConflict, map[string]any{"clientRequestId": requestID})
	case errors.Is(err, idempotency.ErrCapacity):
		return providerError(providerauth.ErrCapacity)
	default:
		return invalidParams(errors.New("clientRequestId must be a UUID"))
	}
}

func providerError(err error) *acp.RPCError {
	switch {
	case errors.Is(err, providerauth.ErrProviderNotFound):
		return protocolError(-32050, "CRUSH_PROVIDER_NOT_FOUND", nil)
	case errors.Is(err, providerauth.ErrAuthMethodUnsupported):
		return protocolError(-32051, "CRUSH_PROVIDER_AUTH_UNSUPPORTED", nil)
	case errors.Is(err, providerauth.ErrLoginInProgress):
		return &acp.RPCError{Code: -32052, Message: "CRUSH_PROVIDER_LOGIN_IN_PROGRESS", Data: ErrorData{Code: "CRUSH_PROVIDER_LOGIN_IN_PROGRESS", Retryable: true}}
	case errors.Is(err, providerauth.ErrLoginNotFound):
		return protocolError(-32053, "CRUSH_PROVIDER_LOGIN_NOT_FOUND", nil)
	case errors.Is(err, providerauth.ErrCapacity):
		return &acp.RPCError{Code: -32055, Message: "CRUSH_PROVIDER_CAPACITY", Data: ErrorData{Code: "CRUSH_PROVIDER_CAPACITY", Retryable: true}}
	default:
		return protocolError(-32054, "CRUSH_PROVIDER_AUTH_UNAVAILABLE", nil)
	}
}
