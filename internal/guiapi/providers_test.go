package guiapi

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/acp"
	"github.com/charmbracelet/crush/internal/providerauth"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type providerTestBackend struct {
	mu          sync.Mutex
	providers   []providerauth.Provider
	models      map[string][]providerauth.Model
	credentials map[string]providerauth.Credential
	clearCount  int
}

func newProviderTestBackend() *providerTestBackend {
	return &providerTestBackend{
		providers: []providerauth.Provider{
			{ID: "device", Name: "Device", Type: "openai", AuthMethods: []providerauth.AuthMethod{providerauth.AuthMethodDeviceCode}, ModelCount: 1},
			{ID: "key", Name: "Key", Type: "anthropic", AuthMethods: []providerauth.AuthMethod{providerauth.AuthMethodAPIKey}},
		},
		models: map[string][]providerauth.Model{
			"device": {{ProviderID: "device", ID: "model", Name: "Model", ContextWindow: 100, MaxOutputTokens: 20, CanReason: true, ReasoningLevels: []string{"low"}, SupportsImages: true}},
		},
		credentials: make(map[string]providerauth.Credential),
	}
}

func (b *providerTestBackend) Providers() []providerauth.Provider {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]providerauth.Provider(nil), b.providers...)
}

func (b *providerTestBackend) Models(providerID string) ([]providerauth.Model, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	models, ok := b.models[providerID]
	if !ok {
		return nil, providerauth.ErrProviderNotFound
	}
	return append([]providerauth.Model(nil), models...), nil
}

func (b *providerTestBackend) AuthStatus(providerID string) (providerauth.AuthStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, provider := range b.providers {
		if provider.ID == providerID {
			_, authenticated := b.credentials[providerID]
			return providerauth.AuthStatus{ProviderID: providerID, Authenticated: authenticated}, nil
		}
	}
	return providerauth.AuthStatus{}, providerauth.ErrProviderNotFound
}

func (b *providerTestBackend) SaveCredential(providerID string, credential providerauth.Credential) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.credentials[providerID] = credential
	return nil
}

func (b *providerTestBackend) ClearCredential(providerID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.credentials, providerID)
	b.clearCount++
	return nil
}

type providerFlowFunc func(context.Context, func(providerauth.Prompt) error) (providerauth.Credential, error)

func (f providerFlowFunc) Run(ctx context.Context, emit func(providerauth.Prompt) error) (providerauth.Credential, error) {
	return f(ctx, emit)
}

type providerRecordingWriter struct {
	mu      sync.Mutex
	methods []string
	payload [][]byte
}

func (w *providerRecordingWriter) NotifySync(_ context.Context, method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	w.mu.Lock()
	w.methods = append(w.methods, method)
	w.payload = append(w.payload, raw)
	w.mu.Unlock()
	return nil
}

func (w *providerRecordingWriter) snapshot() ([]string, [][]byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.methods...), append([][]byte(nil), w.payload...)
}

func negotiatedProviderService(t *testing.T, manager *providerauth.Manager, writer NotificationWriter) *Service {
	t.Helper()
	service := NewService(nil)
	service.SetProviderAuthService(manager)
	service.SetNotificationWriter(writer)
	require.Nil(t, service.NegotiateExperimental(experimentalSelection(t, Selection{
		ProtocolVersion: ProtocolVersion, Features: []Feature{FeatureProviderAuth},
	})))
	t.Cleanup(service.Close)
	return service
}

func TestProviderDiscoveryUsesBoundedSafeDTOs(t *testing.T) {
	t.Parallel()

	backend := newProviderTestBackend()
	manager := providerauth.New(backend, providerauth.Config{})
	t.Cleanup(manager.Close)
	service := negotiatedProviderService(t, manager, &providerRecordingWriter{})

	result, rpcErr := service.HandleExtension(t.Context(), "crush/provider/list", json.RawMessage(`{}`))
	require.Nil(t, rpcErr)
	providers := result.(providerListResult).Providers
	require.Len(t, providers, 2)
	require.Equal(t, "device", providers[0].ProviderID)
	require.Equal(t, []string{"device_code"}, providers[0].AuthMethods)

	result, rpcErr = service.HandleExtension(t.Context(), "crush/provider/models", mustRawJSON(t, providerParams{ProviderID: "device"}))
	require.Nil(t, rpcErr)
	models := result.(providerModelsResult).Models
	require.Len(t, models, 1)
	require.Equal(t, int64(100), models[0].ContextWindow)
	require.Equal(t, []string{"low"}, models[0].ReasoningLevels)
	wire, err := json.Marshal(result)
	require.NoError(t, err)
	for _, forbidden := range []string{"apiKey", "oauth", "headers", "baseUrl", "providerOptions"} {
		require.NotContains(t, string(wire), forbidden)
	}
}

func TestProviderLoginResponsePrecedesEventsAndRetryStartsOnce(t *testing.T) {
	t.Parallel()

	backend := newProviderTestBackend()
	var starts atomic.Int32
	manager := providerauth.New(backend, providerauth.Config{Factories: map[providerauth.FlowKey]providerauth.FlowFactory{
		{ProviderID: "device", Method: providerauth.AuthMethodDeviceCode}: providerauth.FlowFactoryFunc(func(string) providerauth.Flow {
			return providerFlowFunc(func(_ context.Context, emit func(providerauth.Prompt) error) (providerauth.Credential, error) {
				starts.Add(1)
				if err := emit(providerauth.Prompt{
					Kind: providerauth.AuthMethodDeviceCode, VerificationURI: "https://example.test/device", UserCode: "CODE",
				}); err != nil {
					return providerauth.Credential{}, err
				}
				return providerauth.Credential{APIKey: "flow-secret"}, nil
			})
		}),
	}})
	t.Cleanup(manager.Close)
	writer := &providerRecordingWriter{}
	service := negotiatedProviderService(t, manager, writer)
	request := providerLoginParams{ProviderID: "device", AuthMethod: "device_code", ClientRequestID: uuid.NewString()}

	first, rpcErr := service.HandleExtension(t.Context(), "crush/provider/login", mustRawJSON(t, request))
	require.Nil(t, rpcErr)
	second, rpcErr := service.HandleExtension(t.Context(), "crush/provider/login", mustRawJSON(t, request))
	require.Nil(t, rpcErr)
	firstLifecycle := first.(acp.ResponseLifecycle)
	secondLifecycle := second.(acp.ResponseLifecycle)
	firstResult := firstLifecycle.ResponseResult().(providerLoginResult)
	secondResult := secondLifecycle.ResponseResult().(providerLoginResult)
	require.Equal(t, firstResult.LoginID, secondResult.LoginID)
	require.Zero(t, starts.Load())
	methods, _ := writer.snapshot()
	require.Empty(t, methods, "authentication must not start before the response is written")

	firstLifecycle.AfterResponse(t.Context(), nil)
	secondLifecycle.AfterResponse(t.Context(), nil)
	require.Eventually(t, func() bool {
		methods, _ := writer.snapshot()
		return len(methods) == 2
	}, time.Second, time.Millisecond)
	require.Equal(t, int32(1), starts.Load())
	methods, payloads := writer.snapshot()
	require.Equal(t, []string{providerAuthNotification, providerAuthNotification}, methods)
	require.Contains(t, string(payloads[0]), `"status":"waiting_code"`)
	require.Contains(t, string(payloads[1]), `"status":"authenticated"`)
}

func TestProviderAPIKeyNeverEchoesAndConflictIsSafe(t *testing.T) {
	t.Parallel()

	backend := newProviderTestBackend()
	manager := providerauth.New(backend, providerauth.Config{})
	t.Cleanup(manager.Close)
	writer := &providerRecordingWriter{}
	service := negotiatedProviderService(t, manager, writer)
	requestID := uuid.NewString()
	rawSecret := "sk-fake-secret-value"
	request := providerLoginParams{ProviderID: "key", AuthMethod: "api_key", APIKey: rawSecret, ClientRequestID: requestID}

	result, rpcErr := service.HandleExtension(t.Context(), "crush/provider/login", mustRawJSON(t, request))
	require.Nil(t, rpcErr)
	lifecycle := result.(acp.ResponseLifecycle)
	wire, err := json.Marshal(lifecycle.ResponseResult())
	require.NoError(t, err)
	require.NotContains(t, string(wire), rawSecret)
	lifecycle.AfterResponse(t.Context(), nil)
	require.Eventually(t, func() bool {
		_, payloads := writer.snapshot()
		return len(payloads) == 1
	}, time.Second, time.Millisecond)
	_, payloads := writer.snapshot()
	require.NotContains(t, string(payloads[0]), rawSecret)

	request.APIKey = "different-secret"
	_, rpcErr = service.HandleExtension(t.Context(), "crush/provider/login", mustRawJSON(t, request))
	require.Equal(t, errorIdempotencyConflict, rpcErr.Message)
	rpcWire, err := json.Marshal(rpcErr)
	require.NoError(t, err)
	require.NotContains(t, string(rpcWire), rawSecret)
	require.NotContains(t, string(rpcWire), request.APIKey)

	backend.mu.Lock()
	require.Equal(t, rawSecret, backend.credentials["key"].APIKey)
	backend.mu.Unlock()
}

func TestProviderCancelOwnershipLogoutAndConnectionClose(t *testing.T) {
	t.Parallel()

	backend := newProviderTestBackend()
	started := make(chan struct{}, 2)
	manager := providerauth.New(backend, providerauth.Config{Factories: map[providerauth.FlowKey]providerauth.FlowFactory{
		{ProviderID: "device", Method: providerauth.AuthMethodDeviceCode}: providerauth.FlowFactoryFunc(func(string) providerauth.Flow {
			return providerFlowFunc(func(ctx context.Context, _ func(providerauth.Prompt) error) (providerauth.Credential, error) {
				started <- struct{}{}
				<-ctx.Done()
				return providerauth.Credential{}, ctx.Err()
			})
		}),
	}})
	t.Cleanup(manager.Close)
	writerA := &providerRecordingWriter{}
	serviceA := negotiatedProviderService(t, manager, writerA)
	serviceB := negotiatedProviderService(t, manager, &providerRecordingWriter{})
	loginRequest := providerLoginParams{ProviderID: "device", AuthMethod: "device_code", ClientRequestID: uuid.NewString()}
	result, rpcErr := serviceA.HandleExtension(t.Context(), "crush/provider/login", mustRawJSON(t, loginRequest))
	require.Nil(t, rpcErr)
	lifecycle := result.(acp.ResponseLifecycle)
	loginID := lifecycle.ResponseResult().(providerLoginResult).LoginID
	lifecycle.AfterResponse(t.Context(), nil)
	<-started

	cancel := providerLoginCancelParams{LoginID: loginID, ClientRequestID: uuid.NewString()}
	_, rpcErr = serviceB.HandleExtension(t.Context(), "crush/provider/login_cancel", mustRawJSON(t, cancel))
	require.Equal(t, "CRUSH_PROVIDER_LOGIN_NOT_FOUND", rpcErr.Message)
	result, rpcErr = serviceA.HandleExtension(t.Context(), "crush/provider/login_cancel", mustRawJSON(t, cancel))
	require.Nil(t, rpcErr)
	cancelLifecycle := result.(acp.ResponseLifecycle)
	methods, _ := writerA.snapshot()
	require.Empty(t, methods)
	cancelLifecycle.AfterResponse(t.Context(), nil)
	require.Eventually(t, func() bool {
		methods, _ := writerA.snapshot()
		return len(methods) == 1
	}, time.Second, time.Millisecond)

	loginRequest.ClientRequestID = uuid.NewString()
	result, rpcErr = serviceA.HandleExtension(t.Context(), "crush/provider/login", mustRawJSON(t, loginRequest))
	require.Nil(t, rpcErr)
	lifecycle = result.(acp.ResponseLifecycle)
	lifecycle.AfterResponse(t.Context(), nil)
	<-started
	serviceA.Close()
	methodsBefore, _ := writerA.snapshot()
	time.Sleep(10 * time.Millisecond)
	methodsAfter, _ := writerA.snapshot()
	require.Equal(t, methodsBefore, methodsAfter, "connection close must prevent late auth events")

	apiLogin := providerLoginParams{ProviderID: "key", AuthMethod: "api_key", APIKey: "secret", ClientRequestID: uuid.NewString()}
	result, rpcErr = serviceB.HandleExtension(t.Context(), "crush/provider/login", mustRawJSON(t, apiLogin))
	require.Nil(t, rpcErr)
	result.(acp.ResponseLifecycle).AfterResponse(t.Context(), nil)
	require.Eventually(t, func() bool {
		status, _ := manager.AuthStatus("key")
		return status.Authenticated
	}, time.Second, time.Millisecond)
	logout := providerLogoutParams{ProviderID: "key", ClientRequestID: uuid.NewString()}
	_, rpcErr = serviceB.HandleExtension(t.Context(), "crush/provider/logout", mustRawJSON(t, logout))
	require.Nil(t, rpcErr)
	backend.mu.Lock()
	require.Equal(t, 1, backend.clearCount)
	backend.mu.Unlock()
}

func TestProviderFailureEventAndErrorsDiscardRawProviderData(t *testing.T) {
	t.Parallel()

	backend := newProviderTestBackend()
	rawSecret := "Authorization: Bearer fake-token provider-body-secret"
	manager := providerauth.New(backend, providerauth.Config{Factories: map[providerauth.FlowKey]providerauth.FlowFactory{
		{ProviderID: "device", Method: providerauth.AuthMethodDeviceCode}: providerauth.FlowFactoryFunc(func(string) providerauth.Flow {
			return providerFlowFunc(func(context.Context, func(providerauth.Prompt) error) (providerauth.Credential, error) {
				return providerauth.Credential{}, errors.New(rawSecret)
			})
		}),
	}})
	t.Cleanup(manager.Close)
	writer := &providerRecordingWriter{}
	service := negotiatedProviderService(t, manager, writer)
	request := providerLoginParams{ProviderID: "device", AuthMethod: "device_code", ClientRequestID: uuid.NewString()}
	result, rpcErr := service.HandleExtension(t.Context(), "crush/provider/login", mustRawJSON(t, request))
	require.Nil(t, rpcErr)
	result.(acp.ResponseLifecycle).AfterResponse(t.Context(), nil)
	require.Eventually(t, func() bool {
		_, payloads := writer.snapshot()
		return len(payloads) == 1
	}, time.Second, time.Millisecond)
	_, payloads := writer.snapshot()
	require.Contains(t, string(payloads[0]), "CRUSH_PROVIDER_LOGIN_FAILED")
	require.NotContains(t, string(payloads[0]), rawSecret)
}

func TestProviderRenegotiationRevokesActiveLoginAndReplay(t *testing.T) {
	t.Parallel()

	backend := newProviderTestBackend()
	started := make(chan struct{})
	cancelled := make(chan struct{})
	manager := providerauth.New(backend, providerauth.Config{Factories: map[providerauth.FlowKey]providerauth.FlowFactory{
		{ProviderID: "device", Method: providerauth.AuthMethodDeviceCode}: providerauth.FlowFactoryFunc(func(string) providerauth.Flow {
			return providerFlowFunc(func(ctx context.Context, _ func(providerauth.Prompt) error) (providerauth.Credential, error) {
				close(started)
				<-ctx.Done()
				close(cancelled)
				return providerauth.Credential{}, ctx.Err()
			})
		}),
	}})
	t.Cleanup(manager.Close)
	writer := &providerRecordingWriter{}
	service := negotiatedProviderService(t, manager, writer)
	request := providerLoginParams{ProviderID: "device", AuthMethod: "device_code", ClientRequestID: uuid.NewString()}
	result, rpcErr := service.HandleExtension(t.Context(), "crush/provider/login", mustRawJSON(t, request))
	require.Nil(t, rpcErr)
	result.(acp.ResponseLifecycle).AfterResponse(t.Context(), nil)
	<-started

	require.Nil(t, service.NegotiateExperimental(experimentalSelection(t, Selection{
		ProtocolVersion: ProtocolVersion, Features: []Feature{FeatureSessionSync},
	})))
	select {
	case <-cancelled:
	default:
		t.Fatal("renegotiation returned before the provider flow cancellation barrier")
	}
	methods, _ := writer.snapshot()
	require.Empty(t, methods)
	_, rpcErr = service.HandleExtension(t.Context(), "crush/provider/login", mustRawJSON(t, request))
	require.Equal(t, codeFeatureNotNegotiated, rpcErr.Code)
}
