package engine

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// SummaryMaterializer generates a compact, prompt-friendly summary of all
// consolidated memory events. The output is designed to be injected into LLM
// context at the start of each turn, providing quick awareness of past
// decisions, preferences, and project knowledge.
//
// Output: <outputDir>/memory_summary.md
//
// Format: Top N most important events, grouped by kind, with one-line
// summaries. Kept short enough to fit in a prompt prefix.
type SummaryMaterializer struct {
	base      materializerBase
	store     EventStore
	maxEvents int
}

// NewSummaryMaterializer creates a SummaryMaterializer.
//
//   - db: SQLite database for watermark tracking
//   - store: EventStore for reading events
//   - writer: ArtifactWriter for file output
func NewSummaryMaterializer(db *sql.DB, store EventStore, writer *ArtifactWriter) *SummaryMaterializer {
	return &SummaryMaterializer{
		base:      newMaterializerBase(db, writer, "memory_summary"),
		store:     store,
		maxEvents: 50,
	}
}

// Materialize implements Materializer. It reads all consolidated events up to
// the current max watermark, renders a compact summary, writes
// memory_summary.md, and advances the view watermark.
func (m *SummaryMaterializer) Materialize(ctx context.Context, viewName string, _ []MemoryEvent) error {
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

	events, err := m.store.Query(ctx, EventFilter{
		Limit: 1000,
	})
	if err != nil {
		return fmt.Errorf("querying events for summary: %w", err)
	}

	content := m.renderSummary(events)

	if err := m.base.writer.WriteFile("memory_summary.md", []byte(content)); err != nil {
		return fmt.Errorf("writing memory_summary.md: %w", err)
	}

	if err := m.base.setWatermark(ctx, maxWM); err != nil {
		return err
	}

	return nil
}

// ListViews implements Materializer.
func (m *SummaryMaterializer) ListViews(_ context.Context) ([]string, error) {
	return []string{"memory_summary"}, nil
}

func (m *SummaryMaterializer) renderSummary(events []MemoryEvent) string {
	filtered := make([]MemoryEvent, 0, len(events))
	for _, evt := range events {
		if !IsMaterializableEvent(evt) {
			continue
		}
		filtered = append(filtered, evt)
	}

	if len(filtered) == 0 {
		return "# Memory Summary\n\n> No durable memory events yet.\n"
	}

	// Sort by importance descending, take top N.
	sorted := make([]MemoryEvent, len(filtered))
	copy(sorted, filtered)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Importance > sorted[j].Importance
	})
	if len(sorted) > m.maxEvents {
		sorted = sorted[:m.maxEvents]
	}

	// Group by kind.
	byKind := make(map[MemoryKind][]MemoryEvent)
	for _, evt := range sorted {
		byKind[evt.Kind] = append(byKind[evt.Kind], evt)
	}

	// Render groups in a consistent order.
	kindOrder := []MemoryKind{
		MemoryKindDecision,
		MemoryKindPreference,
		MemoryKindProcedure,
		MemoryKindPitfall,
		MemoryKindReference,
		MemoryKindTaskState,
		MemoryKindWorkingMemory,
	}

	var b strings.Builder
	b.WriteString("# Memory Summary\n\n")
	b.WriteString(fmt.Sprintf("> %d events · most important first\n\n", len(sorted)))

	for _, kind := range kindOrder {
		events, ok := byKind[kind]
		if !ok {
			continue
		}
		b.WriteString(fmt.Sprintf("## %s\n\n", kindLabel(kind)))
		for _, evt := range events {
			b.WriteString(fmt.Sprintf("- %s", evt.Summary))
			if evt.Confidence > 0 {
				b.WriteString(fmt.Sprintf(" [%.0f%%]", evt.Confidence*100))
			}
			b.WriteString(fmt.Sprintf(" (%s)", evt.Scope))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	b.WriteString("---\n")
	b.WriteString(fmt.Sprintf("_Last updated: watermark %d_\n", maxWatermark(events)))

	return b.String()
}

func kindLabel(k MemoryKind) string {
	switch k {
	case MemoryKindDecision:
		return "Decisions"
	case MemoryKindPreference:
		return "Preferences"
	case MemoryKindProcedure:
		return "Procedures"
	case MemoryKindPitfall:
		return "Pitfalls & Gotchas"
	case MemoryKindReference:
		return "References"
	case MemoryKindTaskState:
		return "Task State"
	case MemoryKindWorkingMemory:
		return "Working Memory"
	default:
		return string(k)
	}
}
