package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// SummaryRetriever implements the Retriever interface by reading from
// materialized views (memory_summary.md) and the EventStore.
// It is the primary recall path for prompt injection.
type SummaryRetriever struct {
	store     EventStore
	outputDir string
}

// NewSummaryRetriever creates a SummaryRetriever that reads materialized
// views from outputDir and falls back to EventStore queries when views
// are not yet available.
func NewSummaryRetriever(store EventStore, outputDir string) *SummaryRetriever {
	return &SummaryRetriever{
		store:     store,
		outputDir: outputDir,
	}
}

// Recall returns a formatted summary block for prompt injection.
// It reads from:
//  1. memory_summary.md (materialized user + project summary)
//  2. EventStore for current session working memory
//
// If materialized views are not yet available, it falls back to
// querying the EventStore directly for the most important events.
func (r *SummaryRetriever) Recall(ctx context.Context, opts map[string]any) (string, error) {
	var parts []string

	// 1. Read materialized summary from disk (user + project).
	summaryContent, err := r.readFile("memory_summary.md")
	if err == nil && summaryContent != "" {
		parts = append(parts, summaryContent)
	}

	// 2. Read working memory for current session from EventStore.
	if sessionID, ok := opts["session_id"].(string); ok && sessionID != "" {
		wmContent := r.readWorkingMemory(ctx, sessionID)
		if wmContent != "" {
			parts = append(parts, wmContent)
		}
	}

	// 3. Fallback: if nothing from files/working memory, build from Events.
	if len(parts) == 0 {
		return r.recallFromEvents(ctx)
	}

	return strings.Join(parts, "\n\n"), nil
}

// Reflect synthesizes across multiple memory events to answer a query
// about past sessions, decisions, or project history. It queries the
// EventStore and returns a formatted synthesis. Does NOT write to LTM.
func (r *SummaryRetriever) Reflect(ctx context.Context, query string, opts map[string]any) (string, error) {
	// Build query filter from opts.
	filter := EventFilter{
		Limit: 50,
	}
	if scope, ok := opts["scope"].(string); ok && scope != "" {
		s := MemoryScope(scope)
		filter.Scope = &s
	}
	if kind, ok := opts["kind"].(string); ok && kind != "" {
		k := MemoryKind(kind)
		filter.Kind = &k
	}
	if sessionID, ok := opts["session_id"].(string); ok && sessionID != "" {
		filter.SessionID = &sessionID
	}

	events, err := r.store.Query(ctx, filter)
	if err != nil {
		return "", fmt.Errorf("reflecting on memory: %w", err)
	}
	if len(events) == 0 {
		return "", nil
	}

	// Sort by importance descending.
	sorted := make([]MemoryEvent, len(events))
	copy(sorted, events)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Importance > sorted[j].Importance
	})
	if len(sorted) > 10 {
		sorted = sorted[:10]
	}

	// Format as a cross-memory synthesis.
	var b strings.Builder
	if query != "" {
		b.WriteString(fmt.Sprintf("Memory synthesis for: %s\n\n", query))
	}
	for _, evt := range sorted {
		summary := evt.Summary
		if summary == "" {
			summary = truncateContent(evt.Content, 200)
		}
		b.WriteString(fmt.Sprintf("- [%s/%s] %s", evt.Scope, evt.Kind, summary))
		if evt.Source.SessionID != "" {
			b.WriteString(fmt.Sprintf(" (session: %s)", evt.Source.SessionID))
		}
		b.WriteString("\n")
	}

	return b.String(), nil
}

// Retrieve returns the most relevant memory events for a given context.
// opts may include "scope", "kind", "session_id", "limit" to filter results.
func (r *SummaryRetriever) Retrieve(ctx context.Context, query string, opts map[string]any) ([]MemoryEvent, error) {
	limit := 20
	if configuredLimit, ok := opts["limit"].(int); ok && configuredLimit > 0 {
		limit = configuredLimit
	}

	filter := EventFilter{
		Limit: limit,
	}
	if scope, ok := opts["scope"].(string); ok && scope != "" {
		s := MemoryScope(scope)
		filter.Scope = &s
	}
	if kind, ok := opts["kind"].(string); ok && kind != "" {
		k := MemoryKind(kind)
		filter.Kind = &k
	}
	if sessionID, ok := opts["session_id"].(string); ok && sessionID != "" {
		filter.SessionID = &sessionID
	}

	if strings.TrimSpace(query) == "" {
		return r.store.Query(ctx, filter)
	}

	filter.Limit = 1000
	events, err := r.store.Query(ctx, filter)
	if err != nil {
		return nil, err
	}

	ranked := rankMemoryEvents(query, events)
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}

	return ranked, nil
}

type scoredMemoryEvent struct {
	event MemoryEvent
	score float64
}

func rankMemoryEvents(query string, events []MemoryEvent) []MemoryEvent {
	terms := queryTerms(query)
	if len(terms) == 0 {
		return events
	}
	queryLower := strings.ToLower(strings.TrimSpace(query))
	scored := make([]scoredMemoryEvent, 0, len(events))
	for _, evt := range events {
		text := strings.ToLower(strings.Join([]string{
			evt.Summary,
			evt.Content,
			string(evt.Scope),
			string(evt.Kind),
			strings.Join(evt.Tags, " "),
		}, " "))
		score := 0.0
		if queryLower != "" && strings.Contains(text, queryLower) {
			score += 5
		}
		for _, term := range terms {
			if strings.Contains(text, term) {
				score++
			}
		}
		if score == 0 {
			continue
		}
		score += evt.Importance
		score += evt.Confidence * 0.25
		scored = append(scored, scoredMemoryEvent{event: evt, score: score})
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].score == scored[j].score {
			return scored[i].event.Watermark > scored[j].event.Watermark
		}
		return scored[i].score > scored[j].score
	})
	ranked := make([]MemoryEvent, 0, len(scored))
	for _, item := range scored {
		ranked = append(ranked, item.event)
	}
	return ranked
}

func queryTerms(query string) []string {
	seen := make(map[string]struct{})
	rawTerms := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	terms := make([]string, 0, len(rawTerms))
	for _, term := range rawTerms {
		if len([]rune(term)) < 2 {
			continue
		}
		if _, ok := seen[term]; ok {
			continue
		}
		seen[term] = struct{}{}
		terms = append(terms, term)
	}
	return terms
}

func (r *SummaryRetriever) readFile(name string) (string, error) {
	if r.outputDir == "" {
		return "", os.ErrNotExist
	}
	data, err := os.ReadFile(filepath.Join(r.outputDir, name))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (r *SummaryRetriever) readWorkingMemory(ctx context.Context, sessionID string) string {
	scope := MemoryScopeSession
	kind := MemoryKindWorkingMemory
	events, err := r.store.Query(ctx, EventFilter{
		Scope:     &scope,
		Kind:      &kind,
		SessionID: &sessionID,
		Limit:     50,
	})
	if err != nil || len(events) == 0 {
		return ""
	}

	if latest := FilterLatestNonSuperseded(events); latest != nil {
		return fmt.Sprintf("- Current session state: %s", latest.Content)
	}
	return ""
}

func (r *SummaryRetriever) recallFromEvents(ctx context.Context) (string, error) {
	// Query most important events as a fallback.
	events, err := r.store.Query(ctx, EventFilter{
		Limit: 10,
	})
	if err != nil {
		return "", err
	}
	if len(events) == 0 {
		return "", nil
	}

	// Sort by importance descending.
	sorted := make([]MemoryEvent, len(events))
	copy(sorted, events)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Importance > sorted[j].Importance
	})

	var parts []string
	for _, evt := range sorted {
		summary := evt.Summary
		if summary == "" {
			summary = truncateContent(evt.Content, 200)
		}
		parts = append(parts, fmt.Sprintf("- %s (%.0f%%)", summary, evt.Confidence*100))
	}
	return strings.Join(parts, "\n"), nil
}

func truncateContent(content string, maxLen int) string {
	runes := []rune(content)
	if len(runes) <= maxLen {
		return content
	}
	return string(runes[:maxLen]) + "…"
}
