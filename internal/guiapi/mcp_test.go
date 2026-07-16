package guiapi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/agent"
	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	crushlog "github.com/charmbracelet/crush/internal/log"
	"github.com/charmbracelet/crush/internal/mcplifecycle"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/sessionevent"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type guiMCPBackend struct {
	mu         sync.Mutex
	events     chan mcplifecycle.BackendEvent
	states     map[string]mcplifecycle.BackendInfo
	connects   []string
	reconnects []string
	disables   []string
}

func newGUIMCPBackend() *guiMCPBackend {
	return &guiMCPBackend{events: make(chan mcplifecycle.BackendEvent, 128), states: make(map[string]mcplifecycle.BackendInfo)}
}

func (b *guiMCPBackend) Connect(_ context.Context, _ *config.ConfigStore, name string) error {
	b.mu.Lock()
	b.connects = append(b.connects, name)
	b.states[name] = mcplifecycle.BackendInfo{State: mcplifecycle.BackendConnected, Counts: mcplifecycle.Counts{Tools: 1}}
	b.mu.Unlock()
	b.events <- mcplifecycle.BackendEvent{Type: mcplifecycle.BackendEventState, Name: name, State: mcplifecycle.BackendConnected, Counts: mcplifecycle.Counts{Tools: 1}}
	return nil
}

func (b *guiMCPBackend) Reconnect(ctx context.Context, store *config.ConfigStore, name string) error {
	b.mu.Lock()
	b.reconnects = append(b.reconnects, name)
	b.mu.Unlock()
	return b.Connect(ctx, store, name)
}

func (b *guiMCPBackend) Disable(_ context.Context, _ *config.ConfigStore, name string) error {
	b.mu.Lock()
	b.disables = append(b.disables, name)
	b.states[name] = mcplifecycle.BackendInfo{State: mcplifecycle.BackendDisabled}
	b.mu.Unlock()
	return nil
}

func (b *guiMCPBackend) State(name string) (mcplifecycle.BackendInfo, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	value, ok := b.states[name]
	return value, ok
}

func (b *guiMCPBackend) Subscribe(ctx context.Context) <-chan mcplifecycle.BackendEvent {
	output := make(chan mcplifecycle.BackendEvent)
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

func (*guiMCPBackend) MarkScoped(string) {}

func (b *guiMCPBackend) log(name string, data any) {
	b.events <- mcplifecycle.BackendEvent{
		Type: mcplifecycle.BackendEventLog, Name: name,
		Log: mcplifecycle.BackendLog{Timestamp: time.Unix(100, 0), Level: "info", Logger: "mock", Data: data},
	}
}

func (b *guiMCPBackend) snapshot() (connects, reconnects, disables []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.connects...), append([]string(nil), b.reconnects...), append([]string(nil), b.disables...)
}

func guiMCPStore(t *testing.T) *config.ConfigStore {
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

func negotiatedMCPService(t *testing.T, lifecycle *mcplifecycle.Service, reader SessionReader) *Service {
	t.Helper()
	service := NewService(sessionevent.NewHub(sessionevent.Config{}))
	service.SetMCPLifecycleService(lifecycle)
	service.SetSessionContentSources(reader, nil, nil)
	require.Nil(t, service.NegotiateExperimental(experimentalSelection(t, Selection{
		ProtocolVersion: ProtocolVersion, Features: []Feature{FeatureMCPControl},
	})))
	t.Cleanup(service.Close)
	return service
}

type multiSessionReader map[string]session.Session

func (r multiSessionReader) Get(_ context.Context, id string) (session.Session, error) {
	value, ok := r[id]
	if !ok {
		return session.Session{}, errors.New("not found")
	}
	return value, nil
}

func waitMCPStatus(t *testing.T, lifecycle *mcplifecycle.Service, sessionID, serverID string, status mcplifecycle.Status) mcplifecycle.Server {
	t.Helper()
	var result mcplifecycle.Server
	require.Eventually(t, func() bool {
		value, err := lifecycle.Status(sessionID, serverID)
		if err == nil {
			result = value
		}
		return err == nil && value.Status == status
	}, time.Second, time.Millisecond)
	return result
}

func TestMCPListStatusIsolationAndSafeProjection(t *testing.T) {
	store := guiMCPStore(t)
	backend := newGUIMCPBackend()
	hub := sessionevent.NewHub(sessionevent.Config{})
	t.Cleanup(hub.Close)
	lifecycle := mcplifecycle.New(store, backend, hub, mcplifecycle.Config{})
	t.Cleanup(func() { lifecycle.Close(t.Context()) })
	require.NoError(t, lifecycle.ReplaceAsync("owner", "a", []mcplifecycle.ServerConfig{{
		Name: "private", Config: config.MCPConfig{
			Type: config.MCPHttp, URL: "https://user:password@example.test/mcp",
			Headers: map[string]string{"Authorization": "Bearer secret"},
		},
	}}))
	require.NoError(t, lifecycle.ReplaceAsync("owner", "b", []mcplifecycle.ServerConfig{{Name: "other", Config: config.MCPConfig{Type: config.MCPStdio, Command: "mock"}}}))
	waitMCPStatus(t, lifecycle, "a", "session:private", mcplifecycle.StatusConnected)
	waitMCPStatus(t, lifecycle, "b", "session:other", mcplifecycle.StatusConnected)
	reader := multiSessionReader{"a": {ID: "a"}, "b": {ID: "b"}}
	service := negotiatedMCPService(t, lifecycle, reader)

	result, rpcErr := service.HandleExtension(t.Context(), "crush/mcp/list", mustRawJSON(t, mcpSessionParams{SessionID: "a"}))
	require.Nil(t, rpcErr)
	list := result.(mcpListResult)
	foundPrivate, foundOther := false, false
	for _, server := range list.Servers {
		foundPrivate = foundPrivate || server.ServerID == "session:private"
		foundOther = foundOther || server.ServerID == "session:other"
		require.NotContains(t, server.ServerID, "acp-")
	}
	require.True(t, foundPrivate)
	require.False(t, foundOther)
	wire := string(mustRawJSON(t, list))
	for _, secret := range []string{"password", "Bearer secret", "example.test", "acp-"} {
		require.NotContains(t, wire, secret)
	}

	result, rpcErr = service.HandleExtension(t.Context(), "crush/mcp/status", mustRawJSON(t, mcpServerParams{SessionID: "a", ServerID: "session:private"}))
	require.Nil(t, rpcErr)
	require.Equal(t, "connected", result.(mcpServerResult).Status)
	_, rpcErr = service.HandleExtension(t.Context(), "crush/mcp/status", mustRawJSON(t, mcpServerParams{SessionID: "a", ServerID: "session:other"}))
	require.Equal(t, "CRUSH_MCP_NOT_FOUND", rpcErr.Message)
}

func TestMCPReconnectDisableIdempotencyAndEvents(t *testing.T) {
	store := guiMCPStore(t)
	backend := newGUIMCPBackend()
	hub := sessionevent.NewHub(sessionevent.Config{})
	t.Cleanup(hub.Close)
	lifecycle := mcplifecycle.New(store, backend, hub, mcplifecycle.Config{})
	t.Cleanup(func() { lifecycle.Close(t.Context()) })
	require.NoError(t, lifecycle.ReplaceAsync("owner", "session", []mcplifecycle.ServerConfig{{Name: "dynamic", Config: config.MCPConfig{Type: config.MCPStdio, Command: "mock"}}}))
	waitMCPStatus(t, lifecycle, "session", "session:dynamic", mcplifecycle.StatusConnected)
	service := negotiatedMCPService(t, lifecycle, fixedSessionReader{id: "session"})

	request := mcpMutationParams{SessionID: "session", ServerID: "session:dynamic", ClientRequestID: uuid.NewString()}
	first, rpcErr := service.HandleExtension(t.Context(), "crush/mcp/reconnect", mustRawJSON(t, request))
	require.Nil(t, rpcErr)
	second, rpcErr := service.HandleExtension(t.Context(), "crush/mcp/reconnect", mustRawJSON(t, request))
	require.Nil(t, rpcErr)
	require.Equal(t, first, second)
	waitMCPStatus(t, lifecycle, "session", "session:dynamic", mcplifecycle.StatusConnected)
	require.Eventually(t, func() bool {
		_, reconnects, _ := backend.snapshot()
		return len(reconnects) == 1
	}, time.Second, time.Millisecond)

	request.ServerID = "missing"
	_, rpcErr = service.HandleExtension(t.Context(), "crush/mcp/reconnect", mustRawJSON(t, request))
	require.Equal(t, errorIdempotencyConflict, rpcErr.Message)
	disable := mcpMutationParams{SessionID: "session", ServerID: "session:dynamic", ClientRequestID: uuid.NewString()}
	result, rpcErr := service.HandleExtension(t.Context(), "crush/mcp/disable", mustRawJSON(t, disable))
	require.Nil(t, rpcErr)
	require.Equal(t, "stopping", result.(mcpServerResult).Status)
	waitMCPStatus(t, lifecycle, "session", "session:dynamic", mcplifecycle.StatusDisabled)

	events, err := hub.ReplayAfter("session", 0)
	require.NoError(t, err)
	require.NotEmpty(t, events)
	latest := events[len(events)-1]
	require.Equal(t, sessionevent.KindMCPStatus, latest.Kind)
	wirePayload := toWirePayload(latest.Payload)
	raw := string(mustRawJSON(t, wirePayload))
	require.Contains(t, raw, `"status":"disabled"`)
	require.NotContains(t, raw, "acp-")
}

func TestMCPLogsAreBoundedRedactedAndSnapshotIncludesStatus(t *testing.T) {
	store := guiMCPStore(t)
	backend := newGUIMCPBackend()
	lifecycle := mcplifecycle.New(store, backend, nil, mcplifecycle.Config{MaxLogEntries: 4, MaxLogBytes: 4096})
	t.Cleanup(func() { lifecycle.Close(t.Context()) })
	require.NoError(t, lifecycle.ReplaceAsync("owner", "session", []mcplifecycle.ServerConfig{{
		Name: "dynamic", Config: config.MCPConfig{
			Type: config.MCPStdio, Command: "mock",
			Env: map[string]string{"TOKEN": "mcp-secret-value"},
		},
	}}))
	waitMCPStatus(t, lifecycle, "session", "session:dynamic", mcplifecycle.StatusConnected)
	connects, _, _ := backend.snapshot()
	require.Len(t, connects, 1)
	for index := range 6 {
		backend.log(connects[0], map[string]any{"index": index, "token": "mcp-secret-value"})
	}
	service := negotiatedMCPService(t, lifecycle, fixedSessionReader{id: "session"})
	require.Eventually(t, func() bool {
		result, rpcErr := service.HandleExtension(t.Context(), "crush/mcp/logs", mustRawJSON(t, mcpLogsParams{
			SessionID: "session", ServerID: "session:dynamic", Limit: 1000,
		}))
		return rpcErr == nil && result.(mcpLogsResult).LatestSequence == 6
	}, time.Second, time.Millisecond)
	result, rpcErr := service.HandleExtension(t.Context(), "crush/mcp/logs", mustRawJSON(t, mcpLogsParams{
		SessionID: "session", ServerID: "session:dynamic", Limit: 1000,
	}))
	require.Nil(t, rpcErr)
	logs := result.(mcpLogsResult)
	require.Len(t, logs.Entries, 4)
	require.True(t, logs.Truncated)
	require.NotContains(t, string(mustRawJSON(t, logs)), "mcp-secret-value")

	snapshotSource := NewCoordinatorSnapshotSource(nil)
	// A nil Coordinator is permitted for a resource-only snapshot projection.
	snapshotSource.coordinator = &emptyCoordinatorSnapshotReader{}
	snapshotSource.SetMCPSource(lifecycle)
	runtime := snapshotSource.SnapshotRuntime("session")
	require.Contains(t, runtime.MCPServers, sessionevent.ResourceSummary{ID: "session:dynamic", Status: "connected"})
}

type emptyCoordinatorSnapshotReader struct{}

func (*emptyCoordinatorSnapshotReader) IsSessionBusy(string) bool { return false }
func (*emptyCoordinatorSnapshotReader) QueuedPrompts(string) int  { return 0 }
func (*emptyCoordinatorSnapshotReader) IsQueuePaused(string) bool { return false }
func (*emptyCoordinatorSnapshotReader) ModelForSession(string) (agent.Model, bool) {
	return agent.Model{}, false
}

func TestGUITurnInstallsLiveMCPAccessScope(t *testing.T) {
	store := guiMCPStore(t)
	backend := newGUIMCPBackend()
	lifecycle := mcplifecycle.New(store, backend, nil, mcplifecycle.Config{})
	t.Cleanup(func() { lifecycle.Close(t.Context()) })
	require.NoError(t, lifecycle.ReplaceAsync("owner", "session", []mcplifecycle.ServerConfig{{Name: "dynamic", Config: config.MCPConfig{Type: config.MCPStdio, Command: "mock"}}}))
	waitMCPStatus(t, lifecycle, "session", "session:dynamic", mcplifecycle.StatusConnected)
	connects, _, _ := backend.snapshot()
	service := NewService(nil)
	service.SetMCPLifecycleService(lifecycle)
	service.SetSessionContentSources(fixedSessionReader{id: "session"}, nil, nil)
	t.Cleanup(service.Close)
	input, rpcErr := service.turnInput(t.Context(), "session", []turnContentBlock{{Type: "text", Text: "hello"}}, session.InferenceOverrides{})
	require.Nil(t, rpcErr)
	require.NotNil(t, input.Scope)
	runCtx := input.Scope(context.Background())
	require.True(t, agenttools.MCPServerAllowed(runCtx, connects[0]))
	require.True(t, agenttools.MCPServerAllowed(runCtx, "static"))

	_, err := lifecycle.DisableAsync("session", "session:dynamic")
	require.NoError(t, err)
	require.False(t, agenttools.MCPServerAllowed(runCtx, connects[0]), "same context must observe immediate revocation")
}
