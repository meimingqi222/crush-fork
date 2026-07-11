// Package memory provides the Backend abstraction for the memory system,
// decoupling the coordinator and agent from the concrete engine
// implementation. There are two production backends: local (the
// event-sourced engine pipeline) and hindsight (a remote transcript
// retention service). An off/disabled state is represented by a nil
// Backend.
package memory

import (
	"context"

	"github.com/charmbracelet/crush/internal/memory/engine"
)

// Capabilities describes which memory features a Backend supports. Callers
// use this to gate tool registration and behavior instead of comparing
// backend name strings.
type Capabilities struct {
	// Triples indicates the backend has a knowledge-graph triple store,
	// enabling graph/triple query tools.
	Triples bool
	// Reflect indicates the backend supports cross-session reflection
	// (synthesis over past events).
	Reflect bool
	// Retain indicates the backend accepts manual retain writes.
	Retain bool
	// BroadRecallFallback indicates that when a targeted Retrieve query
	// returns no results, a broad Recall fallback is permitted. Hindsight
	// disables this to avoid polluting context with irrelevant results.
	BroadRecallFallback bool
	// TruncateRecallQuery indicates that recall queries should be
	// hard-truncated before being sent to the backend (hindsight has a
	// request size limit).
	TruncateRecallQuery bool
	// MentalModels indicates the backend provides mental-models snippets.
	MentalModels bool
	// SessionWorkingMemory indicates the backend benefits from the session
	// agent's local working-memory generation on compaction. Backends that
	// retrieve memories from a remote service (e.g. hindsight) already have
	// their own recall path, so generating local working memory there is
	// pure cost with no benefit.
	SessionWorkingMemory bool
	// RemoteConsolidation indicates that TriggerConsolidation/
	// TriggerMaterialization are managed by a remote service and are no-ops
	// when called locally (hindsight). Callers of the user-facing "Memory:
	// Consolidate Now" command use this to avoid reporting success for an
	// action that had no effect.
	RemoteConsolidation bool
}

// Status describes the current state of the memory backend for user-facing
// display.
type Status struct {
	// Backend is the backend identifier ("local" | "hindsight").
	Backend string
	// Enabled reports whether the backend is active.
	Enabled bool
	// Degraded reports whether the backend is in a degraded state.
	Degraded bool
	// DegradedReason explains why the backend is degraded, if applicable.
	DegradedReason string
	// EventCount is the total number of stored memory events.
	EventCount int64
	// LastConsolidation is the Unix timestamp of the last consolidation
	// pass, or 0 if none has run.
	LastConsolidation int64
}

// Backend is the memory system abstraction consumed by the coordinator and
// agent. It replaces the former direct *engine.Engine dependency and the
// string-literal backend dispatch ("hindsight" / "local") scattered across
// the coordinator.
type Backend interface {
	// ID returns the backend identifier ("local" | "hindsight").
	ID() string

	// Enabled reports whether the backend is active.
	Enabled() bool

	// Capabilities returns the feature set supported by this backend.
	Capabilities() Capabilities

	// Retriever returns the recall/reflect/auto-recall data source, or nil
	// if the backend does not provide one.
	Retriever() engine.Retriever

	// EventStore returns the write-ahead event log, or nil if the backend
	// does not provide one (e.g. hindsight retains transcripts remotely).
	EventStore() engine.EventStore

	// TripleStore returns the knowledge-graph triple store, or nil if the
	// backend does not support triples.
	TripleStore() *engine.TripleStore

	// TranscriptRetainer returns the transcript retainer for backends that
	// retain bounded raw transcript windows, or nil.
	TranscriptRetainer() engine.TranscriptRetainer

	// IsDegraded reports whether the backend is in a degraded state.
	IsDegraded() bool

	// DegradedReason returns a human-readable reason when degraded.
	DegradedReason() string

	// AfterTurn is called after each successful LLM turn. The implementation
	// decides whether to extract, embed, link, or materialize.
	AfterTurn(ctx context.Context, sessionID string)

	// BeforeCompaction returns rescue text to inject into the compaction
	// prompt so durable memories survive summarization. Returns "" when no
	// rescue is needed.
	BeforeCompaction(ctx context.Context, sessionID string) string

	// OnSessionCreated is called when a new session is created.
	OnSessionCreated(ctx context.Context, sessionID string) error

	// OnSessionDeleted is called when a session is explicitly deleted.
	OnSessionDeleted(ctx context.Context, sessionID string) error

	// TriggerConsolidation triggers a consolidation pass.
	TriggerConsolidation(ctx context.Context) error

	// TriggerMaterialization triggers a materialization pass.
	TriggerMaterialization(ctx context.Context) error

	// Status returns the current backend status for user-facing display.
	Status(ctx context.Context) (*Status, error)

	// Clear deletes all persisted memory state (events, materialized views,
	// triples, background job records). Intended for the user-facing
	// "Memory: Clear" command after explicit confirmation. Hindsight only
	// clears its local cache -- the remote bank is unaffected and must be
	// managed separately.
	Clear(ctx context.Context) error

	// Close releases resources.
	Close() error
}

// MentalModelsSnippet returns the mental-models snippet from the backend's
// retriever if it implements engine.MentalModelsProvider, or "" otherwise.
func MentalModelsSnippet(b Backend) string {
	if b == nil || b.Retriever() == nil {
		return ""
	}
	if mmp, ok := b.Retriever().(engine.MentalModelsProvider); ok {
		return mmp.MentalModelsSnippet()
	}
	return ""
}

// LoadMentalModels loads mental models into the backend's retriever if it
// supports them. Returns false if the backend does not support mental models.
func LoadMentalModels(ctx context.Context, b Backend) bool {
	if b == nil || b.Retriever() == nil {
		return false
	}
	mmp, ok := b.Retriever().(engine.MentalModelsProvider)
	if !ok {
		return false
	}
	if err := mmp.LoadMentalModels(ctx); err != nil {
		return false
	}
	return true
}
