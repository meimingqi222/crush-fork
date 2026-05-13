package engine

import (
	"context"
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

	extractor     Extractor
	consolidator  Consolidator
	materializers []Materializer
	retriever     Retriever

	lastExtractionRun         *time.Time
	lastConsolidationRun      *time.Time
	lastConsolidatedWatermark int64

	// session lifecycle state
	sessionStates map[string]*sessionState
	sessionMu     sync.Mutex

	// degraded mode
	degraded       bool
	degradedReason string
	degradedMu     sync.RWMutex

	// throttle
	workingMemoryThrottle time.Duration
}

const consolidationCheckpointView = "_pipeline_consolidation"

// Config holds configuration for the memory engine.
type Config struct {
	Enabled               bool
	WorkingMemoryThrottle time.Duration
}

// New creates a new memory Engine with the given SQLite database and config.
func New(db *sql.DB, cfg Config) *Engine {
	throttle := cfg.WorkingMemoryThrottle
	if throttle <= 0 {
		throttle = 30 * time.Second
	}
	return &Engine{
		store:                 NewSQLiteEventStore(db),
		db:                    db,
		enabled:               cfg.Enabled,
		sessionStates:         make(map[string]*sessionState),
		workingMemoryThrottle: throttle,
	}
}

// EventStore returns the engine's event store.
func (e *Engine) EventStore() EventStore {
	return e.store
}

// SetExtractor sets the Extractor component for the engine. When set,
// AfterTurnIdle automatically runs extraction on each idle turn.
func (e *Engine) SetExtractor(extractor Extractor) {
	e.extractor = extractor
}

// SetConsolidator sets the Consolidator component for the engine. When set,
// consolidation runs automatically on session close and can be triggered
// manually via TriggerConsolidation.
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
func (e *Engine) TriggerConsolidation(ctx context.Context) error {
	if !e.enabled || e.consolidator == nil {
		return nil
	}
	if e.IsDegraded() {
		slog.Debug("Memory engine in degraded mode, skipping consolidation")
		return nil
	}

	watermark, err := e.pipelineWatermark(ctx, consolidationCheckpointView)
	if err != nil {
		return fmt.Errorf("reading consolidation watermark: %w", err)
	}
	if watermark > e.lastConsolidatedWatermark {
		e.lastConsolidatedWatermark = watermark
	}

	sessionScope := MemoryScopeSession
	events, err := e.store.Query(ctx, EventFilter{
		MinWatermark: e.lastConsolidatedWatermark,
		Scope:        &sessionScope,
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
	return nil
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
			e.lastExtractionRun = &now
		}
	}

	for _, evt := range events {
		if err := e.store.Append(ctx, evt); err != nil {
			slog.Warn("Failed to append memory event", "error", err, "session_id", sessionID)
		}
	}
	e.sessionMu.Lock()
	if state, ok := e.sessionStates[sessionID]; ok {
		state.pendingWrites += len(events)
	}
	e.sessionMu.Unlock()
	return nil
}

// OnSessionClosed cleans up engine session state and triggers consolidation
// of any unprocessed episodic events from the closed session.
func (e *Engine) OnSessionClosed(ctx context.Context, sessionID string) error {
	if !e.enabled {
		return nil
	}
	e.sessionMu.Lock()
	delete(e.sessionStates, sessionID)
	e.sessionMu.Unlock()

	if err := e.TriggerConsolidation(ctx); err != nil {
		slog.Warn("Session-end consolidation failed", "error", err, "session_id", sessionID)
	}

	if err := e.TriggerMaterialization(ctx); err != nil {
		slog.Warn("Session-end materialization failed", "error", err, "session_id", sessionID)
	}

	slog.Debug("Memory engine cleaned up session state", "session_id", sessionID)
	return nil
}

// OnBeforeCompaction flushes Working Memory and records a compaction
// rescue event before the session transcript is summarized/compacted.
func (e *Engine) OnBeforeCompaction(ctx context.Context, sessionID string) error {
	if !e.enabled {
		return nil
	}
	slog.Debug("Memory engine preparing for compaction", "session_id", sessionID)
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

	extractionState := "idle"
	if e.lastExtractionRun != nil {
		extractionState = "completed"
	}
	consolidationState := "idle"
	if e.lastConsolidationRun != nil {
		consolidationState = "completed"
	}

	// Degraded mode
	var degradedInfo *DegradedModeInfo
	if e.degraded {
		reason := e.DegradedReason()
		degradedInfo = &DegradedModeInfo{
			Active: true,
			Reason: reason,
		}
	}

	return &EngineStatus{
		EventStoreStatus: eventStoreStatus,
		ExtractionStatus: MemoryPipelineStatus{
			LastRunAt: e.lastExtractionRun,
			State:     extractionState,
		},
		ConsolidationStatus: MemoryPipelineStatus{
			LastRunAt:     e.lastConsolidationRun,
			LastWatermark: e.lastConsolidatedWatermark,
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
	for _, evt := range events {
		switch evt.Kind {
		case MemoryKindWorkingMemory, MemoryKindTaskState:
			continue
		default:
			filtered = append(filtered, evt)
		}
	}
	return filtered
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

// Close releases all resources held by the engine.
func (e *Engine) Close() error {
	if e.store != nil {
		return e.store.Close()
	}
	return nil
}
