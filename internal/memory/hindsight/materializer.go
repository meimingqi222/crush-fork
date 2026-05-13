package hindsight

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/charmbracelet/crush/internal/memory/engine"
)

const hindsightViewName = "hindsight_replicate"

// Materializer implements engine.Materializer by replicating new
// MemoryEvents to the remote Hindsight service via the retain endpoint.
// It uses watermark tracking to avoid re-sending already-replicated events.
//
// Only project-, user-, and global-scoped events with confidence >= 0.5 are
// sent; session-scoped and working-memory events are skipped.
type Materializer struct {
	client *Client
	db     *sql.DB
	store  engine.EventStore
}

// NewMaterializer creates a Materializer that replicates events via client.
func NewMaterializer(client *Client, db *sql.DB, store engine.EventStore) *Materializer {
	return &Materializer{
		client: client,
		db:     db,
		store:  store,
	}
}

// ListViews implements engine.Materializer.
func (m *Materializer) ListViews(_ context.Context) ([]string, error) {
	return []string{hindsightViewName}, nil
}

// Materialize implements engine.Materializer. It queries events above the
// current replication watermark and sends them to Hindsight via retain.
func (m *Materializer) Materialize(ctx context.Context, _ string, _ []engine.MemoryEvent) error {
	watermark, err := m.getWatermark(ctx)
	if err != nil {
		return err
	}

	maxWM, err := m.store.GetMaxWatermark(ctx)
	if err != nil {
		return fmt.Errorf("hindsight materializer: getting max watermark: %w", err)
	}
	if maxWM <= watermark {
		return nil
	}

	events, err := m.store.Query(ctx, engine.EventFilter{
		MinWatermark: watermark,
		Limit:        200,
	})
	if err != nil {
		return fmt.Errorf("hindsight materializer: querying events: %w", err)
	}

	items := make([]RetainItem, 0, len(events))
	var newWatermark int64
	for _, evt := range events {
		if evt.Watermark > newWatermark {
			newWatermark = evt.Watermark
		}
		if !shouldReplicate(evt) {
			continue
		}
		content := evt.Content
		if evt.Summary != "" {
			content = evt.Summary + "\n\n" + evt.Content
		}
		item := RetainItem{
			Content:    content,
			Context:    "crush",
			DocumentID: evt.ID,
			Tags:       eventTags(evt),
			Metadata: map[string]string{
				"event_id":   evt.ID,
				"scope":      string(evt.Scope),
				"kind":       string(evt.Kind),
				"session_id": evt.Source.SessionID,
			},
			Async: true,
		}
		items = append(items, item)
	}

	if len(items) > 0 {
		if err := m.client.Retain(ctx, items); err != nil {
			// Log and continue; replication failure must not block local pipeline.
			slog.Warn("Hindsight retain failed", "error", err, "items", len(items))
			return nil
		}
		slog.Debug("Hindsight retain complete", "items", len(items))
	}

	if newWatermark > watermark {
		if err := m.setWatermark(ctx, newWatermark); err != nil {
			return err
		}
	}
	return nil
}

func shouldReplicate(evt engine.MemoryEvent) bool {
	if evt.Scope == engine.MemoryScopeSession {
		return false
	}
	if evt.Kind == engine.MemoryKindWorkingMemory || evt.Kind == engine.MemoryKindTaskState {
		return false
	}
	return evt.Confidence >= 0.5
}

func eventTags(evt engine.MemoryEvent) []string {
	tags := []string{
		"scope:" + string(evt.Scope),
		"kind:" + string(evt.Kind),
	}
	if evt.Source.SessionID != "" {
		tags = append(tags, "session:"+evt.Source.SessionID)
	}
	tags = append(tags, evt.Tags...)
	return tags
}

func (m *Materializer) getWatermark(ctx context.Context) (int64, error) {
	var wm int64
	err := m.db.QueryRowContext(ctx,
		"SELECT COALESCE(watermark, 0) FROM memory_materialized_views WHERE view_name = ?",
		hindsightViewName).Scan(&wm)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("hindsight materializer: reading watermark: %w", err)
	}
	return wm, nil
}

func (m *Materializer) setWatermark(ctx context.Context, watermark int64) error {
	now := time.Now().Unix()
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO memory_materialized_views (id, view_name, watermark, schema_version, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(view_name) DO UPDATE SET
			watermark = excluded.watermark,
			updated_at = excluded.updated_at`,
		"mvs-"+hindsightViewName,
		hindsightViewName,
		watermark,
		1,
		now,
		now,
	)
	if err != nil {
		return fmt.Errorf("hindsight materializer: setting watermark: %w", err)
	}
	return nil
}
