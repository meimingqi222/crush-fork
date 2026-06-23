package engine

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// sessionState tracks per-session lifecycle state for the engine.
type sessionState struct {
	firstTurnInjected bool
	lastWMUpdate      time.Time
	pendingWrites     int
}

// Engine is the top-level orchestrator for the memory system pipeline.
// It holds the EventStore and provides lifecycle and status methods.
type Engine struct {
	store   EventStore
	db      *sql.DB
	enabled bool
	backend string

	extractor          Extractor
	consolidator       Consolidator
	materializers      []Materializer
	retriever          Retriever
	transcriptRetainer TranscriptRetainer
	tripleStore        *TripleStore
	conflictDetector   *ConflictDetector

	lastExtractionRun         *time.Time
	lastConsolidationRun      *time.Time
	lastConsolidatedWatermark int64
	pipelineMu                sync.RWMutex

	// session lifecycle state
	sessionStates map[string]*sessionState
	sessionMu     sync.Mutex

	// degraded mode
	degraded       bool
	degradedReason string
	degradedMu     sync.RWMutex

	// throttle
	workingMemoryThrottle time.Duration

	// reranker (optional) applied during Retrieve and compaction recall.
	reranker Reranker

	// proactiveLinker automatically builds related_to/refines edges between
	// new memories and semantically similar existing ones.
	proactiveLinker *ProactiveLinker

	// embeddingStore persists pre-computed embedding vectors to avoid
	// re-embedding all candidates on every recall query.
	embeddingStore    *EmbeddingStore
	embeddingPipeline *EmbeddingPipeline

	// Background materialization
	bgInterval    time.Duration
	bgEveryNTurns int
	bgTurnCounter int
	bgStop        chan struct{}
	bgDone        chan struct{}
	bgStarted     bool
	bgMu          sync.Mutex
	lastBgRun     *time.Time

	// Background consolidation: a separate ticker that periodically runs
	// TriggerConsolidation so long-running sessions merge episodic events
	// into durable memory before session close. Uses its own field set so
	// the two background loops can start/stop independently.
	consBgInterval time.Duration
	consBgStop     chan struct{}
	consBgDone     chan struct{}
	consBgStarted  bool
	consBgMu       sync.Mutex
	lastConsBgRun  *time.Time
}

const consolidationCheckpointView = "_pipeline_consolidation"

// Config holds configuration for the memory engine.
type Config struct {
	Enabled               bool
	Backend               string
	WorkingMemoryThrottle time.Duration

	// BackgroundInterval enables a periodic background materializer when > 0.
	BackgroundInterval time.Duration
	// BackgroundEveryNTurns triggers an opportunistic materialization after
	// the given number of idle turns even before BackgroundInterval elapses.
	// Set to 0 to disable turn-counter triggering.
	BackgroundEveryNTurns int

	// ConsolidationInterval enables a periodic background consolidator when
	// > 0. Unlike session-close consolidation, this runs on a timer so
	// long-running sessions still merge episodic events into durable memory.
	ConsolidationInterval time.Duration
}

// New creates a new memory Engine with the given SQLite database and config.
func New(db *sql.DB, cfg Config) *Engine {
	throttle := cfg.WorkingMemoryThrottle
	if throttle <= 0 {
		throttle = 30 * time.Second
	}
	backend := cfg.Backend
	if backend == "" {
		backend = "local"
	}
	e := &Engine{
		store:                 NewSQLiteEventStore(db),
		db:                    db,
		enabled:               cfg.Enabled,
		backend:               backend,
		sessionStates:         make(map[string]*sessionState),
		workingMemoryThrottle: throttle,
		bgInterval:            cfg.BackgroundInterval,
		bgEveryNTurns:         cfg.BackgroundEveryNTurns,
		consBgInterval:        cfg.ConsolidationInterval,
		tripleStore:           NewTripleStore(db),
		conflictDetector:      NewConflictDetector(db),
		embeddingStore:        NewEmbeddingStore(db),
	}
	e.proactiveLinker = NewProactiveLinker(e.store, e.tripleStore, nil)
	e.embeddingPipeline = NewEmbeddingPipeline(e.store, e.embeddingStore, nil)
	return e
}

// EventStore returns the engine's event store.
func (e *Engine) EventStore() EventStore {
	return e.store
}

// Backend returns the configured memory backend type.
func (e *Engine) Backend() string {
	return e.backend
}

// SetTranscriptRetainer sets the transcript retainer for backends that retain
// bounded raw transcript windows directly.
func (e *Engine) SetTranscriptRetainer(retainer TranscriptRetainer) {
	e.transcriptRetainer = retainer
}

// TranscriptRetainer returns the attached transcript retainer, or nil if not set.
func (e *Engine) TranscriptRetainer() TranscriptRetainer {
	return e.transcriptRetainer
}

// SetExtractor sets the Extractor component for the engine. When set,
// AfterTurnIdle automatically runs extraction on each idle turn.
func (e *Engine) SetExtractor(extractor Extractor) {
	e.extractor = extractor
}

// SetConsolidator sets the Consolidator component for the engine. When set,
// consolidation runs automatically on session deletion (OnSessionDeleted),
// on shutdown (Flush), and via the background consolidator, and can be
// triggered manually via TriggerConsolidation.
func (e *Engine) SetConsolidator(consolidator Consolidator) {
	e.consolidator = consolidator
}

// SetMaterializer registers a Materializer component. When set,
// TriggerMaterialization invokes it to produce consumer-facing views.
func (e *Engine) SetMaterializer(m Materializer) {
	e.materializers = append(e.materializers, m)
}

// SetRetriever sets the Retriever component for query-based memory recall.
func (e *Engine) SetRetriever(r Retriever) {
	e.retriever = r
}

// TripleStore returns the engine's triple store for structured fact queries.
func (e *Engine) TripleStore() *TripleStore {
	return e.tripleStore
}

// EmbeddingPipeline returns the engine's embedding pipeline for cached
// vector lookups and background embedding computation.
func (e *Engine) EmbeddingPipeline() *EmbeddingPipeline {
	return e.embeddingPipeline
}

// ConflictDetector returns the engine's conflict detector for contradiction
// management.
func (e *Engine) ConflictDetector() *ConflictDetector {
	return e.conflictDetector
}

// Retriever returns the attached Retriever, or nil if not set.
func (e *Engine) Retriever() Retriever {
	return e.retriever
}

// TriggerMaterialization iterates over all registered materializers and
// triggers a materialization pass for each of their views. Only views whose
// watermark has advanced since the last run are rebuilt.
func (e *Engine) TriggerMaterialization(ctx context.Context) error {
	if !e.enabled || len(e.materializers) == 0 {
		return nil
	}

	for _, m := range e.materializers {
		views, err := m.ListViews(ctx)
		if err != nil {
			slog.Warn("Failed to list materializer views", "error", err)
			continue
		}
		for _, viewName := range views {
			if err := m.Materialize(ctx, viewName, nil); err != nil {
				slog.Warn("Failed to materialize view",
					"view", viewName,
					"error", err)
			}
		}
	}
	return nil
}

// TriggerConsolidation runs the consolidation pipeline on unprocessed events.
// It queries events above the last consolidated watermark, passes them to the
// Consolidator, and appends the resulting consolidated events to the store.
// Uses a database-level lease to prevent multiple instances from running
// consolidation simultaneously.
func (e *Engine) TriggerConsolidation(ctx context.Context) error {
	if !e.enabled || e.consolidator == nil {
		return nil
	}
	if e.IsDegraded() {
		slog.Debug("Memory engine in degraded mode, skipping consolidation")
		return nil
	}

	// Try to acquire global consolidation lease.
	lease, err := e.acquireConsolidationLease(ctx)
	if err != nil {
		slog.Debug("Consolidation lease acquisition failed, skipping", "error", err)
		return nil
	}
	if lease == nil {
		slog.Debug("Consolidation lease held by another instance, skipping")
		return nil
	}
	defer e.releaseConsolidationLease(ctx, lease)

	e.pipelineMu.Lock()
	defer e.pipelineMu.Unlock()

	watermark, err := e.pipelineWatermark(ctx, consolidationCheckpointView)
	if err != nil {
		return fmt.Errorf("reading consolidation watermark: %w", err)
	}
	if watermark > e.lastConsolidatedWatermark {
		e.lastConsolidatedWatermark = watermark
	}

	events, err := e.store.Query(ctx, EventFilter{
		MinWatermark: e.lastConsolidatedWatermark,
		Limit:        500,
	})
	if err != nil {
		return fmt.Errorf("querying events for consolidation: %w", err)
	}
	if len(events) == 0 {
		return nil
	}

	queriedWatermark := events[len(events)-1].Watermark
	events = filterConsolidatableEvents(events)
	if len(events) == 0 {
		if err := e.setPipelineWatermark(ctx, consolidationCheckpointView, queriedWatermark); err != nil {
			return fmt.Errorf("advancing consolidation watermark: %w", err)
		}
		e.lastConsolidatedWatermark = queriedWatermark
		return nil
	}

	consolidated, err := e.consolidator.Consolidate(ctx, events)
	if err != nil {
		return fmt.Errorf("consolidation failed: %w", err)
	}

	for _, evt := range consolidated {
		if err := e.store.Append(ctx, evt); err != nil {
			slog.Warn("Failed to append consolidated event", "error", err)
		}
	}

	// Enqueue consolidated events for background embedding.
	if e.embeddingPipeline != nil && len(consolidated) > 0 {
		ids := make([]string, 0, len(consolidated))
		for _, evt := range consolidated {
			ids = append(ids, evt.ID)
		}
		e.embeddingPipeline.Enqueue(ids...)
	}

	// Proactively link consolidated memories to related existing memories.
	if e.proactiveLinker != nil && len(consolidated) > 0 {
		go e.proactiveLinker.LinkEvents(context.Background(), consolidated)
	}

	if len(consolidated) > 0 {
		now := time.Now()
		e.lastConsolidationRun = &now
	}

	// Advance watermark past processed events so they are not re-analyzed.
	e.lastConsolidatedWatermark = queriedWatermark
	if err := e.setPipelineWatermark(ctx, consolidationCheckpointView, e.lastConsolidatedWatermark); err != nil {
		return fmt.Errorf("persisting consolidation watermark: %w", err)
	}

	slog.Debug("Consolidation complete",
		"events_processed", len(events),
		"events_consolidated", len(consolidated),
		"watermark", e.lastConsolidatedWatermark,
	)

	// Run conflict detection after consolidation to catch contradictions
	// among the newly consolidated events.
	if e.conflictDetector != nil {
		if n, err := e.conflictDetector.DetectConflicts(); err != nil {
			slog.Warn("Post-consolidation conflict detection failed", "error", err)
		} else if n > 0 {
			slog.Debug("Detected memory conflicts", "count", n)
		}
	}
	if e.tripleStore != nil {
		if n, err := e.tripleStore.DetectTripleConflicts(ctx); err != nil {
			slog.Warn("Post-consolidation triple conflict detection failed", "error", err)
		} else if n > 0 {
			slog.Debug("Detected triple conflicts", "count", n)
		}
	}

	return nil
}

const (
	consolidationLeaseKey     = "consolidation_global"
	consolidationLeaseSeconds = 180
)

// consolidationLease represents an acquired consolidation lease.
type consolidationLease struct {
	token      string
	acquiredAt time.Time
}

// acquireConsolidationLease attempts to acquire a global lease for consolidation.
// Returns nil if another instance holds the lease.
func (e *Engine) acquireConsolidationLease(ctx context.Context) (*consolidationLease, error) {
	if e.db == nil {
		return nil, nil
	}

	now := time.Now().Unix()
	token := fmt.Sprintf("lease-%d-%s", now, randomString(8))

	result, err := e.db.ExecContext(ctx, `
		INSERT INTO memory_materialized_views (id, view_name, watermark, schema_version, created_at, updated_at)
		VALUES (?, ?, 1, 1, ?, ?)
		ON CONFLICT(view_name) DO UPDATE SET
			watermark = 1,
			updated_at = excluded.updated_at
		WHERE view_name = ?
		  AND (watermark = 0 OR updated_at IS NULL OR updated_at < ?)
	`, "lease-"+consolidationLeaseKey, consolidationLeaseKey, now, now, consolidationLeaseKey, now-consolidationLeaseSeconds)
	if err != nil {
		return nil, fmt.Errorf("acquiring consolidation lease: %w", err)
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		return nil, nil
	}

	return &consolidationLease{token: token, acquiredAt: time.Now()}, nil
}

// releaseConsolidationLease releases the consolidation lease.
func (e *Engine) releaseConsolidationLease(ctx context.Context, lease *consolidationLease) {
	if e.db == nil || lease == nil {
		return
	}

	now := time.Now().Unix()
	_, _ = e.db.ExecContext(ctx, `
		UPDATE memory_materialized_views
		SET watermark = 0, updated_at = ?
		WHERE view_name = ?
	`, now, consolidationLeaseKey)
}

func randomString(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		for i := range b {
			b[i] = letters[i%len(letters)]
		}
		return string(b)
	}
	for i := range b {
		b[i] = letters[int(b[i])%len(letters)]
	}
	return string(b)
}

// Enabled returns whether the memory engine is enabled.
func (e *Engine) Enabled() bool {
	return e.enabled
}

// SetEnabled controls whether the memory engine is active at runtime.
// When disabled, memory lifecycle hooks and background pipelines are paused.
func (e *Engine) SetEnabled(enabled bool) {
	e.enabled = enabled
}

// SetDegraded marks the engine as operating in degraded mode.
// In degraded mode, existing materialized summaries are still injected,
// but extraction and consolidation are paused until the model recovers.
func (e *Engine) SetDegraded(active bool, reason string) {
	e.degradedMu.Lock()
	defer e.degradedMu.Unlock()
	e.degraded = active
	e.degradedReason = reason
}

// IsDegraded reports whether the engine is in degraded mode.
func (e *Engine) IsDegraded() bool {
	e.degradedMu.RLock()
	defer e.degradedMu.RUnlock()
	return e.degraded
}

// DegradedReason returns the reason for degraded mode, if any.
func (e *Engine) DegradedReason() string {
	e.degradedMu.RLock()
	defer e.degradedMu.RUnlock()
	return e.degradedReason
}

// OnSessionCreated initializes engine session state for a new session.
// Should be called when a session is first created or first seen by the
// coordinator. It marks the session for first-turn summary injection.
func (e *Engine) OnSessionCreated(ctx context.Context, sessionID string) error {
	if !e.enabled {
		return nil
	}
	e.sessionMu.Lock()
	defer e.sessionMu.Unlock()
	if _, exists := e.sessionStates[sessionID]; !exists {
		e.sessionStates[sessionID] = &sessionState{}
		slog.Debug("Memory engine initialized session state", "session_id", sessionID)
	}
	return nil
}

// AfterTurnIdle appends transcript events to the event log and manages
// Working Memory update throttle. Should be called after each LLM turn.
// events are raw transcript slices with source_hash for deduplication.
// When events is nil and an Extractor is configured, extraction runs
// automatically to produce events from the session transcript.
func (e *Engine) AfterTurnIdle(ctx context.Context, sessionID string, events []MemoryEvent) error {
	if !e.enabled {
		return nil
	}
	if e.IsDegraded() {
		slog.Debug("Memory engine in degraded mode, skipping extraction", "session_id", sessionID)
		return nil
	}

	if len(events) == 0 && e.extractor != nil {
		extracted, err := e.extractor.Extract(ctx, sessionID)
		if err != nil {
			slog.Warn("Memory extraction failed", "error", err, "session_id", sessionID)
		} else {
			events = extracted
			now := time.Now()
			e.pipelineMu.Lock()
			e.lastExtractionRun = &now
			e.pipelineMu.Unlock()

			// Store any triples extracted alongside the events.
			e.storeExtractedTriples(ctx, extracted)
		}
	}

	for _, evt := range events {
		if err := e.store.Append(ctx, evt); err != nil {
			slog.Warn("Failed to append memory event", "error", err, "session_id", sessionID)
		}
	}

	// Enqueue newly extracted events for background embedding.
	if e.embeddingPipeline != nil && len(events) > 0 {
		ids := make([]string, 0, len(events))
		for _, evt := range events {
			ids = append(ids, evt.ID)
		}
		e.embeddingPipeline.Enqueue(ids...)
	}

	// Proactively link newly extracted memories to related existing memories.
	if e.proactiveLinker != nil && len(events) > 0 {
		go e.proactiveLinker.LinkEvents(context.Background(), events)
	}

	if hasMaterializableEvents(events) {
		if err := e.TriggerMaterialization(ctx); err != nil {
			slog.Warn("Turn-end memory materialization failed", "error", err, "session_id", sessionID)
		}
	}
	e.sessionMu.Lock()
	if state, ok := e.sessionStates[sessionID]; ok {
		state.pendingWrites += len(events)
	}
	e.sessionMu.Unlock()

	// Turn-counter trigger: opportunistic background materialization even
	// when this turn produced no directly materializable events. This keeps
	// memory views fresh during long planning/inspection sessions where
	// extraction happens but consolidation is deferred.
	if e.bgEveryNTurns > 0 {
		e.bgMu.Lock()
		e.bgTurnCounter++
		fire := e.bgTurnCounter >= e.bgEveryNTurns
		if fire {
			e.bgTurnCounter = 0
		}
		e.bgMu.Unlock()
		if fire {
			if err := e.runBackgroundPass(ctx); err != nil {
				slog.Warn("Turn-counter background materialization failed", "error", err)
			}
		}
	}
	return nil
}

// OnSessionDeleted cleans up engine session state and runs a final
// consolidation + materialization pass for the deleted session's events.
//
// This fires ONLY when a session is explicitly deleted (e.g. via the sessions
// dialog). It does NOT fire on quit, Ctrl+C, or terminal close — those paths
// call Flush before Close instead. The name reflects this: it is a
// deletion-side-effect hook, not a general "session ended" hook.
func (e *Engine) OnSessionDeleted(ctx context.Context, sessionID string) error {
	if !e.enabled {
		return nil
	}
	e.sessionMu.Lock()
	delete(e.sessionStates, sessionID)
	e.sessionMu.Unlock()

	if err := e.TriggerConsolidation(ctx); err != nil {
		slog.Warn("Session-deletion consolidation failed", "error", err, "session_id", sessionID)
	}

	if err := e.TriggerMaterialization(ctx); err != nil {
		slog.Warn("Session-deletion materialization failed", "error", err, "session_id", sessionID)
	}

	slog.Debug("Memory engine cleaned up session state", "session_id", sessionID)
	return nil
}

// Flush runs a consolidation + materialization pass on demand. It is a public
// API for callers that want to force a memory refresh (e.g. a future explicit
// "remember now" command), but it is NOT called during normal process shutdown.
//
// Shutdown intentionally does not call Flush: episodic events are already
// persisted to SQLite by AfterTurnIdle on every turn, and the background
// consolidator (enabled by default) merges them into durable memory on a
// timer. A fresh LLM consolidation at exit would block the user for 30-45s for
// little gain — it only advances the materialized views, which the next run's
// ticker refreshes anyway. This matches oh-my-pi and MiMo-Code.
//
// The caller MUST pass a context with a timeout generous enough for a
// background LLM call (consolidation can take 30-45s). Cancellation is handled
// gracefully: a cancelled context aborts consolidation with a Debug log.
func (e *Engine) Flush(ctx context.Context) {
	if !e.enabled {
		return
	}
	// Consolidation is the expensive step (LLM call). If the caller cancels,
	// abort without noisy warnings — cancellation is an expected outcome.
	if err := e.TriggerConsolidation(ctx); err != nil {
		if ctx.Err() != nil {
			slog.Debug("Flush consolidation cancelled", "reason", ctx.Err())
			return
		}
		slog.Warn("Flush consolidation failed", "error", err)
	}
	// Materialization is cheap (no LLM) and safe to run from a fresh context
	// so a consolidation cancel still surfaces whatever was already
	// consolidated. This is a no-op when there is nothing new to materialize.
	matCtx, matCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer matCancel()
	if err := e.TriggerMaterialization(matCtx); err != nil {
		slog.Warn("Shutdown materialization failed", "error", err)
	}
}

// OnBeforeCompaction flushes Working Memory and records a compaction
// rescue event before the session transcript is summarized/compacted.
func (e *Engine) OnBeforeCompaction(ctx context.Context, sessionID string) error {
	if !e.enabled {
		return nil
	}
	slog.Debug("Memory engine preparing for compaction", "session_id", sessionID)
	if err := e.AfterTurnIdle(ctx, sessionID, nil); err != nil {
		slog.Warn("Pre-compaction memory extraction failed", "error", err, "session_id", sessionID)
	}
	if err := e.TriggerMaterialization(ctx); err != nil {
		return fmt.Errorf("pre-compaction memory materialization: %w", err)
	}
	return nil
}

// ShouldUpdateWorkingMemory checks the throttle and returns whether a
// Working Memory update should proceed for the given session.
func (e *Engine) ShouldUpdateWorkingMemory(sessionID string) bool {
	e.sessionMu.Lock()
	defer e.sessionMu.Unlock()
	state, ok := e.sessionStates[sessionID]
	if !ok {
		return false
	}
	if time.Since(state.lastWMUpdate) < e.workingMemoryThrottle {
		return false
	}
	state.lastWMUpdate = time.Now()
	return true
}

// Status returns the current engine pipeline status.
func (e *Engine) Status(ctx context.Context) (*EngineStatus, error) {
	eventStoreStatus := "ok"
	if !e.enabled {
		eventStoreStatus = "disabled"
	}
	if e.store == nil {
		eventStoreStatus = "unavailable"
	}

	// Job statuses
	jobs, err := e.queryJobStatuses(ctx)
	if err != nil {
		return nil, fmt.Errorf("querying job statuses: %w", err)
	}

	// Materialized view statuses
	views, err := e.queryViewStatuses(ctx)
	if err != nil {
		return nil, fmt.Errorf("querying view statuses: %w", err)
	}

	e.pipelineMu.RLock()
	extractionState := "idle"
	if e.lastExtractionRun != nil {
		extractionState = "completed"
	}
	consolidationState := "idle"
	if e.lastConsolidationRun != nil {
		consolidationState = "completed"
	}
	lastExtractionRun := e.lastExtractionRun
	lastConsolidationRun := e.lastConsolidationRun
	lastConsolidatedWatermark := e.lastConsolidatedWatermark
	e.pipelineMu.RUnlock()

	// Degraded mode
	var degradedInfo *DegradedModeInfo
	if e.IsDegraded() {
		reason := e.DegradedReason()
		degradedInfo = &DegradedModeInfo{
			Active: true,
			Reason: reason,
		}
	}

	return &EngineStatus{
		Backend:          e.backend,
		EventStoreStatus: eventStoreStatus,
		ExtractionStatus: MemoryPipelineStatus{
			LastRunAt: lastExtractionRun,
			State:     extractionState,
		},
		ConsolidationStatus: MemoryPipelineStatus{
			LastRunAt:     lastConsolidationRun,
			LastWatermark: lastConsolidatedWatermark,
			State:         consolidationState,
		},
		MaterializationViews: views,
		Jobs:                 jobs,
		DegradedMode:         degradedInfo,
	}, nil
}

// RebuildView rebuilds a materialized view from the event log.
func (e *Engine) RebuildView(ctx context.Context, viewName string) error {
	if !e.enabled {
		return fmt.Errorf("memory engine is disabled")
	}

	_, err := e.db.ExecContext(ctx,
		"UPDATE memory_materialized_views SET watermark = 0, updated_at = ? WHERE view_name = ?",
		time.Now().Unix(), viewName)
	if err != nil {
		return fmt.Errorf("resetting view watermark: %w", err)
	}

	return e.TriggerMaterialization(ctx)
}

func filterConsolidatableEvents(events []MemoryEvent) []MemoryEvent {
	filtered := events[:0]
outer:
	for _, evt := range events {
		switch evt.Kind {
		case MemoryKindWorkingMemory, MemoryKindTaskState:
			continue
		}
		for _, tag := range evt.Tags {
			if tag == tagConsolidatedOutput {
				continue outer
			}
		}
		filtered = append(filtered, evt)
	}
	return filtered
}

func hasMaterializableEvents(events []MemoryEvent) bool {
	for _, evt := range events {
		if IsMaterializableEvent(evt) {
			return true
		}
	}
	return false
}

func IsMaterializableEvent(evt MemoryEvent) bool {
	return evt.Scope != MemoryScopeSession &&
		evt.Kind != MemoryKindWorkingMemory &&
		evt.Kind != MemoryKindTaskState
}

func (e *Engine) pipelineWatermark(ctx context.Context, name string) (int64, error) {
	var watermark int64
	err := e.db.QueryRowContext(ctx,
		"SELECT COALESCE(watermark, 0) FROM memory_materialized_views WHERE view_name = ?",
		name).Scan(&watermark)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return watermark, nil
}

func (e *Engine) setPipelineWatermark(ctx context.Context, name string, watermark int64) error {
	now := time.Now().Unix()
	_, err := e.db.ExecContext(ctx, `
		INSERT INTO memory_materialized_views (id, view_name, watermark, schema_version, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(view_name) DO UPDATE SET
			watermark = excluded.watermark,
			updated_at = excluded.updated_at`,
		"mvs-"+name,
		name,
		watermark,
		1,
		now,
		now,
	)
	return err
}

func (e *Engine) queryJobStatuses(ctx context.Context) ([]MemoryJobStatus, error) {
	rows, err := e.db.QueryContext(ctx, `
		SELECT id, job_type, status, owner, lease_expires_at, retry_count, max_retries, error_message, created_at, updated_at
		FROM memory_jobs
		ORDER BY created_at DESC
		LIMIT 50`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []MemoryJobStatus
	for rows.Next() {
		var (
			js           MemoryJobStatus
			leaseExpires sql.NullInt64
			errorMsg     string
			createdAt    int64
			updatedAt    int64
		)
		if err := rows.Scan(
			&js.ID, &js.JobType, &js.Status, &js.Owner,
			&leaseExpires, &js.RetryCount, &js.MaxRetries,
			&errorMsg, &createdAt, &updatedAt,
		); err != nil {
			return nil, err
		}
		js.ErrorMessage = errorMsg
		js.CreatedAt = time.Unix(createdAt, 0)
		js.UpdatedAt = time.Unix(updatedAt, 0)
		if leaseExpires.Valid {
			t := time.Unix(leaseExpires.Int64, 0)
			js.LeaseExpiresAt = &t
		}
		jobs = append(jobs, js)
	}
	return jobs, rows.Err()
}

func (e *Engine) queryViewStatuses(ctx context.Context) ([]MaterializedViewStatus, error) {
	rows, err := e.db.QueryContext(ctx, `
		SELECT view_name, watermark, schema_version, updated_at
		FROM memory_materialized_views
		ORDER BY view_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var views []MaterializedViewStatus
	for rows.Next() {
		var (
			vs        MaterializedViewStatus
			updatedAt int64
		)
		if err := rows.Scan(&vs.ViewName, &vs.Watermark, &vs.SchemaVersion, &updatedAt); err != nil {
			return nil, err
		}
		t := time.Unix(updatedAt, 0)
		vs.LastUpdatedAt = &t
		vs.State = "ok"
		views = append(views, vs)
	}
	return views, rows.Err()
}

// storeExtractedTriples writes any triples embedded in extracted events to the
// TripleStore.  This runs after extraction so that knowledge-graph triples are
// persisted alongside the flat event stream without requiring the caller to
// handle triple storage explicitly.
func (e *Engine) storeExtractedTriples(ctx context.Context, events []MemoryEvent) {
	if e.tripleStore == nil {
		return
	}
	for _, evt := range events {
		if len(evt.Triples) == 0 {
			continue
		}
		for _, tr := range evt.Triples {
			if tr.Subject == "" || tr.Predicate == "" || tr.Object == "" {
				continue
			}
			triple := Triple{
				Subject:       tr.Subject,
				Predicate:     tr.Predicate,
				Object:        tr.Object,
				Confidence:    evt.Confidence,
				Veracity:      evt.Veracity,
				SourceEventID: evt.ID,
				Scope:         evt.Scope,
				ValidFrom:     evt.CreatedAt,
			}
			if err := e.tripleStore.AddTriple(ctx, triple); err != nil {
				slog.Warn("Failed to store extracted triple",
					"error", err,
					"subject", tr.Subject,
					"predicate", tr.Predicate,
					"object", tr.Object)
			}
		}
	}
}

// Close releases all resources held by the engine.
func (e *Engine) Close() error {
	e.stopBackground()
	e.stopConsolidation()
	if e.store != nil {
		return e.store.Close()
	}
	return nil
}
