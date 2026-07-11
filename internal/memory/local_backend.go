package memory

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/charmbracelet/crush/internal/memory/engine"
)

// LocalBackend wraps the event-sourced engine.Engine as a Backend. The
// engine's extractor, consolidator, materializers, triple store, embedding
// pipeline, and background loops are all internal to this implementation.
type LocalBackend struct {
	eng *engine.Engine

	// compactionRecallOpts configures the compaction rescue payload. When
	// nil, no rescue is produced.
	compactionRecallOpts *engine.CompactionRescueOptions
}

// LocalBackendOption configures a LocalBackend.
type LocalBackendOption func(*LocalBackend)

// WithCompactionRecall sets the compaction rescue options for the local
// backend. When set, BeforeCompaction produces a rescue payload from the
// engine's PrepareCompactionRescue.
func WithCompactionRecall(opts engine.CompactionRescueOptions) LocalBackendOption {
	return func(b *LocalBackend) {
		b.compactionRecallOpts = &opts
	}
}

// NewLocalBackend wraps an existing *engine.Engine as a Backend.
func NewLocalBackend(eng *engine.Engine, opts ...LocalBackendOption) *LocalBackend {
	b := &LocalBackend{eng: eng}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

func (b *LocalBackend) ID() string { return "local" }

func (b *LocalBackend) Enabled() bool {
	return b.eng != nil && b.eng.Enabled()
}

func (b *LocalBackend) Capabilities() Capabilities {
	return Capabilities{
		Triples:              b.eng != nil && b.eng.TripleStore() != nil,
		Reflect:              true,
		Retain:               true,
		BroadRecallFallback:  true,
		TruncateRecallQuery:  false,
		MentalModels:         false,
		SessionWorkingMemory: true,
	}
}

func (b *LocalBackend) Retriever() engine.Retriever {
	if b.eng == nil {
		return nil
	}
	return b.eng.Retriever()
}

func (b *LocalBackend) EventStore() engine.EventStore {
	if b.eng == nil {
		return nil
	}
	return b.eng.EventStore()
}

func (b *LocalBackend) TripleStore() *engine.TripleStore {
	if b.eng == nil {
		return nil
	}
	return b.eng.TripleStore()
}

func (b *LocalBackend) TranscriptRetainer() engine.TranscriptRetainer {
	if b.eng == nil {
		return nil
	}
	return b.eng.TranscriptRetainer()
}

func (b *LocalBackend) IsDegraded() bool {
	return b.eng != nil && b.eng.IsDegraded()
}

func (b *LocalBackend) DegradedReason() string {
	if b.eng == nil {
		return ""
	}
	return b.eng.DegradedReason()
}

func (b *LocalBackend) AfterTurn(ctx context.Context, sessionID string) {
	if b.eng == nil || !b.eng.Enabled() {
		return
	}
	go func() {
		if err := b.eng.AfterTurnIdle(context.Background(), sessionID, nil); err != nil {
			slog.Warn("Memory AfterTurnIdle failed", "error", err, "session_id", sessionID)
		}
	}()
}

func (b *LocalBackend) BeforeCompaction(ctx context.Context, sessionID string) string {
	if b.eng == nil {
		return ""
	}
	if err := b.eng.OnBeforeCompaction(ctx, sessionID); err != nil {
		slog.Warn("Memory engine OnBeforeCompaction failed", "error", err, "session_id", sessionID)
	}
	if b.compactionRecallOpts == nil {
		return ""
	}
	rescue, err := b.eng.PrepareCompactionRescue(ctx, sessionID, *b.compactionRecallOpts)
	if err != nil {
		slog.Warn("Compaction rescue preparation failed", "error", err, "session_id", sessionID)
		return ""
	}
	if rescue == nil {
		return ""
	}
	return rescue.Rendered
}

func (b *LocalBackend) OnSessionCreated(ctx context.Context, sessionID string) error {
	if b.eng == nil {
		return nil
	}
	return b.eng.OnSessionCreated(ctx, sessionID)
}

func (b *LocalBackend) OnSessionDeleted(ctx context.Context, sessionID string) error {
	if b.eng == nil {
		return nil
	}
	return b.eng.OnSessionDeleted(ctx, sessionID)
}

func (b *LocalBackend) TriggerConsolidation(ctx context.Context) error {
	if b.eng == nil {
		return nil
	}
	return b.eng.TriggerConsolidation(ctx)
}

func (b *LocalBackend) TriggerMaterialization(ctx context.Context) error {
	if b.eng == nil {
		return nil
	}
	return b.eng.TriggerMaterialization(ctx)
}

func (b *LocalBackend) Status(ctx context.Context) (*Status, error) {
	if b.eng == nil {
		return &Status{Backend: "local", Enabled: false}, nil
	}
	engineStatus, err := b.eng.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("querying engine status: %w", err)
	}
	s := &Status{
		Backend:           "local",
		Enabled:           b.eng.Enabled(),
		Degraded:          engineStatus.DegradedMode != nil && engineStatus.DegradedMode.Active,
		DegradedReason:    "",
		LastConsolidation: 0,
	}
	if engineStatus.DegradedMode != nil {
		s.DegradedReason = engineStatus.DegradedMode.Reason
	}
	if engineStatus.ConsolidationStatus.LastRunAt != nil {
		s.LastConsolidation = engineStatus.ConsolidationStatus.LastRunAt.Unix()
	}
	// Count events. Uses Count (SQL COUNT(*)) rather than Query so the
	// number is not capped at Query's default/max row limit and no rows are
	// materialized just to be counted.
	if b.eng.EventStore() != nil {
		count, err := b.eng.EventStore().Count(ctx, engine.EventFilter{})
		if err == nil {
			s.EventCount = count
		}
	}
	return s, nil
}

func (b *LocalBackend) Clear(ctx context.Context) error {
	if b.eng == nil {
		return nil
	}
	return b.eng.Clear(ctx)
}

func (b *LocalBackend) Close() error {
	if b.eng == nil {
		return nil
	}
	return b.eng.Close()
}

// Engine returns the underlying *engine.Engine. This is used by the factory
// during assembly to wire extractors, consolidators, and materializers
// before the backend is handed to the coordinator.
func (b *LocalBackend) Engine() *engine.Engine {
	return b.eng
}
