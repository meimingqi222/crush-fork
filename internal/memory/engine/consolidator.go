package engine

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// tagConsolidatedOutput is appended to every event produced by consolidation
// so that subsequent consolidation passes can skip already-consolidated events
// and avoid infinite re-processing.
const tagConsolidatedOutput = "consolidated_output"

// ConsolidatedEvent is the structured output from LLM consolidation analysis.
// It represents a higher-level semantic or procedural memory synthesized from
// episodic events across one or more sessions. The Supersedes field allows the
// LLM to flag when a new consolidated event replaces an older one, enabling
// automatic deduplication and versioning of memory.
type ConsolidatedEvent struct {
	Kind       MemoryKind  `json:"kind"`
	Scope      MemoryScope `json:"scope"`
	Content    string      `json:"content"`
	Summary    string      `json:"summary,omitempty"`
	Confidence float64     `json:"confidence"`
	Importance float64     `json:"importance"`
	Tags       []string    `json:"tags,omitempty"`
	Supersedes *string     `json:"supersedes,omitempty"`
}

// LLMConsolidator implements Consolidator by using user-provided callbacks
// for fetching existing consolidated events and LLM-based consolidation
// analysis. This keeps the engine package free of direct dependencies on
// LLM frameworks (mirrors the LLMExtractor pattern).
type LLMConsolidator struct {
	getExisting   func(ctx context.Context) ([]MemoryEvent, error)
	analyzeEvents func(ctx context.Context, episodes, existing string) ([]ConsolidatedEvent, error)
	clock         func() time.Time
}

// NewLLMConsolidator creates a new LLMConsolidator with the given dependencies.
//   - getExisting: returns existing consolidated events for Supersedes detection.
//   - analyzeEvents: calls an LLM to consolidate episodic events into semantic/
//     procedural events, receiving formatted text of both new episodes and
//     existing consolidated events for comparison.
func NewLLMConsolidator(
	getExisting func(ctx context.Context) ([]MemoryEvent, error),
	analyzeEvents func(ctx context.Context, episodes, existing string) ([]ConsolidatedEvent, error),
) *LLMConsolidator {
	return &LLMConsolidator{
		getExisting:   getExisting,
		analyzeEvents: analyzeEvents,
		clock:         time.Now,
	}
}

// Consolidate implements Consolidator. It takes episodic MemoryEvents,
// fetches existing consolidated events for comparison, runs LLM analysis,
// and returns new consolidated MemoryEvents with Supersedes links where
// existing knowledge is replaced.
func (c *LLMConsolidator) Consolidate(ctx context.Context, events []MemoryEvent) ([]MemoryEvent, error) {
	if len(events) == 0 {
		return nil, nil
	}

	existing, err := c.getExisting(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting existing consolidated events: %w", err)
	}

	episodeText := consolidationFormatEpisodes(events)
	existingText := consolidationFormatExisting(existing)

	consolidated, err := c.analyzeEvents(ctx, episodeText, existingText)
	if err != nil {
		return nil, fmt.Errorf("consolidation analysis failed: %w", err)
	}
	if len(consolidated) == 0 {
		return nil, nil
	}

	now := c.clock()
	result := make([]MemoryEvent, 0, len(consolidated))
	for i, ce := range consolidated {
		eventID := fmt.Sprintf("con-%s-%d", string(ce.Kind), now.UnixNano()+int64(i))
		event := MemoryEvent{
			ID:      eventID,
			Scope:   ce.Scope,
			Kind:    ce.Kind,
			Content: ce.Content,
			Summary: ce.Summary,
			Source: MemorySourceRef{
				SessionID: "",
			},
			Confidence: ce.Confidence,
			Importance: ce.Importance,
			CreatedAt:  now,
			UpdatedAt:  now,
			Supersedes: ce.Supersedes,
			Tags:       append(ce.Tags, tagConsolidatedOutput),
		}
		if event.Confidence <= 0 {
			event.Confidence = 0.7
		}
		if event.Importance <= 0 {
			event.Importance = 0.5
		}
		if event.Scope == "" {
			event.Scope = MemoryScopeProject
		}
		result = append(result, event)
	}

	return result, nil
}

// consolidationFormatEpisodes formats episodic events as compact text for
// LLM consumption. Each event is rendered as a numbered block with its
// session origin, kind, scope, content, summary, and tags.
func consolidationFormatEpisodes(events []MemoryEvent) string {
	var b strings.Builder
	for i, evt := range events {
		b.WriteString(fmt.Sprintf(
			"[%d] Session: %s | Kind: %s | Scope: %s | Confidence: %.2f | Importance: %.2f\n",
			i+1, evt.Source.SessionID, string(evt.Kind), string(evt.Scope),
			evt.Confidence, evt.Importance,
		))
		if evt.Content != "" {
			b.WriteString(fmt.Sprintf("    Content: %s\n", evt.Content))
		}
		if evt.Summary != "" {
			b.WriteString(fmt.Sprintf("    Summary: %s\n", evt.Summary))
		}
		if len(evt.Tags) > 0 {
			b.WriteString(fmt.Sprintf("    Tags: %s\n", strings.Join(evt.Tags, ", ")))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// consolidationFormatExisting formats existing consolidated events as text
// for Supersedes analysis. Each event is rendered with its database ID,
// kind, scope, content, summary, and any existing Supersedes chain.
func consolidationFormatExisting(events []MemoryEvent) string {
	var b strings.Builder
	for i, evt := range events {
		b.WriteString(fmt.Sprintf(
			"[EXISTING %d] ID: %s | Kind: %s | Scope: %s\n",
			i+1, evt.ID, string(evt.Kind), string(evt.Scope),
		))
		if evt.Content != "" {
			b.WriteString(fmt.Sprintf("    Content: %s\n", evt.Content))
		}
		if evt.Summary != "" {
			b.WriteString(fmt.Sprintf("    Summary: %s\n", evt.Summary))
		}
		if evt.Supersedes != nil {
			b.WriteString(fmt.Sprintf("    Supersedes: %s\n", *evt.Supersedes))
		}
		b.WriteString("\n")
	}
	return b.String()
}
