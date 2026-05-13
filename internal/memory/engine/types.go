package engine

import "time"

// MemoryScope defines the scope of a memory event.
type MemoryScope string

const (
	MemoryScopeSession MemoryScope = "session"
	MemoryScopeProject MemoryScope = "project"
	MemoryScopeUser    MemoryScope = "user"
	MemoryScopeGlobal  MemoryScope = "global"
)

// MemoryKind defines the kind/category of a memory event.
type MemoryKind string

const (
	MemoryKindPreference    MemoryKind = "preference"
	MemoryKindDecision      MemoryKind = "decision"
	MemoryKindProcedure     MemoryKind = "procedure"
	MemoryKindPitfall       MemoryKind = "pitfall"
	MemoryKindReference     MemoryKind = "reference"
	MemoryKindTaskState     MemoryKind = "task_state"
	MemoryKindWorkingMemory MemoryKind = "working_memory"
)

// MemorySourceRef describes the origin of a memory event.
type MemorySourceRef struct {
	SessionID  string   `json:"session_id"`
	MessageIDs []string `json:"message_ids,omitempty"`
	Files      []string `json:"files,omitempty"`
	Commands   []string `json:"commands,omitempty"`
	CWD        string   `json:"cwd,omitempty"`
}

// MemoryEvent is the central event type in the event-sourced memory system.
type MemoryEvent struct {
	ID         string          `json:"id"`
	Scope      MemoryScope     `json:"scope"`
	Kind       MemoryKind      `json:"kind"`
	Content    string          `json:"content"`
	Summary    string          `json:"summary,omitempty"`
	Source     MemorySourceRef `json:"source"`
	Confidence float64         `json:"confidence"`
	Importance float64         `json:"importance"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
	Supersedes *string         `json:"supersedes,omitempty"`
	Tags       []string        `json:"tags,omitempty"`
	Watermark  int64           `json:"watermark"`
	ExpiresAt  *time.Time      `json:"expires_at,omitempty"`
}

// DegradedModeInfo describes degraded mode state when the background model
// is unavailable. Extraction and consolidation are paused but existing
// materialized summaries continue to be injected.
type DegradedModeInfo struct {
	Active bool   `json:"active"`
	Reason string `json:"reason,omitempty"`
}

// EngineStatus represents the current state of the memory engine pipeline.
type EngineStatus struct {
	Backend              string                   `json:"backend"`
	EventStoreStatus     string                   `json:"event_store_status"`
	ExtractionStatus     MemoryPipelineStatus     `json:"extraction_status"`
	ConsolidationStatus  MemoryPipelineStatus     `json:"consolidation_status"`
	MaterializationViews []MaterializedViewStatus `json:"materialization_status"`
	Jobs                 []MemoryJobStatus        `json:"jobs"`
	DegradedMode         *DegradedModeInfo        `json:"degraded_mode,omitempty"`
}

// MemoryPipelineStatus tracks one stage of the memory pipeline.
type MemoryPipelineStatus struct {
	LastRunAt     *time.Time `json:"last_run_at,omitempty"`
	LastWatermark int64      `json:"last_watermark"`
	State         string     `json:"state"` // idle, running, failed
	Error         string     `json:"error,omitempty"`
}

// MaterializedViewStatus tracks a single materialized view.
type MaterializedViewStatus struct {
	ViewName      string     `json:"view_name"`
	Watermark     int64      `json:"watermark"`
	SchemaVersion int        `json:"schema_version"`
	LastUpdatedAt *time.Time `json:"last_updated_at,omitempty"`
	State         string     `json:"state"`
}

// MemoryJobStatus tracks a single background job.
type MemoryJobStatus struct {
	ID             string     `json:"id"`
	JobType        string     `json:"job_type"`
	Status         string     `json:"status"` // pending, running, completed, failed
	Owner          string     `json:"owner,omitempty"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"`
	RetryCount     int        `json:"retry_count"`
	MaxRetries     int        `json:"max_retries"`
	ErrorMessage   string     `json:"error_message,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}
