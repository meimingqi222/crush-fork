package engine

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// MentalModelDefinition describes one Mental Model view. Each definition
// produces a single file under <outputDir>/mental_models/ with a stable name
// derived from ID, e.g. mental_models/preferences.md.
//
// Each model selects events by kind, optional scope, and an importance floor.
// MaxEvents caps the rendered events so the file stays prompt-budget friendly.
//
// Mental Models are the "stable layer" of the recall path: they update at
// most once per consolidation pass and are read directly during Recall.
type MentalModelDefinition struct {
	ID            string
	Title         string
	FileName      string
	Kinds         []MemoryKind
	Scopes        []MemoryScope
	MaxEvents     int
	MinImportance float64
	Description   string
}

// DefaultMentalModels returns the seed Mental Models registered with the
// engine by default. These mirror the conceptual layers in oh-my-pi:
// user preferences, project conventions, decisions, pitfalls, procedures.
func DefaultMentalModels() []MentalModelDefinition {
	return []MentalModelDefinition{
		{
			ID:        "preferences",
			Title:     "User Preferences",
			FileName:  "preferences.md",
			Kinds:     []MemoryKind{MemoryKindPreference},
			Scopes:    []MemoryScope{MemoryScopeUser, MemoryScopeProject, MemoryScopeGlobal},
			MaxEvents: 30,
			Description: "Stable user preferences such as language, coding style, and " +
				"interaction expectations.",
		},
		{
			ID:        "conventions",
			Title:     "Project Conventions",
			FileName:  "conventions.md",
			Kinds:     []MemoryKind{MemoryKindReference, MemoryKindProcedure},
			Scopes:    []MemoryScope{MemoryScopeProject},
			MaxEvents: 40,
			Description: "Project-specific conventions, patterns, and reference material " +
				"that should be honored by default.",
		},
		{
			ID:        "decisions",
			Title:     "Key Decisions",
			FileName:  "decisions.md",
			Kinds:     []MemoryKind{MemoryKindDecision},
			Scopes:    []MemoryScope{MemoryScopeProject, MemoryScopeUser, MemoryScopeGlobal},
			MaxEvents: 40,
			Description: "Durable decisions and architectural choices that constrain " +
				"future work.",
		},
		{
			ID:          "pitfalls",
			Title:       "Known Pitfalls",
			FileName:    "pitfalls.md",
			Kinds:       []MemoryKind{MemoryKindPitfall},
			Scopes:      []MemoryScope{MemoryScopeProject, MemoryScopeUser, MemoryScopeGlobal},
			MaxEvents:   40,
			Description: "Known traps, sharp edges, and recurring mistakes to avoid.",
		},
		{
			ID:          "procedures",
			Title:       "Procedures",
			FileName:    "procedures.md",
			Kinds:       []MemoryKind{MemoryKindProcedure},
			Scopes:      []MemoryScope{MemoryScopeProject, MemoryScopeUser, MemoryScopeGlobal},
			MaxEvents:   40,
			Description: "Step-by-step procedures and workflows.",
		},
	}
}

// MentalModelsMaterializer renders one stable Markdown file per mental model.
// Output layout: <outputDir>/mental_models/<file>.md plus a TOC index.md.
type MentalModelsMaterializer struct {
	base   materializerBase
	store  EventStore
	models []MentalModelDefinition
}

// NewMentalModelsMaterializer creates a MentalModelsMaterializer using the
// supplied model definitions. Pass DefaultMentalModels() for the seed list.
func NewMentalModelsMaterializer(db *sql.DB, store EventStore, writer *ArtifactWriter, models []MentalModelDefinition) *MentalModelsMaterializer {
	if len(models) == 0 {
		models = DefaultMentalModels()
	}
	return &MentalModelsMaterializer{
		base:   newMaterializerBase(db, writer, "mental_models"),
		store:  store,
		models: models,
	}
}

// Models returns the configured mental model definitions.
func (m *MentalModelsMaterializer) Models() []MentalModelDefinition {
	out := make([]MentalModelDefinition, len(m.models))
	copy(out, m.models)
	return out
}

// ListViews implements Materializer.
func (m *MentalModelsMaterializer) ListViews(_ context.Context) ([]string, error) {
	return []string{"mental_models"}, nil
}

// Materialize implements Materializer. It rebuilds every mental model file
// from scratch when the global watermark advances. Each file is a self-
// contained, prompt-friendly Markdown document.
func (m *MentalModelsMaterializer) Materialize(ctx context.Context, _ string, _ []MemoryEvent) error {
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

	// Fetch a generous slice of events; per-model filters narrow further.
	events, err := m.store.Query(ctx, EventFilter{
		Limit: 1000,
	})
	if err != nil {
		return fmt.Errorf("querying events for mental models: %w", err)
	}
	events = dropSupersededEvents(events)

	type rendered struct {
		def     MentalModelDefinition
		content string
		count   int
	}
	renderedModels := make([]rendered, 0, len(m.models))

	for _, def := range m.models {
		matches := filterEventsForModel(events, def)
		content := renderMentalModelDoc(def, matches)
		fileName := def.FileName
		if fileName == "" {
			fileName = sanitizeFileName(def.ID) + ".md"
		}
		path := fmt.Sprintf("mental_models/%s", fileName)
		if err := m.base.writer.WriteFile(path, []byte(content)); err != nil {
			return fmt.Errorf("writing mental model %s: %w", def.ID, err)
		}
		renderedModels = append(renderedModels, rendered{def: def, content: content, count: len(matches)})
	}

	// Write an index file for human browsing.
	var idx strings.Builder
	idx.WriteString("# Mental Models — Index\n\n")
	idx.WriteString(fmt.Sprintf("_Last updated: %s_\n\n", time.Now().Format(time.RFC3339)))
	for _, r := range renderedModels {
		fileName := r.def.FileName
		if fileName == "" {
			fileName = sanitizeFileName(r.def.ID) + ".md"
		}
		fmt.Fprintf(&idx, "- [%s](%s) — %d events", r.def.Title, fileName, r.count)
		if r.def.Description != "" {
			fmt.Fprintf(&idx, " — %s", r.def.Description)
		}
		idx.WriteString("\n")
	}
	if err := m.base.writer.WriteFile("mental_models/index.md", []byte(idx.String())); err != nil {
		return fmt.Errorf("writing mental_models/index.md: %w", err)
	}

	if err := m.base.setWatermark(ctx, maxWM); err != nil {
		return err
	}
	return nil
}

func filterEventsForModel(events []MemoryEvent, def MentalModelDefinition) []MemoryEvent {
	kindSet := make(map[MemoryKind]struct{}, len(def.Kinds))
	for _, k := range def.Kinds {
		kindSet[k] = struct{}{}
	}
	scopeSet := make(map[MemoryScope]struct{}, len(def.Scopes))
	for _, s := range def.Scopes {
		scopeSet[s] = struct{}{}
	}
	hasScope := len(scopeSet) > 0
	matched := make([]MemoryEvent, 0, len(events))
	for _, evt := range events {
		if !IsMaterializableEvent(evt) {
			continue
		}
		if len(kindSet) > 0 {
			if _, ok := kindSet[evt.Kind]; !ok {
				continue
			}
		}
		if hasScope {
			if _, ok := scopeSet[evt.Scope]; !ok {
				continue
			}
		}
		if def.MinImportance > 0 && evt.Importance < def.MinImportance {
			continue
		}
		matched = append(matched, evt)
	}
	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].Importance == matched[j].Importance {
			return matched[i].Watermark > matched[j].Watermark
		}
		return matched[i].Importance > matched[j].Importance
	})
	if def.MaxEvents > 0 && len(matched) > def.MaxEvents {
		matched = matched[:def.MaxEvents]
	}
	return matched
}

func renderMentalModelDoc(def MentalModelDefinition, events []MemoryEvent) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", def.Title)
	if def.Description != "" {
		fmt.Fprintf(&b, "> %s\n>\n", def.Description)
	}
	fmt.Fprintf(&b, "> Generated: %s · Events: %d\n\n", time.Now().Format(time.RFC3339), len(events))
	if len(events) == 0 {
		b.WriteString("_No events have been captured for this mental model yet._\n")
		return b.String()
	}
	for _, evt := range events {
		summary := strings.TrimSpace(evt.Summary)
		if summary == "" {
			summary = truncateContent(evt.Content, 120)
		}
		fmt.Fprintf(&b, "- %s", summary)
		if evt.Importance > 0 || evt.Confidence > 0 {
			fmt.Fprintf(&b, " _(importance %.0f%%, confidence %.0f%%)_",
				evt.Importance*100, evt.Confidence*100)
		}
		b.WriteString("\n")
		if strings.TrimSpace(evt.Content) != "" && evt.Content != summary {
			fmt.Fprintf(&b, "  - %s\n", truncateContent(evt.Content, 400))
		}
		if len(evt.Tags) > 0 {
			fmt.Fprintf(&b, "  - tags: %s\n", strings.Join(evt.Tags, ", "))
		}
	}
	b.WriteString("\n")
	return b.String()
}

// dropSupersededEvents removes events that have been superseded by a later
// event in the input slice.
func dropSupersededEvents(events []MemoryEvent) []MemoryEvent {
	superseded := make(map[string]bool, len(events))
	for _, evt := range events {
		if evt.Supersedes != nil {
			superseded[*evt.Supersedes] = true
		}
	}
	out := make([]MemoryEvent, 0, len(events))
	for _, evt := range events {
		if superseded[evt.ID] {
			continue
		}
		out = append(out, evt)
	}
	return out
}

func sanitizeFileName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "unnamed"
	}
	out := make([]rune, 0, len(s))
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out = append(out, r)
		case r == '-' || r == '_':
			out = append(out, r)
		case r == ' ' || r == '/':
			out = append(out, '-')
		}
	}
	if len(out) == 0 {
		return "unnamed"
	}
	return string(out)
}
