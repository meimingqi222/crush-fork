package engine

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// materializerBase provides shared watermark tracking for concrete materializers.
// Each materializer embeds this to record how far through the event log it has
// processed, enabling incremental rebuild.
type materializerBase struct {
	db       *sql.DB
	writer   *ArtifactWriter
	viewName string
}

func newMaterializerBase(db *sql.DB, writer *ArtifactWriter, viewName string) materializerBase {
	return materializerBase{
		db:       db,
		writer:   writer,
		viewName: viewName,
	}
}

// getWatermark returns the current watermark for this view, or 0 if no view
// entry exists yet.
func (b *materializerBase) getWatermark(ctx context.Context) (int64, error) {
	var watermark int64
	err := b.db.QueryRowContext(ctx,
		"SELECT COALESCE(watermark, 0) FROM memory_materialized_views WHERE view_name = ?",
		b.viewName).Scan(&watermark)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading watermark for view %s: %w", b.viewName, err)
	}
	return watermark, nil
}

// setWatermark updates or inserts the watermark for this view.
func (b *materializerBase) setWatermark(ctx context.Context, watermark int64) error {
	now := time.Now().Unix()
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO memory_materialized_views (id, view_name, watermark, schema_version, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(view_name) DO UPDATE SET
			watermark = excluded.watermark,
			updated_at = excluded.updated_at`,
		"mvs-"+b.viewName,
		b.viewName,
		watermark,
		1,
		now,
		now,
	)
	if err != nil {
		return fmt.Errorf("updating watermark for view %s: %w", b.viewName, err)
	}
	return nil
}
