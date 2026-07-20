// Package sessionevent provides ordered, replayable in-memory events for live
// desktop session projections.
package sessionevent

import "time"

// Kind identifies a session event payload.
type Kind string

const (
	KindSessionUpdated      Kind = "session.updated"
	KindTurnStarted         Kind = "turn.started"
	KindTurnCompleted       Kind = "turn.completed"
	KindTurnFailed          Kind = "turn.failed"
	KindTurnCancelled       Kind = "turn.cancelled"
	KindTurnSteered         Kind = "turn.steered"
	// KindTurnProgress is a best-effort heartbeat while a turn is still active
	// but producing no message/tool deltas (e.g. provider stream retry delay).
	KindTurnProgress       Kind = "turn.progress"
	KindCancelAcknowledged Kind = "cancel.acknowledged"
	KindMessageDelta        Kind = "message.delta"
	KindMessageCreated      Kind = "message.created"
	KindMessageCompleted    Kind = "message.completed"
	KindMessageReset        Kind = "message.reset"
	KindReasoningDelta      Kind = "reasoning.delta"
	KindToolProgress        Kind = "tool.progress"
	KindToolCompleted       Kind = "tool.completed"
	KindPermissionRequested Kind = "permission.requested"
	KindUsageUpdated        Kind = "usage.updated"
	KindQueueUpdated        Kind = "queue.updated"
	KindTerminalOutput      Kind = "terminal.output"
	KindTerminalExited      Kind = "terminal.exited"
	KindMCPStatus           Kind = "mcp.status"
	KindSnapshotRequired    Kind = "snapshot.required"
)

// DeliveryClass defines how a subscriber queue handles an event under
// backpressure.
type DeliveryClass uint8

const (
	// DeliveryReliable events are retained until delivered or until the
	// subscription explicitly transitions to snapshot recovery.
	DeliveryReliable DeliveryClass = iota
	// DeliveryMerge combines adjacent compatible deltas.
	DeliveryMerge
	// DeliveryLatest keeps only the newest state for a coalesce key.
	DeliveryLatest
)

// Event is the immutable envelope stored in a session journal. FirstSequence
// equals Sequence for normal events. Subscriber-specific merged events cover
// the inclusive FirstSequence..Sequence range.
type Event struct {
	SessionID       string
	FirstSequence   uint64
	Sequence        uint64
	SessionRevision uint64
	EventID         string
	Timestamp       time.Time
	Kind            Kind
	Payload         any
	Delivery        DeliveryClass
	CoalesceKey     string
	// MergedCount is internal batching metadata. Normal journal events use 1.
	MergedCount uint16
}

// TextDelta is a mergeable message or reasoning payload.
type TextDelta struct {
	MessageID string
	PartID    string
	Text      string
}

// TerminalOutput is mergeable only when offsets are contiguous.
type TerminalOutput struct {
	TerminalID string
	Offset     uint64
	Data       []byte
}

type TerminalExit struct {
	TerminalID string
	State      string
	Code       int
	Signal     string
	Timestamp  int64
	Offset     uint64
}

type MCPStatus struct {
	ServerID  string
	Name      string
	Scope     string
	Status    string
	Tools     int
	Prompts   int
	Resources int
	Revision  uint64
	ErrorCode string
}

// SnapshotRequired explains why a subscription must rebuild its projection.
type SnapshotRequired struct {
	Reason string
}

// MessageEvent identifies a live assistant message state transition.
type MessageEvent struct {
	MessageID    string
	FinishReason string
}

// ToolEvent is a bounded live projection of tool lifecycle state.
type ToolEvent struct {
	MessageID  string
	ToolCallID string
	Name       string
	Status     string
	Input      string
	Result     string
	IsError    bool
	Truncated  bool
	Files      []ToolFile
}

// ToolFile preserves the client-owned source identity observed by a file
// tool without exposing file content or arbitrary tool metadata.
type ToolFile struct {
	Path      string
	SourceURI string
	Revision  string
}

// TurnEvent identifies a turn terminal, cancellation, or progress state.
type TurnEvent struct {
	TurnID    string
	MessageID string
	Reason    string
	// Phase is optional public progress detail (e.g. "provider_retry").
	Phase string
}

type QueueEvent struct {
	Revision uint64
	Turns    []QueuedTurn
}

type QueuedTurn struct {
	TurnID   string
	Status   string
	Position int
	Preview  string
}

// UsageEvent carries normalized usage without provider-specific metadata.
type UsageEvent struct {
	InputTokens      int64
	OutputTokens     int64
	ReasoningTokens  int64
	CacheReadTokens  int64
	CacheWriteTokens int64
}

// NewEvent contains caller-supplied fields for publication. The hub assigns
// identity, timestamp, and sequence fields. Payload values must be immutable
// after Publish returns; TerminalOutput bytes are defensively copied.
type NewEvent struct {
	SessionRevision uint64
	AdvanceRevision bool
	Kind            Kind
	Payload         any
	Delivery        DeliveryClass
	CoalesceKey     string
}
