// Package mcp provides functionality for managing Model Context Protocol (MCP)
// clients within the Crush application.
package mcp

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/home"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/version"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func parseLevel(level mcp.LoggingLevel) slog.Level {
	switch level {
	case "info":
		return slog.LevelInfo
	case "notice":
		return slog.LevelInfo
	case "warning":
		return slog.LevelWarn
	default:
		return slog.LevelDebug
	}
}

// ClientSession wraps an mcp.ClientSession with a context cancel function so
// that the context created during session establishment is properly cleaned up
// on close.
type ClientSession struct {
	*mcp.ClientSession
	cancel context.CancelFunc
}

// Close cancels the session context and then closes the underlying session.
func (s *ClientSession) Close() error {
	s.cancel()
	return s.ClientSession.Close()
}

var (
	sessions = csync.NewMap[string, *ClientSession]()
	states   = csync.NewMap[string, ClientInfo]()
	broker   = pubsub.NewBroker[Event]()
	initOnce sync.Once
	initDone = make(chan struct{})
	// queries holds the database handle used to persist and read cached MCP
	// tool definitions. It is injected via SetQueries and may be nil when no
	// database is available, in which case caching is silently disabled.
	queries *db.Queries
	// configStore holds the application config store used to read MCP OAuth
	// configuration and persist refreshed tokens. It is injected via
	// SetConfigStore and may be nil when no config store is available.
	configStore *config.ConfigStore
	// authorizers tracks the live OAuth authorizer for each MCP server that
	// supports interactive auth. RefreshToken looks up the authorizer by
	// server name so it can reuse the authorizer's mutex and refresh logic
	// instead of racing with the round tripper's pre-refresh path.
	authorizers = csync.NewMap[string, *mcpOAuthAuthorizer]()
	// circuitBreakers tracks per-server reconnect failure counts so the
	// auto-reconnect loop can trip a breaker after too many failures.
	circuitBreakers = csync.NewMap[string, circuitBreakerState]()
	// disconnecting marks servers being deliberately disconnected (e.g.
	// disabled or manually reconnected) so in-flight auto-reconnect loops
	// know to stop.
	disconnecting = csync.NewMap[string, struct{}]()
	// reconnecting marks servers with an active reconnectLoop goroutine so
	// we never spawn two loops for the same server. Stored via sync.Map's
	// atomic LoadOrStore to avoid the TOCTOU race a csync.Map check-then-set
	// would introduce.
	reconnecting sync.Map
	// reconnectMus holds a per-server mutex that serializes reconnect
	// operations (Reconnect, ResetCircuitBreaker, synchronous
	// reconnectClient calls) so concurrent callers cannot race on session
	// double-close or state overwrites. A sync.Map is used (rather than
	// csync.Map) because LoadOrStore is required to atomically create each
	// server's mutex.
	reconnectMus sync.Map // map[string]*sync.Mutex
	// reconnectWg tracks all in-flight reconnectLoop goroutines so Close
	// can wait for them to exit before tearing down sessions. This
	// prevents a loop from racing with session teardown (e.g. calling
	// Reconnect→Close on the same session, or overwriting state after
	// Close returns).
	reconnectWg sync.WaitGroup
	// closeMu serializes Close's lifecycleCancel+Wait with
	// maybeStartReconnectLoop's tryStartReconnect+Add. This ensures the
	// WaitGroup Add happens-before Wait, avoiding the classic WaitGroup
	// race where Add(1) occurs after Wait() has already observed a zero
	// counter.
	closeMu sync.Mutex
	// lifecycleCtx is the long-lived context used by background reconnect
	// loops. It is cancelled when Close is called so loops exit promptly on
	// application shutdown.
	lifecycleCtx    context.Context
	lifecycleCancel context.CancelFunc
	lifecycleOnce   sync.Once
	// reconnectFn is the function used by reconnectLoop and
	// ResetCircuitBreaker to attempt a reconnect. It defaults to Reconnect;
	// tests may override it to avoid real network operations.
	reconnectFn = Reconnect
	// reconnectBackoffs is the exponential backoff schedule used between
	// reconnect attempts. Tests may shorten it to run quickly.
	reconnectBackoffs = []time.Duration{
		500 * time.Millisecond,
		time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
	}
)

// Circuit breaker configuration. The breaker opens after a server fails to
// reconnect circuitBreakerThreshold times within a circuitBreakerWindow
// rolling window, suspending further auto-reconnect attempts until the user
// manually resets it via ResetCircuitBreaker.
const (
	circuitBreakerWindow    = 30 * time.Second
	circuitBreakerThreshold = 5
)

// ErrCircuitOpen is the sentinel error recorded against a server's state
// when its circuit breaker opens.
var ErrCircuitOpen = errors.New("circuit breaker open: reconnect paused after repeated failures")

// SetQueries injects the database handle used to persist and read cached MCP
// tool definitions. It should be called before Initialize; passing nil
// disables caching.
func SetQueries(q *db.Queries) {
	queries = q
}

// SetConfigStore injects the config store used to read MCP OAuth configuration
// and persist refreshed tokens. It should be called before Initialize; passing
// nil disables automatic token refresh from callToolWithRetry.
func SetConfigStore(cs *config.ConfigStore) {
	configStore = cs
}

// State represents the current state of an MCP client
type State int

const (
	StateDisabled State = iota
	StateStarting
	StateConnected
	StateNeedsAuth
	StateError
	// StateCached means the server connection failed but its tool
	// definitions were restored from the on-disk cache, so the tools remain
	// available for the LLM while a live connection is retried on demand.
	StateCached
	// StateCircuitOpen means the circuit breaker has opened after repeated
	// reconnect failures. Auto-reconnect is paused until the user triggers
	// a manual reconnect via ResetCircuitBreaker.
	StateCircuitOpen
)

func (s State) String() string {
	switch s {
	case StateDisabled:
		return "disabled"
	case StateStarting:
		return "starting"
	case StateConnected:
		return "connected"
	case StateNeedsAuth:
		return "needs_auth"
	case StateError:
		return "error"
	case StateCached:
		return "cached"
	case StateCircuitOpen:
		return "circuit_open"
	default:
		return "unknown"
	}
}

// EventType represents the type of MCP event
type EventType uint

const (
	EventStateChanged EventType = iota
	EventToolsListChanged
	EventPromptsListChanged
	EventResourcesListChanged
	EventLogMessage
)

// Event represents an event in the MCP system
type Event struct {
	Type   EventType
	Name   string
	State  State
	Error  error
	Counts Counts
	Log    LogMessage
}

// LogMessage is the raw in-process MCP logging notification. Consumers must
// redact and bound Data before exposing or retaining it outside this package.
type LogMessage struct {
	Timestamp time.Time
	Level     string
	Logger    string
	Data      any
}

// Counts number of available tools, prompts, etc.
type Counts struct {
	Tools     int
	Prompts   int
	Resources int
}

// ClientInfo holds information about an MCP client's state
type ClientInfo struct {
	Name        string
	State       State
	Error       error
	Client      *ClientSession
	Counts      Counts
	ConnectedAt time.Time
}

// SubscribeEvents returns a channel for MCP events
func SubscribeEvents(ctx context.Context) <-chan pubsub.Event[Event] {
	return broker.Subscribe(ctx)
}

// GetStates returns the current state of all MCP clients
func GetStates() map[string]ClientInfo {
	return states.Copy()
}

// GetState returns the state of a specific MCP client
func GetState(name string) (ClientInfo, bool) {
	return states.Get(name)
}

// Close closes all MCP clients. This should be called during application shutdown.
func Close(ctx context.Context) error {
	// Cancel the lifecycle context first so any in-flight reconnect loops
	// exit promptly and do not race with the session teardown below.
	// closeMu serializes lifecycleCancel with maybeStartReconnectLoop's
	// tryStartReconnect+Add so the WaitGroup Add happens-before Wait below.
	ensureLifecycle()
	closeMu.Lock()
	lifecycleCancel()
	closeMu.Unlock()
	// Wait for all in-flight reconnect loops to exit so they cannot race
	// with the session teardown below. The lifecycle context was cancelled
	// above, so each loop's reconnectFn (which derives its context from
	// lifecycleCtx) returns promptly and the loop observes ctx.Done() on
	// its next iteration. The wait is bounded by ctx so a stuck loop
	// cannot block shutdown indefinitely.
	waitDone := make(chan struct{})
	go func() {
		reconnectWg.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-ctx.Done():
	}
	var wg sync.WaitGroup
	for name, session := range sessions.Seq2() {
		wg.Go(func() {
			done := make(chan error, 1)
			go func() {
				done <- session.Close()
			}()
			select {
			case err := <-done:
				if err != nil &&
					!errors.Is(err, io.EOF) &&
					!errors.Is(err, context.Canceled) &&
					err.Error() != "signal: killed" {
					slog.Warn("Failed to shutdown MCP client", "name", name, "error", err)
				}
			case <-ctx.Done():
			}
		})
	}
	wg.Wait()
	broker.Shutdown()
	return nil
}

// Initialize initializes MCP clients based on the provided configuration.
func Initialize(ctx context.Context, permissions permission.Service, cfg *config.ConfigStore) {
	slog.Info("Initializing MCP clients")
	ensureLifecycle()
	var wg sync.WaitGroup
	// Initialize states for all configured MCPs
	for name, m := range cfg.Config().MCP {
		if m.Disabled {
			updateState(name, StateDisabled, nil, nil, Counts{})
			slog.Debug("Skipping disabled MCP", "name", name)
			continue
		}

		// Set initial starting state
		wg.Add(1)
		go func(name string, m config.MCPConfig) {
			defer func() {
				wg.Done()
				if r := recover(); r != nil {
					var err error
					switch v := r.(type) {
					case error:
						err = v
					case string:
						err = fmt.Errorf("panic: %s", v)
					default:
						err = fmt.Errorf("panic: %v", v)
					}
					updateState(name, StateError, err, nil, Counts{})
					slog.Error("Panic in MCP client initialization", "error", err, "name", name)
				}
			}()

			if err := Reconnect(ctx, cfg, name); err != nil {
				return
			}
		}(name, m)
	}

	// Wait for either all servers to connect or the startup grace period to
	// elapse. Servers that haven't finished connecting are NOT cancelled —
	// they continue in the background and publish state changes via pubsub
	// once they complete. The grace period only controls when the main flow
	// is unblocked (initDone closed).
	gracePeriod := cfg.MCPStartupGracePeriod()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		// All servers connected within the grace period.
	case <-time.After(gracePeriod):
		slog.Warn("MCP startup grace period elapsed; continuing with background connections", "grace_period", gracePeriod)
	}
	initOnce.Do(func() { close(initDone) })
}

// WaitForInit blocks until MCP initialization is complete.
// If Initialize was never called, this returns immediately.
func WaitForInit(ctx context.Context) error {
	select {
	case <-initDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WaitForInitWithTimeout blocks until MCP initialization is complete or the
// given timeout elapses. It returns nil if initialization completed within
// the timeout, or an error describing the timeout otherwise. This is useful
// for non-interactive callers that want to bound how long they block on MCP
// startup; servers still connecting after the timeout continue in the
// background.
func WaitForInitWithTimeout(timeout time.Duration) error {
	return waitForInitWithTimeout(initDone, timeout)
}

// waitForInitWithTimeout is the testable core of WaitForInitWithTimeout. It
// accepts the initDone channel as a parameter so tests can exercise the
// timeout behavior with a controlled channel without relying on package
// state.
func waitForInitWithTimeout(initDone <-chan struct{}, timeout time.Duration) error {
	select {
	case <-initDone:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("MCP initialization did not complete within %v", timeout)
	}
}

// InitializeSingle initializes a single MCP client by name.
func InitializeSingle(ctx context.Context, name string, cfg *config.ConfigStore) error {
	m, exists := cfg.Config().MCP[name]
	if !exists {
		return fmt.Errorf("mcp '%s' not found in configuration", name)
	}

	if m.Disabled {
		updateState(name, StateDisabled, nil, nil, Counts{})
		slog.Debug("Skipping disabled MCP", "name", name)
		return nil
	}

	return initClient(ctx, cfg, name, m, cfg.Resolver())
}

// initClient initializes a single MCP client with the given configuration.
func initClient(ctx context.Context, cfg *config.ConfigStore, name string, m config.MCPConfig, resolver config.VariableResolver) error {
	// Set initial starting state.
	updateState(name, StateStarting, nil, nil, Counts{})

	// createSession handles its own timeout internally.
	session, err := createSession(ctx, cfg, name, m, resolver)
	if err != nil {
		return err
	}

	tools, err := getTools(ctx, session)
	if err != nil {
		slog.Error("Error listing tools", "error", err)
		updateState(name, stateForError(err), err, nil, Counts{})
		session.Close()
		return err
	}

	prompts, err := getPrompts(ctx, session)
	if err != nil {
		slog.Error("Error listing prompts", "error", err)
		updateState(name, stateForError(err), err, nil, Counts{})
		session.Close()
		return err
	}

	resources, err := getResources(ctx, session)
	if err != nil {
		slog.Error("Error listing resources", "error", err)
		updateState(name, stateForError(err), err, nil, Counts{})
		session.Close()
		return err
	}

	toolCount := updateTools(cfg, name, tools)
	updatePrompts(name, prompts)
	resourceCount := updateResources(name, resources)
	sessions.Set(name, session)

	// Persist the freshly fetched tools so a later startup can fall back to
	// them if this server becomes unreachable.
	saveCachedToolsFallback(ctx, name, m, tools)

	updateState(name, StateConnected, nil, session, Counts{
		Tools:     toolCount,
		Prompts:   len(prompts),
		Resources: resourceCount,
	})

	return nil
}

// DisableSingle disables and closes a single MCP client by name.
func DisableSingle(cfg *config.ConfigStore, name string) error {
	// Mark as disconnecting so any in-flight auto-reconnect loop stops
	// rather than racing with the disable.
	markDisconnecting(name)
	defer clearDisconnecting(name)
	// A disabled server must not retain a tripped breaker; otherwise the
	// user could never re-enable it without a manual reset.
	resetCircuitBreaker(name)

	session, ok := sessions.Get(name)
	if ok {
		if err := session.Close(); err != nil &&
			!errors.Is(err, io.EOF) &&
			!errors.Is(err, context.Canceled) &&
			err.Error() != "signal: killed" {
			slog.Warn("Error closing MCP session", "name", name, "error", err)
		}
		sessions.Del(name)
	}

	// Clear tools and prompts for this MCP.
	updateTools(cfg, name, nil)
	updatePrompts(name, nil)

	// Update state to disabled.
	updateState(name, StateDisabled, nil, nil, Counts{})

	slog.Info("Disabled mcp client", "name", name)
	return nil
}

func getOrRenewClient(ctx context.Context, cfg *config.ConfigStore, name string) (*ClientSession, error) {
	sess, ok := sessions.Get(name)
	if !ok {
		if err := Reconnect(ctx, cfg, name); err != nil {
			// Spawn a background reconnect loop with exponential backoff
			// so subsequent requests can find a live session once the
			// server recovers. The current request still fails.
			maybeStartReconnectLoop(cfg, name)
			return nil, err
		}
		sess, ok = sessions.Get(name)
		if !ok {
			return nil, fmt.Errorf("mcp '%s' not available", name)
		}
		return sess, nil
	}

	m := cfg.Config().MCP[name]
	state, _ := states.Get(name)

	timeout := mcpTimeout(m)
	pingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := sess.Ping(pingCtx, nil)
	if err == nil {
		return sess, nil
	}
	updateState(name, stateForError(maybeTimeoutErr(err, timeout)), maybeTimeoutErr(err, timeout), nil, state.Counts)

	if err := Reconnect(ctx, cfg, name); err != nil {
		// Spawn a background reconnect loop with exponential backoff so
		// subsequent requests can find a live session once the server
		// recovers. The current request still fails.
		maybeStartReconnectLoop(cfg, name)
		return nil, err
	}
	sess, ok = sessions.Get(name)
	if !ok {
		return nil, fmt.Errorf("mcp '%s' not available", name)
	}
	return sess, nil
}

// updateState updates the state of an MCP client and publishes an event
func updateState(name string, state State, err error, client *ClientSession, counts Counts) {
	info := ClientInfo{
		Name:   name,
		State:  state,
		Error:  err,
		Client: client,
		Counts: counts,
	}
	switch state {
	case StateConnected:
		info.ConnectedAt = time.Now()
	case StateCached:
		// Tools come from the cache; there is no live session to retain.
		// Prompts and resources are not cached, so clear any stale entries
		// left over from a previous live connection. This keeps the maps
		// consistent with Counts, which reports zero prompts and resources
		// in the cached state.
		info.Client = nil
		sessions.Del(name)
		allPrompts.Del(name)
		allResources.Del(name)
	case StateCircuitOpen:
		// The breaker only opens after both the live connection and the
		// cache fallback have failed, at which point StateError has already
		// cleared all tools, prompts, and resources. Clear them again here
		// for defensive consistency with StateError so the maps and counts
		// stay coherent regardless of how the breaker was reached.
		info.Client = nil
		info.Counts = Counts{}
		sessions.Del(name)
		allTools.Del(name)
		allPrompts.Del(name)
		allResources.Del(name)
	case StateDisabled, StateNeedsAuth, StateError:
		info.Client = nil
		info.Counts = Counts{}
		sessions.Del(name)
		allTools.Del(name)
		allPrompts.Del(name)
		allResources.Del(name)
	}
	states.Set(name, info)

	// Publish state change event
	broker.Publish(pubsub.UpdatedEvent, Event{
		Type:   EventStateChanged,
		Name:   name,
		State:  state,
		Error:  err,
		Counts: counts,
	})
}

func createSession(ctx context.Context, cfg *config.ConfigStore, name string, m config.MCPConfig, resolver config.VariableResolver) (*ClientSession, error) {
	timeout := mcpTimeout(m)
	mcpCtx, cancel := context.WithCancel(ctx)
	cancelTimer := time.AfterFunc(timeout, cancel)

	transport, err := createTransport(mcpCtx, cfg, name, m, resolver)
	if err != nil {
		updateState(name, stateForError(err), err, nil, Counts{})
		slog.Error("Error creating MCP client", "error", err, "name", name)
		cancel()
		cancelTimer.Stop()
		return nil, err
	}

	client := mcp.NewClient(
		&mcp.Implementation{
			Name:    "crush",
			Version: version.Version,
			Title:   "Crush",
		},
		&mcp.ClientOptions{
			ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
				broker.Publish(pubsub.UpdatedEvent, Event{
					Type: EventToolsListChanged,
					Name: name,
				})
			},
			PromptListChangedHandler: func(context.Context, *mcp.PromptListChangedRequest) {
				broker.Publish(pubsub.UpdatedEvent, Event{
					Type: EventPromptsListChanged,
					Name: name,
				})
			},
			ResourceListChangedHandler: func(context.Context, *mcp.ResourceListChangedRequest) {
				broker.Publish(pubsub.UpdatedEvent, Event{
					Type: EventResourcesListChanged,
					Name: name,
				})
			},
			LoggingMessageHandler: func(ctx context.Context, req *mcp.LoggingMessageRequest) {
				level := parseLevel(req.Params.Level)
				broker.Publish(pubsub.UpdatedEvent, Event{
					Type: EventLogMessage, Name: name,
					Log: LogMessage{
						Timestamp: time.Now(), Level: string(req.Params.Level),
						Logger: req.Params.Logger, Data: req.Params.Data,
					},
				})
				// Raw MCP log data may contain credentials. Structured GUI consumers
				// receive it only through the bounded redaction boundary above.
				slog.Log(ctx, level, "MCP log received", "name", name, "logger", req.Params.Logger)
			},
		},
	)

	session, err := client.Connect(mcpCtx, transport, nil)
	if err != nil {
		err = maybeStdioErr(err, transport)
		err = maybeTimeoutErr(err, timeout)
		updateState(name, stateForError(err), err, nil, Counts{})
		slog.Error("MCP client failed to initialize", "error", err, "name", name)
		cancel()
		cancelTimer.Stop()
		return nil, err
	}

	cancelTimer.Stop()
	slog.Debug("MCP client initialized", "name", name)
	return &ClientSession{session, cancel}, nil
}

// maybeStdioErr if a stdio mcp prints an error in non-json format, it'll fail
// to parse, and the cli will then close it, causing the EOF error.
// so, if we got an EOF err, and the transport is STDIO, we try to exec it
// again with a timeout and collect the output so we can add details to the
// error.
// this happens particularly when starting things with npx, e.g. if node can't
// be found or some other error like that.
func maybeStdioErr(err error, transport mcp.Transport) error {
	if !errors.Is(err, io.EOF) {
		return err
	}
	ct, ok := transport.(*mcp.CommandTransport)
	if !ok {
		return err
	}
	if err2 := stdioCheck(ct.Command); err2 != nil {
		err = errors.Join(err, err2)
	}
	return err
}

func maybeTimeoutErr(err error, timeout time.Duration) error {
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("timed out after %s", timeout)
	}
	return err
}

func stateForError(err error) State {
	if _, ok := NeedsAuth(err); ok {
		return StateNeedsAuth
	}
	if isAuthLikeError(err) {
		return StateNeedsAuth
	}
	return StateError
}

func isAuthLikeError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	// Network errors that mention auth-like words (e.g. "Proxy unauthorized:
	// connection refused") must not be classified as auth errors — they
	// indicate connectivity issues, not missing credentials. Check these
	// before the auth markers so the broad "unauthorized"/"forbidden"
	// markers do not misfire on transport errors.
	for _, netMarker := range []string{
		"connection refused",
		"connection reset",
		"dial tcp",
		"dial unix",
		"no such host",
		"i/o timeout",
		"network is unreachable",
		"proxy unauthorized",
	} {
		if strings.Contains(msg, netMarker) {
			return false
		}
	}
	for _, marker := range []string{
		"unauthorized",
		"forbidden",
		"www-authenticate",
		"authentication required",
		"status 401",
		"status: 401",
		"status code 401",
		"http 401",
		"http/1.1 401",
		"401 unauthorized",
		"status 403",
		"status: 403",
		"status code 403",
		"http 403",
		"http/1.1 403",
		"403 forbidden",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

func Reconnect(ctx context.Context, cfg *config.ConfigStore, name string) error {
	// Serialize reconnects per server so concurrent callers (reconnectLoop,
	// ResetCircuitBreaker, callToolWithRetry, RunTool) cannot race on
	// session double-close or state overwrites. Reconnect does not recurse
	// — none of its callees invoke Reconnect or reconnectLockFor — so this
	// cannot self-deadlock.
	mu := reconnectLockFor(name)
	mu.Lock()
	defer mu.Unlock()

	m, ok := cfg.Config().MCP[name]
	if !ok {
		return fmt.Errorf("mcp %s not found", name)
	}
	if m.Disabled {
		updateState(name, StateDisabled, nil, nil, Counts{})
		return nil
	}
	if existing, ok := sessions.Get(name); ok {
		_ = existing.Close()
	}
	updateState(name, StateStarting, nil, nil, Counts{})
	resolver := cfg.Resolver()
	session, err := createSession(ctx, cfg, name, m, resolver)
	if err != nil {
		if loadCachedToolsFallback(ctx, cfg, name, m, err) {
			return nil
		}
		return err
	}

	tools, err := getTools(ctx, session)
	if err != nil {
		slog.Error("Error listing tools", "error", err, "name", name)
		_ = session.Close()
		if loadCachedToolsFallback(ctx, cfg, name, m, err) {
			return nil
		}
		updateState(name, stateForError(err), err, nil, Counts{})
		return err
	}

	prompts, err := getPrompts(ctx, session)
	if err != nil {
		slog.Error("Error listing prompts", "error", err, "name", name)
		_ = session.Close()
		if loadCachedToolsFallback(ctx, cfg, name, m, err) {
			return nil
		}
		updateState(name, stateForError(err), err, nil, Counts{})
		return err
	}

	resources, err := getResources(ctx, session)
	if err != nil {
		slog.Error("Error listing resources", "error", err, "name", name)
		_ = session.Close()
		if loadCachedToolsFallback(ctx, cfg, name, m, err) {
			return nil
		}
		updateState(name, stateForError(err), err, nil, Counts{})
		return err
	}

	toolCount := updateTools(cfg, name, tools)
	updatePrompts(name, prompts)
	resourceCount := updateResources(name, resources)
	sessions.Set(name, session)

	// Persist the freshly fetched tools so a later startup can fall back to
	// them if this server becomes unreachable.
	saveCachedToolsFallback(ctx, name, m, tools)

	updateState(name, StateConnected, nil, session, Counts{
		Tools:     toolCount,
		Prompts:   len(prompts),
		Resources: resourceCount,
	})
	return nil
}

// loadCachedToolsFallback attempts to load cached tool definitions for the
// server and register them as deferred tools when a live connection could not
// be established. It reports whether the cache was successfully used. origErr
// is the error that triggered the fallback and is recorded on the resulting
// state so callers can inspect the cause.
func loadCachedToolsFallback(ctx context.Context, cfg *config.ConfigStore, name string, m config.MCPConfig, origErr error) bool {
	if queries == nil {
		return false
	}
	configHash := ComputeConfigHash(m)
	cacheCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tools, err := LoadCachedTools(cacheCtx, queries, name, configHash, DefaultCacheTTL)
	if err != nil {
		slog.Warn("MCP cache fallback failed", "name", name, "error", err)
		return false
	}
	toolCount := updateTools(cfg, name, tools)
	updateState(name, StateCached, origErr, nil, Counts{Tools: toolCount})
	slog.Info("MCP server connected from cache", "name", name, "tools", toolCount)
	return true
}

// saveCachedToolsFallback persists the current tool definitions so they can be
// reused on a later startup if the server is unreachable. Failures are
// non-fatal and only logged.
func saveCachedToolsFallback(ctx context.Context, name string, m config.MCPConfig, tools []*Tool) {
	if queries == nil {
		return
	}
	configHash := ComputeConfigHash(m)
	cacheCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := SaveCachedTools(cacheCtx, queries, name, configHash, tools); err != nil {
		slog.Warn("Failed to persist mcp tool cache", "name", name, "error", err)
	}
}

func createTransport(ctx context.Context, cfg *config.ConfigStore, name string, m config.MCPConfig, resolver config.VariableResolver) (mcp.Transport, error) {
	switch m.Type {
	case config.MCPStdio:
		command, err := resolver.ResolveValue(m.Command)
		if err != nil {
			return nil, fmt.Errorf("invalid mcp command: %w", err)
		}
		if strings.TrimSpace(command) == "" {
			return nil, fmt.Errorf("mcp stdio config requires a non-empty 'command' field")
		}
		cmd := exec.CommandContext(ctx, home.Long(command), m.Args...)
		cmd.Env = append(os.Environ(), m.ResolvedEnv()...)
		return &mcp.CommandTransport{
			Command: cmd,
		}, nil
	case config.MCPHttp:
		if strings.TrimSpace(m.URL) == "" {
			return nil, fmt.Errorf("mcp http config requires a non-empty 'url' field")
		}
		headers := m.ResolvedHeaders()
		transport := http.RoundTripper(&headerRoundTripper{
			headers: headers,
		})
		if m.SupportsInteractiveAuth() {
			authorizer := newMCPOAuthAuthorizer(name, cfg, headers)
			// Register the authorizer so RefreshToken can reuse its mutex
			// and refresh logic, avoiding a race with the round tripper's
			// pre-refresh path.
			authorizers.Set(name, authorizer)
			transport = &oauthRoundTripper{
				base:       http.DefaultTransport,
				headers:    headers,
				authorizer: authorizer,
			}
		}
		client := &http.Client{Transport: transport}
		return &mcp.StreamableClientTransport{
			Endpoint:   m.URL,
			HTTPClient: client,
		}, nil
	case config.MCPSSE:
		if strings.TrimSpace(m.URL) == "" {
			return nil, fmt.Errorf("mcp sse config requires a non-empty 'url' field")
		}
		client := &http.Client{
			Transport: &headerRoundTripper{
				headers: m.ResolvedHeaders(),
			},
		}
		return &mcp.SSEClientTransport{
			Endpoint:   m.URL,
			HTTPClient: client,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported mcp type: %s", m.Type)
	}
}

type headerRoundTripper struct {
	headers map[string]string
}

func (rt headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	for k, v := range rt.headers {
		req.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(req)
}

func mcpTimeout(m config.MCPConfig) time.Duration {
	return time.Duration(cmp.Or(m.Timeout, 15)) * time.Second
}

func stdioCheck(old *exec.Cmd) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	cmd := exec.CommandContext(ctx, old.Path, old.Args...)
	cmd.Env = old.Env
	out, err := cmd.CombinedOutput()
	if err == nil || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil
	}
	return fmt.Errorf("%w: %s", err, string(out))
}

// ensureLifecycle lazily initializes the package-level lifecycle context.
// The context is cancelled when Close is called, signalling background
// reconnect loops to exit.
func ensureLifecycle() {
	lifecycleOnce.Do(func() {
		lifecycleCtx, lifecycleCancel = context.WithCancel(context.Background())
	})
}

// circuitBreakerState holds the per-server circuit breaker state used to
// throttle auto-reconnect attempts when a server is unreachable. The
// methods below are pure functions on the value, so the breaker logic can
// be unit-tested without spinning up a real MCP server.
type circuitBreakerState struct {
	// failures counts reconnect failures within the current window.
	failures int
	// windowStart is the time at which the current failure window opened.
	// A zero value means no failures have been recorded yet.
	windowStart time.Time
	// open is true once the breaker has tripped; auto-reconnect loops must
	// stop while it is set.
	open bool
}

// recordFailure returns the breaker state after recording a reconnect
// failure at now. If the rolling window has elapsed since windowStart, the
// failure count is reset before the new failure is recorded. The breaker
// opens once failures reach threshold within the window.
func (s circuitBreakerState) recordFailure(now time.Time, window time.Duration, threshold int) circuitBreakerState {
	if s.windowStart.IsZero() || now.Sub(s.windowStart) >= window {
		s.failures = 0
		s.windowStart = now
		s.open = false
	}
	s.failures++
	if s.failures >= threshold {
		s.open = true
	}
	return s
}

// isOpen reports whether the breaker is currently open at the given time.
// A breaker whose window has elapsed since the last failure is considered
// closed so reconnects can resume.
func (s circuitBreakerState) isOpen(now time.Time, window time.Duration) bool {
	if !s.open {
		return false
	}
	if s.windowStart.IsZero() {
		return false
	}
	if now.Sub(s.windowStart) >= window {
		return false
	}
	return true
}

// recordCircuitBreakerFailure records a reconnect failure for the named
// server and reports whether its breaker is now open.
func recordCircuitBreakerFailure(name string) bool {
	now := time.Now()
	prev, ok := circuitBreakers.Get(name)
	if !ok {
		prev = circuitBreakerState{}
	}
	updated := prev.recordFailure(now, circuitBreakerWindow, circuitBreakerThreshold)
	circuitBreakers.Set(name, updated)
	return updated.open
}

// isCircuitBreakerOpen reports whether the breaker for the named server is
// currently open. Breakers whose window has elapsed are treated as closed.
func isCircuitBreakerOpen(name string) bool {
	prev, ok := circuitBreakers.Get(name)
	if !ok {
		return false
	}
	return prev.isOpen(time.Now(), circuitBreakerWindow)
}

// resetCircuitBreaker clears the breaker state for the named server so
// auto-reconnect can resume immediately.
func resetCircuitBreaker(name string) {
	circuitBreakers.Del(name)
}

// markDisconnecting records that the named server is being deliberately
// disconnected (e.g. disabled or manually reconnected). Auto-reconnect
// loops check this flag to avoid fighting with user-initiated actions.
func markDisconnecting(name string) {
	disconnecting.Set(name, struct{}{})
}

// clearDisconnecting clears the disconnecting flag. Call this after the
// user-initiated disconnect/reconnect completes.
func clearDisconnecting(name string) {
	disconnecting.Del(name)
}

// isDisconnecting reports whether the named server is being deliberately
// disconnected.
func isDisconnecting(name string) bool {
	_, ok := disconnecting.Get(name)
	return ok
}

// tryStartReconnect attempts to mark a reconnect loop as started for the
// named server. It returns false if a loop is already running, the server
// is being deliberately disconnected, or the lifecycle context is already
// cancelled. The atomic LoadOrStore on the reconnecting map guarantees
// that at most one loop runs per server even under concurrent callers.
func tryStartReconnect(name string) bool {
	ensureLifecycle()
	if lifecycleCtx.Err() != nil {
		return false
	}
	if isDisconnecting(name) {
		return false
	}
	_, loaded := reconnecting.LoadOrStore(name, struct{}{})
	return !loaded
}

// stopReconnect clears the reconnecting flag. It must be called when a
// reconnect loop exits so a future loop can be started.
func stopReconnect(name string) {
	reconnecting.Delete(name)
}

// reconnectLockFor returns the per-server mutex used to serialize reconnect
// operations. The mutex is created on first use (atomically via
// LoadOrStore) and reused for the lifetime of the server.
func reconnectLockFor(name string) *sync.Mutex {
	v, _ := reconnectMus.LoadOrStore(name, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// isReconnecting reports whether a background reconnectLoop is currently
// running for the named server. Tool-call retries consult this to avoid
// triggering a redundant synchronous reconnect that would race with the
// background loop and risk a reconnect storm.
func isReconnecting(name string) bool {
	_, ok := reconnecting.Load(name)
	return ok
}

// reconnectLoop attempts to reconnect to the named MCP server using the
// exponential backoff schedule in reconnectBackoffs. The loop stops early
// if:
//   - the context is cancelled (e.g. application shutdown),
//   - the server is being deliberately disconnected,
//   - the circuit breaker opens (after circuitBreakerThreshold failures
//     within circuitBreakerWindow), or
//   - a reconnect succeeds.
//
// Each failure is recorded against the breaker; once the breaker opens the
// server is published as StateCircuitOpen so the UI can surface that
// auto-reconnect is paused. The function is intended to run in a
// background goroutine spawned by maybeStartReconnectLoop.
func reconnectLoop(ctx context.Context, cfg *config.ConfigStore, name string) {
	for i, delay := range reconnectBackoffs {
		if ctx.Err() != nil {
			return
		}
		if isDisconnecting(name) {
			slog.Debug("Skipping reconnect; server is being disconnected manually", "name", name)
			return
		}
		if isCircuitBreakerOpen(name) {
			slog.Warn("Circuit breaker open; stopping reconnect loop", "name", name)
			prev, _ := states.Get(name)
			updateState(name, StateCircuitOpen, ErrCircuitOpen, nil, prev.Counts)
			return
		}
		err := reconnectFn(ctx, cfg, name)
		if err == nil {
			resetCircuitBreaker(name)
			return
		}
		slog.Warn("Reconnect attempt failed", "name", name, "attempt", i+1, "error", err)
		if recordCircuitBreakerFailure(name) {
			slog.Warn("Circuit breaker opened", "name", name)
			prev, _ := states.Get(name)
			updateState(name, StateCircuitOpen, ErrCircuitOpen, nil, prev.Counts)
			return
		}
		// Wait for the backoff delay before the next attempt. Skip the
		// sleep after the final attempt so the goroutine exits promptly.
		if i < len(reconnectBackoffs)-1 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
		}
	}
}

// maybeStartReconnectLoop starts a reconnectLoop goroutine for the named
// server if one is not already running and the server is not being
// deliberately disconnected. The loop uses the package lifecycle context
// so it is cancelled when Close is called.
func maybeStartReconnectLoop(cfg *config.ConfigStore, name string) {
	// closeMu serializes tryStartReconnect+Add with Close's
	// lifecycleCancel+Wait. This ensures the WaitGroup Add happens-before
	// Close's Wait, avoiding the classic WaitGroup race where Add(1)
	// occurs after Wait() has already observed a zero counter.
	closeMu.Lock()
	if !tryStartReconnect(name) {
		closeMu.Unlock()
		return
	}
	reconnectWg.Add(1)
	closeMu.Unlock()
	go func() {
		defer reconnectWg.Done()
		defer stopReconnect(name)
		reconnectLoop(lifecycleCtx, cfg, name)
	}()
}

// ResetCircuitBreaker clears the circuit breaker state for the named server
// and attempts a manual reconnect. It is intended for user-initiated
// reconnects (e.g. the /mcp reconnect command) to override a tripped
// breaker. The disconnecting flag is set for the duration of the call so
// any in-flight auto-reconnect loop stops and does not race with the
// manual reconnect.
func ResetCircuitBreaker(ctx context.Context, cfg *config.ConfigStore, name string) error {
	resetCircuitBreaker(name)
	markDisconnecting(name)
	defer clearDisconnecting(name)
	return reconnectFn(ctx, cfg, name)
}
