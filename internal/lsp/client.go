package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/fsext"
	"github.com/charmbracelet/crush/internal/home"
	"github.com/charmbracelet/crush/internal/lsp/util"
	powernap "github.com/charmbracelet/x/powernap/pkg/lsp"
	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
	"github.com/charmbracelet/x/powernap/pkg/transport"
)

// DiagnosticCounts holds the count of diagnostics by severity.
type DiagnosticCounts struct {
	Error       int
	Warning     int
	Information int
	Hint        int
}

const (
	MethodTextDocumentDefinition           = "textDocument/definition"
	MethodTextDocumentDeclaration          = "textDocument/declaration"
	MethodTextDocumentImplementation       = "textDocument/implementation"
	MethodTextDocumentTypeDefinition       = "textDocument/typeDefinition"
	MethodTextDocumentDocumentSymbol       = "textDocument/documentSymbol"
	MethodTextDocumentCodeAction           = "textDocument/codeAction"
	MethodTextDocumentRename               = "textDocument/rename"
	MethodTextDocumentFormatting           = "textDocument/formatting"
	MethodTextDocumentPrepareCallHierarchy = "textDocument/prepareCallHierarchy"
	MethodCallHierarchyIncomingCalls       = "callHierarchy/incomingCalls"
	MethodCallHierarchyOutgoingCalls       = "callHierarchy/outgoingCalls"
	MethodWorkspaceSymbol                  = "workspace/symbol"
)

type Client struct {
	client *powernap.Client
	name   string
	debug  bool

	// Working directory this LSP is scoped to.
	cwd string

	// File types this LSP server handles (e.g., .go, .rs, .py)
	fileTypes []string

	// Configuration for this LSP client
	config config.LSPConfig

	// Original context and resolver for recreating the client
	ctx      context.Context
	resolver config.VariableResolver

	// Diagnostic change callback
	onDiagnosticsChanged func(name string, count int)

	// Diagnostic cache
	diagnostics *csync.VersionedMap[protocol.DocumentURI, []protocol.Diagnostic]

	// Cached diagnostic counts to avoid map copy on every UI render.
	diagCountsCache   DiagnosticCounts
	diagCountsVersion uint64
	diagCountsMu      sync.Mutex

	// Files are currently opened by the LSP
	openFiles *csync.Map[string, *OpenFileInfo]

	// Server state
	serverState atomic.Value

	// Active work progress tracking
	progressMu sync.Mutex
	progresses map[string]*ProgressInfo

	// Callback triggered when server state or progress changes
	onUpdate func()

	// Protects client field from concurrent read/write during Restart
	clientMu sync.RWMutex
}

// New creates a new LSP client using the powernap implementation.
func New(
	ctx context.Context,
	name string,
	cfg config.LSPConfig,
	resolver config.VariableResolver,
	cwd string,
	debug bool,
) (*Client, error) {
	client := &Client{
		name:        name,
		fileTypes:   cfg.FileTypes,
		diagnostics: csync.NewVersionedMap[protocol.DocumentURI, []protocol.Diagnostic](),
		openFiles:   csync.NewMap[string, *OpenFileInfo](),
		config:      cfg,
		ctx:         ctx,
		debug:       debug,
		resolver:    resolver,
		cwd:         cwd,
		progresses:  make(map[string]*ProgressInfo),
	}
	client.serverState.Store(StateStopped)

	if err := client.createPowernapClient(); err != nil {
		return nil, err
	}

	return client, nil
}

// Initialize initializes the LSP client and returns the server capabilities.
func (c *Client) Initialize(ctx context.Context, workspaceDir string) (*protocol.InitializeResult, error) {
	c.registerHandlers()

	client := c.getClient()
	if client == nil {
		return nil, fmt.Errorf("LSP client is unavailable")
	}

	if err := client.Initialize(ctx, false); err != nil {
		return nil, fmt.Errorf("failed to initialize the lsp client: %w", err)
	}

	// Convert powernap capabilities to protocol capabilities
	caps := client.GetCapabilities()
	protocolCaps := protocol.ServerCapabilities{
		TextDocumentSync: caps.TextDocumentSync,
		CompletionProvider: func() *protocol.CompletionOptions {
			if caps.CompletionProvider != nil {
				return &protocol.CompletionOptions{
					TriggerCharacters:   caps.CompletionProvider.TriggerCharacters,
					AllCommitCharacters: caps.CompletionProvider.AllCommitCharacters,
					ResolveProvider:     caps.CompletionProvider.ResolveProvider,
				}
			}
			return nil
		}(),
		HoverProvider:              caps.HoverProvider,
		DefinitionProvider:         caps.DefinitionProvider,
		DeclarationProvider:        caps.DeclarationProvider,
		TypeDefinitionProvider:     caps.TypeDefinitionProvider,
		ImplementationProvider:     caps.ImplementationProvider,
		ReferencesProvider:         caps.ReferencesProvider,
		DocumentSymbolProvider:     caps.DocumentSymbolProvider,
		CodeActionProvider:         caps.CodeActionProvider,
		DocumentFormattingProvider: caps.DocumentFormattingProvider,
		RenameProvider:             caps.RenameProvider,
		WorkspaceSymbolProvider:    caps.WorkspaceSymbolProvider,
	}

	result := &protocol.InitializeResult{
		Capabilities: protocolCaps,
	}

	return result, nil
}

// closeTimeout is the maximum time to wait for a graceful LSP shutdown.
const closeTimeout = 5 * time.Second

// Kill kills the client without doing anything else.
func (c *Client) Kill() {
	if client := c.getClient(); client != nil {
		client.Kill()
	}
}

// Close closes all open files in the client, then shuts down gracefully.
// If shutdown takes longer than closeTimeout, it falls back to Kill().
func (c *Client) Close(ctx context.Context) error {
	c.CloseAllFiles(ctx)

	client := c.getClient()
	if client == nil {
		return nil
	}

	// Use a timeout to prevent hanging on unresponsive LSP servers.
	// jsonrpc2's send lock doesn't respect context cancellation, so we
	// need to fall back to Kill() which closes the underlying connection.
	closeCtx, cancel := context.WithTimeout(ctx, closeTimeout)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		if err := client.Shutdown(closeCtx); err != nil {
			slog.Warn("Failed to shutdown LSP client", "error", err)
		}
		done <- client.Exit()
	}()

	select {
	case err := <-done:
		return err
	case <-closeCtx.Done():
		client.Kill()
		return closeCtx.Err()
	}
}

// createPowernapClient creates a new powernap client with the current configuration.
func (c *Client) createPowernapClient() error {
	rootURI := string(protocol.URIFromPath(c.cwd))

	command, err := c.resolver.ResolveValue(c.config.Command)
	if err != nil {
		return fmt.Errorf("invalid lsp command: %w", err)
	}

	clientConfig := powernap.ClientConfig{
		Command:     home.Long(command),
		Args:        c.config.Args,
		RootURI:     rootURI,
		Environment: maps.Clone(c.config.Env),
		Settings:    c.config.Options,
		InitOptions: c.config.InitOptions,
		WorkspaceFolders: []protocol.WorkspaceFolder{
			{
				URI:  rootURI,
				Name: filepath.Base(c.cwd),
			},
		},
	}

	powernapClient, err := powernap.NewClient(clientConfig)
	if err != nil {
		return fmt.Errorf("failed to create lsp client: %w", err)
	}

	c.clientMu.Lock()
	c.client = powernapClient
	c.clientMu.Unlock()
	return nil
}

// registerHandlers registers the standard LSP notification and request handlers.
func (c *Client) registerHandlers() {
	client := c.getClient()
	if client == nil {
		return
	}
	c.RegisterServerRequestHandler("workspace/applyEdit", HandleApplyEdit(client.GetOffsetEncoding()))
	c.RegisterServerRequestHandler("workspace/configuration", HandleWorkspaceConfiguration)
	c.RegisterServerRequestHandler("client/registerCapability", HandleRegisterCapability)
	c.RegisterServerRequestHandler("window/workDoneProgress/create", func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		return HandleWorkDoneProgressCreate(c, ctx, method, params)
	})
	c.RegisterNotificationHandler("$/progress", func(_ context.Context, _ string, params json.RawMessage) {
		HandleProgressNotification(c, params)
	})
	c.RegisterNotificationHandler("window/showMessage", func(ctx context.Context, method string, params json.RawMessage) {
		if c.debug {
			HandleServerMessage(ctx, method, params)
		}
	})
	c.RegisterNotificationHandler("textDocument/publishDiagnostics", func(_ context.Context, _ string, params json.RawMessage) {
		HandleDiagnostics(c, params)
	})
}

// Restart closes the current LSP client and creates a new one with the same
// configuration. The provided context controls the overall restart operation;
// individual phases (close and initialize) are additionally capped with their
// own shorter timeouts so a hung LSP process cannot block indefinitely.
func (c *Client) Restart(ctx context.Context) error {
	var openFiles []string
	for uri := range c.openFiles.Seq2() {
		openFiles = append(openFiles, string(uri))
	}

	closeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if err := c.Close(closeCtx); err != nil {
		slog.Warn("Error closing client during restart", "name", c.name, "error", err)
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	c.SetServerState(StateStopped)

	c.diagCountsCache = DiagnosticCounts{}
	c.diagCountsVersion = 0

	c.progressMu.Lock()
	c.progresses = make(map[string]*ProgressInfo)
	c.progressMu.Unlock()

	if err := c.createPowernapClient(); err != nil {
		return err
	}

	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	c.SetServerState(StateStarting)

	c.registerHandlers()

	powernapClient := c.getClient()
	if powernapClient == nil {
		c.SetServerState(StateError)
		return fmt.Errorf("LSP client is unavailable after restart")
	}

	if err := powernapClient.Initialize(initCtx, false); err != nil {
		c.SetServerState(StateError)
		return fmt.Errorf("failed to initialize lsp client: %w", err)
	}

	if err := c.WaitForServerReady(initCtx); err != nil {
		slog.Error("Server failed to become ready after restart", "name", c.name, "error", err)
		c.SetServerState(StateError)
		return err
	}

	for _, uri := range openFiles {
		if err := c.OpenFile(initCtx, uri); err != nil {
			slog.Warn("Failed to reopen file after restart", "file", uri, "error", err)
		}
	}
	return nil
}

// ServerState represents the state of an LSP server
type ServerState int

const (
	StateUnstarted ServerState = iota
	StateStarting
	StateReady
	StateError
	StateStopped
	StateDisabled
	StateIndexing
)

// ProgressInfo represents details of a work progress reported by LSP.
type ProgressInfo struct {
	Title      string
	Message    string
	Percentage float64
}

// getClient returns the powernap client under RLock.
func (c *Client) getClient() *powernap.Client {
	if c == nil {
		return nil
	}
	c.clientMu.RLock()
	defer c.clientMu.RUnlock()
	return c.client
}

// IsRunning returns whether the underlying LSP client is running.
func (c *Client) IsRunning() bool {
	client := c.getClient()
	return client != nil && client.IsRunning()
}

// GetServerState returns the current state of the LSP server
func (c *Client) GetServerState() ServerState {
	if val := c.serverState.Load(); val != nil {
		return val.(ServerState)
	}
	return StateStarting
}

// ProgressDescription returns a formatted status description of the active work progresses.
func (c *Client) ProgressDescription() string {
	c.progressMu.Lock()
	defer c.progressMu.Unlock()

	if len(c.progresses) == 0 {
		return ""
	}

	var keys []string
	for k := range c.progresses {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	var parts []string
	for _, k := range keys {
		info := c.progresses[k]
		desc := ""
		if info.Title != "" {
			desc += info.Title
		}
		if info.Percentage > 0 {
			if desc != "" {
				desc += fmt.Sprintf(": %.0f%%", info.Percentage)
			} else {
				desc += fmt.Sprintf("%.0f%%", info.Percentage)
			}
		}
		if info.Message != "" {
			if desc != "" {
				desc += fmt.Sprintf(" (%s)", info.Message)
			} else {
				desc += info.Message
			}
		}
		if desc != "" {
			parts = append(parts, desc)
		}
	}

	if len(parts) == 0 {
		return "indexing..."
	}

	return strings.Join(parts, "; ")
}

// SetServerState sets the current state of the LSP server.
// When setting StateReady, if there are active progress tokens the state is
// promoted to StateIndexing. When setting StateIndexing, if there are no
// active progress tokens the state is demoted back to StateReady.
func (c *Client) SetServerState(state ServerState) {
	c.progressMu.Lock()
	switch state {
	case StateReady:
		if len(c.progresses) > 0 {
			state = StateIndexing
		}
	case StateIndexing:
		if len(c.progresses) == 0 {
			state = StateReady
		}
	}
	c.progressMu.Unlock()

	prevState := c.GetServerState()
	c.serverState.Store(state)
	if prevState != state && c.onUpdate != nil {
		c.onUpdate()
	}
}

// GetName returns the name of the LSP client
func (c *Client) GetName() string {
	return c.name
}

// SetDiagnosticsCallback sets the callback function for diagnostic changes
func (c *Client) SetDiagnosticsCallback(callback func(name string, count int)) {
	c.onDiagnosticsChanged = callback
}

// WaitForServerReady waits for the server to be ready
func (c *Client) WaitForServerReady(ctx context.Context) error {
	// Try to ping the server with a simple request
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	if c.debug {
		slog.Debug("Waiting for LSP server to be ready...")
	}

	c.openKeyConfigFiles(ctx)

	for {
		select {
		case <-ctx.Done():
			c.SetServerState(StateError)
			return fmt.Errorf("timeout waiting for LSP server to be ready")
		case <-ticker.C:
			// Check if client is running
			if !c.IsRunning() {
				if c.debug {
					slog.Debug("LSP server not ready yet", "server", c.name)
				}
				continue
			}

			// Server is ready
			c.SetServerState(StateReady)
			if c.debug {
				slog.Debug("LSP server is ready")
			}
			return nil
		}
	}
}

// OpenFileInfo contains information about an open file
type OpenFileInfo struct {
	Version int32
	URI     protocol.DocumentURI
}

// HandlesFile checks if this LSP client handles the given file based on its
// extension and whether it's within the working directory.
func (c *Client) HandlesFile(path string) bool {
	if c == nil {
		return false
	}
	if !fsext.HasPrefix(path, c.cwd) {
		slog.Debug("File outside workspace", "name", c.name, "file", path, "workDir", c.cwd)
		return false
	}
	return handlesFiletype(c.name, c.fileTypes, path)
}

// OpenFile opens a file in the LSP server.
func (c *Client) OpenFile(ctx context.Context, filepath string) error {
	if !c.HandlesFile(filepath) {
		return nil
	}

	uri := string(protocol.URIFromPath(filepath))

	if _, exists := c.openFiles.Get(uri); exists {
		return nil // Already open
	}

	// Skip files that do not exist or cannot be read
	content, err := os.ReadFile(filepath)
	if err != nil {
		return fmt.Errorf("error reading file: %w", err)
	}

	client := c.getClient()
	if client == nil {
		return fmt.Errorf("LSP client is unavailable")
	}

	// Notify the server about the opened document
	if err = client.NotifyDidOpenTextDocument(ctx, uri, string(powernap.DetectLanguage(filepath)), 1, string(content)); err != nil {
		return err
	}

	c.openFiles.Set(uri, &OpenFileInfo{
		Version: 1,
		URI:     protocol.DocumentURI(uri),
	})

	return nil
}

// NotifyChange notifies the server about a file change.
func (c *Client) NotifyChange(ctx context.Context, filepath string) error {
	if c == nil {
		return nil
	}
	uri := string(protocol.URIFromPath(filepath))

	content, err := os.ReadFile(filepath)
	if err != nil {
		return fmt.Errorf("error reading file: %w", err)
	}

	fileInfo, isOpen := c.openFiles.Get(uri)
	if !isOpen {
		return fmt.Errorf("cannot notify change for unopened file: %s", filepath)
	}

	// Increment version atomically to avoid race conditions
	newVersion := atomic.AddInt32(&fileInfo.Version, 1)

	// Create change event
	changes := []protocol.TextDocumentContentChangeEvent{
		{
			Value: protocol.TextDocumentContentChangeWholeDocument{
				Text: string(content),
			},
		},
	}

	client := c.getClient()
	if client == nil {
		return fmt.Errorf("LSP client is unavailable")
	}

	return client.NotifyDidChangeTextDocument(ctx, uri, int(newVersion), changes)
}

// IsFileOpen checks if a file is currently open.
func (c *Client) IsFileOpen(filepath string) bool {
	uri := string(protocol.URIFromPath(filepath))
	_, exists := c.openFiles.Get(uri)
	return exists
}

// CloseAllFiles closes all currently open files.
func (c *Client) CloseAllFiles(ctx context.Context) {
	client := c.getClient()
	if client == nil {
		return
	}

	for uri := range c.openFiles.Seq2() {
		if c.debug {
			slog.Debug("Closing file", "file", uri)
		}
		if err := client.NotifyDidCloseTextDocument(ctx, uri); err != nil {
			slog.Warn("Error closing file", "uri", uri, "error", err)
			continue
		}
		c.openFiles.Del(uri)
	}
}

// GetFileDiagnostics returns diagnostics for a specific file.
func (c *Client) GetFileDiagnostics(uri protocol.DocumentURI) []protocol.Diagnostic {
	diags, _ := c.diagnostics.Get(uri)
	return diags
}

// GetDiagnostics returns all diagnostics for all files.
func (c *Client) GetDiagnostics() map[protocol.DocumentURI][]protocol.Diagnostic {
	if c == nil {
		return nil
	}
	return c.diagnostics.Copy()
}

// GetDiagnosticCounts returns cached diagnostic counts by severity.
// Uses the VersionedMap version to avoid recomputing on every call.
func (c *Client) GetDiagnosticCounts() DiagnosticCounts {
	if c == nil {
		return DiagnosticCounts{}
	}
	currentVersion := c.diagnostics.Version()

	c.diagCountsMu.Lock()
	defer c.diagCountsMu.Unlock()

	if currentVersion == c.diagCountsVersion {
		return c.diagCountsCache
	}

	// Recompute counts.
	counts := DiagnosticCounts{}
	for _, diags := range c.diagnostics.Seq2() {
		for _, diag := range diags {
			switch diag.Severity {
			case protocol.SeverityError:
				counts.Error++
			case protocol.SeverityWarning:
				counts.Warning++
			case protocol.SeverityInformation:
				counts.Information++
			case protocol.SeverityHint:
				counts.Hint++
			}
		}
	}

	c.diagCountsCache = counts
	c.diagCountsVersion = currentVersion
	return counts
}

// OpenFileOnDemand opens a file only if it's not already open.
func (c *Client) OpenFileOnDemand(ctx context.Context, filepath string) error {
	if c == nil {
		return nil
	}
	// Check if the file is already open
	if c.IsFileOpen(filepath) {
		return nil
	}

	// Open the file
	return c.OpenFile(ctx, filepath)
}

// RegisterNotificationHandler registers a notification handler.
func (c *Client) RegisterNotificationHandler(method string, handler transport.NotificationHandler) {
	if client := c.getClient(); client != nil {
		client.RegisterNotificationHandler(method, handler)
	}
}

// RegisterServerRequestHandler handles server requests.
func (c *Client) RegisterServerRequestHandler(method string, handler transport.Handler) {
	if client := c.getClient(); client != nil {
		client.RegisterHandler(method, handler)
	}
}

// openKeyConfigFiles opens important configuration files that help initialize the server.
func (c *Client) openKeyConfigFiles(ctx context.Context) {
	// Try to open each file, ignoring errors if they don't exist
	for _, file := range c.config.RootMarkers {
		file = filepath.Join(c.cwd, file)
		if _, err := os.Stat(file); err == nil {
			// File exists, try to open it
			if err := c.OpenFile(ctx, file); err != nil {
				slog.Error("Failed to open key config file", "file", file, "error", err)
			} else {
				slog.Debug("Opened key config file for initialization", "file", file)
			}
		}
	}
}

// WaitForDiagnostics waits until diagnostics change or the timeout is reached.
func (c *Client) WaitForDiagnostics(ctx context.Context, d time.Duration) {
	if c == nil {
		return
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.After(d)
	pv := c.diagnostics.Version()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timeout:
			return
		case <-ticker.C:
			if pv != c.diagnostics.Version() {
				return
			}
		}
	}
}

// FindReferences finds all references to the symbol at the given position.
func (c *Client) FindReferences(ctx context.Context, filepath string, line, character int, includeDeclaration bool) ([]protocol.Location, error) {
	if err := c.OpenFileOnDemand(ctx, filepath); err != nil {
		return nil, err
	}

	// Add timeout to prevent hanging on slow LSP servers.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	client := c.getClient()
	if client == nil {
		return nil, fmt.Errorf("LSP client is unavailable")
	}

	// NOTE: line and character should be 0-based.
	// See: https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification/#position
	return client.FindReferences(
		ctx,
		filepath,
		line-1,
		character-1,
		includeDeclaration,
	)
}

func (c *Client) Hover(ctx context.Context, filepath string, line, character int) (*protocol.Hover, error) {
	if err := c.OpenFileOnDemand(ctx, filepath); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	client := c.getClient()
	if client == nil {
		return nil, fmt.Errorf("LSP client is unavailable")
	}

	uri := string(protocol.URIFromPath(filepath))
	return client.RequestHover(ctx, uri, protocol.Position{
		Line:      uint32(line - 1),
		Character: uint32(character - 1),
	})
}

func (c *Client) FindDefinition(ctx context.Context, filepath string, line, character int) ([]protocol.Location, error) {
	if err := c.OpenFileOnDemand(ctx, filepath); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	params := protocol.DefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.URIFromPath(filepath)},
			Position: protocol.Position{
				Line:      uint32(line - 1),
				Character: uint32(character - 1),
			},
		},
	}

	client := c.getClient()
	if client == nil {
		return nil, fmt.Errorf("LSP client is unavailable")
	}

	if caps := client.GetCapabilities(); caps.DefinitionProvider == nil {
		return nil, fmt.Errorf("definition requests are not supported by this LSP server")
	}

	var result protocol.Or_Definition
	if err := c.call(ctx, MethodTextDocumentDefinition, params, &result); err != nil {
		return nil, fmt.Errorf("definition request failed: %w", err)
	}
	return locationResults(result.Value), nil
}

// PrepareCallHierarchy returns call hierarchy items for the symbol at the
// given position.
func (c *Client) PrepareCallHierarchy(ctx context.Context, filepath string, line, character int) ([]protocol.CallHierarchyItem, error) {
	if err := c.OpenFileOnDemand(ctx, filepath); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	params := protocol.CallHierarchyPrepareParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.URIFromPath(filepath)},
			Position: protocol.Position{
				Line:      uint32(line - 1),
				Character: uint32(character - 1),
			},
		},
	}

	client := c.getClient()
	if client == nil {
		return nil, fmt.Errorf("LSP client is unavailable")
	}
	if caps := client.GetCapabilities(); caps.CallHierarchyProvider == nil {
		return nil, fmt.Errorf("call hierarchy is not supported by this LSP server")
	}

	var result []protocol.CallHierarchyItem
	if err := c.call(ctx, MethodTextDocumentPrepareCallHierarchy, params, &result); err != nil {
		return nil, fmt.Errorf("prepare call hierarchy failed: %w", err)
	}
	return result, nil
}

// IncomingCalls returns all callers of the given call hierarchy item.
func (c *Client) IncomingCalls(ctx context.Context, item protocol.CallHierarchyItem) ([]protocol.CallHierarchyIncomingCall, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	params := protocol.CallHierarchyIncomingCallsParams{Item: item}
	var result []protocol.CallHierarchyIncomingCall
	if err := c.call(ctx, MethodCallHierarchyIncomingCalls, params, &result); err != nil {
		return nil, fmt.Errorf("incoming calls failed: %w", err)
	}
	return result, nil
}

// OutgoingCalls returns all callees of the given call hierarchy item.
func (c *Client) OutgoingCalls(ctx context.Context, item protocol.CallHierarchyItem) ([]protocol.CallHierarchyOutgoingCall, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	params := protocol.CallHierarchyOutgoingCallsParams{Item: item}
	var result []protocol.CallHierarchyOutgoingCall
	if err := c.call(ctx, MethodCallHierarchyOutgoingCalls, params, &result); err != nil {
		return nil, fmt.Errorf("outgoing calls failed: %w", err)
	}
	return result, nil
}

func (c *Client) FindDeclaration(ctx context.Context, filepath string, line, character int) ([]protocol.Location, error) {
	if err := c.OpenFileOnDemand(ctx, filepath); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	params := protocol.DeclarationParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.URIFromPath(filepath)},
			Position: protocol.Position{
				Line:      uint32(line - 1),
				Character: uint32(character - 1),
			},
		},
	}

	client := c.getClient()
	if client == nil {
		return nil, fmt.Errorf("LSP client is unavailable")
	}

	if caps := client.GetCapabilities(); caps.DeclarationProvider == nil {
		return nil, fmt.Errorf("declaration requests are not supported by this LSP server")
	}

	var result protocol.Or_Declaration
	if err := c.call(ctx, MethodTextDocumentDeclaration, params, &result); err != nil {
		return nil, fmt.Errorf("declaration request failed: %w", err)
	}
	return locationResults(result.Value), nil
}

func (c *Client) FindImplementation(ctx context.Context, filepath string, line, character int) ([]protocol.Location, error) {
	if err := c.OpenFileOnDemand(ctx, filepath); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	params := protocol.ImplementationParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.URIFromPath(filepath)},
			Position: protocol.Position{
				Line:      uint32(line - 1),
				Character: uint32(character - 1),
			},
		},
	}

	client := c.getClient()
	if client == nil {
		return nil, fmt.Errorf("LSP client is unavailable")
	}

	if caps := client.GetCapabilities(); caps.ImplementationProvider == nil {
		return nil, fmt.Errorf("implementation requests are not supported by this LSP server")
	}

	var result protocol.Or_Result_textDocument_implementation
	if err := c.call(ctx, MethodTextDocumentImplementation, params, &result); err != nil {
		return nil, fmt.Errorf("implementation request failed: %w", err)
	}
	return locationResults(result.Value), nil
}

func (c *Client) FindTypeDefinition(ctx context.Context, filepath string, line, character int) ([]protocol.Location, error) {
	if err := c.OpenFileOnDemand(ctx, filepath); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	params := protocol.TypeDefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.URIFromPath(filepath)},
			Position: protocol.Position{
				Line:      uint32(line - 1),
				Character: uint32(character - 1),
			},
		},
	}

	client := c.getClient()
	if client == nil {
		return nil, fmt.Errorf("LSP client is unavailable")
	}

	if caps := client.GetCapabilities(); caps.TypeDefinitionProvider == nil {
		return nil, fmt.Errorf("type definition requests are not supported by this LSP server")
	}

	var result protocol.Or_Result_textDocument_typeDefinition
	if err := c.call(ctx, MethodTextDocumentTypeDefinition, params, &result); err != nil {
		return nil, fmt.Errorf("type definition request failed: %w", err)
	}
	return locationResults(result.Value), nil
}

func (c *Client) DocumentSymbols(ctx context.Context, filepath string) ([]protocol.DocumentSymbolResult, error) {
	if err := c.OpenFileOnDemand(ctx, filepath); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	params := protocol.DocumentSymbolParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: protocol.URIFromPath(filepath)},
	}

	client := c.getClient()
	if client == nil {
		return nil, fmt.Errorf("LSP client is unavailable")
	}

	if caps := client.GetCapabilities(); caps.DocumentSymbolProvider == nil {
		return nil, fmt.Errorf("document symbol requests are not supported by this LSP server")
	}

	var result protocol.Or_Result_textDocument_documentSymbol
	if err := c.call(ctx, MethodTextDocumentDocumentSymbol, params, &result); err != nil {
		return nil, fmt.Errorf("document symbol request failed: %w", err)
	}
	return result.Results()
}

func (c *Client) WorkspaceSymbols(ctx context.Context, query string) ([]protocol.WorkspaceSymbolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	client := c.getClient()
	if client == nil {
		return nil, fmt.Errorf("LSP client is unavailable")
	}

	if caps := client.GetCapabilities(); caps.WorkspaceSymbolProvider == nil {
		return nil, fmt.Errorf("workspace symbol requests are not supported by this LSP server")
	}

	var result protocol.Or_Result_workspace_symbol
	if err := c.call(ctx, MethodWorkspaceSymbol, protocol.WorkspaceSymbolParams{Query: query}, &result); err != nil {
		return nil, fmt.Errorf("workspace symbol request failed: %w", err)
	}
	return result.Results()
}

func (c *Client) CodeActions(ctx context.Context, filepath string, line, character int, only []protocol.CodeActionKind) ([]protocol.CodeAction, error) {
	if err := c.OpenFileOnDemand(ctx, filepath); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	client := c.getClient()
	if client == nil {
		return nil, fmt.Errorf("LSP client is unavailable")
	}

	if !capabilityEnabled(client.GetCapabilities().CodeActionProvider) {
		return nil, fmt.Errorf("code action requests are not supported by this LSP server")
	}

	position := protocol.Position{Line: uint32(line - 1), Character: uint32(character - 1)}
	params := protocol.CodeActionParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: protocol.URIFromPath(filepath)},
		Range:        protocol.Range{Start: position, End: position},
		Context: protocol.CodeActionContext{
			Diagnostics: []protocol.Diagnostic{},
			Only:        only,
		},
	}

	var result []protocol.Or_Result_textDocument_codeAction_Item0_Elem
	if err := c.call(ctx, MethodTextDocumentCodeAction, params, &result); err != nil {
		return nil, fmt.Errorf("code action request failed: %w", err)
	}
	return codeActionResults(result), nil
}

func (c *Client) Rename(ctx context.Context, filepath string, line, character int, newName string) (*protocol.WorkspaceEdit, error) {
	if err := c.OpenFileOnDemand(ctx, filepath); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	client := c.getClient()
	if client == nil {
		return nil, fmt.Errorf("LSP client is unavailable")
	}

	if !capabilityEnabled(client.GetCapabilities().RenameProvider) {
		return nil, fmt.Errorf("rename requests are not supported by this LSP server")
	}

	params := protocol.RenameParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: protocol.URIFromPath(filepath)},
		Position: protocol.Position{
			Line:      uint32(line - 1),
			Character: uint32(character - 1),
		},
		NewName: newName,
	}

	var result *protocol.WorkspaceEdit
	if err := c.call(ctx, MethodTextDocumentRename, params, &result); err != nil {
		return nil, fmt.Errorf("rename request failed: %w", err)
	}
	return result, nil
}

func (c *Client) FormatDocument(ctx context.Context, filepath string, options protocol.FormattingOptions) ([]protocol.TextEdit, error) {
	if err := c.OpenFileOnDemand(ctx, filepath); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	client := c.getClient()
	if client == nil {
		return nil, fmt.Errorf("LSP client is unavailable")
	}

	if !capabilityEnabled(client.GetCapabilities().DocumentFormattingProvider) {
		return nil, fmt.Errorf("document formatting requests are not supported by this LSP server")
	}

	params := protocol.DocumentFormattingParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: protocol.URIFromPath(filepath)},
		Options:      options,
	}

	var result []protocol.TextEdit
	if err := c.call(ctx, MethodTextDocumentFormatting, params, &result); err != nil {
		return nil, fmt.Errorf("document formatting request failed: %w", err)
	}
	return result, nil
}

func (c *Client) ApplyWorkspaceEdit(edit protocol.WorkspaceEdit) error {
	client := c.getClient()
	if c == nil || client == nil {
		return fmt.Errorf("LSP client is unavailable")
	}
	return util.ApplyWorkspaceEdit(edit, client.GetOffsetEncoding())
}

func (c *Client) call(ctx context.Context, method string, params any, result any) error {
	conn := c.connection()
	if conn == nil {
		c.SetServerState(StateError)
		return fmt.Errorf("LSP connection is unavailable")
	}
	err := conn.Call(ctx, method, params, result)
	if err != nil {
		errStr := err.Error()
		if strings.Contains(errStr, "closed") || strings.Contains(errStr, "EOF") || strings.Contains(errStr, "broken pipe") {
			c.SetServerState(StateError)
		}
	}
	return err
}

func (c *Client) connection() *transport.Connection {
	if c == nil || c.client == nil {
		return nil
	}
	// Note: This uses reflect+unsafe to access the private conn field.
	// This is fragile and may break if powernap library changes.
	// A panic recover is added as a safety net.
	defer func() {
		if r := recover(); r != nil {
			slog.Error("Failed to access LSP connection via reflect", "error", r)
		}
	}()
	value := reflect.ValueOf(c.client).Elem().FieldByName("conn")
	if !value.IsValid() || value.IsNil() {
		return nil
	}
	conn, ok := reflect.NewAt(value.Type(), unsafe.Pointer(value.UnsafeAddr())).Elem().Interface().(*transport.Connection)
	if !ok {
		slog.Error("Failed to cast reflected value to transport.Connection")
		return nil
	}
	return conn
}

func locationResults(value any) []protocol.Location {
	switch v := value.(type) {
	case nil:
		return nil
	case protocol.Location:
		return []protocol.Location{v}
	case []protocol.Location:
		return append([]protocol.Location(nil), v...)
	case protocol.LocationLink:
		return []protocol.Location{{
			URI:   v.TargetURI,
			Range: v.TargetSelectionRange,
		}}
	case []protocol.LocationLink:
		locs := make([]protocol.Location, 0, len(v))
		for _, link := range v {
			locs = append(locs, protocol.Location{
				URI:   link.TargetURI,
				Range: link.TargetSelectionRange,
			})
		}
		return locs
	case protocol.Or_Definition:
		return locationResults(v.Value)
	case protocol.Or_Declaration:
		return locationResults(v.Value)
	case protocol.Or_Result_textDocument_implementation:
		return locationResults(v.Value)
	case protocol.Or_Result_textDocument_typeDefinition:
		return locationResults(v.Value)
	default:
		return nil
	}
}

func capabilityEnabled(capability any) bool {
	switch value := capability.(type) {
	case nil:
		return false
	case bool:
		return value
	case protocol.Or_ServerCapabilities_codeActionProvider:
		return capabilityEnabled(value.Value)
	case *protocol.Or_ServerCapabilities_codeActionProvider:
		if value == nil {
			return false
		}
		return capabilityEnabled(value.Value)
	case protocol.Or_ServerCapabilities_documentFormattingProvider:
		return capabilityEnabled(value.Value)
	case *protocol.Or_ServerCapabilities_documentFormattingProvider:
		if value == nil {
			return false
		}
		return capabilityEnabled(value.Value)
	case protocol.Or_ServerCapabilities_renameProvider:
		return capabilityEnabled(value.Value)
	case *protocol.Or_ServerCapabilities_renameProvider:
		if value == nil {
			return false
		}
		return capabilityEnabled(value.Value)
	default:
		return true
	}
}

func codeActionResults(results []protocol.Or_Result_textDocument_codeAction_Item0_Elem) []protocol.CodeAction {
	actions := make([]protocol.CodeAction, 0, len(results))
	for _, result := range results {
		switch value := result.Value.(type) {
		case protocol.CodeAction:
			actions = append(actions, value)
		case *protocol.CodeAction:
			if value != nil {
				actions = append(actions, *value)
			}
		case protocol.Command:
			command := value
			actions = append(actions, protocol.CodeAction{Title: command.Title, Command: &command})
		case *protocol.Command:
			if value != nil {
				command := *value
				actions = append(actions, protocol.CodeAction{Title: command.Title, Command: &command})
			}
		}
	}
	return actions
}
