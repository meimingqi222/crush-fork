// Package app wires together services, coordinates agents, and manages
// application lifecycle.
package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/agent/notify"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/autopermission"
	"github.com/charmbracelet/crush/internal/checkpoint"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/event"
	"github.com/charmbracelet/crush/internal/filetracker"
	"github.com/charmbracelet/crush/internal/format"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/log"
	"github.com/charmbracelet/crush/internal/lsp"
	"github.com/charmbracelet/crush/internal/memory/engine"
	"github.com/charmbracelet/crush/internal/memory/hindsight"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/plugin"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/redact"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/shell"
	"github.com/charmbracelet/crush/internal/skills"
	"github.com/charmbracelet/crush/internal/timeline"
	"github.com/charmbracelet/crush/internal/toolruntime"
	"github.com/charmbracelet/crush/internal/ui/anim"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/crush/internal/update"
	"github.com/charmbracelet/crush/internal/userinput"
	"github.com/charmbracelet/crush/internal/version"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/charmtone"
	"github.com/charmbracelet/x/term"
)

// UpdateAvailableMsg is sent when a new version is available.
type UpdateAvailableMsg struct {
	CurrentVersion string
	LatestVersion  string
	IsDevelopment  bool
}

type App struct {
	Sessions      session.Service
	Messages      message.Service
	History       history.Service
	UserInput     userinput.Service
	Permissions   permission.Service
	FileTracker   filetracker.Service
	Checkpoint    checkpoint.Service
	ToolRuntime   toolruntime.Service
	Timeline      timeline.Service
	PluginRuntime *plugin.Runtime
	MemoryEngine  *engine.Engine

	AgentCoordinator agent.Coordinator

	LSPManager *lsp.Manager

	config *config.ConfigStore

	serviceEventsWG *sync.WaitGroup
	eventsCtx       context.Context
	events          chan tea.Msg
	tuiWG           *sync.WaitGroup

	// global context and cleanup functions
	globalCtx          context.Context
	cleanupFuncs       []func(context.Context) error
	agentNotifications *pubsub.Broker[notify.Notification]
	previousPluginRT   *plugin.Runtime
}

// New initializes a new application instance.
func New(ctx context.Context, conn *sql.DB, store *config.ConfigStore) (*App, error) {
	q := db.New(conn)
	cfg := store.Config()
	runtimeService := toolruntime.NewService()
	timelineService := timeline.NewService()

	var memoryEngine *engine.Engine
	var app *App
	sessions := session.NewServiceWithDeleteCallback(q, conn, func(sessionID string) {
		tools.GlobalFileCache.Clear(sessionID)
		runtimeService.DeleteSession(sessionID)
		if app != nil && app.AgentCoordinator != nil {
			if coord, ok := app.AgentCoordinator.(interface {
				onSessionDeleted(context.Context, string)
			}); ok {
				coord.onSessionDeleted(context.Background(), sessionID)
				return
			}
		}
		if memoryEngine != nil {
			if err := memoryEngine.OnSessionDeleted(context.Background(), sessionID); err != nil {
				slog.Warn("Memory engine OnSessionDeleted failed", "error", err, "session_id", sessionID)
			}
		}
	}, session.CollaborationModeDefault)
	preferredPermissionMode := session.NormalizePermissionMode(cfg.Options.PreferredPermissionMode)
	if skip := cfg.Permissions != nil && cfg.Permissions.SkipRequests; skip {
		preferredPermissionMode = session.PermissionModeYolo
	}
	sessions.SetDefaultPermissionMode(preferredPermissionMode)
	messages := message.NewService(q)
	files := history.NewService(q, conn)
	checkpointSvc := checkpoint.NewService(q, conn, files, store.WorkingDir())
	skipPermissionsRequests := cfg.Permissions != nil && cfg.Permissions.SkipRequests
	var allowedTools []string
	if cfg.Permissions != nil && cfg.Permissions.AllowedTools != nil {
		allowedTools = cfg.Permissions.AllowedTools
	}
	var autoModeConfig *config.AutoMode
	if cfg.Permissions != nil {
		autoModeConfig = cfg.Permissions.AutoMode
	}
	basePermissions := permission.NewPermissionService(store.WorkingDir(), skipPermissionsRequests, nil)
	pluginRuntime := plugin.NewRuntime()

	app = &App{
		Sessions:  sessions,
		Messages:  messages,
		History:   files,
		UserInput: userinput.NewService(),
		Permissions: autopermission.New(basePermissions, sessions, pluginRuntime, func() permission.Classifier {
			if app == nil || app.AgentCoordinator == nil {
				return nil
			}
			classifier, ok := app.AgentCoordinator.(permission.Classifier)
			if !ok {
				return nil
			}
			return classifier
		}, store.WorkingDir(), cfg.Permissions != nil && cfg.Permissions.FailClosedOnClassifierError, allowedTools, autoModeConfig),
		FileTracker:   filetracker.NewService(q),
		Checkpoint:    checkpointSvc,
		ToolRuntime:   runtimeService,
		Timeline:      timelineService,
		PluginRuntime: pluginRuntime,
		LSPManager:    lsp.NewManager(store),

		globalCtx: ctx,

		config: store,

		events:             make(chan tea.Msg, 100),
		serviceEventsWG:    &sync.WaitGroup{},
		tuiWG:              &sync.WaitGroup{},
		agentNotifications: pubsub.NewBroker[notify.Notification](),
	}

	app.previousPluginRT = plugin.SetDefaultRuntime(app.PluginRuntime)
	cleanupPluginRuntimeOnError := true
	defer func() {
		if !cleanupPluginRuntimeOnError {
			return
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if app != nil && app.PluginRuntime != nil {
			app.PluginRuntime.Close(shutdownCtx)
		}
		plugin.SetDefaultRuntime(app.previousPluginRT)
	}()

	plugin.Register(redact.NewPlugin())

	if err := app.PluginRuntime.Init(ctx, plugin.PluginInput{
		Config:     store,
		Sessions:   sessions,
		Messages:   messages,
		WorkingDir: store.WorkingDir(),
	}); err != nil {
		return nil, fmt.Errorf("failed to initialize plugins: %w", err)
	}

	app.setupEvents()

	// Check for updates in the background.
	go app.checkForUpdates(ctx)

	go mcp.Initialize(ctx, app.Permissions, store)

	// cleanup database upon app shutdown
	app.cleanupFuncs = append(
		app.cleanupFuncs,
		func(context.Context) error {
			return conn.Close()
		},
		func(ctx context.Context) error { return mcp.Close(ctx) },
	)

	// Wire the local memory engine before creating the coder agent so the
	// agent receives working-memory stores and compaction hooks at construction.
	if cfg.Options != nil && memoryConfigEnabled(cfg.Options.Memory) {
		memCfg := cfg.Options.Memory
		if memCfg == nil {
			memCfg = &config.MemoryConfig{}
		}
		backend := memCfg.BackendName()
		var bgInterval time.Duration
		var bgEveryNTurns int
		if backend == "local" && memCfg.BackgroundMaterialize.IsEnabled() {
			bgInterval = time.Duration(memCfg.BackgroundMaterialize.GetIntervalSeconds()) * time.Second
			bgEveryNTurns = memCfg.BackgroundMaterialize.GetEveryNTurns()
		}
		var consBgInterval time.Duration
		if backend == "local" && memCfg.BackgroundConsolidation.IsEnabled() {
			consBgInterval = time.Duration(memCfg.BackgroundConsolidation.GetIntervalSeconds()) * time.Second
		}
		eng := engine.New(conn, engine.Config{
			Enabled:               true,
			Backend:               backend,
			BackgroundInterval:    bgInterval,
			BackgroundEveryNTurns: bgEveryNTurns,
			ConsolidationInterval: consBgInterval,
		})
		startupMaterialization := true

		switch backend {
		case "hindsight":
			if memCfg.Remote == "" {
				eng.SetDegraded(true, "hindsight backend configured without memory.remote")
				slog.Warn("Hindsight memory backend requires memory.remote")
				break
			}
			token := memCfg.RemoteToken
			if token == "" {
				token = os.Getenv("HINDSIGHT_API_TOKEN")
			}
			projectLabel := config.ProjectSlug(store.WorkingDir())
			scope := hindsight.ResolveScope(memCfg.RemoteBankID, memCfg.RemoteScopingName(), projectLabel)
			hsClient := hindsight.NewClient(memCfg.Remote, scope.BankID, token)
			eng.SetTranscriptRetainer(hindsight.NewTranscriptRetainer(
				hsClient,
				hindsight.WithRetainTags(scope.RetainTags),
			))
			eng.SetRetriever(hindsight.NewRetriever(
				hsClient,
				hindsight.WithRecallTags(scope.RecallTags, scope.RecallTagsMatch),
			))
			startupMaterialization = false
			go func() {
				if err := hsClient.EnsureBank(context.Background(), ""); err != nil {
					slog.Warn("Hindsight EnsureBank failed", "error", err)
				}
			}()
			slog.Info(
				"Hindsight remote memory enabled",
				"url", memCfg.Remote,
				"bank", hsClient.BankID(),
				"scoping", memCfg.RemoteScopingName(),
				"project", projectLabel,
			)
		default:
			writer := engine.NewArtifactWriter(filepath.Join(cfg.Options.DataDirectory, "memory"))
			eng.SetMaterializer(engine.NewSummaryMaterializer(conn, eng.EventStore(), writer))
			eng.SetMaterializer(engine.NewMemoryMDMaterializer(conn, eng.EventStore(), writer))
			eng.SetMaterializer(engine.NewSkillsMaterializer(conn, eng.EventStore(), writer))
			if memCfg.MentalModels.IsEnabled() {
				eng.SetMaterializer(engine.NewMentalModelsMaterializer(conn, eng.EventStore(), writer, engine.DefaultMentalModels()))
			}
			if memCfg.Rollout.IsEnabled() {
				eng.SetMaterializer(engine.NewRolloutSummaryMaterializer(
					conn, eng.EventStore(), writer,
					memCfg.Rollout.GetMaxKeep(),
					memCfg.Rollout.GetMinEvents(),
				))
			}
			summaryRetriever := engine.NewSummaryRetriever(eng.EventStore(), conn, writer.OutputDir()).
				WithTripleStore(eng.TripleStore())
			if memCfg.Reranker.GetMaxCandidates() > 0 {
				summaryRetriever.WithMaxCandidates(memCfg.Reranker.GetMaxCandidates())
			}
			if rerank := buildLocalMemoryReranker(memCfg); rerank != nil {
				eng.SetReranker(rerank)
				summaryRetriever.WithReranker(rerank)
			}
			summaryRetriever.WithEmbeddingPipeline(eng.EmbeddingPipeline())
			eng.SetRetriever(summaryRetriever)
		}

		if startupMaterialization {
			go func() {
				if err := eng.TriggerMaterialization(context.Background()); err != nil {
					slog.Warn("Startup memory materialization failed", "error", err)
				}
			}()
		}

		// Start the background materializer goroutine. It is a no-op when
		// the configured interval is 0 (e.g. hindsight backend has none).
		eng.StartBackgroundMaterializer(context.Background())

		// Start the background consolidator goroutine. It is a no-op when
		// the configured interval is 0 (e.g. hindsight backend has none,
		// since it delegates consolidation to its remote service). For the
		// local backend, background consolidation is enabled by default so
		// long-running sessions merge episodic events into durable memory
		// on a timer instead of only on session close.
		eng.StartBackgroundConsolidator(context.Background())

		memoryEngine = eng
		app.MemoryEngine = eng
		// Note: eng.Close() is called from the serial section of Shutdown()
		// (before the parallel cleanup that closes the DB), NOT from
		// cleanupFuncs. This ordering guarantees the background loops have
		// fully stopped before conn.Close() runs, avoiding a race where an
		// in-flight pass uses a closed DB connection.
	}

	// TODO: remove the concept of agent config, most likely.
	if !cfg.IsConfigured() {
		slog.Warn("No agent configuration found")
		cleanupPluginRuntimeOnError = false
		return app, nil
	}
	if err := app.InitCoderAgent(ctx); err != nil {
		// If the failure is because the user's selected model can no longer
		// be resolved against the current provider config (e.g., a custom
		// provider's models were edited or removed), don't abort startup.
		// Let the TUI come up so the user can pick a valid model from the
		// model dialog and re-initialize the coder agent.
		if errors.Is(err, agent.ErrUnresolvedModel) {
			slog.Warn(
				"Coder agent not initialized: selected model is unavailable. "+
					"Open the model picker to choose a valid model.",
				"err", err,
			)
		} else {
			return nil, fmt.Errorf("failed to initialize coder agent: %w", err)
		}
	}

	// Set up callback for LSP state updates.
	app.LSPManager.SetCallback(func(name string, client *lsp.Client) {
		if client == nil {
			updateLSPState(name, lsp.StateUnstarted, nil, nil, 0)
			return
		}
		client.SetDiagnosticsCallback(updateLSPDiagnostics)
		updateLSPState(name, client.GetServerState(), nil, client, 0)
	})
	app.LSPManager.StartHealthCheck(ctx)
	go app.LSPManager.TrackConfigured()

	cleanupPluginRuntimeOnError = false
	return app, nil
}

// Config returns the pure-data configuration.
func (app *App) Config() *config.Config {
	return app.config.Config()
}

// Store returns the config store.
func (app *App) Store() *config.ConfigStore {
	return app.config
}

// AgentNotifications returns the broker for agent notification events.
func (app *App) AgentNotifications() *pubsub.Broker[notify.Notification] {
	return app.agentNotifications
}

func (app *App) GetSessions() session.Service {
	return app.Sessions
}

func (app *App) GetMessages() message.Service {
	return app.Messages
}

func (app *App) GetCoordinator() agent.Coordinator {
	return app.AgentCoordinator
}

func (app *App) GetConfig() *config.ConfigStore {
	return app.config
}

func (app *App) GetPermissions() permission.Service {
	return app.Permissions
}

func (app *App) GetToolRuntime() toolruntime.Service {
	return app.ToolRuntime
}

func (app *App) GetTimeline() timeline.Service {
	return app.Timeline
}

// resolveSession resolves which session to use for a non-interactive run.
// If continueSessionID is set, it looks up that session by ID.
// If useLast is set, it returns the most recently updated top-level session.
// Otherwise, it creates a new session.
func (app *App) resolveSession(ctx context.Context, continueSessionID string, useLast bool) (session.Session, error) {
	switch {
	case continueSessionID != "":
		if app.Sessions.IsAgentToolSession(continueSessionID) {
			return session.Session{}, fmt.Errorf("cannot continue an agent tool session: %s", continueSessionID)
		}
		sess, err := app.Sessions.Get(ctx, continueSessionID)
		if err != nil {
			return session.Session{}, fmt.Errorf("session not found: %s", continueSessionID)
		}
		if sess.ParentSessionID != "" {
			return session.Session{}, fmt.Errorf("cannot continue a child session: %s", continueSessionID)
		}
		return sess, nil

	case useLast:
		sess, err := app.Sessions.GetLast(ctx)
		if err != nil {
			return session.Session{}, fmt.Errorf("no sessions found to continue")
		}
		if sess.ParentSessionID != "" {
			return session.Session{}, fmt.Errorf("cannot continue a child session: %s", sess.ID)
		}
		return sess, nil

	default:
		return app.Sessions.Create(ctx, agent.DefaultSessionName)
	}
}

// RunNonInteractive runs the application in non-interactive mode with the
// given prompt, printing to stdout.
func (app *App) RunNonInteractive(ctx context.Context, output io.Writer, prompt, largeModel, smallModel string, hideSpinner bool, continueSessionID string, useLast bool) error {
	slog.Info("Running in non-interactive mode")

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if largeModel != "" || smallModel != "" {
		if err := app.overrideModelsForNonInteractive(ctx, largeModel, smallModel); err != nil {
			return fmt.Errorf("failed to override models: %w", err)
		}
	}

	var (
		spinner   *format.Spinner
		stdoutTTY bool
		stderrTTY bool
		stdinTTY  bool
		progress  bool
	)

	if f, ok := output.(*os.File); ok {
		stdoutTTY = term.IsTerminal(f.Fd())
	}
	stderrTTY = term.IsTerminal(os.Stderr.Fd())
	stdinTTY = term.IsTerminal(os.Stdin.Fd())
	progress = app.config.Config().Options.Progress == nil || *app.config.Config().Options.Progress

	if !hideSpinner && stderrTTY {
		t := styles.DefaultStyles()

		// Detect background color to set the appropriate color for the
		// spinner's 'Generating...' text. Without this, that text would be
		// unreadable in light terminals.
		hasDarkBG := true
		if f, ok := output.(*os.File); ok && stdinTTY && stdoutTTY {
			hasDarkBG = lipgloss.HasDarkBackground(os.Stdin, f)
		}
		defaultFG := lipgloss.LightDark(hasDarkBG)(charmtone.Pepper, t.FgBase)

		spinner = format.NewSpinner(ctx, cancel, anim.Settings{
			Size:        10,
			Label:       "Generating",
			LabelColor:  defaultFG,
			GradColorA:  t.Primary,
			GradColorB:  t.Secondary,
			CycleColors: true,
		})
		spinner.Start()
	}

	// Helper function to stop spinner once.
	stopSpinner := func() {
		if !hideSpinner && spinner != nil {
			spinner.Stop()
			spinner = nil
		}
	}

	// Wait for MCP initialization to complete before reading MCP tools.
	if err := mcp.WaitForInit(ctx); err != nil {
		return fmt.Errorf("failed to wait for MCP initialization: %w", err)
	}

	// force update of agent models before running so mcp tools are loaded
	app.AgentCoordinator.UpdateModels(ctx)

	defer stopSpinner()

	sess, err := app.resolveSession(ctx, continueSessionID, useLast)
	if err != nil {
		return fmt.Errorf("failed to resolve session: %w", err)
	}

	if continueSessionID != "" || useLast {
		slog.Info("Continuing session for non-interactive run", "session_id", sess.ID)
	} else {
		slog.Info("Created session for non-interactive run", "session_id", sess.ID)
	}

	// Automatically approve all permission requests for this non-interactive
	// session.
	app.Permissions.AutoApproveSession(sess.ID)

	type response struct {
		result *fantasy.AgentResult
		err    error
	}
	done := make(chan response, 1)

	go func(ctx context.Context, sessionID, prompt string) {
		result, err := app.AgentCoordinator.Run(ctx, sess.ID, prompt)
		if err != nil {
			done <- response{
				err: fmt.Errorf("failed to start agent processing stream: %w", err),
			}
			return
		}
		done <- response{
			result: result,
		}
	}(ctx, sess.ID, prompt)

	messageEvents := app.Messages.Subscribe(ctx)
	messageReadBytes := make(map[string]int)
	var printed bool

	defer func() {
		if progress && stderrTTY {
			_, _ = fmt.Fprintf(os.Stderr, ansi.ResetProgressBar)
		}

		// Always print a newline at the end. If output is a TTY this will
		// prevent the prompt from overwriting the last line of output.
		_, _ = fmt.Fprintln(output)
	}()

	for {
		if progress && stderrTTY {
			// HACK: Reinitialize the terminal progress bar on every iteration
			// so it doesn't get hidden by the terminal due to inactivity.
			_, _ = fmt.Fprintf(os.Stderr, ansi.SetIndeterminateProgressBar)
		}

		select {
		case result := <-done:
			stopSpinner()
			if result.err != nil {
				if errors.Is(result.err, context.Canceled) || errors.Is(result.err, agent.ErrRequestCancelled) {
					slog.Debug("Non-interactive: agent processing cancelled", "session_id", sess.ID)
					return nil
				}
				return fmt.Errorf("agent processing failed: %w", result.err)
			}
			return nil

		case event := <-messageEvents:
			msg := event.Payload
			if msg.SessionID == sess.ID && msg.Role == message.Assistant && len(msg.Parts) > 0 {
				stopSpinner()

				content := msg.Content().String()
				readBytes := messageReadBytes[msg.ID]

				if len(content) < readBytes {
					slog.Error("Non-interactive: message content is shorter than read bytes", "message_length", len(content), "read_bytes", readBytes)
					return fmt.Errorf("message content is shorter than read bytes: %d < %d", len(content), readBytes)
				}

				part := content[readBytes:]
				// Trim leading whitespace. Sometimes the LLM includes leading
				// formatting and intentation, which we don't want here.
				if readBytes == 0 {
					part = strings.TrimLeft(part, " \t")
				}
				// Ignore initial whitespace-only messages.
				if printed || strings.TrimSpace(part) != "" {
					printed = true
					fmt.Fprint(output, part)
				}
				messageReadBytes[msg.ID] = len(content)
			}

		case <-ctx.Done():
			stopSpinner()
			return ctx.Err()
		}
	}
}

func (app *App) UpdateAgentModel(ctx context.Context) error {
	if app.AgentCoordinator == nil {
		return fmt.Errorf("agent configuration is missing")
	}
	return app.AgentCoordinator.UpdateModels(ctx)
}

// overrideModelsForNonInteractive parses the model strings and temporarily
// overrides the model configurations, then rebuilds the agent.
// Format: "model-name" (searches all providers) or "provider/model-name".
// Model matching is case-insensitive.
// If largeModel is provided but smallModel is not, the small model defaults to
// the provider's default small model.
func (app *App) overrideModelsForNonInteractive(ctx context.Context, largeModel, smallModel string) error {
	providers := app.config.Config().Providers.Copy()

	largeMatches, smallMatches, err := findModels(providers, largeModel, smallModel)
	if err != nil {
		return err
	}

	var largeProviderID string

	// Override large model.
	if largeModel != "" {
		found, err := validateMatches(largeMatches, largeModel, "large")
		if err != nil {
			return err
		}
		largeProviderID = found.provider
		slog.Info("Overriding large model for non-interactive run", "provider", found.provider, "model", found.modelID)
		app.config.Config().Models[config.SelectedModelTypeLarge] = config.SelectedModel{
			Provider: found.provider,
			Model:    found.modelID,
		}
	}

	// Override small model.
	switch {
	case smallModel != "":
		found, err := validateMatches(smallMatches, smallModel, "small")
		if err != nil {
			return err
		}
		slog.Info("Overriding small model for non-interactive run", "provider", found.provider, "model", found.modelID)
		app.config.Config().Models[config.SelectedModelTypeSmall] = config.SelectedModel{
			Provider: found.provider,
			Model:    found.modelID,
		}

	case largeModel != "":
		// No small model specified, but large model was - use provider's default.
		smallCfg := app.GetDefaultSmallModel(largeProviderID)
		app.config.Config().Models[config.SelectedModelTypeSmall] = smallCfg
	}

	return app.AgentCoordinator.UpdateModels(ctx)
}

// GetDefaultSmallModel returns the default small model for the given
// provider. Falls back to the large model if no default is found.
func (app *App) GetDefaultSmallModel(providerID string) config.SelectedModel {
	cfg := app.config.Config()
	largeModelCfg := cfg.Models[config.SelectedModelTypeLarge]

	// Find the provider in the known providers list to get its default small model.
	knownProviders, _ := config.Providers(cfg)
	var knownProvider *catwalk.Provider
	for _, p := range knownProviders {
		if string(p.ID) == providerID {
			knownProvider = &p
			break
		}
	}

	// For unknown/local providers, use the large model as small.
	if knownProvider == nil {
		slog.Warn("Using large model as small model for unknown provider", "provider", providerID, "model", largeModelCfg.Model)
		return largeModelCfg
	}

	defaultSmallModelID := knownProvider.DefaultSmallModelID
	model := cfg.GetModel(providerID, defaultSmallModelID)
	if model == nil {
		slog.Warn("Default small model not found, using large model", "provider", providerID, "model", largeModelCfg.Model)
		return largeModelCfg
	}

	slog.Info("Using provider default small model", "provider", providerID, "model", defaultSmallModelID)
	return config.SelectedModel{
		Provider:        providerID,
		Model:           defaultSmallModelID,
		MaxTokens:       model.DefaultMaxTokens,
		ReasoningEffort: model.DefaultReasoningEffort,
	}
}

func (app *App) setupEvents() {
	ctx, cancel := context.WithCancel(app.globalCtx)
	app.eventsCtx = ctx
	app.setupTimeline(ctx)
	setupSubscriber(ctx, app.serviceEventsWG, "sessions", app.Sessions.Subscribe, app.events)
	setupMessageSubscriber(ctx, app.serviceEventsWG, app.PluginRuntime, app.Messages.Subscribe, app.events)
	setupSubscriber(ctx, app.serviceEventsWG, "user-input", app.UserInput.Subscribe, app.events)
	setupSubscriber(ctx, app.serviceEventsWG, "permissions", app.Permissions.Subscribe, app.events)
	setupSubscriber(ctx, app.serviceEventsWG, "permissions-notifications", app.Permissions.SubscribeNotifications, app.events)
	setupSubscriber(ctx, app.serviceEventsWG, "history", app.History.Subscribe, app.events)
	setupSubscriber(ctx, app.serviceEventsWG, "agent-notifications", app.agentNotifications.Subscribe, app.events)
	setupSubscriber(ctx, app.serviceEventsWG, "tool-runtime", app.ToolRuntime.Subscribe, app.events)
	setupSubscriber(ctx, app.serviceEventsWG, "timeline", app.Timeline.Subscribe, app.events)
	setupSubscriber(ctx, app.serviceEventsWG, "mcp", mcp.SubscribeEvents, app.events)
	setupSubscriber(ctx, app.serviceEventsWG, "lsp", SubscribeLSPEvents, app.events)
	setupSubscriber(ctx, app.serviceEventsWG, "skills", skills.SubscribeEvents, app.events)
	cleanupFunc := func(context.Context) error {
		cancel()
		if app.Timeline != nil {
			app.Timeline.Shutdown()
		}
		app.serviceEventsWG.Wait()
		if app.PluginRuntime != nil {
			plugin.SetDefaultRuntime(app.previousPluginRT)
		}
		return nil
	}
	app.cleanupFuncs = append(app.cleanupFuncs, cleanupFunc)
}

const subscriberSendTimeout = 2 * time.Second

func setupSubscriber[T any](
	ctx context.Context,
	wg *sync.WaitGroup,
	name string,
	subscriber func(context.Context) <-chan pubsub.Event[T],
	outputCh chan<- tea.Msg,
) {
	wg.Go(func() {
		subCh := subscriber(ctx)
		sendTimer := time.NewTimer(0)
		<-sendTimer.C
		defer sendTimer.Stop()

		for {
			select {
			case event, ok := <-subCh:
				if !ok {
					slog.Debug("Subscription channel closed", "name", name)
					return
				}
				var msg tea.Msg = event
				if !sendTimer.Stop() {
					select {
					case <-sendTimer.C:
					default:
					}
				}
				sendTimer.Reset(subscriberSendTimeout)

				select {
				case outputCh <- msg:
				case <-sendTimer.C:
					slog.Debug("Message dropped due to slow consumer", "name", name)
				case <-ctx.Done():
					slog.Debug("Subscription cancelled", "name", name)
					return
				}
			case <-ctx.Done():
				slog.Debug("Subscription cancelled", "name", name)
				return
			}
		}
	})
}

func setupMessageSubscriber(
	ctx context.Context,
	wg *sync.WaitGroup,
	runtime *plugin.Runtime,
	subscriber func(context.Context) <-chan pubsub.Event[message.Message],
	outputCh chan<- tea.Msg,
) {
	wg.Go(func() {
		if runtime == nil {
			runtime = plugin.DefaultRuntime()
		}
		subCh := subscriber(ctx)
		sendTimer := time.NewTimer(0)
		<-sendTimer.C
		defer sendTimer.Stop()

		for {
			select {
			case event, ok := <-subCh:
				if !ok {
					slog.Debug("Subscription channel closed", "name", "messages")
					return
				}
				if event.Type == pubsub.CreatedEvent {
					if err := runtime.TriggerMessageCreated(ctx, event.Payload); err != nil {
						slog.Error("Plugin message created hook failed", "error", err, "message_id", event.Payload.ID)
					}
				}
				var msg tea.Msg = event
				if !sendTimer.Stop() {
					select {
					case <-sendTimer.C:
					default:
					}
				}
				sendTimer.Reset(subscriberSendTimeout)

				select {
				case outputCh <- msg:
				case <-sendTimer.C:
					slog.Debug("Message dropped due to slow consumer", "name", "messages")
				case <-ctx.Done():
					slog.Debug("Subscription cancelled", "name", "messages")
					return
				}
			case <-ctx.Done():
				slog.Debug("Subscription cancelled", "name", "messages")
				return
			}
		}
	})
}

func (app *App) InitCoderAgent(ctx context.Context) error {
	coderAgentCfg := app.config.Config().Agents[config.AgentCoder]
	if coderAgentCfg.ID == "" {
		return fmt.Errorf("coder agent configuration is missing")
	}
	var err error
	app.AgentCoordinator, err = agent.NewCoordinator(
		ctx,
		app.config,
		app.Sessions,
		app.Messages,
		app.Permissions,
		app.UserInput,
		app.History,
		app.FileTracker,
		app.Checkpoint,
		app.LSPManager,
		app.agentNotifications,
		app.ToolRuntime,
		app.Timeline,
		app.PluginRuntime,
		app.MemoryEngine,
	)
	if err != nil {
		slog.Error("Failed to create coder agent", "err", err)
		return err
	}
	return nil
}

func memoryConfigEnabled(memCfg *config.MemoryConfig) bool {
	if memCfg == nil {
		return true
	}
	return memCfg.IsEnabled()
}

func buildLocalMemoryReranker(memCfg *config.MemoryConfig) engine.Reranker {
	if memCfg == nil {
		return nil
	}
	if memCfg.Embeddings != nil && memCfg.Embeddings.IsEnabled() {
		switch memCfg.Embeddings.BackendName() {
		case "hashing", "":
			return engine.NewEmbeddingReranker(engine.NewHashingEmbedder(memCfg.Embeddings.GetDimensions()))
		case "provider":
			embedder := engine.NewProviderEmbedder(engine.ProviderEmbedderConfig{
				APIURL:     memCfg.Embeddings.ProviderAPIURL,
				APIKey:     memCfg.Embeddings.ProviderAPIKey,
				Model:      memCfg.Embeddings.ProviderModel,
				Dimensions: memCfg.Embeddings.GetDimensions(),
			})
			return engine.NewEmbeddingReranker(embedder)
		default:
			slog.Warn("Memory embedding backend not implemented, falling back to hashing",
				"backend", memCfg.Embeddings.BackendName())
			return engine.NewEmbeddingReranker(engine.NewHashingEmbedder(memCfg.Embeddings.GetDimensions()))
		}
	}
	if !memCfg.Reranker.IsEnabled() {
		return nil
	}
	switch memCfg.Reranker.GetType() {
	case "embedding", "hybrid":
		dimensions := 384
		if memCfg.Embeddings != nil {
			dimensions = memCfg.Embeddings.GetDimensions()
		}
		return engine.NewEmbeddingReranker(engine.NewHashingEmbedder(dimensions))
	case "heuristic", "":
		return engine.NewHeuristicReranker()
	default:
		slog.Warn("Memory reranker type not implemented, falling back to heuristic",
			"type", memCfg.Reranker.GetType())
		return engine.NewHeuristicReranker()
	}
}

// Subscribe sends events to the TUI as tea.Msgs.
func (app *App) Subscribe(program *tea.Program) {
	defer log.RecoverPanic("app.Subscribe", func() {
		slog.Info("TUI subscription panic: attempting graceful shutdown")
		program.Quit()
	})

	app.tuiWG.Add(1)
	tuiCtx, tuiCancel := context.WithCancel(app.globalCtx)
	app.cleanupFuncs = append(app.cleanupFuncs, func(context.Context) error {
		slog.Debug("Cancelling TUI message handler")
		tuiCancel()
		app.tuiWG.Wait()
		return nil
	})
	defer app.tuiWG.Done()

	for {
		select {
		case <-tuiCtx.Done():
			slog.Debug("TUI message handler shutting down")
			return
		case msg, ok := <-app.events:
			if !ok {
				slog.Debug("TUI message channel closed")
				return
			}
			program.Send(msg)
		}
	}
}

// Shutdown performs a graceful shutdown of the application.
//
// Intentionally does NOT run a final memory consolidation on exit: episodic
// events are already persisted to SQLite by AfterTurnIdle on every turn, and
// the background consolidator (enabled by default) merges them into durable
// memory on a timer. A fresh LLM consolidation call at exit would block the
// user for 30-45s for little gain — it only advances the materialized views
// (MEMORY.md, mental_models) which the next run's ticker refreshes anyway.
// This matches oh-my-pi and MiMo-Code, which both treat consolidation as a
// background task and never block exit on it.
//
// A second SIGINT during shutdown cancels the in-flight cleanup, so the user
// can always force a faster exit by pressing Ctrl+C again.
func (app *App) Shutdown() {
	start := time.Now()
	defer func() { slog.Debug("Shutdown took " + time.Since(start).String()) }()

	// Root context for the entire shutdown. Cancelled by a second
	// SIGINT/SIGTERM so cleanup tasks abort promptly if the user wants out.
	shutdownRootCtx, stopSignalCancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignalCancel()

	// First, cancel all agents and wait for them to finish. This must complete
	// before closing the DB so agents can finish writing their state.
	if app.AgentCoordinator != nil {
		app.AgentCoordinator.CancelAll()
	}

	// Tear down the engine serially before the parallel cleanup closes the DB
	// connection. Close() stops the background goroutines, cancelling any
	// in-flight pass so it returns quickly (no LLM call on this path).
	if app.MemoryEngine != nil {
		if err := app.MemoryEngine.Close(); err != nil {
			slog.Warn("Memory engine close failed", "error", err)
		}
	}

	// Now run remaining cleanup tasks in parallel.
	var wg sync.WaitGroup

	// Shared context for all timeout-bounded cleanup. Also derived from
	// shutdownRootCtx so it respects a second Ctrl+C.
	shutdownCtx, cancel := context.WithTimeout(shutdownRootCtx, 5*time.Second)
	defer cancel()

	// Send exit event
	wg.Go(func() {
		event.AppExited()
	})

	// Kill all background shells.
	wg.Go(func() {
		shell.GetBackgroundShellManager().KillAll(shutdownCtx)
	})

	// Shutdown all LSP clients.
	wg.Go(func() {
		app.LSPManager.KillAll(shutdownCtx)
	})

	// Shutdown all persistent plugins.
	wg.Go(func() {
		if app.PluginRuntime != nil {
			app.PluginRuntime.Close(shutdownCtx)
			return
		}
		plugin.Close(shutdownCtx)
	})

	// Call all cleanup functions.
	for _, cleanup := range app.cleanupFuncs {
		if cleanup != nil {
			wg.Go(func() {
				if err := cleanup(shutdownCtx); err != nil {
					slog.Error("Failed to cleanup app properly on shutdown", "error", err)
				}
			})
		}
	}
	wg.Wait()
}

// checkForUpdates checks for available updates.
func (app *App) checkForUpdates(ctx context.Context) {
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	info, err := update.Check(checkCtx, version.Version, update.Default)
	if err != nil || !info.Available() {
		return
	}
	app.events <- UpdateAvailableMsg{
		CurrentVersion: info.Current,
		LatestVersion:  info.Latest,
		IsDevelopment:  info.IsDevelopment(),
	}
}
