package engine

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// MemoryMDMaterializer generates a full human-readable MEMORY.md from all
// consolidated memory events. This is the "long-term memory" document that a
// human (or an LLM reviewing context) can read to understand everything the
// system remembers.
//
// Output: <outputDir>/MEMORY.md
//
// Format: Sections organized by MemoryScope, then by MemoryKind. Each event is
// rendered with its full content, summary, confidence, and provenance. This is
// the canonical human-facing memory document.
type MemoryMDMaterializer struct {
	base  materializerBase
	store EventStore
}

// NewMemoryMDMaterializer creates a MemoryMDMaterializer.
func NewMemoryMDMaterializer(db *sql.DB, store EventStore, writer *ArtifactWriter) *MemoryMDMaterializer {
	return &MemoryMDMaterializer{
		base:  newMaterializerBase(db, writer, "MEMORY"),
		store: store,
	}
}

// Materialize implements Materializer.
func (m *MemoryMDMaterializer) Materialize(ctx context.Context, viewName string, _ []MemoryEvent) error {
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
		MinWatermark: watermark,
		Limit:        1000,
	})
	if err != nil {
		return fmt.Errorf("querying events for MEMORY.md: %w", err)
	}
	if len(events) == 0 {
		return nil
	}

	content := m.renderMemoryMD(events)

	if err := m.base.writer.WriteFile("MEMORY.md", []byte(content)); err != nil {
		return fmt.Errorf("writing MEMORY.md: %w", err)
	}

	if err := m.base.setWatermark(ctx, maxWM); err != nil {
		return err
	}

	return nil
}

// ListViews implements Materializer.
func (m *MemoryMDMaterializer) ListViews(_ context.Context) ([]string, error) {
	return []string{"MEMORY"}, nil
}

func (m *MemoryMDMaterializer) renderMemoryMD(events []MemoryEvent) string {
	scopeOrder := []MemoryScope{
		MemoryScopeProject,
		MemoryScopeUser,
		MemoryScopeSession,
		MemoryScopeGlobal,
	}

	kindOrder := []MemoryKind{
		MemoryKindDecision,
		MemoryKindPreference,
		MemoryKindProcedure,
		MemoryKindPitfall,
		MemoryKindReference,
		MemoryKindTaskState,
		MemoryKindWorkingMemory,
	}

	// Group by scope, then by kind.
	type scopeGroup map[MemoryKind][]MemoryEvent
	byScope := make(map[MemoryScope]scopeGroup)

	for _, evt := range events {
		if byScope[evt.Scope] == nil {
			byScope[evt.Scope] = make(scopeGroup)
		}
		byScope[evt.Scope][evt.Kind] = append(byScope[evt.Scope][evt.Kind], evt)
	}

	var b strings.Builder
	b.WriteString("# MEMORY.md — Long-Term Memory\n\n")
	b.WriteString(fmt.Sprintf("> Generated from %d events\n>\n", len(events)))
	b.WriteString(fmt.Sprintf("> Last updated: %s\n\n", time.Now().Format(time.RFC3339)))

	for _, scope := range scopeOrder {
		group, ok := byScope[scope]
		if !ok {
			continue
		}

		b.WriteString(fmt.Sprintf("## %s\n\n", scopeLabel(scope)))

		for _, kind := range kindOrder {
			events, ok := group[kind]
			if !ok {
				continue
			}

			b.WriteString(fmt.Sprintf("### %s (%d)\n\n", kindLabel(kind), len(events)))

			// Sort by importance descending within each group.
			sort.Slice(events, func(i, j int) bool {
				return events[i].Importance > events[j].Importance
			})

			for _, evt := range events {
				b.WriteString(fmt.Sprintf("#### %s\n\n", evt.Summary))

				if evt.Content != "" {
					b.WriteString(evt.Content)
					b.WriteString("\n\n")
				}

				b.WriteString(fmt.Sprintf(
					"- **Confidence**: %.0f%%\n", evt.Confidence*100))
				b.WriteString(fmt.Sprintf(
					"- **Importance**: %.0f%%\n", evt.Importance*100))

				if evt.CreatedAt != evt.UpdatedAt {
					b.WriteString(fmt.Sprintf(
						"- **Updated**: %s\n", evt.UpdatedAt.Format(time.RFC3339)))
				}

				if len(evt.Tags) > 0 {
					b.WriteString(fmt.Sprintf(
						"- **Tags**: %s\n", strings.Join(evt.Tags, ", ")))
				}

				if evt.Supersedes != nil {
					b.WriteString(fmt.Sprintf(
						"- **Supersedes**: `%s`\n", *evt.Supersedes))
				}

				if evt.Source.SessionID != "" {
					b.WriteString(fmt.Sprintf(
						"- **Session**: `%s`\n", evt.Source.SessionID))
				}

				// Watermark as a comment for debugging.
				b.WriteString(fmt.Sprintf(
					"- <small>watermark: %d</small>\n", evt.Watermark))

				b.WriteString("\n")
			}
		}
	}

	b.WriteString("---\n")
	b.WriteString(fmt.Sprintf("_Watermark: %d_\n", events[len(events)-1].Watermark))

	return b.String()
}

func scopeLabel(s MemoryScope) string {
	switch s {
	case MemoryScopeSession:
		return "Session"
	case MemoryScopeProject:
		return "Project"
	case MemoryScopeUser:
		return "User"
	case MemoryScopeGlobal:
		return "Global"
	default:
		return string(s)
	}
}
