package memory

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/crush/internal/memory/engine"
	"github.com/charmbracelet/crush/internal/memory/hindsight"
)

// HindsightBackend wraps the engine configured with a hindsight
// TranscriptRetainer and Retriever. Unlike LocalBackend, it does not run the
// extraction/consolidation pipeline per turn; instead it retains bounded
// transcript windows remotely and retrieves memories via the hindsight API.
type HindsightBackend struct {
	eng    *engine.Engine
	client *hindsight.Client
	scorer *hindsight.Retriever

	// retainTranscript is called during AfterTurn/BeforeCompaction to flush
	// the latest transcript window to the remote hindsight service. It is
	// provided by the coordinator (via SetRetainTranscript) because the
	// coordinator owns the message store and turn-counting logic.
	retainTranscript func(ctx context.Context, sessionID string)

	// buildRescueQuery builds the compaction-rescue recall query from recent
	// conversation context. Provided by the coordinator (via
	// SetRescueQueryBuilder) because it owns the message store; the query it
	// returns is already truncated according to Capabilities.TruncateRecallQuery.
	buildRescueQuery func(ctx context.Context, sessionID string) string
}

// NewHindsightBackend wraps an engine configured for hindsight as a Backend.
func NewHindsightBackend(eng *engine.Engine, client *hindsight.Client, retriever *hindsight.Retriever) *HindsightBackend {
	return &HindsightBackend{
		eng:    eng,
		client: client,
		scorer: retriever,
	}
}

// SetRetainTranscript wires the callback that retains a transcript window for
// a session. Called by coordinator.SetMemoryBackend once the coordinator's
// message store dependency is available. Without this wiring, AfterTurn and
// BeforeCompaction have no way to flush transcripts and silently no-op.
func (b *HindsightBackend) SetRetainTranscript(fn func(ctx context.Context, sessionID string)) {
	b.retainTranscript = fn
}

// SetRescueQueryBuilder wires the callback that builds the compaction-rescue
// recall query from recent conversation context. Called by
// coordinator.SetMemoryBackend alongside SetRetainTranscript.
func (b *HindsightBackend) SetRescueQueryBuilder(fn func(ctx context.Context, sessionID string) string) {
	b.buildRescueQuery = fn
}

func (b *HindsightBackend) ID() string { return "hindsight" }

func (b *HindsightBackend) Enabled() bool {
	return b.eng != nil && b.eng.Enabled()
}

func (b *HindsightBackend) Capabilities() Capabilities {
	return Capabilities{
		Triples:              false, // Hindsight has no local triple store data.
		Reflect:              true,
		Retain:               true,
		BroadRecallFallback:  false, // No broad recall fallback to avoid context pollution.
		TruncateRecallQuery:  true,  // Hindsight has a request size limit.
		MentalModels:         true,
		SessionWorkingMemory: false, // Remote retrieval already covers this; local working memory would be pure cost.
		RemoteConsolidation:  true,  // Consolidation/materialization are managed by the remote hindsight service.
	}
}

func (b *HindsightBackend) Retriever() engine.Retriever {
	if b.eng == nil {
		return nil
	}
	return b.eng.Retriever()
}

func (b *HindsightBackend) EventStore() engine.EventStore {
	if b.eng == nil {
		return nil
	}
	return b.eng.EventStore()
}

func (b *HindsightBackend) TripleStore() *engine.TripleStore {
	return nil // Hindsight has no local triple store data.
}

func (b *HindsightBackend) TranscriptRetainer() engine.TranscriptRetainer {
	if b.eng == nil {
		return nil
	}
	return b.eng.TranscriptRetainer()
}

func (b *HindsightBackend) IsDegraded() bool {
	return b.eng != nil && b.eng.IsDegraded()
}

func (b *HindsightBackend) DegradedReason() string {
	if b.eng == nil {
		return ""
	}
	return b.eng.DegradedReason()
}

func (b *HindsightBackend) AfterTurn(ctx context.Context, sessionID string) {
	if b.retainTranscript != nil {
		b.retainTranscript(ctx, sessionID)
	}
	// Mental models are refreshed via the coordinator's TTL-throttled
	// tryLoadMentalModels during prefetch (see coordinator.Run), not here.
	// An unconditional per-turn refresh used to run in this method too,
	// duplicating that work on every single turn with no throttle; it was
	// removed (see docs/refactor-memory.md Phase 5).
}

// BeforeCompaction flushes the transcript and retrieves remote memories to
// inject as a rescue payload into the compaction prompt.
func (b *HindsightBackend) BeforeCompaction(ctx context.Context, sessionID string) string {
	if b.retainTranscript != nil {
		b.retainTranscript(ctx, sessionID)
	}
	if b.Retriever() == nil {
		return ""
	}
	// The rescue query is built from recent conversation context via the
	// coordinator-provided callback (there is no "current prompt" at
	// compaction time). Passing an empty query here would silently short-
	// circuit: the hindsight retriever treats an empty query with no
	// scope/kind/tags filter as "nothing to search" and returns no results
	// without even calling the remote service (see
	// hindsight.Retriever.Retrieve / hasRecallFilters), which made this
	// rescue path dead code before buildRescueQuery was wired in.
	query := ""
	if b.buildRescueQuery != nil {
		query = b.buildRescueQuery(ctx, sessionID)
	}
	events, err := b.Retriever().Retrieve(ctx, query, map[string]any{"session_id": sessionID, "limit": 6})
	if err != nil || len(events) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("<memory_rescue>\n")
	sb.WriteString("The following remote hindsight memories should be preserved through compaction. ")
	sb.WriteString("They are ordered by relevance; copy or paraphrase them into the new summary.\n\n")
	for i, e := range events {
		fmt.Fprintf(&sb, "%d. %s\n", i+1, e.Content)
	}
	sb.WriteString("</memory_rescue>")
	return sb.String()
}

func (b *HindsightBackend) OnSessionCreated(ctx context.Context, sessionID string) error {
	if b.eng == nil {
		return nil
	}
	return b.eng.OnSessionCreated(ctx, sessionID)
}

func (b *HindsightBackend) OnSessionDeleted(ctx context.Context, sessionID string) error {
	if b.eng == nil {
		return nil
	}
	return b.eng.OnSessionDeleted(ctx, sessionID)
}

func (b *HindsightBackend) TriggerConsolidation(ctx context.Context) error {
	if b.eng == nil {
		return nil
	}
	return b.eng.TriggerConsolidation(ctx)
}

func (b *HindsightBackend) TriggerMaterialization(ctx context.Context) error {
	if b.eng == nil {
		return nil
	}
	return b.eng.TriggerMaterialization(ctx)
}

func (b *HindsightBackend) Status(ctx context.Context) (*Status, error) {
	if b.eng == nil {
		return &Status{Backend: "hindsight", Enabled: false}, nil
	}
	engineStatus, err := b.eng.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("querying engine status: %w", err)
	}
	s := &Status{
		Backend:        "hindsight",
		Enabled:        b.eng.Enabled(),
		Degraded:       engineStatus.DegradedMode != nil && engineStatus.DegradedMode.Active,
		DegradedReason: "",
	}
	if engineStatus.DegradedMode != nil {
		s.DegradedReason = engineStatus.DegradedMode.Reason
	}
	if engineStatus.ConsolidationStatus.LastRunAt != nil {
		s.LastConsolidation = engineStatus.ConsolidationStatus.LastRunAt.Unix()
	}
	return s, nil
}

// Clear wipes the local cache only (materialized views, job records, and any
// locally-cached events). The remote hindsight bank is not touched -- it must
// be managed through the remote service directly. Callers presenting this to
// the user should make that distinction clear (see the Memory: Clear
// command).
func (b *HindsightBackend) Clear(ctx context.Context) error {
	if b.eng == nil {
		return nil
	}
	return b.eng.Clear(ctx)
}

func (b *HindsightBackend) Close() error {
	if b.eng == nil {
		return nil
	}
	return b.eng.Close()
}

// Engine returns the underlying *engine.Engine for assembly-time wiring.
func (b *HindsightBackend) Engine() *engine.Engine {
	return b.eng
}

// Client returns the hindsight client (for EnsureBank during startup).
func (b *HindsightBackend) Client() *hindsight.Client {
	return b.client
}
