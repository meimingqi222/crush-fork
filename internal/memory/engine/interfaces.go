package engine

import "context"

// EventFilter defines query parameters for EventStore.Query.
type EventFilter struct {
	Scope        *MemoryScope
	Kind         *MemoryKind
	SessionID    *string
	AfterTime    *int64 // Unix timestamp in seconds, inclusive
	BeforeTime   *int64 // Unix timestamp in seconds, exclusive
	MinWatermark int64
	Tags         []string
	Limit        int
	// OrderDesc returns the newest/highest-watermark events first when true.
	OrderDesc bool
	// IncludeExpired when true includes events whose expires_at is in the past.
	// By default, expired events are excluded from query results.
	IncludeExpired bool
}

// EventStore is the core write-ahead log for memory events.
type EventStore interface {
	// Append writes a new event. It is idempotent: events with the same
	// (session_id, source_hash) are silently skipped.
	Append(ctx context.Context, event MemoryEvent) error

	// Query retrieves events matching the given filter.
	Query(ctx context.Context, filter EventFilter) ([]MemoryEvent, error)

	// GetByID retrieves a single event by its ID.
	GetByID(ctx context.Context, id string) (*MemoryEvent, error)

	// GetMaxWatermark returns the highest watermark across all events.
	GetMaxWatermark(ctx context.Context) (int64, error)

	// RecentSessions returns distinct session IDs whose events were created
	// at or after the given Unix-seconds timestamp, ordered by most-recent
	// event time descending. Empty session IDs are filtered out. When
	// sinceUnix is zero, the store returns the most recently active sessions
	// regardless of time. The limit caps the number of returned IDs.
	RecentSessions(ctx context.Context, sinceUnix int64, limit int) ([]string, error)

	// Close releases the underlying database resources.
	Close() error
}

// Extractor converts session transcripts into episodic memory events.
type Extractor interface {
	// Extract processes a session transcript and returns new memory events.
	Extract(ctx context.Context, sessionID string) ([]MemoryEvent, error)
}

// Consolidator groups and generalizes episodic events into semantic and
// procedural memory events.
type Consolidator interface {
	// Consolidate processes episodic events and returns consolidated events.
	Consolidate(ctx context.Context, events []MemoryEvent) ([]MemoryEvent, error)
}

// TranscriptRetainer retains bounded raw transcript windows without LLM extraction.
type TranscriptRetainer interface {
	RetainTranscript(ctx context.Context, sessionID string, turnCount int, content string) error
}

// Materializer renders consolidated memory events into consumer-facing
// artifacts (memory_summary.md, MEMORY.md, skills/, vector index, etc.).
// Each Materializer manages its own materialized views and tracks watermark
// progress via the memory_materialized_views table for incremental rebuild.
type Materializer interface {
	// Materialize generates or updates a specific view.
	// The events parameter may be nil; implementations should query the
	// EventStore directly for all events they need to render.
	// The viewName identifies which view to update (e.g. "memory_summary").
	Materialize(ctx context.Context, viewName string, events []MemoryEvent) error

	// ListViews returns all materialized view names managed by this materializer.
	ListViews(ctx context.Context) ([]string, error)
}

// Retriever provides query-based access to consolidated memory for prompt
// injection and recall.
type Retriever interface {
	// Retrieve returns the most relevant memory events for a given context.
	Retrieve(ctx context.Context, query string, opts map[string]any) ([]MemoryEvent, error)

	// Recall returns a formatted summary of materialized memory for prompt
	// injection. This is the primary recall path: it reads from materialized
	// views (memory_summary.md) and the event store to produce a prompt-ready
	// block. Called at session start for automatic summary injection.
	// opts may include "session_id" for working memory lookup.
	Recall(ctx context.Context, opts map[string]any) (string, error)

	// Reflect synthesizes across multiple memory events to answer a query
	// about past sessions, decisions, or project history. Unlike Recall which
	// returns materialized summaries, Reflect queries the event store across
	// sessions and returns a synthesized response. It does NOT write to
	// long-term memory.
	Reflect(ctx context.Context, query string, opts map[string]any) (string, error)
}
