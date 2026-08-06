package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/blob"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/log"
	"github.com/charmbracelet/crush/internal/mcplifecycle"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/plugin"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/sessionevent"
	"github.com/charmbracelet/crush/internal/terminal"
	"github.com/charmbracelet/crush/internal/timeline"
	"github.com/charmbracelet/crush/internal/toolruntime"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

func TestSetupSubscriber_NormalFlow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newSubscriberFixture(t, 10)

		time.Sleep(10 * time.Millisecond)
		synctest.Wait()

		f.broker.Publish(pubsub.CreatedEvent, "event1")
		f.broker.Publish(pubsub.CreatedEvent, "event2")

		for range 2 {
			select {
			case <-f.outputCh:
			case <-time.After(5 * time.Second):
				t.Fatal("Timed out waiting for messages")
			}
		}

		f.cancel()
		f.wg.Wait()
	})
}

func TestSetupSubscriber_SlowConsumer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newSubscriberFixture(t, 0)

		const numEvents = 5

		var pubWg sync.WaitGroup
		pubWg.Go(func() {
			for range numEvents {
				f.broker.Publish(pubsub.CreatedEvent, "event")
				time.Sleep(10 * time.Millisecond)
				synctest.Wait()
			}
		})

		time.Sleep(time.Duration(numEvents) * (subscriberSendTimeout + 20*time.Millisecond))
		synctest.Wait()

		received := 0
		for {
			select {
			case <-f.outputCh:
				received++
			default:
				pubWg.Wait()
				f.cancel()
				f.wg.Wait()
				require.Less(t, received, numEvents, "Slow consumer should have dropped some messages")
				return
			}
		}
	})
}

func TestSetupSubscriber_ContextCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newSubscriberFixture(t, 10)

		f.broker.Publish(pubsub.CreatedEvent, "event1")
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()

		f.cancel()
		f.wg.Wait()
	})
}

func TestSetupSubscriber_DrainAfterDrop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newSubscriberFixture(t, 0)

		time.Sleep(10 * time.Millisecond)
		synctest.Wait()

		// First event: nobody reads outputCh so the timer fires (message dropped).
		f.broker.Publish(pubsub.CreatedEvent, "event1")
		time.Sleep(subscriberSendTimeout + 25*time.Millisecond)
		synctest.Wait()

		// Second event: triggers Stop()==false path; without the fix this deadlocks.
		f.broker.Publish(pubsub.CreatedEvent, "event2")

		// If the timer drain deadlocks, wg.Wait never returns.
		done := make(chan struct{})
		go func() {
			f.cancel()
			f.wg.Wait()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("setupSubscriber goroutine hung — likely timer drain deadlock")
		}
	})
}

func TestSetupSubscriber_NoTimerLeak(t *testing.T) {
	// Snapshot goroutines that existed before this test so goleak only
	// reports goroutines leaked by this test, not by earlier tests whose
	// async cleanup (e.g. MCP connection teardown) is still in flight.
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	synctest.Test(t, func(t *testing.T) {
		f := newSubscriberFixture(t, 100)

		for range 100 {
			f.broker.Publish(pubsub.CreatedEvent, "event")
			time.Sleep(5 * time.Millisecond)
			synctest.Wait()
		}

		f.cancel()
		f.wg.Wait()
	})
}

type subscriberFixture struct {
	broker   *pubsub.Broker[string]
	wg       sync.WaitGroup
	outputCh chan tea.Msg
	cancel   context.CancelFunc
}

type messageCreatedPlugin struct {
	called atomic.Int32
}

func (p *messageCreatedPlugin) Name() string {
	return "message-created-plugin"
}

func (p *messageCreatedPlugin) Close(ctx context.Context) error {
	return nil
}

func (p *messageCreatedPlugin) Init(ctx context.Context, input plugin.PluginInput) (plugin.Hooks, error) {
	return plugin.Hooks{
		MessageCreated: func(ctx context.Context, msg message.Message) error {
			p.called.Add(1)
			return nil
		},
	}, nil
}

func TestSetupMessageSubscriber_TriggersMessageCreatedHook(t *testing.T) {
	plugin.Reset()
	defer plugin.Reset()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	testPlugin := &messageCreatedPlugin{}
	plugin.Register(testPlugin)

	conn, store := setupMessageSubscriberDependencies(t)
	defer func() {
		require.NoError(t, conn.Close())
	}()

	sessions := session.NewService(db.New(conn), conn)
	messages := message.NewService(db.New(conn))

	require.NoError(t, plugin.Init(ctx, plugin.PluginInput{
		Config:     store,
		Sessions:   sessions,
		Messages:   messages,
		WorkingDir: store.WorkingDir(),
	}))

	var wg sync.WaitGroup
	outputCh := make(chan tea.Msg, 8)
	setupMessageSubscriber(ctx, &wg, plugin.DefaultRuntime(), messages.Subscribe, outputCh)

	testSession, err := sessions.Create(ctx, "message hook")
	require.NoError(t, err)
	_, err = messages.Create(ctx, testSession.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "hello"}},
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return testPlugin.called.Load() == 1
	}, time.Second, 10*time.Millisecond)

	cancel()
	wg.Wait()
}

func TestNew_RestoresDefaultPluginRuntimeWhenInitCoderAgentFails(t *testing.T) {
	originalRuntime := plugin.DefaultRuntime()
	t.Cleanup(func() {
		plugin.SetDefaultRuntime(originalRuntime)
	})

	conn, store := setupMessageSubscriberDependencies(t)

	cfg := store.Config()
	cfg.Models[config.SelectedModelTypeLarge] = config.SelectedModel{
		Provider: "missing-provider",
		Model:    "missing-model",
	}

	// A missing/changed provider for the selected model is recoverable: the
	// app should still start so the user can pick a valid model from the
	// model dialog. The coder agent will be left uninitialized.
	app, err := New(t.Context(), conn, store)
	require.NoError(t, err)
	require.NotNil(t, app)
	require.Nil(t, app.AgentCoordinator)
	t.Cleanup(func() {
		// App.Shutdown closes the underlying DB connection.
		app.Shutdown()
	})
}

func TestNew_EnablesDefaultLocalMemoryWhenMemoryConfigMissing(t *testing.T) {
	conn, store := setupMessageSubscriberDependencies(t)

	store.Config().Options.Memory = nil
	app, err := New(t.Context(), conn, store)
	require.NoError(t, err)
	require.NotNil(t, app)
	require.NotNil(t, app.MemoryBackend)
	require.Equal(t, "local", app.MemoryBackend.ID())
	t.Cleanup(func() {
		app.Shutdown()
	})
}

func TestNewOwnsSessionEventHubAndCleansDeletedSession(t *testing.T) {
	conn, store := setupMessageSubscriberDependencies(t)
	app, err := New(t.Context(), conn, store)
	require.NoError(t, err)
	require.NotNil(t, app.GetSessionEvents())
	require.NotNil(t, app.Turns)
	require.NotNil(t, app.Idempotency)
	require.NotNil(t, app.GetBlobs())
	require.NotNil(t, app.GetTerminals())
	require.NotNil(t, app.GetProviderAuth())
	require.NotNil(t, app.GetMCPLifecycle())
	t.Cleanup(func() {
		app.Shutdown()
	})

	sess, err := app.Sessions.Create(t.Context(), "live event cleanup")
	require.NoError(t, err)
	app.MCPLifecycle.Close(t.Context())
	mcpBackend := &appMCPBackend{states: make(map[string]mcplifecycle.BackendInfo)}
	app.MCPLifecycle = mcplifecycle.New(store, mcpBackend, app.SessionEvents, mcplifecycle.Config{})
	require.NoError(t, app.MCPLifecycle.ReplaceAsync("client", sess.ID, []mcplifecycle.ServerConfig{{
		Name: "dynamic", Config: config.MCPConfig{Type: config.MCPStdio, Command: "mock"},
	}}))
	require.Eventually(t, func() bool {
		value, statusErr := app.MCPLifecycle.Status(sess.ID, "session:dynamic")
		return statusErr == nil && value.Status == mcplifecycle.StatusConnected
	}, time.Second, time.Millisecond)
	_, err = app.SessionEvents.Publish(sess.ID, sessionevent.NewEvent{Kind: sessionevent.KindTurnStarted})
	require.NoError(t, err)
	data := []byte("blob")
	sum := sha256.Sum256(data)
	_, err = app.Blobs.Create(t.Context(), "client", blob.CreateInput{
		SessionID: sess.ID, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), Data: data,
	})
	require.NoError(t, err)
	app.Terminals.Close()
	terminalFactory := &appTerminalFactory{}
	app.Terminals = terminal.New(terminal.Config{Factory: terminalFactory})
	terminalMetadata, err := app.Terminals.Open(t.Context(), terminal.OpenRequest{
		ClientID: "client", SessionID: sess.ID, Command: "fake",
	})
	require.NoError(t, err)
	require.NoError(t, app.Sessions.Delete(t.Context(), sess.ID))

	events, err := app.SessionEvents.ReplayAfter(sess.ID, 0)
	require.NoError(t, err)
	require.Empty(t, events)
	count, retained := app.Blobs.Retained()
	require.Zero(t, count)
	require.Zero(t, retained)
	_, err = app.Terminals.Get("client", sess.ID, terminalMetadata.ID)
	require.ErrorIs(t, err, terminal.ErrNotFound)
	require.True(t, terminalFactory.process.isClosed())
	require.Eventually(t, func() bool { return mcpBackend.disableCount.Load() == 1 }, time.Second, time.Millisecond)
}

type appMCPBackend struct {
	mu           sync.Mutex
	states       map[string]mcplifecycle.BackendInfo
	disableCount atomic.Int32
}

func (b *appMCPBackend) Connect(context.Context, *config.ConfigStore, string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for name := range b.states {
		_ = name
	}
	return nil
}

func (b *appMCPBackend) Reconnect(ctx context.Context, store *config.ConfigStore, name string) error {
	return b.Connect(ctx, store, name)
}

func (b *appMCPBackend) Disable(context.Context, *config.ConfigStore, string) error {
	b.disableCount.Add(1)
	return nil
}

func (b *appMCPBackend) State(name string) (mcplifecycle.BackendInfo, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	value, ok := b.states[name]
	return value, ok
}

func (b *appMCPBackend) Subscribe(ctx context.Context) <-chan mcplifecycle.BackendEvent {
	result := make(chan mcplifecycle.BackendEvent)
	go func() {
		<-ctx.Done()
		close(result)
	}()
	return result
}

func (b *appMCPBackend) MarkScoped(name string) {
	b.mu.Lock()
	b.states[name] = mcplifecycle.BackendInfo{State: mcplifecycle.BackendConnected}
	b.mu.Unlock()
}

type appTerminalFactory struct{ process *appTerminalProcess }

func (f *appTerminalFactory) Start(context.Context, terminal.OpenRequest) (terminal.Process, error) {
	f.process = &appTerminalProcess{readDone: make(chan struct{}), exit: make(chan terminal.ProcessExit, 1)}
	return f.process, nil
}

type appTerminalProcess struct {
	mu       sync.Mutex
	readDone chan struct{}
	exit     chan terminal.ProcessExit
	closed   bool
	once     sync.Once
}

func (p *appTerminalProcess) Read([]byte) (int, error)      { <-p.readDone; return 0, io.EOF }
func (*appTerminalProcess) Write(value []byte) (int, error) { return len(value), nil }
func (*appTerminalProcess) Resize(int, int) error           { return nil }
func (p *appTerminalProcess) Kill(signal string) error {
	select {
	case p.exit <- terminal.ProcessExit{Signal: signal}:
	default:
	}
	return p.Close()
}

func (p *appTerminalProcess) Wait(ctx context.Context) (terminal.ProcessExit, error) {
	select {
	case value := <-p.exit:
		return value, nil
	case <-ctx.Done():
		return terminal.ProcessExit{}, ctx.Err()
	}
}

func (p *appTerminalProcess) Close() error {
	p.once.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
		close(p.readDone)
	})
	return nil
}

func (p *appTerminalProcess) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

func setupMessageSubscriberDependencies(t *testing.T) (*sql.DB, *config.ConfigStore) {
	t.Helper()
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, log.ResetForTesting())
	})
	require.NoError(t, os.WriteFile(filepath.Join(workingDir, "crush.json"), []byte(`{"options":{"disable_provider_auto_update":true}}`), 0o644))

	store, err := config.Init(workingDir, dataDir, false)
	require.NoError(t, err)

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	return conn, store
}

func newSubscriberFixture(t *testing.T, bufSize int) *subscriberFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	f := &subscriberFixture{
		broker:   pubsub.NewBroker[string](),
		outputCh: make(chan tea.Msg, bufSize),
		cancel:   cancel,
	}
	t.Cleanup(f.broker.Shutdown)

	setupSubscriber(ctx, &f.wg, "test", func(ctx context.Context) <-chan pubsub.Event[string] {
		return f.broker.Subscribe(ctx)
	}, f.outputCh)

	return f
}

func TestSetupTimelineFromToolRuntimePublishesTimelineEvents(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	app := &App{
		ToolRuntime:     toolruntime.NewService(),
		Timeline:        timeline.NewService(),
		serviceEventsWG: &sync.WaitGroup{},
	}
	app.setupTimelineFromToolRuntime(ctx)

	sub := app.Timeline.Subscribe(ctx)
	app.ToolRuntime.Publish(toolruntime.State{SessionID: "sess-1", ToolCallID: "tool-1", ToolName: "bash", Status: toolruntime.StatusRunning, SnapshotText: "first"})
	app.ToolRuntime.Publish(toolruntime.State{SessionID: "sess-1", ToolCallID: "tool-1", ToolName: "bash", Status: toolruntime.StatusCompleted, SnapshotText: "done"})

	seen := make([]timeline.EventType, 0, 3)
	for len(seen) < 3 {
		select {
		case event := <-sub:
			seen = append(seen, event.Payload.Type)
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for timeline events: %v", seen)
		}
	}

	require.Equal(t, []timeline.EventType{timeline.EventToolStarted, timeline.EventToolProgress, timeline.EventToolFinished}, seen)
	cancel()
	app.serviceEventsWG.Wait()
}
