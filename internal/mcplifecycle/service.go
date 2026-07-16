package mcplifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/redact"
	"github.com/charmbracelet/crush/internal/sessionevent"
	"github.com/google/uuid"
)

const (
	defaultMaxSessions          = 1024
	defaultMaxServersPerSession = 64
	defaultMaxLogEntries        = 4096
	defaultMaxLogBytes          = 1 << 20
	maxServerNameBytes          = 128
	maxLogFieldBytes            = 4096
)

type Config struct {
	MaxSessions          int
	MaxServersPerSession int
	MaxLogEntries        int
	MaxLogBytes          int
	Clock                func() time.Time
}

type Service struct {
	mu      sync.Mutex
	store   *config.ConfigStore
	backend Backend
	events  *sessionevent.Hub
	config  Config
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	instanceID    string
	generation    uint64
	sessions      map[string]*sessionState
	dynamic       map[string]*serverEntry
	tombstones    map[string]struct{}
	staticPending map[string]Status
	staticState   map[string]Server
	staticEpoch   map[string]uint64
	staticCancel  map[string]context.CancelFunc
	staticTail    map[string]chan struct{}
	logs          map[string][]LogEntry
	logSequence   map[string]uint64
	logOrder      []logReference
	logBytes      int
	closed        bool
}

type sessionState struct {
	owner    string
	epoch    uint64
	revision uint64
	servers  map[string]*serverEntry
}

type serverEntry struct {
	sessionID   string
	epoch       uint64
	id          string
	name        string
	internal    string
	config      config.MCPConfig
	status      Status
	counts      Counts
	errorCode   string
	updatedAt   time.Time
	authorized  bool
	retired     bool
	cleanupOnce sync.Once
	cleanupDone chan struct{}
	cancel      context.CancelFunc
	ctx         context.Context
}

type logReference struct {
	key      string
	sequence uint64
	bytes    int
}

func New(store *config.ConfigStore, backend Backend, events *sessionevent.Hub, cfg Config) *Service {
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = defaultMaxSessions
	}
	if cfg.MaxServersPerSession <= 0 {
		cfg.MaxServersPerSession = defaultMaxServersPerSession
	}
	if cfg.MaxLogEntries <= 0 {
		cfg.MaxLogEntries = defaultMaxLogEntries
	}
	if cfg.MaxLogBytes <= 0 {
		cfg.MaxLogBytes = defaultMaxLogBytes
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	service := &Service{
		store: store, backend: backend, events: events, config: cfg,
		ctx: ctx, cancel: cancel, instanceID: uuid.NewString(),
		sessions: make(map[string]*sessionState), dynamic: make(map[string]*serverEntry),
		tombstones: make(map[string]struct{}), staticPending: make(map[string]Status),
		staticState: make(map[string]Server),
		staticEpoch: make(map[string]uint64), staticCancel: make(map[string]context.CancelFunc),
		staticTail: make(map[string]chan struct{}),
		logs:       make(map[string][]LogEntry), logSequence: make(map[string]uint64),
	}
	if backend != nil {
		service.wg.Add(1)
		go service.watchBackend()
	}
	return service
}

// ReplaceAsync immediately revokes the previous session generation and starts
// transport replacement in the background.
func (s *Service) ReplaceAsync(owner, sessionID string, configs []ServerConfig) error {
	if sessionID == "" {
		return ErrNotFound
	}
	validated := s.validateConfigs(configs)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	state := s.sessions[sessionID]
	if state == nil {
		if len(s.sessions) >= s.config.MaxSessions {
			s.mu.Unlock()
			return ErrCapacity
		}
		state = &sessionState{servers: make(map[string]*serverEntry)}
		s.sessions[sessionID] = state
	}
	state.owner = owner
	state.epoch++
	state.revision++
	epoch := state.epoch
	old := make([]*serverEntry, 0, len(state.servers))
	for _, entry := range state.servers {
		entry.cancel()
		entry.authorized = false
		entry.retired = true
		entry.status = StatusStopping
		entry.updatedAt = s.config.Clock()
		old = append(old, entry)
	}
	state.servers = make(map[string]*serverEntry, len(validated))
	created := make([]*serverEntry, 0, len(validated))
	for _, server := range validated {
		internal := s.nextInternalNameLocked(sessionID, server.Name)
		entryCtx, entryCancel := context.WithCancel(s.ctx)
		entry := &serverEntry{
			sessionID: sessionID, epoch: epoch, id: dynamicID(server.Name),
			name: server.Name, internal: internal, config: cloneMCPConfig(server.Config),
			status: StatusStarting, updatedAt: s.config.Clock(), cleanupDone: make(chan struct{}),
			ctx: entryCtx, cancel: entryCancel,
		}
		state.servers[entry.id] = entry
		s.dynamic[internal] = entry
		s.tombstones[internal] = struct{}{}
		created = append(created, entry)
	}
	revision := state.revision
	s.wg.Add(1)
	s.mu.Unlock()

	for _, entry := range old {
		s.publish(entry.sessionID, entry, revision)
	}
	for _, entry := range created {
		s.backend.MarkScoped(entry.internal)
		s.publish(entry.sessionID, entry, revision)
	}
	go s.runReplacement(old, created, false)
	return nil
}

func (s *Service) Access(sessionID string) *Access {
	return &Access{service: s, sessionID: sessionID}
}

func (s *Service) List(sessionID string) ([]Server, uint64, error) {
	s.mu.Lock()
	state, err := s.ensureSessionLocked(sessionID)
	if err != nil {
		s.mu.Unlock()
		return nil, 0, err
	}
	result := make([]Server, 0, len(state.servers))
	for _, entry := range state.servers {
		result = append(result, serverProjection(entry, state.revision))
	}
	revision := state.revision
	staticPending := maps.Clone(s.staticPending)
	staticState := maps.Clone(s.staticState)
	tombstones := maps.Clone(s.tombstones)
	s.mu.Unlock()

	if s.store != nil {
		for name, serverCfg := range s.store.MCPSnapshot() {
			if _, dynamic := tombstones[name]; dynamic {
				continue
			}
			status := StatusStarting
			counts := Counts{}
			if serverCfg.Disabled {
				status = StatusDisabled
			} else if info, ok := s.backend.State(name); ok {
				status, _ = publicState(info.State)
				counts = info.Counts
			}
			if pending, ok := staticPending[name]; ok {
				status = pending
			}
			errorCode := ""
			if current, ok := staticState[name]; ok {
				status, counts, errorCode = current.Status, current.Counts, current.ErrorCode
				if pending, pendingOK := staticPending[name]; pendingOK {
					status = pending
				}
			}
			result = append(result, Server{
				ID: staticID(name), Name: name, Scope: ScopeStatic, Status: status,
				Counts: counts, Revision: revision, ErrorCode: errorCode, UpdatedAt: s.config.Clock(),
			})
		}
	}
	slices.SortFunc(result, compareServers)
	return result, revision, nil
}

func (s *Service) Status(sessionID, serverID string) (Server, error) {
	servers, _, err := s.List(sessionID)
	if err != nil {
		return Server{}, err
	}
	for _, server := range servers {
		if server.ID == serverID {
			return server, nil
		}
	}
	return Server{}, ErrNotFound
}

func (s *Service) ReconnectAsync(sessionID, serverID string) (Server, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Server{}, ErrClosed
	}
	state := s.sessions[sessionID]
	if state == nil {
		s.mu.Unlock()
		return Server{}, ErrNotFound
	}
	if current := state.servers[serverID]; current != nil {
		current.cancel()
		state.revision++
		current.authorized = false
		current.status = StatusStopping
		current.updatedAt = s.config.Clock()
		old := current
		old.retired = true
		entryCtx, entryCancel := context.WithCancel(s.ctx)
		entry := &serverEntry{
			sessionID: sessionID, epoch: old.epoch, id: old.id, name: old.name,
			internal: s.nextInternalNameLocked(sessionID, old.name),
			config:   cloneMCPConfig(old.config), status: StatusReconnecting,
			updatedAt: s.config.Clock(), cleanupDone: make(chan struct{}),
			ctx: entryCtx, cancel: entryCancel,
		}
		state.servers[serverID] = entry
		s.dynamic[entry.internal] = entry
		s.tombstones[entry.internal] = struct{}{}
		revision := state.revision
		result := serverProjection(entry, revision)
		s.wg.Add(1)
		s.mu.Unlock()
		s.publish(sessionID, old, revision)
		s.backend.MarkScoped(entry.internal)
		s.publish(sessionID, entry, revision)
		go s.runReplacement([]*serverEntry{old}, []*serverEntry{entry}, true)
		return result, nil
	}
	name, ok := s.staticNameLocked(serverID)
	if !ok {
		s.mu.Unlock()
		return Server{}, ErrNotFound
	}
	state.revision++
	revision := state.revision
	if cancel := s.staticCancel[name]; cancel != nil {
		cancel()
	}
	operationCtx, operationCancel := context.WithCancel(s.ctx)
	s.staticCancel[name] = operationCancel
	s.staticEpoch[name]++
	epoch := s.staticEpoch[name]
	previous, done := s.chainStaticOperationLocked(name)
	s.staticPending[name] = StatusReconnecting
	s.wg.Add(1)
	s.mu.Unlock()
	s.publishStatic(name, StatusReconnecting, Counts{}, "")
	go s.runStaticReconnect(operationCtx, name, epoch, previous, done)
	return Server{ID: serverID, Name: name, Scope: ScopeStatic, Status: StatusReconnecting, Revision: revision, UpdatedAt: s.config.Clock()}, nil
}

func (s *Service) DisableAsync(sessionID, serverID string) (Server, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Server{}, ErrClosed
	}
	state := s.sessions[sessionID]
	if state == nil {
		s.mu.Unlock()
		return Server{}, ErrNotFound
	}
	if entry := state.servers[serverID]; entry != nil {
		entry.cancel()
		state.revision++
		entry.authorized = false
		entry.retired = true
		entry.status = StatusStopping
		entry.updatedAt = s.config.Clock()
		revision := state.revision
		result := serverProjection(entry, revision)
		delete(s.dynamic, entry.internal)
		s.wg.Add(1)
		s.mu.Unlock()
		s.publish(sessionID, entry, revision)
		go s.runDynamicDisable(entry)
		return result, nil
	}
	name, ok := s.staticNameLocked(serverID)
	if !ok {
		s.mu.Unlock()
		return Server{}, ErrNotFound
	}
	state.revision++
	revision := state.revision
	if cancel := s.staticCancel[name]; cancel != nil {
		cancel()
	}
	operationCtx, operationCancel := context.WithCancel(s.ctx)
	s.staticCancel[name] = operationCancel
	s.staticEpoch[name]++
	epoch := s.staticEpoch[name]
	previous, done := s.chainStaticOperationLocked(name)
	s.staticPending[name] = StatusStopping
	s.wg.Add(1)
	s.mu.Unlock()
	s.publishStatic(name, StatusStopping, Counts{}, "")
	go s.runStaticDisable(operationCtx, name, epoch, previous, done)
	return Server{ID: serverID, Name: name, Scope: ScopeStatic, Status: StatusStopping, Revision: revision, UpdatedAt: s.config.Clock()}, nil
}

func (s *Service) Logs(sessionID, serverID string, after uint64, limit int) (LogPage, error) {
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, 1000)
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.sessions[sessionID]
	if state == nil {
		return LogPage{}, ErrNotFound
	}
	key := ""
	if entry := state.servers[serverID]; entry != nil {
		key = entry.internal
	} else if name, ok := s.staticNameLocked(serverID); ok {
		key = staticLogKey(name)
	} else {
		return LogPage{}, ErrNotFound
	}
	entries := s.logs[key]
	page := LogPage{LatestSequence: s.logSequence[key]}
	for _, entry := range entries {
		if entry.Sequence <= after {
			continue
		}
		if len(page.Entries) >= limit {
			page.Truncated = true
			break
		}
		page.Entries = append(page.Entries, entry)
	}
	if len(entries) > 0 && after+1 < entries[0].Sequence {
		page.Truncated = true
	}
	return page, nil
}

func (s *Service) SnapshotResources(sessionID string) []sessionevent.ResourceSummary {
	servers, _, err := s.List(sessionID)
	if err != nil {
		return nil
	}
	result := make([]sessionevent.ResourceSummary, 0, len(servers))
	for _, server := range servers {
		result = append(result, sessionevent.ResourceSummary{ID: server.ID, Status: string(server.Status)})
	}
	return result
}

func (s *Service) CloseSession(ctx context.Context, sessionID string) {
	s.mu.Lock()
	state := s.sessions[sessionID]
	if state == nil {
		s.mu.Unlock()
		return
	}
	state.revision++
	revision := state.revision
	entries := make([]*serverEntry, 0, len(state.servers))
	for _, entry := range state.servers {
		entry.cancel()
		entry.authorized = false
		entry.retired = true
		entry.status = StatusStopping
		entry.updatedAt = s.config.Clock()
		entries = append(entries, entry)
	}
	delete(s.sessions, sessionID)
	s.mu.Unlock()
	for _, entry := range entries {
		s.publish(sessionID, entry, revision)
		s.cleanupEntry(ctx, entry)
	}
}

func (s *Service) CloseOwner(ctx context.Context, owner string) {
	s.mu.Lock()
	type closingSession struct {
		id       string
		revision uint64
		entries  []*serverEntry
	}
	closing := make([]closingSession, 0)
	for id, state := range s.sessions {
		if state.owner != owner {
			continue
		}
		state.revision++
		item := closingSession{id: id, revision: state.revision, entries: make([]*serverEntry, 0, len(state.servers))}
		for _, entry := range state.servers {
			entry.cancel()
			entry.authorized = false
			entry.retired = true
			entry.status = StatusStopping
			entry.updatedAt = s.config.Clock()
			item.entries = append(item.entries, entry)
		}
		delete(s.sessions, id)
		closing = append(closing, item)
	}
	s.mu.Unlock()
	for _, session := range closing {
		for _, entry := range session.entries {
			s.publish(session.id, entry, session.revision)
			s.cleanupEntry(ctx, entry)
		}
	}
}

func (s *Service) Close(ctx context.Context) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.cancel()
	ids := make([]string, 0, len(s.sessions))
	for id := range s.sessions {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.CloseSession(ctx, id)
	}
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func (s *Service) runReplacement(old, created []*serverEntry, manual bool) {
	defer s.wg.Done()
	var cleanup sync.WaitGroup
	for _, entry := range old {
		cleanup.Go(func() { s.cleanupEntry(s.ctx, entry) })
	}
	cleanup.Wait()
	var connect sync.WaitGroup
	for _, entry := range created {
		connect.Go(func() { s.connectEntry(entry, manual) })
	}
	connect.Wait()
}

func (s *Service) connectEntry(entry *serverEntry, manual bool) {
	ctx := entry.ctx
	operationEpoch := entry.epoch
	if ctx.Err() != nil {
		s.cleanupEntry(context.Background(), entry)
		return
	}
	s.store.AddMCP(entry.internal, cloneMCPConfig(entry.config))
	var err error
	if manual {
		err = s.backend.Reconnect(ctx, s.store, entry.internal)
	} else {
		err = s.backend.Connect(ctx, s.store, entry.internal)
	}
	if ctx.Err() != nil || !s.isCurrent(entry, operationEpoch) {
		s.cleanupEntry(context.Background(), entry)
		return
	}
	if err != nil {
		s.store.RemoveMCP(entry.internal)
		s.updateDynamic(entry, StatusFailed, Counts{}, "CRUSH_MCP_CONNECTION_FAILED", false)
		s.appendLifecycleLog(entry.internal, "error", "Connection failed", entry.config)
		return
	}
	info, ok := s.backend.State(entry.internal)
	status := StatusConnected
	counts := Counts{}
	if ok {
		status, _ = publicState(info.State)
		counts = info.Counts
	}
	s.updateDynamic(entry, status, counts, "", status == StatusConnected || status == StatusDegraded)
}

func (s *Service) runDynamicDisable(entry *serverEntry) {
	defer s.wg.Done()
	s.cleanupEntry(s.ctx, entry)
	s.finishDynamicDisabled(entry)
}

func (s *Service) runStaticReconnect(
	ctx context.Context,
	name string,
	epoch uint64,
	previous <-chan struct{},
	done chan<- struct{},
) {
	defer s.wg.Done()
	defer close(done)
	if !waitStaticOperation(ctx, previous) || !s.isStaticOperation(name, epoch) {
		return
	}
	if s.store != nil {
		if err := s.store.SetMCPDisabled(config.ScopeWorkspace, name, false); err != nil {
			s.finishStaticOperation(name, epoch, StatusFailed, Counts{}, "CRUSH_MCP_RECONNECT_FAILED")
			return
		}
	}
	err := s.backend.Reconnect(ctx, s.store, name)
	if err != nil {
		s.finishStaticOperation(name, epoch, StatusFailed, Counts{}, "CRUSH_MCP_CONNECTION_FAILED")
		return
	}
	info, ok := s.backend.State(name)
	status, counts := StatusConnected, Counts{}
	if ok {
		status, _ = publicState(info.State)
		counts = info.Counts
	}
	s.finishStaticOperation(name, epoch, status, counts, "")
}

func (s *Service) runStaticDisable(
	ctx context.Context,
	name string,
	epoch uint64,
	previous <-chan struct{},
	done chan<- struct{},
) {
	defer s.wg.Done()
	defer close(done)
	if !waitStaticOperation(ctx, previous) || !s.isStaticOperation(name, epoch) {
		return
	}
	err := s.backend.Disable(ctx, s.store, name)
	if err == nil && s.store != nil && s.isStaticOperation(name, epoch) {
		err = s.store.SetMCPDisabled(config.ScopeWorkspace, name, true)
	}
	if err != nil {
		s.finishStaticOperation(name, epoch, StatusFailed, Counts{}, "CRUSH_MCP_DISABLE_FAILED")
		return
	}
	s.finishStaticOperation(name, epoch, StatusDisabled, Counts{}, "")
}

func (s *Service) cleanupEntry(ctx context.Context, entry *serverEntry) {
	entry.cleanupOnce.Do(func() {
		defer close(entry.cleanupDone)
		_ = s.backend.Disable(ctx, s.store, entry.internal)
		if s.store != nil {
			s.store.RemoveMCP(entry.internal)
		}
		s.mu.Lock()
		delete(s.dynamic, entry.internal)
		s.removeLogsLocked(entry.internal)
		s.mu.Unlock()
	})
	<-entry.cleanupDone
}

func (s *Service) watchBackend() {
	defer s.wg.Done()
	for event := range s.backend.Subscribe(s.ctx) {
		if event.Type == BackendEventLog {
			s.handleBackendLog(event)
			continue
		}
		s.handleBackendState(event)
	}
}

func (s *Service) handleBackendState(event BackendEvent) {
	status, errorCode := publicState(event.State)
	s.mu.Lock()
	entry := s.dynamic[event.Name]
	_, tombstone := s.tombstones[event.Name]
	if event.State == BackendStarting && entry != nil &&
		(entry.status == StatusFailed || entry.status == StatusDegraded || entry.status == StatusReconnecting) {
		status = StatusReconnecting
	}
	if event.State == BackendStarting && entry == nil {
		if current := s.staticState[event.Name]; current.Status == StatusFailed || current.Status == StatusDegraded || current.Status == StatusReconnecting {
			status = StatusReconnecting
		}
	}
	s.mu.Unlock()
	if entry != nil {
		s.updateDynamic(entry, status, event.Counts, errorCode, status == StatusConnected || status == StatusDegraded)
		return
	}
	if tombstone {
		return
	}
	s.mu.Lock()
	_, pending := s.staticPending[event.Name]
	s.mu.Unlock()
	if pending {
		return
	}
	s.finishStatic(event.Name, status, event.Counts, errorCode)
}

func (s *Service) handleBackendLog(event BackendEvent) {
	s.mu.Lock()
	entry := s.dynamic[event.Name]
	_, tombstone := s.tombstones[event.Name]
	key := staticLogKey(event.Name)
	serverCfg := config.MCPConfig{}
	if entry != nil {
		key = entry.internal
		serverCfg = entry.config
	} else if tombstone {
		s.mu.Unlock()
		return
	} else if s.store != nil {
		serverCfg = s.store.MCPSnapshot()[event.Name]
	}
	s.appendLogLocked(key, event.Log, serverCfg)
	s.mu.Unlock()
}

func (s *Service) updateDynamic(entry *serverEntry, status Status, counts Counts, errorCode string, authorize bool) {
	s.mu.Lock()
	state := s.sessions[entry.sessionID]
	if state == nil || state.epoch != entry.epoch || state.servers[entry.id] != entry || entry.retired {
		s.mu.Unlock()
		return
	}
	changed := entry.status != status || entry.counts != counts || entry.errorCode != errorCode || entry.authorized != authorize
	if !changed {
		s.mu.Unlock()
		return
	}
	entry.status, entry.counts, entry.errorCode = status, counts, errorCode
	entry.authorized = authorize
	entry.updatedAt = s.config.Clock()
	state.revision++
	revision := state.revision
	s.mu.Unlock()
	s.publish(entry.sessionID, entry, revision)
}

func (s *Service) finishDynamicDisabled(entry *serverEntry) {
	s.mu.Lock()
	state := s.sessions[entry.sessionID]
	if state == nil || state.servers[entry.id] != entry {
		s.mu.Unlock()
		return
	}
	entry.status = StatusDisabled
	entry.counts = Counts{}
	entry.errorCode = ""
	entry.authorized = false
	entry.updatedAt = s.config.Clock()
	state.revision++
	revision := state.revision
	s.mu.Unlock()
	s.publish(entry.sessionID, entry, revision)
}

func (s *Service) finishStatic(name string, status Status, counts Counts, errorCode string) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if _, dynamic := s.tombstones[name]; dynamic || !s.isConfiguredStaticLocked(name) {
		s.mu.Unlock()
		return
	}
	next := Server{ID: staticID(name), Name: name, Scope: ScopeStatic, Status: status, Counts: counts, ErrorCode: errorCode, UpdatedAt: s.config.Clock()}
	if current, ok := s.staticState[name]; ok && current.Status == next.Status && current.Counts == next.Counts && current.ErrorCode == next.ErrorCode {
		delete(s.staticPending, name)
		s.mu.Unlock()
		return
	}
	s.staticState[name] = next
	delete(s.staticPending, name)
	sessions := make([]struct {
		id       string
		revision uint64
	}, 0, len(s.sessions))
	for id, state := range s.sessions {
		state.revision++
		sessions = append(sessions, struct {
			id       string
			revision uint64
		}{id, state.revision})
	}
	s.mu.Unlock()
	for _, session := range sessions {
		s.publishServer(session.id, Server{
			ID: staticID(name), Name: name, Scope: ScopeStatic, Status: status,
			Counts: counts, Revision: session.revision, ErrorCode: errorCode,
			UpdatedAt: s.config.Clock(),
		})
	}
}

func (s *Service) finishStaticOperation(name string, epoch uint64, status Status, counts Counts, errorCode string) {
	s.mu.Lock()
	if s.staticEpoch[name] != epoch {
		s.mu.Unlock()
		return
	}
	delete(s.staticCancel, name)
	s.mu.Unlock()
	s.finishStatic(name, status, counts, errorCode)
}

func (s *Service) isStaticOperation(name string, epoch uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.staticEpoch[name] == epoch
}

// chainStaticOperationLocked preserves request order for persistent enabled
// state changes. Cancellation alone is insufficient because an older config
// write may already be in progress when a newer reconnect or disable arrives.
func (s *Service) chainStaticOperationLocked(name string) (<-chan struct{}, chan struct{}) {
	previous := s.staticTail[name]
	done := make(chan struct{})
	s.staticTail[name] = done
	return previous, done
}

func waitStaticOperation(ctx context.Context, previous <-chan struct{}) bool {
	if previous == nil {
		return ctx.Err() == nil
	}
	select {
	case <-previous:
		return ctx.Err() == nil
	case <-ctx.Done():
		return false
	}
}

func (s *Service) publishStatic(name string, status Status, counts Counts, errorCode string) {
	s.mu.Lock()
	next := Server{ID: staticID(name), Name: name, Scope: ScopeStatic, Status: status, Counts: counts, ErrorCode: errorCode, UpdatedAt: s.config.Clock()}
	if current, ok := s.staticState[name]; ok && current.Status == next.Status && current.Counts == next.Counts && current.ErrorCode == next.ErrorCode {
		s.mu.Unlock()
		return
	}
	s.staticState[name] = next
	sessions := make([]struct {
		id       string
		revision uint64
	}, 0, len(s.sessions))
	for id, state := range s.sessions {
		state.revision++
		sessions = append(sessions, struct {
			id       string
			revision uint64
		}{id, state.revision})
	}
	s.mu.Unlock()
	for _, session := range sessions {
		s.publishServer(session.id, Server{
			ID: staticID(name), Name: name, Scope: ScopeStatic, Status: status,
			Counts: counts, Revision: session.revision, ErrorCode: errorCode,
			UpdatedAt: s.config.Clock(),
		})
	}
}

func (s *Service) publish(sessionID string, entry *serverEntry, revision uint64) {
	s.mu.Lock()
	server := serverProjection(entry, revision)
	s.mu.Unlock()
	s.publishServer(sessionID, server)
}

func (s *Service) publishServer(sessionID string, server Server) {
	if s.events == nil {
		return
	}
	_, _ = s.events.Publish(sessionID, sessionevent.NewEvent{
		Kind: sessionevent.KindMCPStatus, Delivery: sessionevent.DeliveryReliable,
		AdvanceRevision: true,
		Payload: sessionevent.MCPStatus{
			ServerID: server.ID, Name: server.Name, Scope: string(server.Scope),
			Status: string(server.Status), Tools: server.Counts.Tools,
			Prompts: server.Counts.Prompts, Resources: server.Counts.Resources,
			Revision: server.Revision, ErrorCode: server.ErrorCode,
		},
	})
}

func (s *Service) ensureSessionLocked(sessionID string) (*sessionState, error) {
	if s.closed || sessionID == "" {
		if s.closed {
			return nil, ErrClosed
		}
		return nil, ErrNotFound
	}
	state := s.sessions[sessionID]
	if state != nil {
		return state, nil
	}
	if len(s.sessions) >= s.config.MaxSessions {
		return nil, ErrCapacity
	}
	state = &sessionState{servers: make(map[string]*serverEntry)}
	s.sessions[sessionID] = state
	return state, nil
}

func (s *Service) staticNameLocked(serverID string) (string, bool) {
	if !strings.HasPrefix(serverID, "static:") || s.store == nil {
		return "", false
	}
	name := strings.TrimPrefix(serverID, "static:")
	_, ok := s.store.MCPSnapshot()[name]
	if _, dynamic := s.tombstones[name]; dynamic {
		return "", false
	}
	return name, ok
}

func (s *Service) allows(sessionID, name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, scoped := s.tombstones[name]; !scoped {
		return true
	}
	entry := s.dynamic[name]
	return entry != nil && entry.sessionID == sessionID && entry.authorized
}

func (s *Service) revision(sessionID string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if state := s.sessions[sessionID]; state != nil {
		return state.revision
	}
	return 0
}

func (s *Service) isCurrent(entry *serverEntry, epoch uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.sessions[entry.sessionID]
	return state != nil && state.epoch == epoch && entry.epoch == epoch && state.servers[entry.id] == entry && !entry.retired
}

func (s *Service) isConfiguredStaticLocked(name string) bool {
	if s.store == nil {
		return false
	}
	_, ok := s.store.MCPSnapshot()[name]
	return ok
}

func (s *Service) validateConfigs(values []ServerConfig) []ServerConfig {
	result := make([]ServerConfig, 0, min(len(values), s.config.MaxServersPerSession))
	seen := make(map[string]struct{})
	for _, value := range values {
		name := strings.TrimSpace(value.Name)
		if name == "" || len(name) > maxServerNameBytes || !utf8.ValidString(name) || strings.IndexByte(name, 0) >= 0 {
			continue
		}
		if _, exists := seen[name]; exists || len(result) >= s.config.MaxServersPerSession {
			continue
		}
		seen[name] = struct{}{}
		value.Name = name
		value.Config = cloneMCPConfig(value.Config)
		value.Config.Disabled = false
		result = append(result, value)
	}
	return result
}

func publicState(state BackendState) (Status, string) {
	switch state {
	case BackendDisabled:
		return StatusDisabled, ""
	case BackendStarting:
		return StatusStarting, ""
	case BackendConnected:
		return StatusConnected, ""
	case BackendCached:
		return StatusDegraded, "CRUSH_MCP_LIVE_CONNECTION_UNAVAILABLE"
	case BackendNeedsAuth:
		return StatusFailed, "CRUSH_MCP_AUTH_REQUIRED"
	case BackendCircuitOpen:
		return StatusFailed, "CRUSH_MCP_CIRCUIT_OPEN"
	default:
		return StatusFailed, "CRUSH_MCP_CONNECTION_FAILED"
	}
}

func serverProjection(entry *serverEntry, revision uint64) Server {
	return Server{
		ID: entry.id, Name: entry.name, Scope: ScopeSession, Status: entry.status,
		Counts: entry.counts, Revision: revision, ErrorCode: entry.errorCode,
		UpdatedAt: entry.updatedAt,
	}
}

func compareServers(a, b Server) int {
	if value := strings.Compare(string(a.Scope), string(b.Scope)); value != 0 {
		return value
	}
	return strings.Compare(a.Name, b.Name)
}

func staticID(name string) string     { return "static:" + name }
func dynamicID(name string) string    { return "session:" + name }
func staticLogKey(name string) string { return "static\x00" + name }

func internalName(instanceID, sessionID, name string, generation uint64) string {
	sum := sha256.Sum256([]byte(sessionID))
	return fmt.Sprintf("acp-%s-%x-%d-%s", instanceID, sum[:6], generation, name)
}

func (s *Service) nextInternalNameLocked(sessionID, name string) string {
	configured := map[string]config.MCPConfig{}
	if s.store != nil {
		configured = s.store.MCPSnapshot()
	}
	for {
		s.generation++
		candidate := internalName(s.instanceID, sessionID, name, s.generation)
		if _, exists := configured[candidate]; exists {
			continue
		}
		if _, tombstone := s.tombstones[candidate]; !tombstone {
			return candidate
		}
	}
}

func cloneMCPConfig(value config.MCPConfig) config.MCPConfig {
	return config.CloneMCPConfig(value)
}

func (s *Service) appendLifecycleLog(key, level, message string, serverCfg config.MCPConfig) {
	s.mu.Lock()
	s.appendLogLocked(key, BackendLog{Timestamp: s.config.Clock(), Level: level, Logger: "crush", Data: message}, serverCfg)
	s.mu.Unlock()
}

func (s *Service) appendLogLocked(key string, input BackendLog, serverCfg config.MCPConfig) {
	raw, err := json.Marshal(input.Data)
	if err != nil {
		raw = []byte(`"Unserializable MCP log"`)
	}
	message := strings.Trim(string(raw), `"`)
	message = redact.RedactString(message, redact.BuiltinPatterns, make(map[string]string))
	for _, secret := range mcpSecrets(serverCfg) {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED:mcp-credential]")
		}
	}
	message, _ = truncate(message, maxLogFieldBytes)
	logger := redact.RedactString(input.Logger, redact.BuiltinPatterns, make(map[string]string))
	logger, _ = truncate(logger, 256)
	level, _ := truncate(input.Level, 32)
	s.logSequence[key]++
	entry := LogEntry{
		Sequence: s.logSequence[key], Timestamp: input.Timestamp,
		Level: level, Logger: logger, Message: message,
	}
	if entry.Timestamp.IsZero() {
		entry.Timestamp = s.config.Clock()
	}
	size := len(entry.Level) + len(entry.Logger) + len(entry.Message) + 32
	s.logs[key] = append(s.logs[key], entry)
	s.logOrder = append(s.logOrder, logReference{key: key, sequence: entry.Sequence, bytes: size})
	s.logBytes += size
	for len(s.logOrder) > s.config.MaxLogEntries || s.logBytes > s.config.MaxLogBytes {
		s.evictOldestLogLocked()
	}
}

func (s *Service) evictOldestLogLocked() {
	if len(s.logOrder) == 0 {
		return
	}
	oldest := s.logOrder[0]
	s.logOrder = s.logOrder[1:]
	s.logBytes -= oldest.bytes
	entries := s.logs[oldest.key]
	if len(entries) > 0 && entries[0].Sequence == oldest.sequence {
		entries = entries[1:]
	}
	if len(entries) == 0 {
		delete(s.logs, oldest.key)
	} else {
		s.logs[oldest.key] = entries
	}
}

func (s *Service) removeLogsLocked(key string) {
	delete(s.logs, key)
	filtered := s.logOrder[:0]
	s.logBytes = 0
	for _, ref := range s.logOrder {
		if ref.key != key {
			filtered = append(filtered, ref)
			s.logBytes += ref.bytes
		}
	}
	s.logOrder = filtered
}

func mcpSecrets(value config.MCPConfig) []string {
	secrets := make([]string, 0, len(value.Env)+len(value.Headers)+4)
	for _, secret := range value.Env {
		secrets = append(secrets, secret)
	}
	for _, secret := range value.Headers {
		secrets = append(secrets, secret)
	}
	if parsed, err := url.Parse(value.URL); err == nil {
		if parsed.User != nil || parsed.RawQuery != "" {
			secrets = append(secrets, value.URL)
		}
		if parsed.User != nil {
			secrets = append(secrets, parsed.User.Username())
			if password, ok := parsed.User.Password(); ok {
				secrets = append(secrets, password)
			}
		}
		for _, values := range parsed.Query() {
			secrets = append(secrets, values...)
		}
	}
	if value.OAuth != nil {
		secrets = append(secrets, value.OAuth.ClientSecret)
		if value.OAuth.Token != nil {
			secrets = append(secrets, value.OAuth.Token.AccessToken, value.OAuth.Token.RefreshToken)
		}
	}
	return secrets
}

func truncate(value string, maxBytes int) (string, bool) {
	if len(value) <= maxBytes {
		return value, false
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end], true
}
