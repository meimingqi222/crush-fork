package providerauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/oauth"
	"github.com/stretchr/testify/require"
)

type mockBackend struct {
	mu          sync.Mutex
	providers   []Provider
	models      map[string][]Model
	credentials map[string]Credential
	saveErr     error
	clearCount  int
}

func newMockBackend() *mockBackend {
	return &mockBackend{
		providers: []Provider{
			{ID: "browser", Name: "Browser", AuthMethods: []AuthMethod{AuthMethodBrowser}},
			{ID: "device", Name: "Device", AuthMethods: []AuthMethod{AuthMethodDeviceCode}},
			{ID: "key", Name: "Key", AuthMethods: []AuthMethod{AuthMethodAPIKey}},
		},
		models: map[string][]Model{
			"device": {{ProviderID: "device", ID: "z-model"}, {ProviderID: "device", ID: "a-model"}},
		},
		credentials: make(map[string]Credential),
	}
}

func (b *mockBackend) Providers() []Provider {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Provider(nil), b.providers...)
}

func (b *mockBackend) Models(providerID string) ([]Model, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	models, ok := b.models[providerID]
	if !ok {
		return nil, ErrProviderNotFound
	}
	return append([]Model(nil), models...), nil
}

func (b *mockBackend) AuthStatus(providerID string) (AuthStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, provider := range b.providers {
		if provider.ID == providerID {
			_, authenticated := b.credentials[providerID]
			return AuthStatus{ProviderID: providerID, Authenticated: authenticated}, nil
		}
	}
	return AuthStatus{}, ErrProviderNotFound
}

func (b *mockBackend) SaveCredential(providerID string, credential Credential) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.saveErr != nil {
		return b.saveErr
	}
	b.credentials[providerID] = credential
	return nil
}

func (b *mockBackend) ClearCredential(providerID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.credentials, providerID)
	b.clearCount++
	return nil
}

type flowFunc func(context.Context, func(Prompt) error) (Credential, error)

func (f flowFunc) Run(ctx context.Context, emit func(Prompt) error) (Credential, error) {
	return f(ctx, emit)
}

func TestManagerInteractiveFlowsAndSortedDiscovery(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	var starts atomic.Int32
	manager := New(backend, Config{Factories: map[FlowKey]FlowFactory{
		{ProviderID: "device", Method: AuthMethodDeviceCode}: FlowFactoryFunc(func(string) Flow {
			return flowFunc(func(_ context.Context, emit func(Prompt) error) (Credential, error) {
				starts.Add(1)
				err := emit(Prompt{
					Kind: AuthMethodDeviceCode, VerificationURI: "https://example.test/device",
					UserCode: "ABCD-EFGH", ExpiresAt: time.Unix(200, 0),
				})
				return Credential{Token: &oauth.Token{AccessToken: "access-secret", RefreshToken: "refresh-secret"}}, err
			})
		}),
	}})
	t.Cleanup(manager.Close)

	providers := manager.Providers()
	require.Equal(t, []string{"browser", "device", "key"}, []string{providers[0].ID, providers[1].ID, providers[2].ID})
	models, err := manager.Models("device")
	require.NoError(t, err)
	require.Equal(t, []string{"a-model", "z-model"}, []string{models[0].ID, models[1].ID})

	events := make(chan Event, 2)
	loginID, err := manager.PrepareLogin("owner-a", "device", AuthMethodDeviceCode, "", func(_ context.Context, event Event) error {
		events <- event
		return nil
	})
	require.NoError(t, err)
	require.Zero(t, starts.Load(), "prepared login must not start before response lifecycle")
	require.NoError(t, manager.Start("owner-a", loginID))

	waiting := <-events
	require.Equal(t, StatusWaitingCode, waiting.Status)
	require.Equal(t, "ABCD-EFGH", waiting.UserCode)
	require.Equal(t, "https://example.test/device", waiting.VerificationURI)
	authenticated := <-events
	require.Equal(t, StatusAuthenticated, authenticated.Status)
	require.Equal(t, loginID, authenticated.LoginID)
	require.Equal(t, int32(1), starts.Load())

	backend.mu.Lock()
	credential := backend.credentials["device"]
	backend.mu.Unlock()
	require.Equal(t, "access-secret", credential.Token.AccessToken)
}

func TestManagerBrowserAPIKeyAndLogout(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	manager := New(backend, Config{Factories: map[FlowKey]FlowFactory{
		{ProviderID: "browser", Method: AuthMethodBrowser}: FlowFactoryFunc(func(string) Flow {
			return flowFunc(func(_ context.Context, emit func(Prompt) error) (Credential, error) {
				if err := emit(Prompt{Kind: AuthMethodBrowser, VerificationURI: "https://example.test/login"}); err != nil {
					return Credential{}, err
				}
				return Credential{APIKey: "browser-secret"}, nil
			})
		}),
	}})
	t.Cleanup(manager.Close)

	for _, test := range []struct {
		provider string
		method   AuthMethod
		apiKey   string
		waiting  LoginStatus
	}{
		{provider: "browser", method: AuthMethodBrowser, waiting: StatusWaitingBrowser},
		{provider: "key", method: AuthMethodAPIKey, apiKey: "api-secret"},
	} {
		events := make(chan Event, 2)
		loginID, err := manager.PrepareLogin("owner", test.provider, test.method, test.apiKey, func(_ context.Context, event Event) error {
			events <- event
			return nil
		})
		require.NoError(t, err)
		require.NoError(t, manager.Start("owner", loginID))
		if test.waiting != "" {
			require.Equal(t, test.waiting, (<-events).Status)
		}
		require.Equal(t, StatusAuthenticated, (<-events).Status)
	}

	status, err := manager.AuthStatus("key")
	require.NoError(t, err)
	require.True(t, status.Authenticated)
	require.NoError(t, manager.Logout("key"))
	status, err = manager.AuthStatus("key")
	require.NoError(t, err)
	require.False(t, status.Authenticated)
	backend.mu.Lock()
	require.Equal(t, 1, backend.clearCount)
	backend.mu.Unlock()
}

func TestManagerCancelOwnershipAndConnectionBarrier(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	started := make(chan struct{})
	manager := New(backend, Config{Factories: map[FlowKey]FlowFactory{
		{ProviderID: "device", Method: AuthMethodDeviceCode}: FlowFactoryFunc(func(string) Flow {
			return flowFunc(func(ctx context.Context, _ func(Prompt) error) (Credential, error) {
				close(started)
				<-ctx.Done()
				return Credential{}, ctx.Err()
			})
		}),
	}})
	t.Cleanup(manager.Close)

	events := make(chan Event, 1)
	loginID, err := manager.PrepareLogin("owner-a", "device", AuthMethodDeviceCode, "", func(_ context.Context, event Event) error {
		events <- event
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, manager.Start("owner-a", loginID))
	<-started
	require.ErrorIs(t, manager.CanCancel("owner-b", loginID), ErrLoginNotFound)
	require.ErrorIs(t, manager.Cancel("owner-b", loginID), ErrLoginNotFound)
	require.NoError(t, manager.Cancel("owner-a", loginID))
	require.Equal(t, StatusCancelled, (<-events).Status)

	started = make(chan struct{})
	loginID, err = manager.PrepareLogin("owner-a", "device", AuthMethodDeviceCode, "", func(_ context.Context, event Event) error {
		events <- event
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, manager.Start("owner-a", loginID))
	<-started
	manager.CloseOwner("owner-a")
	select {
	case event := <-events:
		require.Failf(t, "late event", "unexpected event after CloseOwner: %+v", event)
	default:
	}
}

func TestManagerFailureNeverExposesRawProviderError(t *testing.T) {
	backend := newMockBackend()
	rawSecret := "Bearer fake-token Authorization: secret-header body-secret"
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	manager := New(backend, Config{Factories: map[FlowKey]FlowFactory{
		{ProviderID: "device", Method: AuthMethodDeviceCode}: FlowFactoryFunc(func(string) Flow {
			return flowFunc(func(context.Context, func(Prompt) error) (Credential, error) {
				return Credential{}, errors.New(rawSecret)
			})
		}),
	}})
	t.Cleanup(manager.Close)
	events := make(chan Event, 1)
	loginID, err := manager.PrepareLogin("owner", "device", AuthMethodDeviceCode, "", func(_ context.Context, event Event) error {
		events <- event
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, manager.Start("owner", loginID))
	event := <-events
	require.Equal(t, StatusFailed, event.Status)
	require.Equal(t, "CRUSH_PROVIDER_LOGIN_FAILED", event.ErrorCode)
	require.Equal(t, "Provider authentication failed", event.Message)

	wire, err := json.Marshal(event)
	require.NoError(t, err)
	require.NotContains(t, string(wire), rawSecret)
	require.NotContains(t, logs.String(), rawSecret)

	backend.mu.Lock()
	backend.saveErr = errors.New(rawSecret)
	backend.mu.Unlock()
	loginID, err = manager.PrepareLogin("owner", "key", AuthMethodAPIKey, "api-key-secret", func(_ context.Context, event Event) error {
		events <- event
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, manager.Start("owner", loginID))
	event = <-events
	require.Equal(t, StatusFailed, event.Status)
	wire, err = json.Marshal(event)
	require.NoError(t, err)
	require.NotContains(t, string(wire), rawSecret)
	require.NotContains(t, string(wire), "api-key-secret")
	require.NotContains(t, logs.String(), rawSecret)
}

func TestManagerBoundsRetentionAndValidation(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	backend.providers = append(backend.providers, Provider{ID: "key2", Name: "Key 2", AuthMethods: []AuthMethod{AuthMethodAPIKey}})
	var now atomic.Int64
	now.Store(100)
	manager := New(backend, Config{MaxLogins: 1, Retention: time.Minute, Clock: func() time.Time { return time.Unix(now.Load(), 0) }})
	t.Cleanup(manager.Close)
	sink := func(context.Context, Event) error { return nil }

	loginID, err := manager.PrepareLogin("owner", "key", AuthMethodAPIKey, "secret", sink)
	require.NoError(t, err)
	_, err = manager.PrepareLogin("owner", "key2", AuthMethodAPIKey, "other-secret", sink)
	require.ErrorIs(t, err, ErrCapacity)
	require.NoError(t, manager.Start("owner", loginID))
	require.Eventually(t, func() bool {
		status, statusErr := manager.AuthStatus("key")
		return statusErr == nil && status.Authenticated
	}, time.Second, time.Millisecond)
	require.Eventually(t, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		_, active := manager.active["key"]
		return !active
	}, time.Second, time.Millisecond)
	now.Add(int64((2 * time.Minute).Seconds()))
	loginID, err = manager.PrepareLogin("owner", "key", AuthMethodAPIKey, "secret-2", sink)
	require.NoError(t, err, "expired terminal login should be pruned")
	manager.AbortPrepared("owner", loginID)

	_, err = manager.PrepareLogin("owner", "key", AuthMethodAPIKey, "", sink)
	require.Error(t, err)
	_, err = manager.PrepareLogin("owner", "key", AuthMethodAPIKey, string([]byte{'a', 0, 'b'}), sink)
	require.Error(t, err)
	_, err = manager.PrepareLogin("owner", "bad.provider", AuthMethodAPIKey, "secret", sink)
	require.ErrorIs(t, err, ErrProviderNotFound)
}

func TestManagerConcurrentPrepareAllowsOneProviderLogin(t *testing.T) {
	t.Parallel()

	manager := New(newMockBackend(), Config{})
	t.Cleanup(manager.Close)
	var successes atomic.Int32
	var wait sync.WaitGroup
	for range 32 {
		wait.Go(func() {
			_, err := manager.PrepareLogin("owner", "key", AuthMethodAPIKey, "secret", func(context.Context, Event) error { return nil })
			if err == nil {
				successes.Add(1)
				return
			}
			require.ErrorIs(t, err, ErrLoginInProgress)
		})
	}
	wait.Wait()
	require.Equal(t, int32(1), successes.Load())
}

func TestManagerLogoutWaitsForTerminalCallbackBeforeClearing(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	manager := New(backend, Config{})
	t.Cleanup(manager.Close)
	entered := make(chan struct{})
	release := make(chan struct{})
	loginID, err := manager.PrepareLogin("owner", "key", AuthMethodAPIKey, "secret", func(_ context.Context, event Event) error {
		if event.Status == StatusAuthenticated {
			close(entered)
			<-release
		}
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, manager.Start("owner", loginID))
	<-entered

	done := make(chan error, 1)
	go func() { done <- manager.Logout("key") }()
	select {
	case err := <-done:
		require.Failf(t, "early logout", "logout completed before callback barrier: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	backend.mu.Lock()
	require.Zero(t, backend.clearCount)
	backend.mu.Unlock()
	close(release)
	require.NoError(t, <-done)
	backend.mu.Lock()
	require.Equal(t, 1, backend.clearCount)
	backend.mu.Unlock()
}
