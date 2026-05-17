package engine

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// RolloutSummaryMaterializer writes a per-session rollout summary file each
// time a session produces durable memory events. Files live under
// <outputDir>/rollouts/<sessionID>.md. The materializer maintains a global
// watermark so it only rebuilds files for sessions whose events advanced
// since the last pass.
//
// This complements MEMORY.md (full corpus) and Mental Models (stable layer)
// by giving humans and the LLM a per-session view that mirrors how
// oh-my-pi's transcript retainer surfaces session-scoped context.
type RolloutSummaryMaterializer struct {
	base      materializerBase
	store     EventStore
	maxKeep   int
	minEvents int
}

// NewRolloutSummaryMaterializer creates a RolloutSummaryMaterializer.
// maxKeep bounds the number of rollout files kept on disk (oldest pruned
// first); minEvents skips sessions with fewer durable events than the
// configured threshold so trivial sessions do not pollute the directory.
func NewRolloutSummaryMaterializer(db *sql.DB, store EventStore, writer *ArtifactWriter, maxKeep, minEvents int) *RolloutSummaryMaterializer {
	if maxKeep <= 0 {
		maxKeep = 200
	}
	if minEvents <= 0 {
		minEvents = 3
	}
	return &RolloutSummaryMaterializer{
		base:      newMaterializerBase(db, writer, "rollout_summary"),
		store:     store,
		maxKeep:   maxKeep,
		minEvents: minEvents,
	}
}

// ListViews implements Materializer.
func (m *RolloutSummaryMaterializer) ListViews(_ context.Context) ([]string, error) {
	return []string{"rollout_summary"}, nil
}

// Materialize implements Materializer.
func (m *RolloutSummaryMaterializer) Materialize(ctx context.Context, _ string, _ []MemoryEvent) error {
	watermark, err := m.base.getWatermark(ctx)
	if err != nil {
		return err
	}
	maxWM, err := m.store.GetMaxWatermark(ctx)
	if err != nil {
		return fmt.Errorf("getting max watermark: %w", err)
	}
	if maxWM <= watermark {
		return nil
	}

	// Only process events appended since the last watermark.
	events, err := m.store.Query(ctx, EventFilter{
		MinWatermark: watermark,
		Limit:        1000,
	})
	if err != nil {
		return fmt.Errorf("querying events for rollout summary: %w", err)
	}
	if len(events) == 0 {
		return m.base.setWatermark(ctx, maxWM)
	}

	// Group affected session IDs and rebuild each one from its full history.
	sessionSet := make(map[string]struct{}, len(events))
	for _, evt := range events {
		if evt.Source.SessionID == "" {
			continue
		}
		if !IsMaterializableEvent(evt) {
			continue
		}
		sessionSet[evt.Source.SessionID] = struct{}{}
	}

	for sessionID := range sessionSet {
		sessionEvents, err := m.store.Query(ctx, EventFilter{
			SessionID: ptrString(sessionID),
			Limit:     1000,
		})
		if err != nil {
			return fmt.Errorf("querying events for session %s: %w", sessionID, err)
		}
		filtered := make([]MemoryEvent, 0, len(sessionEvents))
		for _, evt := range sessionEvents {
			if IsMaterializableEvent(evt) {
				filtered = append(filtered, evt)
			}
		}
		if len(filtered) < m.minEvents {
			continue
		}
		content := renderRolloutSummary(sessionID, filtered)
		fileName := fmt.Sprintf("rollouts/%s.md", sanitizeFileName(sessionID))
		if err := m.base.writer.WriteFile(fileName, []byte(content)); err != nil {
			return fmt.Errorf("writing rollout summary %s: %w", fileName, err)
		}
	}

	if err := m.pruneOldRollouts(); err != nil {
		return err
	}
	return m.base.setWatermark(ctx, maxWM)
}

func (m *RolloutSummaryMaterializer) pruneOldRollouts() error {
	dir := filepath.Join(m.base.writer.OutputDir(), "rollouts")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading rollouts dir: %w", err)
	}
	type entryInfo struct {
		name    string
		modTime time.Time
	}
	files := make([]entryInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, entryInfo{name: e.Name(), modTime: info.ModTime()})
	}
	if len(files) <= m.maxKeep {
		return nil
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.Before(files[j].modTime)
	})
	excess := len(files) - m.maxKeep
	for i := 0; i < excess; i++ {
		_ = os.Remove(filepath.Join(dir, files[i].name))
	}
	return nil
}

func renderRolloutSummary(sessionID string, events []MemoryEvent) string {
	sort.SliceStable(events, func(i, j int) bool {
		return events[i].Watermark < events[j].Watermark
	})

	byKind := make(map[MemoryKind][]MemoryEvent)
	for _, evt := range events {
		byKind[evt.Kind] = append(byKind[evt.Kind], evt)
	}
	kindOrder := []MemoryKind{
		MemoryKindDecision,
		MemoryKindPreference,
		MemoryKindProcedure,
		MemoryKindPitfall,
		MemoryKindReference,
		MemoryKindTaskState,
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Session Rollout — %s\n\n", sessionID)
	fmt.Fprintf(&b, "_Events: %d · Generated: %s_\n\n", len(events), time.Now().Format(time.RFC3339))

	for _, kind := range kindOrder {
		group, ok := byKind[kind]
		if !ok || len(group) == 0 {
			continue
		}
		fmt.Fprintf(&b, "## %s\n\n", kindLabel(kind))
		for _, evt := range group {
			summary := strings.TrimSpace(evt.Summary)
			if summary == "" {
				summary = truncateContent(evt.Content, 200)
			}
			fmt.Fprintf(&b, "- %s", summary)
			if evt.Confidence > 0 {
				fmt.Fprintf(&b, " _(confidence %.0f%%)_", evt.Confidence*100)
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	return b.String()
}

func ptrString(s string) *string { return &s }
