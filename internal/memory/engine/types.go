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
// Each kind has dedicated Weibull decay parameters tuned to its expected
// retention horizon, inspired by cognitive psychology and the Mnemopi
// multi-type memory taxonomy.
type MemoryKind string

const (
	// Long-lived stable knowledge (months to years).
	MemoryKindProfile    MemoryKind = "profile"     // User/team profile and identity
	MemoryKindIdentity   MemoryKind = "identity"    // Identity information
	MemoryKindPreference MemoryKind = "preference"  // User preferences and style
	MemoryKindCorrection MemoryKind = "correction"  // Explicit corrections or do/don't rules
	MemoryKindConstraint MemoryKind = "constraint"  // Hard constraints and requirements
	MemoryKindTeam       MemoryKind = "team"        // Team composition and conventions

	// Medium-term knowledge (weeks to months).
	MemoryKindSkill     MemoryKind = "skill"     // Acquired skills and capabilities
	MemoryKindProject   MemoryKind = "project"   // Project context and structure
	MemoryKindProcedure MemoryKind = "procedure" // Workflows and procedures
	MemoryKindPitfall   MemoryKind = "pitfall"   // Pitfalls, gotchas, anti-patterns
	MemoryKindLesson    MemoryKind = "lesson"    // Lessons learned
	MemoryKindPattern   MemoryKind = "pattern"   // Recurring patterns observed

	// Shorter-term knowledge (days to weeks).
	MemoryKindFact      MemoryKind = "fact"      // Isolated factual knowledge
	MemoryKindReference MemoryKind = "reference" // Reference material lookups
	MemoryKindDecision  MemoryKind = "decision"  // Architectural/design decisions
	MemoryKindApproach  MemoryKind = "approach"  // Approach taken for a problem
	MemoryKindAttempt   MemoryKind = "attempt"   // Attempted solutions
	MemoryKindOutcome   MemoryKind = "outcome"   // Outcomes of actions
	MemoryKindContext   MemoryKind = "context"   // Situational context
	MemoryKindEvent     MemoryKind = "event"     // Notable events
	MemoryKindConversation MemoryKind = "conversation" // Conversation segments

	// Transient state (hours to days).
	MemoryKindRequest       MemoryKind = "request"        // User requests
	MemoryKindWorkingMemory MemoryKind = "working_memory" // Session working memory
	MemoryKindTaskState     MemoryKind = "task_state"     // Active task state
)

// MemoryVeracity defines how a memory fact was established.  Higher-weight
// veracity labels produce larger Bayesian confidence updates during
// consolidation.
type MemoryVeracity string

const (
	// MemoryVeracityStated means the user explicitly stated this fact.
	MemoryVeracityStated MemoryVeracity = "stated"
	// MemoryVeracityInferred means the LLM inferred this from context.
	MemoryVeracityInferred MemoryVeracity = "inferred"
	// MemoryVeracityTool means a tool output confirmed this fact.
	MemoryVeracityTool MemoryVeracity = "tool"
	// MemoryVeracityImported means this fact was imported from an external source.
	MemoryVeracityImported MemoryVeracity = "imported"
	// MemoryVeracityUnknown means the source is unclear.
	MemoryVeracityUnknown MemoryVeracity = "unknown"
	// MemoryVeracityFalse means the fact has been contradicted or superseded.
	MemoryVeracityFalse MemoryVeracity = "false"
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
	Veracity   MemoryVeracity  `json:"veracity,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
	Supersedes *string         `json:"supersedes,omitempty"`
	Tags       []string        `json:"tags,omitempty"`
	Watermark  int64           `json:"watermark"`
	ExpiresAt  *time.Time      `json:"expires_at,omitempty"`
	Triples    []ExtractedTriple `json:"triples,omitempty"`
}

// FilterLatestNonSuperseded returns the most recent event that has not been
// superseded by another event in the slice. The slice must be ordered by
// watermark (ascending). Returns nil if no valid event remains.
func FilterLatestNonSuperseded(events []MemoryEvent) *MemoryEvent {
	superseded := make(map[string]bool, len(events))
	for _, evt := range events {
		if evt.Supersedes != nil {
			superseded[*evt.Supersedes] = true
		}
	}
	for i := len(events) - 1; i >= 0; i-- {
		if !superseded[events[i].ID] {
			return &events[i]
		}
	}
	return nil
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
