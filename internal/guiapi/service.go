// Package guiapi owns the negotiated crush/* desktop protocol extensions.
package guiapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/crush/internal/acp"
	"github.com/charmbracelet/crush/internal/blob"
	"github.com/charmbracelet/crush/internal/clientfs"
	"github.com/charmbracelet/crush/internal/guimetrics"
	"github.com/charmbracelet/crush/internal/idempotency"
	"github.com/charmbracelet/crush/internal/mcplifecycle"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/providerauth"
	"github.com/charmbracelet/crush/internal/sessionevent"
	"github.com/charmbracelet/crush/internal/terminal"
	"github.com/charmbracelet/crush/internal/turn"
	"github.com/google/uuid"
)

const ProtocolVersion = 1

// Feature is a negotiated group of private methods.
type Feature string

const (
	FeatureSessionSync    Feature = "sessionSync"
	FeatureSessionControl Feature = "sessionControl"
	FeatureTerminal       Feature = "terminal"
	FeatureBlob           Feature = "blob"
	FeatureClientFS       Feature = "clientFS"
	FeatureProviderAuth   Feature = "providerAuth"
	FeatureMCPControl     Feature = "mcpControl"
)

const (
	codeFeatureNotNegotiated = -32010
	codeUnsupportedProtocol  = -32011
	codeUnsupportedFeature   = -32012
)

const (
	errorFeatureNotNegotiated = "CRUSH_FEATURE_NOT_NEGOTIATED"
	errorUnsupportedProtocol  = "CRUSH_UNSUPPORTED_PROTOCOL_VERSION"
	errorUnsupportedFeature   = "CRUSH_UNSUPPORTED_FEATURE"
	errorSequenceExpired      = "CRUSH_SEQUENCE_EXPIRED"
)

var supportedFeatures = []Feature{
	FeatureSessionSync,
	FeatureSessionControl,
	FeatureTerminal,
	FeatureBlob,
	FeatureClientFS,
	FeatureProviderAuth,
	FeatureMCPControl,
}

// Capability is advertised in initialize.experimental.crush.
type Capability struct {
	ProtocolVersion int       `json:"protocolVersion"`
	Features        []Feature `json:"features"`
}

// Selection is echoed by a client to select a version and feature subset.
type Selection struct {
	ProtocolVersion int       `json:"protocolVersion"`
	Features        []Feature `json:"features"`
}

// ErrorData is the structured data attached to private-protocol JSON-RPC
// errors.
type ErrorData struct {
	Code      string         `json:"code"`
	Retryable bool           `json:"retryable"`
	Details   map[string]any `json:"details,omitempty"`
}

// Handler implements one negotiated private method.
type Handler func(context.Context, json.RawMessage) (any, *acp.RPCError)

type route struct {
	feature Feature
	handler Handler
}

// Service is connection-scoped. Negotiation state must never be shared between
// independent ACP clients, while application services such as the event Hub may
// be process/workspace scoped.
type Service struct {
	mu sync.RWMutex

	events           *sessionevent.Hub
	snapshots        SnapshotSource
	sessions         SessionReader
	sessionMutations SessionMutationService
	sessionRuntime   SessionRuntimeCloser
	inference        InferenceResolver
	messagePages     MessagePageReader
	messageSearch    MessageSearchReader
	turns            *turn.Service
	idempotency      *idempotency.Store
	blobs            *blob.Service
	blobOwner        string
	ownsBlobService  bool
	blobIdempotency  *idempotency.Store
	terminals        *terminal.Manager
	permissions      permission.Service
	terminalReplay   *idempotency.Store
	clientFSCaller   clientfs.Caller
	clientFS         map[string]clientFSScope
	providerAuth     *providerauth.Manager
	providerReplay   *idempotency.Store
	mcpLifecycle     *mcplifecycle.Service
	mcpReplay        *idempotency.Store
	writer           NotificationWriter
	routes           map[string]route
	subscriptions    map[string]*managedSubscription
	negotiated       bool
	version          int
	features         map[Feature]struct{}
	closed           bool
	negotiationEpoch uint64
}

// NewService creates a private protocol router for one ACP connection.
func NewService(events *sessionevent.Hub) *Service {
	service := &Service{
		events:          events,
		routes:          make(map[string]route),
		features:        make(map[Feature]struct{}),
		subscriptions:   make(map[string]*managedSubscription),
		blobs:           blob.New(blob.Config{}),
		blobOwner:       uuid.NewString(),
		ownsBlobService: true,
		blobIdempotency: idempotency.New(idempotency.Config{}),
		terminalReplay:  idempotency.New(idempotency.Config{}),
		providerReplay:  idempotency.New(idempotency.Config{}),
		mcpReplay:       idempotency.New(idempotency.Config{}),
		clientFS:        make(map[string]clientFSScope),
	}
	service.registerProtocolSurface()
	service.routes["crush/protocol/status"] = route{handler: service.handleStatus}
	service.registerSessionSyncHandlers()
	service.registerSnapshotHandler()
	service.registerMessageHandlers()
	service.registerTurnHandlers()
	service.registerSessionMutationHandlers()
	service.registerBlobHandlers()
	service.registerTerminalHandlers()
	service.registerProviderHandlers()
	service.registerMCPHandlers()
	return service
}

// SetProviderAuthService attaches the App-owned provider authentication
// manager. Login ownership remains scoped to this connection's opaque owner.
func (s *Service) SetProviderAuthService(manager *providerauth.Manager) {
	s.mu.Lock()
	s.providerAuth = manager
	s.mu.Unlock()
}

// SetMCPLifecycleService attaches the App-owned root-session MCP service.
func (s *Service) SetMCPLifecycleService(service *mcplifecycle.Service) {
	s.mu.Lock()
	s.mcpLifecycle = service
	s.mu.Unlock()
}

// SetTurnServices attaches the process-owned turn runtime and idempotency
// replay store.
func (s *Service) SetTurnServices(turns *turn.Service, store *idempotency.Store) {
	s.mu.Lock()
	s.turns = turns
	s.idempotency = store
	s.mu.Unlock()
}

// NotificationWriter serializes reliable GUI notifications to the client.
type NotificationWriter interface {
	NotifySync(context.Context, string, any) error
}

// SetNotificationWriter attaches the connection transport. It must be called
// before a client invokes session/subscribe.
func (s *Service) SetNotificationWriter(writer NotificationWriter) {
	s.mu.Lock()
	s.writer = writer
	s.mu.Unlock()
}

// SetSnapshotSource attaches the bounded projection service.
func (s *Service) SetSnapshotSource(source SnapshotSource) {
	s.mu.Lock()
	s.snapshots = source
	s.mu.Unlock()
}

// ExperimentalCapabilities implements acp.ExperimentalExtension.
func (s *Service) ExperimentalCapabilities() map[string]any {
	features := append([]Feature(nil), supportedFeatures...)
	return map[string]any{
		"crush": Capability{ProtocolVersion: ProtocolVersion, Features: features},
	}
}

// NegotiateExperimental implements acp.ExperimentalExtension. Omitting the
// crush key resets the connection to standard ACP-only mode.
func (s *Service) NegotiateExperimental(experimental map[string]json.RawMessage) *acp.RPCError {
	var selection Selection
	var selectionPresent bool
	var selectionErr *acp.RPCError
	raw, ok := experimental["crush"]
	if ok && len(raw) != 0 && string(raw) != "null" {
		selectionPresent = true
		if err := json.Unmarshal(raw, &selection); err != nil {
			selectionErr = &acp.RPCError{Code: acp.CodeInvalidParams, Message: "invalid crush experimental capability: " + err.Error()}
		} else if selection.ProtocolVersion != ProtocolVersion {
			selectionErr = protocolError(codeUnsupportedProtocol, errorUnsupportedProtocol, map[string]any{
				"requestedVersion": selection.ProtocolVersion,
				"supportedVersion": ProtocolVersion,
			})
		} else {
			for _, feature := range selection.Features {
				if !slices.Contains(supportedFeatures, feature) {
					selectionErr = protocolError(codeUnsupportedFeature, errorUnsupportedFeature, map[string]any{"feature": feature})
					break
				}
			}
		}
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return &acp.RPCError{Code: acp.CodeInternalError, Message: "crush extension service is closed"}
	}
	s.negotiationEpoch++
	epoch := s.negotiationEpoch
	// Every initialize is fail-closed. A malformed renegotiation must not leave
	// permissions selected by an earlier initialize active.
	s.negotiated = false
	s.version = 0
	clear(s.features)
	for sessionID, entry := range s.clientFS {
		entry.scope.Close()
		delete(s.clientFS, sessionID)
	}
	providerAuth := s.providerAuth
	providerOwner := s.blobOwner
	oldProviderReplay := s.providerReplay
	s.providerReplay = idempotency.New(idempotency.Config{})
	oldMCPReplay := s.mcpReplay
	s.mcpReplay = idempotency.New(idempotency.Config{})
	s.mu.Unlock()

	if providerAuth != nil {
		providerAuth.CloseOwner(providerOwner)
	}
	if oldProviderReplay != nil {
		oldProviderReplay.Close()
	}
	if oldMCPReplay != nil {
		oldMCPReplay.Close()
	}
	if selectionErr != nil {
		return selectionErr
	}
	if !selectionPresent {
		return nil
	}

	selected := make(map[Feature]struct{}, len(selection.Features))
	for _, feature := range selection.Features {
		selected[feature] = struct{}{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return &acp.RPCError{Code: acp.CodeInternalError, Message: "crush extension service is closed"}
	}
	if s.negotiationEpoch != epoch {
		return nil
	}
	s.negotiated = true
	s.version = selection.ProtocolVersion
	s.features = selected
	return nil
}

// HandleExtension implements acp.ExtensionRouter.
func (s *Service) HandleExtension(ctx context.Context, method string, params json.RawMessage) (result any, rpcErr *acp.RPCError) {
	started := time.Now()
	s.mu.RLock()
	route, known := s.routes[method]
	negotiated := s.negotiated
	_, featureSelected := s.features[route.feature]
	closed := s.closed
	s.mu.RUnlock()
	metricMethod := "crush/other"
	if known {
		if route.feature == "" {
			metricMethod = "crush/protocol"
		} else {
			metricMethod = "crush/" + string(route.feature)
		}
	}
	defer func() {
		outcome := "success"
		if rpcErr != nil {
			outcome = "error"
		}
		guimetrics.FromContext(ctx).ObserveDuration(guimetrics.ACPRequestDuration, time.Since(started), guimetrics.Labels{
			Method:    metricMethod,
			Outcome:   outcome,
			Transport: acp.TransportName(ctx),
		})
	}()

	if closed {
		return nil, &acp.RPCError{Code: acp.CodeInternalError, Message: "crush extension service is closed"}
	}
	if !known {
		return nil, &acp.RPCError{Code: acp.CodeMethodNotFound, Message: fmt.Sprintf("method not found: %s", method)}
	}
	if !negotiated {
		return nil, featureNotNegotiated(route.feature, "initialize did not select the crush extension")
	}
	if route.feature != "" && !featureSelected {
		return nil, featureNotNegotiated(route.feature, "feature was not selected during initialize")
	}
	if route.handler == nil {
		return nil, &acp.RPCError{Code: acp.CodeMethodNotFound, Message: fmt.Sprintf("method not implemented: %s", method)}
	}
	return route.handler(ctx, params)
}

// Register attaches a handler to a method declared by the protocol surface.
// Later work packages use this without changing ACP dispatch.
func (s *Service) Register(method string, feature Feature, handler Handler) error {
	if !strings.HasPrefix(method, "crush/") {
		return errors.New("guiapi: method must use the crush/ namespace")
	}
	if feature != "" && !slices.Contains(supportedFeatures, feature) {
		return fmt.Errorf("guiapi: unsupported feature %q", feature)
	}
	if handler == nil {
		return errors.New("guiapi: handler is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("guiapi: service is closed")
	}
	existing, ok := s.routes[method]
	if ok && existing.handler != nil {
		return fmt.Errorf("guiapi: method already registered: %s", method)
	}
	if ok && existing.feature != feature {
		return fmt.Errorf("guiapi: method %s requires feature %s", method, existing.feature)
	}
	s.routes[method] = route{feature: feature, handler: handler}
	return nil
}

// Close clears connection negotiation and rejects later calls.
func (s *Service) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.negotiated = false
	clear(s.features)
	subscriptions := make([]*managedSubscription, 0, len(s.subscriptions))
	for _, subscription := range s.subscriptions {
		subscriptions = append(subscriptions, subscription)
	}
	clear(s.subscriptions)
	blobs := s.blobs
	blobOwner := s.blobOwner
	ownsBlobService := s.ownsBlobService
	blobIdempotency := s.blobIdempotency
	terminals := s.terminals
	terminalReplay := s.terminalReplay
	providerAuth := s.providerAuth
	providerReplay := s.providerReplay
	mcpReplay := s.mcpReplay
	clientFSScopes := make([]*clientfs.Scope, 0, len(s.clientFS))
	for _, entry := range s.clientFS {
		clientFSScopes = append(clientFSScopes, entry.scope)
	}
	clear(s.clientFS)
	s.mu.Unlock()
	for _, subscription := range subscriptions {
		subscription.stop()
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, subscription := range subscriptions {
		subscription.wait(ctx)
	}
	if providerReplay != nil {
		providerReplay.Close()
	}
	if mcpReplay != nil {
		mcpReplay.Close()
	}
	if providerAuth != nil {
		providerAuth.CloseOwner(blobOwner)
	}
	if blobs != nil {
		if ownsBlobService {
			blobs.Close()
		} else {
			blobs.ReleaseClient(blobOwner)
		}
	}
	if blobIdempotency != nil {
		blobIdempotency.Close()
	}
	if terminals != nil {
		terminals.CloseClient(blobOwner)
	}
	if terminalReplay != nil {
		terminalReplay.Close()
	}
	for _, scope := range clientFSScopes {
		scope.Close()
	}
}

type statusResult struct {
	ProtocolVersion int       `json:"protocolVersion"`
	Features        []Feature `json:"features"`
}

func (s *Service) handleStatus(context.Context, json.RawMessage) (any, *acp.RPCError) {
	s.mu.RLock()
	features := make([]Feature, 0, len(s.features))
	for feature := range s.features {
		features = append(features, feature)
	}
	version := s.version
	s.mu.RUnlock()
	slices.Sort(features)
	return statusResult{ProtocolVersion: version, Features: features}, nil
}

func (s *Service) registerProtocolSurface() {
	for _, method := range []string{
		"crush/session/get", "crush/session/subscribe", "crush/session/unsubscribe",
		"crush/session/snapshot", "crush/session/sync", "crush/session/messages",
		"crush/session/config/get",
	} {
		s.routes[method] = route{feature: FeatureSessionSync}
	}
	for _, method := range []string{
		"crush/session/rename", "crush/session/archive", "crush/session/delete",
		"crush/session/fork", "crush/session/pin", "crush/session/search",
		"crush/session/config/update",
		"crush/turn/start", "crush/turn/wait", "crush/turn/cancel",
		"crush/session/queue/list", "crush/session/queue/remove",
		"crush/session/queue/reorder", "crush/session/steer", "crush/session/retry",
	} {
		s.routes[method] = route{feature: FeatureSessionControl}
	}
	for _, method := range []string{
		"crush/terminal/open", "crush/terminal/input", "crush/terminal/resize",
		"crush/terminal/kill", "crush/terminal/snapshot",
	} {
		s.routes[method] = route{feature: FeatureTerminal}
	}
	for _, method := range []string{"crush/blob/create", "crush/blob/read", "crush/blob/release"} {
		s.routes[method] = route{feature: FeatureBlob}
	}
	for _, method := range []string{"crush/fs/read", "crush/fs/write", "crush/fs/stat"} {
		s.routes[method] = route{feature: FeatureClientFS}
	}
	for _, method := range []string{
		"crush/provider/list", "crush/provider/models", "crush/provider/auth_status",
		"crush/provider/login", "crush/provider/login_cancel", "crush/provider/logout",
	} {
		s.routes[method] = route{feature: FeatureProviderAuth}
	}
	for _, method := range []string{
		"crush/mcp/list", "crush/mcp/status", "crush/mcp/reconnect",
		"crush/mcp/disable", "crush/mcp/logs",
	} {
		s.routes[method] = route{feature: FeatureMCPControl}
	}
}

func featureNotNegotiated(feature Feature, reason string) *acp.RPCError {
	details := map[string]any{"reason": reason}
	if feature != "" {
		details["feature"] = feature
	}
	return protocolError(codeFeatureNotNegotiated, errorFeatureNotNegotiated, details)
}

func protocolError(code int, name string, details map[string]any) *acp.RPCError {
	return &acp.RPCError{
		Code:    code,
		Message: name,
		Data: ErrorData{
			Code:      name,
			Retryable: false,
			Details:   details,
		},
	}
}
