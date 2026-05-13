package engine

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// SkillsMaterializer generates executable SKILL.md files from procedural
// memory events. Each procedural event is rendered in a standard skill format
// with trigger description, steps, and context. Skills are only produced when
// the procedural memories are sufficiently stable (minimum event threshold and
// minimum confidence).
//
// Output: <outputDir>/skills/SKILL.md
type SkillsMaterializer struct {
	base          materializerBase
	store         EventStore
	minEvents     int
	minConfidence float64
}

// NewSkillsMaterializer creates a SkillsMaterializer.
//
// Parameters:
//   - db: SQLite database for watermark tracking
//   - store: EventStore for reading events
//   - writer: ArtifactWriter for file output
func NewSkillsMaterializer(db *sql.DB, store EventStore, writer *ArtifactWriter) *SkillsMaterializer {
	return &SkillsMaterializer{
		base:          newMaterializerBase(db, writer, "skills"),
		store:         store,
		minEvents:     3,
		minConfidence: 0.6,
	}
}

// Materialize implements Materializer. It reads procedure-kind events, and
// only writes skills/SKILL.md when there are enough stable events.
func (m *SkillsMaterializer) Materialize(ctx context.Context, viewName string, _ []MemoryEvent) error {
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

	procKind := MemoryKindProcedure
	events, err := m.store.Query(ctx, EventFilter{
		Kind:  &procKind,
		Limit: 500,
	})
	if err != nil {
		return fmt.Errorf("querying procedure events: %w", err)
	}
	if len(events) == 0 {
		return nil
	}

	content := m.renderSkills(events)

	if err := m.base.writer.WriteFile("skills/SKILL.md", []byte(content)); err != nil {
		return fmt.Errorf("writing skills/SKILL.md: %w", err)
	}

	if err := m.base.setWatermark(ctx, maxWM); err != nil {
		return err
	}

	return nil
}

// ListViews implements Materializer.
func (m *SkillsMaterializer) ListViews(_ context.Context) ([]string, error) {
	return []string{"skills"}, nil
}

func (m *SkillsMaterializer) renderSkills(events []MemoryEvent) string {
	// Filter by confidence stability.
	var stable []MemoryEvent
	for _, evt := range events {
		if evt.Scope != MemoryScopeSession && evt.Confidence >= m.minConfidence {
			stable = append(stable, evt)
		}
	}

	var b strings.Builder
	b.WriteString("# Skills — Procedural Memory\n\n")

	if len(stable) < m.minEvents {
		b.WriteString(fmt.Sprintf(
			"> ⚠ Only %d stable procedures found (need %d). This file is primarily a\n",
			len(stable), m.minEvents))
		b.WriteString("> placeholder. As more procedural memories are consolidated, individual\n")
		b.WriteString("> skill files will appear here.\n\n")

		// Still list the procedures we have, even if below threshold.
		if len(stable) > 0 {
			b.WriteString("## Developing Procedures\n\n")
			for _, evt := range stable {
				b.WriteString(fmt.Sprintf("- **%s**", evt.Summary))
				if evt.Confidence > 0 {
					b.WriteString(fmt.Sprintf(" (confidence: %.0f%%)", evt.Confidence*100))
				}
				b.WriteString("\n")
			}
			b.WriteString("\n")
		}

		if len(events) > len(stable) {
			b.WriteString(fmt.Sprintf(
				"_%d procedures below confidence threshold omitted._\n\n",
				len(events)-len(stable)))
		}

		return b.String()
	}

	// Sort by importance descending.
	sort.Slice(stable, func(i, j int) bool {
		return stable[i].Importance > stable[j].Importance
	})

	b.WriteString(fmt.Sprintf(
		"> %d stable procedural memories\n\n", len(stable)))

	for i, evt := range stable {
		b.WriteString(fmt.Sprintf("## %d. %s\n\n", i+1, evt.Summary))

		if evt.Content != "" {
			b.WriteString(evt.Content)
			b.WriteString("\n\n")
		}

		if len(evt.Tags) > 0 {
			b.WriteString(fmt.Sprintf(
				"- **Tags**: %s\n", strings.Join(evt.Tags, ", ")))
		}
		b.WriteString(fmt.Sprintf(
			"- **Confidence**: %.0f%%\n", evt.Confidence*100))
		b.WriteString(fmt.Sprintf(
			"- **Scope**: %s\n", evt.Scope))

		if evt.Source.SessionID != "" {
			b.WriteString(fmt.Sprintf(
				"- **Source session**: `%s`\n", evt.Source.SessionID))
		}

		b.WriteString("\n")
	}

	b.WriteString("---\n")
	b.WriteString(fmt.Sprintf("_Watermark: %d | %d total, %d stable_\n",
		maxWatermark(events),
		len(events),
		len(stable)))

	return b.String()
}
