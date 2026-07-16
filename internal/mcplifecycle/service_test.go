package mcplifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	crushlog "github.com/charmbracelet/crush/internal/log"
	"github.com/charmbracelet/crush/internal/sessionevent"
	"github.com/stretchr/testify/require"
)

type mockBackend struct {
	mu                    sync.Mutex
	events                chan BackendEvent
	states                map[string]BackendInfo
	started               chan string
	release               chan struct{}
	reconnectRelease      chan struct{}
	ignoreReconnectCancel bool
	failSuffix            string
	connects              []string
	reconnects            []string
	disables              []string
	marked                []string
}

func newMockBackend() *mockBackend {
	return &mockBackend{
		events: make(chan BackendEvent, 256), states: make(map[string]BackendInfo),
		started: make(chan string, 256),
	}
}

func (b *mockBackend) Connect(ctx context.Context, _ *config.ConfigStore, name string) error {
	b.mu.Lock()
	b.connects = append(b.connects, name)
	release, fail := b.release, b.failSuffix != "" && strings.HasSuffix(name, b.failSuffix)
	b.states[name] = BackendInfo{State: BackendStarting}
	b.mu.Unlock()
	b.started <- name
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if fail {
		b.setState(name, BackendInfo{State: BackendError})
		return errors.New("raw connection failure with secret")
	}
	b.setState(name, BackendInfo{State: BackendConnected, Counts: Counts{Tools: 2, Prompts: 1}})
	return nil
}

func (b *mockBackend) Reconnect(ctx context.Context, store *config.ConfigStore, name string) error {
	b.mu.Lock()
	b.reconnects = append(b.reconnects, name)
	release, ignoreCancel := b.reconnectRelease, b.ignoreReconnectCancel
	b.mu.Unlock()
	if release != nil {
		b.started <- name
		if ignoreCancel {
			<-release
		} else {
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		b.setState(name, BackendInfo{State: BackendConnected, Counts: Counts{Tools: 2}})
		return nil
	}
	return b.Connect(ctx, store, name)
}

func (b *mockBackend) Disable(_ context.Context, _ *config.ConfigStore, name string) error {
	b.mu.Lock()
	b.disables = append(b.disables, name)
	b.states[name] = BackendInfo{State: BackendDisabled}
	b.mu.Unlock()
	b.events <- BackendEvent{Type: BackendEventState, Name: name, State: BackendDisabled}
	return nil
}

func (b *mockBackend) State(name string) (BackendInfo, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	state, ok := b.states[name]
	return state, ok
}

func (b *mockBackend) Subscribe(ctx context.Context) <-chan BackendEvent {
	output := make(chan BackendEvent)
	go func() {
		defer close(output)
		for {
			select {
			case <-ctx.Done():
				return
			case event := <-b.events:
				select {
				case output <- event:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return output
}

func (b *mockBackend) MarkScoped(name string) {
	b.mu.Lock()
	b.marked = append(b.marked, name)
	b.mu.Unlock()
}

func (b *mockBackend) setState(name string, info BackendInfo) {
	b.mu.Lock()
	b.states[name] = info
	b.mu.Unlock()
	b.events <- BackendEvent{Type: BackendEventState, Name: name, State: info.State, Counts: info.Counts}
}

func (b *mockBackend) log(name string, data any) {
	b.events <- BackendEvent{
		Type: BackendEventLog, Name: name,
		Log: BackendLog{Timestamp: time.Unix(100, 0), Level: "info", Logger: "server", Data: data},
	}
}

func (b *mockBackend) snapshot() (connects, reconnects, disables, marked []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slicesClone(b.connects), slicesClone(b.reconnects), slicesClone(b.disables), slicesClone(b.marked)
}

func slicesClone(values []string) []string { return append([]string(nil), values...) }

func testStore(t *testing.T) *config.ConfigStore {
	t.Helper()
	base := t.TempDir()
	working := filepath.Join(base, "work")
	require.NoError(t, os.MkdirAll(working, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(working, "crush.json"), []byte(`{
  "options":{"disable_provider_auto_update":true},"tools":{}
}`), 0o600))
	store, err := config.Init(working, filepath.Join(base, "state"), false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, crushlog.ResetForTesting()) })
	return store
}

func server(name string) ServerConfig {
	return ServerConfig{Name: name, Config: config.MCPConfig{Type: config.MCPStdio, Command: "mock"}}
}

func waitConnected(t *testing.T, service *Service, sessionID, serverID string) Server {
	t.Helper()
	var current Server
	require.Eventually(t, func() bool {
		value, err := service.Status(sessionID, serverID)
		if err == nil {
			current = value
		}
		return err == nil && (value.Status == StatusConnected || value.Status == StatusDegraded)
	}, 2*time.Second, time.Millisecond)
	return current
}

func TestReplaceAsyncImmediateIsolationGenerationAndCleanup(t *testing.T) {
	store := testStore(t)
	backend := newMockBackend()
	backend.release = make(chan struct{})
	hub := sessionevent.NewHub(sessionevent.Config{})
	t.Cleanup(hub.Close)
	service := New(store, backend, hub, Config{})
	t.Cleanup(func() { service.Close(t.Context()) })

	started := time.Now()
	require.NoError(t, service.ReplaceAsync("owner", "session-a", []ServerConfig{server("shared")}))
	require.Less(t, time.Since(started), 50*time.Millisecond)
	internalA := <-backend.started
	require.False(t, service.Access("session-a").AllowsMCPServer(internalA))
	require.False(t, service.Access("session-b").AllowsMCPServer(internalA))
	require.True(t, service.Access("session-a").AllowsMCPServer("static-server"))
	close(backend.release)
	waitConnected(t, service, "session-a", dynamicID("shared"))
	require.True(t, service.Access("session-a").AllowsMCPServer(internalA))
	require.False(t, service.Access("session-b").AllowsMCPServer(internalA))

	backend.release = nil
	require.NoError(t, service.ReplaceAsync("owner", "session-a", []ServerConfig{server("shared")}))
	internalB := <-backend.started
	require.NotEqual(t, internalA, internalB)
	require.False(t, service.Access("session-a").AllowsMCPServer(internalA), "old generation must lose access immediately")
	waitConnected(t, service, "session-a", dynamicID("shared"))
	require.True(t, service.Access("session-a").AllowsMCPServer(internalB))
	require.False(t, service.Access("session-a").AllowsMCPServer(internalA), "tombstone must deny stale tools permanently")
	require.Eventually(t, func() bool {
		_, exists := store.MCPSnapshot()[internalA]
		return !exists
	}, time.Second, time.Millisecond)
	_, exists := store.MCPSnapshot()[internalB]
	require.True(t, exists)

	events, err := hub.ReplayAfter("session-a", 0)
	require.NoError(t, err)
	require.NotEmpty(t, events)
	require.Equal(t, sessionevent.KindMCPStatus, events[len(events)-1].Kind)
}

func TestSessionIsolationPartialFailureAndReplacementDeduplication(t *testing.T) {
	store := testStore(t)
	backend := newMockBackend()
	backend.failSuffix = "bad"
	service := New(store, backend, nil, Config{})
	t.Cleanup(func() { service.Close(t.Context()) })

	require.NoError(t, service.ReplaceAsync("owner-a", "session-a", []ServerConfig{
		server("good"), server("bad"), server("good"), {Name: ""},
	}))
	require.NoError(t, service.ReplaceAsync("owner-b", "session-b", []ServerConfig{server("other")}))
	internal := map[string]string{}
	for range 3 {
		name := <-backend.started
		switch {
		case strings.HasSuffix(name, "good"):
			internal["good"] = name
		case strings.HasSuffix(name, "bad"):
			internal["bad"] = name
		case strings.HasSuffix(name, "other"):
			internal["other"] = name
		}
	}
	waitConnected(t, service, "session-a", dynamicID("good"))
	waitConnected(t, service, "session-b", dynamicID("other"))
	require.Eventually(t, func() bool {
		value, err := service.Status("session-a", dynamicID("bad"))
		return err == nil && value.Status == StatusFailed
	}, time.Second, time.Millisecond)
	require.True(t, service.Access("session-a").AllowsMCPServer(internal["good"]))
	require.False(t, service.Access("session-a").AllowsMCPServer(internal["bad"]))
	require.False(t, service.Access("session-a").AllowsMCPServer(internal["other"]))
	require.True(t, service.Access("session-b").AllowsMCPServer(internal["other"]))
	_, failedConfigExists := store.MCPSnapshot()[internal["bad"]]
	require.False(t, failedConfigExists, "partial failure must remove ephemeral config")
	backend.setState(internal["bad"], BackendInfo{State: BackendStarting})
	require.Eventually(t, func() bool {
		value, statusErr := service.Status("session-a", dynamicID("bad"))
		return statusErr == nil && value.Status == StatusReconnecting
	}, time.Second, time.Millisecond)
	servers, _, err := service.List("session-a")
	require.NoError(t, err)
	dynamicServers := make([]Server, 0)
	for _, value := range servers {
		if value.Scope == ScopeSession {
			dynamicServers = append(dynamicServers, value)
		}
	}
	require.Len(t, dynamicServers, 2)
}

func TestDynamicReconnectDisableAndShutdown(t *testing.T) {
	store := testStore(t)
	backend := newMockBackend()
	service := New(store, backend, nil, Config{})

	require.NoError(t, service.ReplaceAsync("owner", "session", []ServerConfig{server("dynamic")}))
	first := <-backend.started
	waitConnected(t, service, "session", dynamicID("dynamic"))
	value, err := service.ReconnectAsync("session", dynamicID("dynamic"))
	require.NoError(t, err)
	require.Equal(t, StatusReconnecting, value.Status)
	second := <-backend.started
	require.NotEqual(t, first, second)
	require.False(t, service.Access("session").AllowsMCPServer(first))
	waitConnected(t, service, "session", dynamicID("dynamic"))
	require.True(t, service.Access("session").AllowsMCPServer(second))

	value, err = service.DisableAsync("session", dynamicID("dynamic"))
	require.NoError(t, err)
	require.Equal(t, StatusStopping, value.Status)
	require.False(t, service.Access("session").AllowsMCPServer(second))
	require.Eventually(t, func() bool {
		current, statusErr := service.Status("session", dynamicID("dynamic"))
		return statusErr == nil && current.Status == StatusDisabled
	}, time.Second, time.Millisecond)
	_, exists := store.MCPSnapshot()[second]
	require.False(t, exists)

	service.CloseOwner(t.Context(), "owner")
	_, _, err = service.List("session")
	require.NoError(t, err, "a later GUI query may re-establish a static-only projection")
	service.Close(t.Context())
	_, err = service.ReconnectAsync("session", dynamicID("dynamic"))
	require.ErrorIs(t, err, ErrClosed)
	_, _, disables, marked := backend.snapshot()
	require.GreaterOrEqual(t, len(disables), 2)
	require.Len(t, marked, 2)
}

func TestDynamicReconnectDoesNotCancelSiblingStartup(t *testing.T) {
	store := testStore(t)
	backend := newMockBackend()
	backend.release = make(chan struct{})
	service := New(store, backend, nil, Config{})
	t.Cleanup(func() { service.Close(t.Context()) })

	require.NoError(t, service.ReplaceAsync("owner", "session", []ServerConfig{server("one"), server("two")}))
	started := map[string]string{}
	for range 2 {
		name := <-backend.started
		if strings.HasSuffix(name, "one") {
			started["one"] = name
		} else if strings.HasSuffix(name, "two") {
			started["two"] = name
		}
	}
	require.Len(t, started, 2)

	_, err := service.ReconnectAsync("session", dynamicID("one"))
	require.NoError(t, err)
	reconnected := <-backend.started
	require.NotEqual(t, started["one"], reconnected)
	close(backend.release)

	waitConnected(t, service, "session", dynamicID("one"))
	waitConnected(t, service, "session", dynamicID("two"))
	require.True(t, service.Access("session").AllowsMCPServer(reconnected))
	require.True(t, service.Access("session").AllowsMCPServer(started["two"]))
}

func TestCloseOwnerDoesNotRevokeReplacementOwner(t *testing.T) {
	store := testStore(t)
	backend := newMockBackend()
	service := New(store, backend, nil, Config{})
	t.Cleanup(func() { service.Close(t.Context()) })

	require.NoError(t, service.ReplaceAsync("owner-a", "session", []ServerConfig{server("dynamic")}))
	first := <-backend.started
	waitConnected(t, service, "session", dynamicID("dynamic"))
	require.NoError(t, service.ReplaceAsync("owner-b", "session", []ServerConfig{server("dynamic")}))
	second := <-backend.started
	waitConnected(t, service, "session", dynamicID("dynamic"))
	require.NotEqual(t, first, second)

	service.CloseOwner(t.Context(), "owner-a")
	require.True(t, service.Access("session").AllowsMCPServer(second))
	current, err := service.Status("session", dynamicID("dynamic"))
	require.NoError(t, err)
	require.Equal(t, StatusConnected, current.Status)
}

func TestStaticControlAndBoundedRedactedLogs(t *testing.T) {
	store := testStore(t)
	store.AddMCP("static", config.MCPConfig{
		Type: config.MCPHttp, URL: "https://user:query-secret@example.test/mcp?token=query-secret",
		Headers: map[string]string{"Authorization": "Bearer header-secret"},
		Env:     map[string]string{"API_KEY": "env-secret"},
	})
	backend := newMockBackend()
	backend.setState("static", BackendInfo{State: BackendConnected, Counts: Counts{Tools: 1}})
	service := New(store, backend, nil, Config{MaxLogEntries: 3, MaxLogBytes: 4096})
	t.Cleanup(func() { service.Close(t.Context()) })
	servers, _, err := service.List("session")
	require.NoError(t, err)
	foundStatic := false
	for _, value := range servers {
		if value.ID == staticID("static") {
			foundStatic = true
			require.Equal(t, StatusConnected, value.Status)
			require.Equal(t, 1, value.Counts.Tools)
		}
	}
	require.True(t, foundStatic, "configured static server must be projected")

	for index := range 5 {
		backend.log("static", map[string]any{
			"index": index, "header": "Bearer header-secret", "env": "env-secret",
			"query": "query-secret", "url": "https://user:query-secret@example.test/mcp?token=query-secret",
			"token": "sk-abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ12",
		})
	}
	require.Eventually(t, func() bool {
		page, pageErr := service.Logs("session", staticID("static"), 0, 1000)
		return pageErr == nil && page.LatestSequence == 5
	}, time.Second, time.Millisecond)
	page, err := service.Logs("session", staticID("static"), 0, 1000)
	require.NoError(t, err)
	require.Len(t, page.Entries, 3)
	require.True(t, page.Truncated)
	for _, entry := range page.Entries {
		require.NotContains(t, entry.Message, "header-secret")
		require.NotContains(t, entry.Message, "env-secret")
		require.NotContains(t, entry.Message, "query-secret")
		require.NotContains(t, entry.Message, "example.test")
		require.NotContains(t, entry.Message, "sk-abcdefghijklmnopqrstuvwxyz")
	}

	value, err := service.ReconnectAsync("session", staticID("static"))
	require.NoError(t, err)
	require.Equal(t, StatusReconnecting, value.Status)
	<-backend.started
	require.Eventually(t, func() bool {
		_, reconnects, _, _ := backend.snapshot()
		return len(reconnects) == 1
	}, time.Second, time.Millisecond)
	value, err = service.DisableAsync("session", staticID("static"))
	require.NoError(t, err)
	require.Equal(t, StatusStopping, value.Status)
	require.Eventually(t, func() bool {
		current, statusErr := service.Status("session", staticID("static"))
		return statusErr == nil && current.Status == StatusDisabled
	}, time.Second, time.Millisecond)
}

func TestStaticOperationsCommitPersistentStateInRequestOrder(t *testing.T) {
	store := testStore(t)
	store.AddMCP("static", config.MCPConfig{
		Type: config.MCPStdio, Command: "mock", Disabled: true,
	})
	backend := newMockBackend()
	backend.reconnectRelease = make(chan struct{})
	backend.ignoreReconnectCancel = true
	service := New(store, backend, nil, Config{})
	t.Cleanup(func() { service.Close(t.Context()) })

	_, _, err := service.List("session")
	require.NoError(t, err)
	_, err = service.ReconnectAsync("session", staticID("static"))
	require.NoError(t, err)
	require.Equal(t, "static", <-backend.started)
	require.False(t, store.MCPSnapshot()["static"].Disabled)

	_, err = service.DisableAsync("session", staticID("static"))
	require.NoError(t, err)
	require.Never(t, func() bool {
		_, _, disables, _ := backend.snapshot()
		return len(disables) != 0
	}, 50*time.Millisecond, time.Millisecond, "newer disable must wait for the older config mutation")

	close(backend.reconnectRelease)
	require.Eventually(t, func() bool {
		current, statusErr := service.Status("session", staticID("static"))
		return statusErr == nil && current.Status == StatusDisabled && store.MCPSnapshot()["static"].Disabled
	}, time.Second, time.Millisecond)
}

func TestCapacityAndSessionCloseRevokeBeforeBlockedCleanup(t *testing.T) {
	store := testStore(t)
	backend := newMockBackend()
	backend.release = make(chan struct{})
	service := New(store, backend, nil, Config{MaxSessions: 1, MaxServersPerSession: 1})
	t.Cleanup(func() { service.Close(t.Context()) })

	require.NoError(t, service.ReplaceAsync("owner", "one", []ServerConfig{server("a"), server("b")}))
	internal := <-backend.started
	require.ErrorIs(t, service.ReplaceAsync("owner", "two", nil), ErrCapacity)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	service.CloseSession(ctx, "one")
	require.False(t, service.Access("one").AllowsMCPServer(internal))
	_, exists := store.MCPSnapshot()[internal]
	require.False(t, exists)
	close(backend.release)
}
