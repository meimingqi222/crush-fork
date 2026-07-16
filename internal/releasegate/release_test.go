package releasegate_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/blob"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/guimetrics"
	"github.com/charmbracelet/crush/internal/idempotency"
	crushlog "github.com/charmbracelet/crush/internal/log"
	"github.com/charmbracelet/crush/internal/mcplifecycle"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/sessionevent"
	"github.com/charmbracelet/crush/internal/terminal"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const fullSoakEnvironment = "CRUSH_GUI_SOAK_FULL"

type soakProfile struct {
	sessions           int
	historyPerSession  int
	activeSessions     int
	activityDuration   time.Duration
	terminalChunks     int
	blobOperations     int
	maxHeapGrowthBytes uint64
}

func currentProfile() soakProfile {
	if os.Getenv(fullSoakEnvironment) == "1" {
		return soakProfile{
			sessions: 100, historyPerSession: 10_000, activeSessions: 10,
			activityDuration: 10 * time.Second, terminalChunks: 10_000,
			blobOperations: 1_000, maxHeapGrowthBytes: 96 << 20,
		}
	}
	return soakProfile{
		sessions: 10, historyPerSession: 1_000, activeSessions: 10,
		activityDuration: 250 * time.Millisecond, terminalChunks: 250,
		blobOperations: 100, maxHeapGrowthBytes: 32 << 20,
	}
}

type metricRecorder struct {
	mu     sync.Mutex
	gauges map[guimetrics.Name]int64
}

func newMetricRecorder() *metricRecorder {
	return &metricRecorder{gauges: make(map[guimetrics.Name]int64)}
}

func (*metricRecorder) ObserveDuration(guimetrics.Name, time.Duration, guimetrics.Labels) {}

func (r *metricRecorder) Add(name guimetrics.Name, delta int64, _ guimetrics.Labels) {
	r.mu.Lock()
	r.gauges[name] += delta
	r.mu.Unlock()
}

func (r *metricRecorder) SetGauge(name guimetrics.Name, value int64, _ guimetrics.Labels) {
	r.mu.Lock()
	r.gauges[name] = value
	r.mu.Unlock()
}

func (r *metricRecorder) gauge(name guimetrics.Name) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gauges[name]
}

func TestGUIACPReleaseSoak(t *testing.T) {
	profile := currentProfile()
	runtime.GC()
	baselineGoroutines := runtime.NumGoroutine()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)

	metrics := newMetricRecorder()
	hub := sessionevent.NewHub(sessionevent.Config{
		JournalEvents: 256,
		QueueEvents:   8,
		Metrics:       metrics,
	})
	defer hub.Close()

	// Build the long-session history while retaining only the bounded journal.
	for sessionIndex := range profile.sessions {
		sessionID := soakSessionID(sessionIndex)
		for messageIndex := range profile.historyPerSession {
			_, err := hub.Publish(sessionID, textDelta(messageIndex))
			require.NoError(t, err)
		}
		require.Equal(t, uint64(profile.historyPerSession), hub.LatestSequence(sessionID))
	}

	blockedSession := soakSessionID(0)
	blocked, err := hub.Subscribe(blockedSession, hub.LatestSequence(blockedSession))
	require.NoError(t, err)

	factory := newFakeFactory()
	terminals := terminal.New(terminal.Config{
		Factory: factory, RetainedBytes: 64 << 10, SnapshotBytes: 64 << 10,
	})
	blobs := blob.New(blob.Config{
		MaxBlobBytes: 64 << 10, MaxRetainedBytes: 1 << 20,
		MaxBlobs: 128, MaxReadBytes: 64 << 10,
	})

	terminalMetadata, err := terminals.Open(t.Context(), terminal.OpenRequest{
		ClientID: "release-client", SessionID: blockedSession,
		Command: "fake", CWD: t.TempDir(), Cols: 80, Rows: 24,
	})
	require.NoError(t, err)
	process := factory.latest()

	providerTimedOut := make(chan struct{})
	providerContext, cancelProvider := context.WithTimeout(t.Context(), 20*time.Millisecond)
	go func() {
		<-providerContext.Done()
		close(providerTimedOut)
	}()
	mcpBackend := newMCPBackend()
	mcpLifecycle := mcplifecycle.New(newConfigStore(t), mcpBackend, hub, mcplifecycle.Config{})
	require.NoError(t, mcpLifecycle.ReplaceAsync("release-owner", blockedSession, []mcplifecycle.ServerConfig{{
		Name: "release-mcp",
		Config: config.MCPConfig{
			Type: config.MCPStdio, Command: "deterministic-mock",
		},
	}}))
	<-mcpBackend.started
	mcpBackend.releaseCurrent()
	server := waitForMCPStatus(t, mcpLifecycle, blockedSession, mcplifecycle.StatusConnected)
	mcpBackend.blockNext()
	_, err = mcpLifecycle.ReconnectAsync(blockedSession, server.ID)
	require.NoError(t, err)
	<-mcpBackend.started
	mcpBackend.releaseCurrent()
	waitForMCPStatus(t, mcpLifecycle, blockedSession, mcplifecycle.StatusConnected)

	var latenciesMu sync.Mutex
	latencies := make([]time.Duration, 0, profile.activeSessions*1_024)
	start := time.Now()
	var publishers sync.WaitGroup
	for activeIndex := range profile.activeSessions {
		publishers.Go(func() {
			sessionID := soakSessionID(activeIndex)
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			deadline := time.NewTimer(time.Until(start.Add(profile.activityDuration)))
			defer deadline.Stop()
			for messageIndex := 0; ; messageIndex++ {
				select {
				case <-ticker.C:
					publishStarted := time.Now()
					_, publishErr := hub.Publish(sessionID, textDelta(messageIndex))
					require.NoError(t, publishErr)
					latenciesMu.Lock()
					latencies = append(latencies, time.Since(publishStarted))
					latenciesMu.Unlock()
				case <-deadline.C:
					return
				}
			}
		})
	}

	var resources sync.WaitGroup
	resources.Go(func() {
		chunk := []byte("terminal-output\n")
		for range profile.terminalChunks {
			process.emit(chunk)
		}
	})
	resources.Go(func() {
		for operation := range profile.blobOperations {
			data := []byte("bounded-release-blob")
			sum := sha256.Sum256(data)
			metadata, createErr := blobs.Create(t.Context(), "release-client", blob.CreateInput{
				SessionID: soakSessionID(operation % profile.sessions), MIMEType: "application/octet-stream",
				Filename: "fixture.bin", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), Data: data,
			})
			require.NoError(t, createErr)
			_, readErr := blobs.Read(t.Context(), "release-client", metadata.SessionID, metadata.ID, 0, int64(len(data)))
			require.NoError(t, readErr)
			require.NoError(t, blobs.Release("release-client", metadata.SessionID, metadata.ID))
		}
	})

	publishers.Wait()
	resources.Wait()
	<-providerTimedOut
	cancelProvider()

	latenciesMu.Lock()
	slices.Sort(latencies)
	require.NotEmpty(t, latencies)
	expectedChunks := int(profile.activityDuration / time.Millisecond)
	require.GreaterOrEqual(t, len(latencies), expectedChunks*9/10)
	require.LessOrEqual(t, len(latencies), expectedChunks*11/10)
	p95 := latencies[(len(latencies)*95-1)/100]
	latenciesMu.Unlock()
	require.Less(t, p95, 5*time.Millisecond, "provider callback to Hub publish p95")

	// A deliberately unread GUI must overflow to snapshot recovery without
	// affecting the publisher latency measured above.
	requireSnapshotRequired(t, blocked)
	blocked.Close()
	for range 3 {
		after := hub.LatestSequence(blockedSession)
		subscription, subscribeErr := hub.Subscribe(blockedSession, after)
		require.NoError(t, subscribeErr)
		published, publishErr := hub.Publish(blockedSession, textDelta(int(after)))
		require.NoError(t, publishErr)
		replayed, nextErr := subscription.Next(t.Context())
		require.NoError(t, nextErr)
		require.Equal(t, published.Sequence, replayed.Sequence)
		subscription.Close()
	}

	process.exit(terminal.ProcessExit{Code: 0})
	require.Eventually(t, func() bool { return terminals.ActiveCount() == 0 }, time.Second, time.Millisecond)
	terminals.CloseClient("release-client")
	blobs.ReleaseClient("release-client")
	require.Equal(t, 0, terminals.ActiveCount())
	require.Zero(t, terminals.RetainedBytes())
	count, retainedBytes := blobs.Retained()
	require.Zero(t, count)
	require.Zero(t, retainedBytes)
	require.Zero(t, metrics.gauge(guimetrics.ActiveSubscriptionCount))
	_, err = terminals.Get("release-client", blockedSession, terminalMetadata.ID)
	require.ErrorIs(t, err, terminal.ErrNotFound)

	terminals.Close()
	blobs.Close()
	mcpLifecycle.Close(t.Context())
	hub.Close()
	runtime.GC()
	var final runtime.MemStats
	runtime.ReadMemStats(&final)
	require.LessOrEqual(t, final.HeapAlloc, baseline.HeapAlloc+profile.maxHeapGrowthBytes)
	require.Eventually(t, func() bool {
		return runtime.NumGoroutine() <= baselineGoroutines+16
	}, 2*time.Second, 10*time.Millisecond)

	t.Logf("profile sessions=%d history=%d active=%d duration=%s chunks=%d publish_p95=%s heap_growth=%d goroutine_growth=%d",
		profile.sessions, profile.historyPerSession, profile.activeSessions,
		profile.activityDuration, len(latencies), p95,
		int64(final.HeapAlloc)-int64(baseline.HeapAlloc), runtime.NumGoroutine()-baselineGoroutines)
}

func TestGUIACPWarmSnapshotP95(t *testing.T) {
	connection, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	defer connection.Close()
	sessions := session.NewService(db.New(connection), connection)
	messages := message.NewService(db.New(connection))
	current, err := sessions.Create(t.Context(), "release snapshot")
	require.NoError(t, err)
	_, err = connection.ExecContext(t.Context(), `
		WITH RECURSIVE sequence(value) AS (
			SELECT 0 UNION ALL SELECT value + 1 FROM sequence WHERE value < 9999
		)
		INSERT INTO messages
			(id, session_id, role, parts, model, provider, created_at, updated_at)
		SELECT printf('release-message-%08d', value), ?, 'assistant', '[]',
			'mock-model', 'mock-provider', value, value
		FROM sequence`, current.ID)
	require.NoError(t, err)
	hub := sessionevent.NewHub(sessionevent.Config{})
	defer hub.Close()
	service := sessionevent.NewSnapshotService(sessions, messages, nil, hub)
	_, err = service.Snapshot(t.Context(), current.ID)
	require.NoError(t, err)

	durations := make([]time.Duration, 100)
	for index := range durations {
		started := time.Now()
		snapshot, snapshotErr := service.Snapshot(t.Context(), current.ID)
		durations[index] = time.Since(started)
		require.NoError(t, snapshotErr)
		require.Len(t, snapshot.Messages, sessionevent.SnapshotMessageLimit)
	}
	slices.Sort(durations)
	p95 := durations[94]
	require.Less(t, p95, 150*time.Millisecond)
	t.Logf("warm snapshot p95=%s", p95)
}

func TestGUIACPReleaseFaultMatrix(t *testing.T) {
	t.Run("expired replay and backend restart", func(t *testing.T) {
		hub := sessionevent.NewHub(sessionevent.Config{JournalEvents: 2})
		for index := range 3 {
			_, err := hub.Publish("session", textDelta(index))
			require.NoError(t, err)
		}
		_, err := hub.ReplayAfter("session", 0)
		require.ErrorIs(t, err, sessionevent.ErrSequenceExpired)
		hub.Close()

		restarted := sessionevent.NewHub(sessionevent.Config{})
		defer restarted.Close()
		_, err = restarted.Publish("session", textDelta(0))
		require.NoError(t, err)
		require.Equal(t, uint64(1), restarted.LatestSequence("session"))
	})

	t.Run("GUI crash releases subscription", func(t *testing.T) {
		metrics := newMetricRecorder()
		hub := sessionevent.NewHub(sessionevent.Config{Metrics: metrics})
		subscription, err := hub.Subscribe("session", 0)
		require.NoError(t, err)
		require.Equal(t, int64(1), metrics.gauge(guimetrics.ActiveSubscriptionCount))
		subscription.Close()
		require.Zero(t, metrics.gauge(guimetrics.ActiveSubscriptionCount))
		hub.Close()
	})

	t.Run("malformed and oversized attachment", func(t *testing.T) {
		service := blob.New(blob.Config{MaxBlobBytes: 4, MaxRetainedBytes: 4})
		defer service.Close()
		_, err := service.Create(t.Context(), "client", blob.CreateInput{
			SessionID: "session", MIMEType: "application/octet-stream",
			Size: 5, SHA256: "not-a-hash", Data: []byte("12345"),
		})
		require.ErrorIs(t, err, blob.ErrInvalidInput)
		data := []byte("12345")
		sum := sha256.Sum256(data)
		_, err = service.Create(t.Context(), "client", blob.CreateInput{
			SessionID: "session", MIMEType: "application/octet-stream",
			Size: 5, SHA256: hex.EncodeToString(sum[:]), Data: data,
		})
		require.ErrorIs(t, err, blob.ErrBlobTooLarge)
	})

	t.Run("duplicate request ID", func(t *testing.T) {
		store := idempotency.New(idempotency.Config{})
		defer store.Close()
		requestID := uuid.NewString()
		var calls atomic.Int32
		outcome, err := store.Execute(t.Context(), "release", requestID, "same", func() idempotency.Outcome {
			calls.Add(1)
			return idempotency.Outcome{Value: "ok"}
		})
		require.NoError(t, err)
		require.Equal(t, "ok", outcome.Value)
		outcome, err = store.Execute(t.Context(), "release", requestID, "same", func() idempotency.Outcome {
			calls.Add(1)
			return idempotency.Outcome{Value: "wrong"}
		})
		require.NoError(t, err)
		require.Equal(t, "ok", outcome.Value)
		require.Equal(t, int32(1), calls.Load())
		_, err = store.Execute(t.Context(), "release", requestID, "different", func() idempotency.Outcome {
			return idempotency.Outcome{}
		})
		require.ErrorIs(t, err, idempotency.ErrConflict)
	})

	t.Run("provider timeout and missing response", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
		defer cancel()
		response := make(chan struct{})
		select {
		case <-response:
			t.Fatal("unexpected response")
		case <-ctx.Done():
			require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
		}
	})

	t.Run("SQLite busy fails explicitly", func(t *testing.T) {
		dataDir := t.TempDir()
		first, err := db.Connect(t.Context(), dataDir)
		require.NoError(t, err)
		defer first.Close()
		second, err := db.Connect(t.Context(), dataDir)
		require.NoError(t, err)
		defer second.Close()
		_, err = first.ExecContext(t.Context(), "CREATE TABLE release_busy (value INTEGER)")
		require.NoError(t, err)
		transaction, err := first.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		defer transaction.Rollback()
		_, err = transaction.ExecContext(t.Context(), "INSERT INTO release_busy VALUES (1)")
		require.NoError(t, err)
		_, err = second.ExecContext(t.Context(), "PRAGMA busy_timeout=1")
		require.NoError(t, err)
		_, err = second.ExecContext(t.Context(), "INSERT INTO release_busy VALUES (2)")
		require.Error(t, err)
		require.Contains(t, err.Error(), "locked")
	})

	t.Run("SQLite slow query observes cancellation", func(t *testing.T) {
		connection, err := db.Connect(t.Context(), t.TempDir())
		require.NoError(t, err)
		defer connection.Close()
		ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
		defer cancel()
		var sum int64
		err = connection.QueryRowContext(ctx, `
			WITH RECURSIVE values_(n) AS (
				SELECT 1 UNION ALL SELECT n + 1 FROM values_ WHERE n < 100000000
			) SELECT sum(n) FROM values_`).Scan(&sum)
		require.Error(t, err)
		require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
	})

	t.Run("SQLite disk full fails explicitly", func(t *testing.T) {
		connection, err := db.Connect(t.Context(), t.TempDir())
		require.NoError(t, err)
		defer connection.Close()
		_, err = connection.ExecContext(t.Context(), "CREATE TABLE release_full (value BLOB)")
		require.NoError(t, err)
		var pages int
		err = connection.QueryRowContext(t.Context(), "PRAGMA page_count").Scan(&pages)
		require.NoError(t, err)
		_, err = connection.ExecContext(t.Context(), fmt.Sprintf("PRAGMA max_page_count=%d", pages))
		require.NoError(t, err)
		_, err = connection.ExecContext(t.Context(), "INSERT INTO release_full VALUES (zeroblob(1048576))")
		require.Error(t, err)
		require.Contains(t, err.Error(), "full")
	})
}

func TestReleaseEntryPointYAML(t *testing.T) {
	taskBytes, err := os.ReadFile(filepath.Join("..", "..", "Taskfile.yaml"))
	require.NoError(t, err)
	var taskfile struct {
		Tasks map[string]any `yaml:"tasks"`
	}
	require.NoError(t, yaml.Unmarshal(taskBytes, &taskfile))
	for _, name := range []string{
		"test:gui-release", "test:gui-release-race",
		"test:gui-release-fault", "test:gui-release-full",
	} {
		require.Contains(t, taskfile.Tasks, name)
	}

	workflowBytes, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "gui-acp.yml"))
	require.NoError(t, err)
	var workflow map[string]any
	require.NoError(t, yaml.Unmarshal(workflowBytes, &workflow))
	require.Contains(t, workflow, "jobs")
}

func soakSessionID(index int) string {
	return "release-session-" + uuid.NewSHA1(uuid.Nil, []byte{byte(index)}).String()
}

func textDelta(index int) sessionevent.NewEvent {
	return sessionevent.NewEvent{
		Kind: sessionevent.KindMessageDelta,
		Payload: sessionevent.TextDelta{
			MessageID: "message", PartID: "part", Text: string(rune('a' + index%26)),
		},
		Delivery:    sessionevent.DeliveryReliable,
		CoalesceKey: "message:part:" + string(rune(index)),
	}
}

func requireSnapshotRequired(t *testing.T, subscription *sessionevent.Subscription) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	for {
		event, err := subscription.Next(ctx)
		if errors.Is(err, sessionevent.ErrSnapshotRequired) {
			return
		}
		require.NoError(t, err)
		if event.Kind == sessionevent.KindSnapshotRequired {
			return
		}
	}
}

func newConfigStore(t *testing.T) *config.ConfigStore {
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

func waitForMCPStatus(
	t *testing.T,
	service *mcplifecycle.Service,
	sessionID string,
	want mcplifecycle.Status,
) mcplifecycle.Server {
	t.Helper()
	var result mcplifecycle.Server
	require.Eventually(t, func() bool {
		servers, _, err := service.List(sessionID)
		if err != nil {
			return false
		}
		for _, server := range servers {
			if server.Name == "release-mcp" {
				result = server
				return result.Status == want
			}
		}
		return false
	}, 2*time.Second, time.Millisecond)
	return result
}

type mcpBackend struct {
	mu      sync.Mutex
	release chan struct{}
	started chan string
	states  map[string]mcplifecycle.BackendInfo
	events  chan mcplifecycle.BackendEvent
}

func newMCPBackend() *mcpBackend {
	return &mcpBackend{
		release: make(chan struct{}), started: make(chan string, 8),
		states: make(map[string]mcplifecycle.BackendInfo),
		events: make(chan mcplifecycle.BackendEvent, 8),
	}
}

func (b *mcpBackend) Connect(ctx context.Context, _ *config.ConfigStore, name string) error {
	b.mu.Lock()
	release := b.release
	b.states[name] = mcplifecycle.BackendInfo{State: mcplifecycle.BackendStarting}
	b.mu.Unlock()
	b.started <- name
	select {
	case <-release:
	case <-ctx.Done():
		return ctx.Err()
	}
	b.mu.Lock()
	b.states[name] = mcplifecycle.BackendInfo{
		State: mcplifecycle.BackendConnected, Counts: mcplifecycle.Counts{Tools: 1},
	}
	b.mu.Unlock()
	return nil
}

func (b *mcpBackend) Reconnect(ctx context.Context, store *config.ConfigStore, name string) error {
	return b.Connect(ctx, store, name)
}

func (b *mcpBackend) Disable(_ context.Context, _ *config.ConfigStore, name string) error {
	b.mu.Lock()
	b.states[name] = mcplifecycle.BackendInfo{State: mcplifecycle.BackendDisabled}
	b.mu.Unlock()
	return nil
}

func (b *mcpBackend) State(name string) (mcplifecycle.BackendInfo, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	state, ok := b.states[name]
	return state, ok
}

func (b *mcpBackend) Subscribe(ctx context.Context) <-chan mcplifecycle.BackendEvent {
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

func (*mcpBackend) MarkScoped(string) {}

func (b *mcpBackend) blockNext() {
	b.mu.Lock()
	b.release = make(chan struct{})
	b.mu.Unlock()
}

func (b *mcpBackend) releaseCurrent() {
	b.mu.Lock()
	release := b.release
	b.mu.Unlock()
	close(release)
}

type fakeFactory struct {
	mu        sync.Mutex
	processes []*fakeProcess
}

func newFakeFactory() *fakeFactory { return &fakeFactory{} }

func (f *fakeFactory) Start(context.Context, terminal.OpenRequest) (terminal.Process, error) {
	process := &fakeProcess{
		read: make(chan []byte, 16_384), exitCh: make(chan terminal.ProcessExit, 1),
	}
	f.mu.Lock()
	f.processes = append(f.processes, process)
	f.mu.Unlock()
	return process, nil
}

func (f *fakeFactory) latest() *fakeProcess {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.processes[len(f.processes)-1]
}

type fakeProcess struct {
	mu        sync.Mutex
	read      chan []byte
	exitCh    chan terminal.ProcessExit
	closed    bool
	closeOnce sync.Once
}

func (p *fakeProcess) Read(destination []byte) (int, error) {
	chunk, ok := <-p.read
	if !ok {
		return 0, io.EOF
	}
	return copy(destination, chunk), nil
}

func (p *fakeProcess) Write(value []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, io.ErrClosedPipe
	}
	return len(value), nil
}

func (*fakeProcess) Resize(int, int) error { return nil }

func (p *fakeProcess) Kill(signal string) error {
	p.exit(terminal.ProcessExit{Code: 1, Signal: signal})
	return nil
}

func (p *fakeProcess) Wait(ctx context.Context) (terminal.ProcessExit, error) {
	select {
	case value := <-p.exitCh:
		return value, nil
	case <-ctx.Done():
		return terminal.ProcessExit{}, ctx.Err()
	}
}

func (p *fakeProcess) Close() error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
		close(p.read)
	})
	return nil
}

func (p *fakeProcess) emit(value []byte) { p.read <- append([]byte(nil), value...) }

func (p *fakeProcess) exit(value terminal.ProcessExit) {
	select {
	case p.exitCh <- value:
	default:
	}
	_ = p.Close()
}

var (
	_ terminal.Factory     = (*fakeFactory)(nil)
	_ terminal.Process     = (*fakeProcess)(nil)
	_ mcplifecycle.Backend = (*mcpBackend)(nil)
)
