package guiapi

import (
	"context"
	"encoding/base64"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/sessionevent"
	"github.com/charmbracelet/crush/internal/terminal"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestTerminalHandlersLifecycleOffsetsEventsAndIdempotency(t *testing.T) {
	t.Parallel()

	env := newTerminalHandlerEnvironment(t, true)
	openParams := terminalOpenParams{
		SessionID: env.sessionID, Command: "fake", Args: []string{"arg"}, Cols: 80, Rows: 24,
		Env:             map[string]string{"SECRET_TOKEN": "must-not-enter-permission-metadata"},
		ClientRequestID: uuid.NewString(),
	}
	opened, rpcErr := env.service.HandleExtension(t.Context(), "crush/terminal/open", mustRawJSON(t, openParams))
	require.Nil(t, rpcErr)
	metadata := opened.(terminalResult)
	require.Equal(t, "running", metadata.State)
	require.Equal(t, 1, env.permissions.callCount())
	replayed, rpcErr := env.service.HandleExtension(t.Context(), "crush/terminal/open", mustRawJSON(t, openParams))
	require.Nil(t, rpcErr)
	require.Equal(t, opened, replayed)
	require.Equal(t, 1, env.factory.count())
	require.Equal(t, 1, env.permissions.callCount())

	proc := env.factory.latest()
	inputParams := terminalInputParams{
		SessionID: env.sessionID, TerminalID: metadata.TerminalID,
		Bytes: base64.StdEncoding.EncodeToString([]byte("input")), ClientRequestID: uuid.NewString(),
	}
	input, rpcErr := env.service.HandleExtension(t.Context(), "crush/terminal/input", mustRawJSON(t, inputParams))
	require.Nil(t, rpcErr)
	require.Equal(t, 5, input.(terminalInputResult).Written)
	_, rpcErr = env.service.HandleExtension(t.Context(), "crush/terminal/input", mustRawJSON(t, inputParams))
	require.Nil(t, rpcErr)
	require.Equal(t, []byte("input"), proc.writtenBytes())
	require.Equal(t, 2, env.permissions.callCount())

	resized, rpcErr := env.service.HandleExtension(t.Context(), "crush/terminal/resize", mustRawJSON(t, terminalResizeParams{
		SessionID: env.sessionID, TerminalID: metadata.TerminalID, Cols: 120, Rows: 40, ClientRequestID: uuid.NewString(),
	}))
	require.Nil(t, rpcErr)
	require.Equal(t, 120, resized.(terminalResult).Cols)
	proc.emit([]byte("0123456789"))
	require.Eventually(t, func() bool {
		value, err := env.manager.Snapshot(env.service.blobOwnerID(), env.sessionID, metadata.TerminalID, 0)
		return err == nil && value.EndOffset == 10
	}, time.Second, time.Millisecond)
	snapshot, rpcErr := env.service.HandleExtension(t.Context(), "crush/terminal/snapshot", mustRawJSON(t, terminalSnapshotParams{
		SessionID: env.sessionID, TerminalID: metadata.TerminalID, AfterOffset: 4,
	}))
	require.Nil(t, rpcErr)
	projection := snapshot.(terminalSnapshotResult)
	require.Equal(t, uint64(4), projection.StartOffset)
	require.Equal(t, uint64(10), projection.EndOffset)
	require.Equal(t, []byte("456789"), projection.Data)

	killParams := terminalKillParams{
		SessionID: env.sessionID, TerminalID: metadata.TerminalID,
		Signal: "interrupt", ClientRequestID: uuid.NewString(),
	}
	killed, rpcErr := env.service.HandleExtension(t.Context(), "crush/terminal/kill", mustRawJSON(t, killParams))
	require.Nil(t, rpcErr)
	require.True(t, killed.(terminalKillResult).Acknowledged)
	_, rpcErr = env.service.HandleExtension(t.Context(), "crush/terminal/kill", mustRawJSON(t, killParams))
	require.Nil(t, rpcErr)
	require.Equal(t, 3, env.permissions.callCount())
	require.Eventually(t, func() bool {
		value, err := env.manager.Get(env.service.blobOwnerID(), env.sessionID, metadata.TerminalID)
		return err == nil && value.State == terminal.StateKilled
	}, time.Second, time.Millisecond)

	require.Eventually(t, func() bool {
		events, err := env.hub.ReplayAfter(env.sessionID, 0)
		return err == nil && slices.Contains(eventKinds(events), sessionevent.KindTerminalOutput) &&
			slices.Contains(eventKinds(events), sessionevent.KindTerminalExited)
	}, time.Second, time.Millisecond)
	events, err := env.hub.ReplayAfter(env.sessionID, 0)
	require.NoError(t, err)
	kinds := eventKinds(events)
	require.Less(t, slices.Index(kinds, sessionevent.KindTerminalOutput), slices.Index(kinds, sessionevent.KindTerminalExited))
	requests := env.permissions.requests()
	for _, request := range requests {
		require.Equal(t, "bash", request.ToolName)
		require.Equal(t, "execute", request.Action)
		require.Equal(t, env.sessionID, request.SessionID)
	}
	openPermission := requests[0].Params.(tools.BashPermissionsParams)
	require.Contains(t, openPermission.Command, "fake")
	require.Contains(t, openPermission.Command, "arg")
	require.True(t, openPermission.RunInBackground)
	require.NotContains(t, openPermission.Command, "must-not-enter-permission-metadata")
	inputPermission := requests[1].Params.(tools.BashPermissionsParams)
	require.Equal(t, openPermission.Command, inputPermission.Command)
	require.NotContains(t, inputPermission.Command, "input")
}

func TestTerminalPermissionDenialAndOwnershipIsolation(t *testing.T) {
	t.Parallel()

	denied := newTerminalHandlerEnvironment(t, false)
	deniedParams := terminalOpenParams{
		SessionID: denied.sessionID, Command: "fake", ClientRequestID: uuid.NewString(),
	}
	_, rpcErr := denied.service.HandleExtension(t.Context(), "crush/terminal/open", mustRawJSON(t, deniedParams))
	require.Equal(t, errorPermissionDenied, rpcErr.Message)
	_, rpcErr = denied.service.HandleExtension(t.Context(), "crush/terminal/open", mustRawJSON(t, deniedParams))
	require.Equal(t, errorPermissionDenied, rpcErr.Message)
	require.Equal(t, 1, denied.permissions.callCount())
	require.Zero(t, denied.factory.count())

	allowed := newTerminalHandlerEnvironment(t, true)
	opened, rpcErr := allowed.service.HandleExtension(t.Context(), "crush/terminal/open", mustRawJSON(t, terminalOpenParams{
		SessionID: allowed.sessionID, Command: "fake", ClientRequestID: uuid.NewString(),
	}))
	require.Nil(t, rpcErr)
	id := opened.(terminalResult).TerminalID
	peer := NewService(allowed.hub)
	peer.SetSessionContentSources(fixedSessionReader{id: allowed.sessionID}, nil, nil)
	peer.SetTerminalServices(allowed.manager, allowed.permissions)
	require.Nil(t, peer.NegotiateExperimental(experimentalSelection(t, Selection{
		ProtocolVersion: ProtocolVersion, Features: []Feature{FeatureTerminal},
	})))
	_, rpcErr = peer.HandleExtension(t.Context(), "crush/terminal/snapshot", mustRawJSON(t, terminalSnapshotParams{
		SessionID: allowed.sessionID, TerminalID: id,
	}))
	require.Equal(t, errorTerminalNotFound, rpcErr.Message)
	peer.Close()
	allowed.service.Close()
	_, err := allowed.manager.Get(allowed.service.blobOwnerID(), allowed.sessionID, id)
	require.ErrorIs(t, err, terminal.ErrNotFound)
}

func TestTerminalRejectsOversizedInputBeforePermission(t *testing.T) {
	env := newTerminalHandlerEnvironment(t, true)
	_, rpcErr := env.service.HandleExtension(t.Context(), "crush/terminal/input", mustRawJSON(t, terminalInputParams{
		SessionID: env.sessionID, TerminalID: uuid.NewString(),
		Bytes:           strings.Repeat("A", base64.StdEncoding.EncodedLen(maxTerminalInputBytes+1)),
		ClientRequestID: uuid.NewString(),
	}))
	require.Equal(t, errorPayloadTooLarge, rpcErr.Message)
	require.Zero(t, env.permissions.callCount())
}

func TestCoordinatorSnapshotIncludesBoundedTerminalMetadata(t *testing.T) {
	t.Parallel()

	factory := newGUITerminalFactory()
	manager := terminal.New(terminal.Config{Factory: factory})
	t.Cleanup(manager.Close)
	metadata, err := manager.Open(t.Context(), terminal.OpenRequest{
		ClientID: "client", SessionID: "session-1", Command: "fake",
	})
	require.NoError(t, err)
	source := NewCoordinatorSnapshotSource(fakeSnapshotCoordinator{})
	source.SetTerminalSource(manager)
	projection := source.SnapshotRuntime("session-1")
	require.Equal(t, []sessionevent.ResourceSummary{{ID: metadata.ID, Status: "running"}}, projection.Terminals)
}

type terminalHandlerEnvironment struct {
	service     *Service
	manager     *terminal.Manager
	factory     *guiTerminalFactory
	permissions *terminalPermissionService
	hub         *sessionevent.Hub
	sessionID   string
}

func newTerminalHandlerEnvironment(t *testing.T, allow bool) *terminalHandlerEnvironment {
	t.Helper()
	hub := sessionevent.NewHub(sessionevent.Config{})
	factory := newGUITerminalFactory()
	manager := terminal.New(terminal.Config{
		Factory: factory,
		OnOutput: func(sessionID, terminalID string, offset uint64, data []byte) {
			_, _ = hub.Publish(sessionID, sessionevent.NewEvent{
				Kind: sessionevent.KindTerminalOutput, Delivery: sessionevent.DeliveryMerge,
				CoalesceKey: terminalID,
				Payload:     sessionevent.TerminalOutput{TerminalID: terminalID, Offset: offset, Data: data},
			})
		},
		OnExit: func(sessionID, terminalID string, exit terminal.Exit) {
			_, _ = hub.Publish(sessionID, sessionevent.NewEvent{
				Kind: sessionevent.KindTerminalExited, Delivery: sessionevent.DeliveryReliable,
				Payload: sessionevent.TerminalExit{TerminalID: terminalID, State: string(exit.State), Code: exit.Code, Signal: exit.Signal, Timestamp: exit.Timestamp.UnixMilli(), Offset: exit.Offset},
			})
		},
	})
	permissions := &terminalPermissionService{Service: permission.NewPermissionService(t.TempDir(), true, nil), allow: allow}
	service := NewService(hub)
	service.SetSessionContentSources(fixedSessionReader{id: "session-1"}, nil, nil)
	service.SetTerminalServices(manager, permissions)
	require.Nil(t, service.NegotiateExperimental(experimentalSelection(t, Selection{
		ProtocolVersion: ProtocolVersion, Features: []Feature{FeatureTerminal},
	})))
	t.Cleanup(func() {
		service.Close()
		manager.Close()
		hub.Close()
	})
	return &terminalHandlerEnvironment{service: service, manager: manager, factory: factory, permissions: permissions, hub: hub, sessionID: "session-1"}
}

type terminalPermissionService struct {
	permission.Service
	mu    sync.Mutex
	allow bool
	calls []permission.CreatePermissionRequest
}

func (s *terminalPermissionService) Request(_ context.Context, request permission.CreatePermissionRequest) (bool, error) {
	s.mu.Lock()
	s.calls = append(s.calls, request)
	s.mu.Unlock()
	return s.allow, nil
}

func (s *terminalPermissionService) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *terminalPermissionService) requests() []permission.CreatePermissionRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]permission.CreatePermissionRequest(nil), s.calls...)
}

type guiTerminalFactory struct {
	mu        sync.Mutex
	processes []*guiTerminalProcess
}

func newGUITerminalFactory() *guiTerminalFactory { return &guiTerminalFactory{} }
func (f *guiTerminalFactory) Start(context.Context, terminal.OpenRequest) (terminal.Process, error) {
	value := &guiTerminalProcess{readCh: make(chan []byte, 16), exitCh: make(chan terminal.ProcessExit, 1)}
	f.mu.Lock()
	f.processes = append(f.processes, value)
	f.mu.Unlock()
	return value, nil
}
func (f *guiTerminalFactory) latest() *guiTerminalProcess {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.processes[len(f.processes)-1]
}
func (f *guiTerminalFactory) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.processes)
}

type guiTerminalProcess struct {
	mu        sync.Mutex
	readCh    chan []byte
	exitCh    chan terminal.ProcessExit
	written   []byte
	closed    bool
	closeOnce sync.Once
}

func (p *guiTerminalProcess) Read(value []byte) (int, error) {
	chunk, ok := <-p.readCh
	if !ok {
		return 0, io.EOF
	}
	return copy(value, chunk), nil
}
func (p *guiTerminalProcess) Write(value []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.written = append(p.written, value...)
	return len(value), nil
}
func (*guiTerminalProcess) Resize(int, int) error { return nil }
func (p *guiTerminalProcess) Kill(signal string) error {
	select {
	case p.exitCh <- terminal.ProcessExit{Code: 1, Signal: signal}:
	default:
	}
	return p.Close()
}
func (p *guiTerminalProcess) Wait(ctx context.Context) (terminal.ProcessExit, error) {
	select {
	case value := <-p.exitCh:
		return value, nil
	case <-ctx.Done():
		return terminal.ProcessExit{}, ctx.Err()
	}
}
func (p *guiTerminalProcess) Close() error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
		close(p.readCh)
	})
	return nil
}
func (p *guiTerminalProcess) emit(value []byte) { p.readCh <- append([]byte(nil), value...) }
func (p *guiTerminalProcess) writtenBytes() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.written...)
}

func eventKinds(events []sessionevent.Event) []sessionevent.Kind {
	result := make([]sessionevent.Kind, len(events))
	for index := range events {
		result[index] = events[index].Kind
	}
	return result
}
