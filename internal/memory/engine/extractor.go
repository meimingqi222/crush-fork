package engine

import (
	"context"
	"fmt"
	"time"
)

// Transcript holds a formatted session transcript and its message IDs.
type Transcript struct {
	Text       string   `json:"text"`
	MessageIDs []string `json:"message_ids,omitempty"`
}

// ExtractedEvent is the structured output from LLM analysis of a session
// transcript. It maps directly to MemoryEvent fields.
type ExtractedEvent struct {
	Kind       MemoryKind        `json:"kind"`
	Scope      MemoryScope       `json:"scope"`
	Content    string            `json:"content"`
	Summary    string            `json:"summary,omitempty"`
	Confidence float64           `json:"confidence"`
	Importance float64           `json:"importance"`
	Veracity   MemoryVeracity    `json:"veracity,omitempty"`
	Tags       []string          `json:"tags,omitempty"`
	Triples    []ExtractedTriple `json:"triples,omitempty"`
}

// ExtractedTriple represents a knowledge-graph triple extracted from
// conversation alongside an event.
type ExtractedTriple struct {
	Subject   string `json:"subject"`
	Predicate string `json:"predicate"`
	Object    string `json:"object"`
}

// LLMExtractor implements Extractor by using user-provided callbacks for
// transcript retrieval and LLM-based event analysis. This keeps the engine
// package free of direct dependencies on message services or LLM frameworks.
type LLMExtractor struct {
	getTranscript func(ctx context.Context, sessionID string) (Transcript, error)
	analyzeEvents func(ctx context.Context, transcript string) ([]ExtractedEvent, error)
	getFiles      func(ctx context.Context, sessionID string) []string
	clock         func() time.Time
}

// Compile-time interface compliance check.
var _ Extractor = (*LLMExtractor)(nil)

// NewLLMExtractor creates a new LLMExtractor with the given dependencies.
//   - getTranscript: retrieves a session transcript (text + message IDs).
//   - analyzeEvents: calls an LLM to extract events from transcript text.
//   - getFiles: returns files touched in a session for provenance.
func NewLLMExtractor(
	getTranscript func(ctx context.Context, sessionID string) (Transcript, error),
	analyzeEvents func(ctx context.Context, transcript string) ([]ExtractedEvent, error),
	getFiles func(ctx context.Context, sessionID string) []string,
) *LLMExtractor {
	return &LLMExtractor{
		getTranscript: getTranscript,
		analyzeEvents: analyzeEvents,
		getFiles:      getFiles,
		clock:         time.Now,
	}
}

// Extract implements Extractor. It fetches the session transcript, runs LLM
// analysis, wraps each result with provenance data, and returns MemoryEvents.
// The caller is responsible for storing any triples from the extracted events.
func (e *LLMExtractor) Extract(ctx context.Context, sessionID string) ([]MemoryEvent, error) {
	transcript, err := e.getTranscript(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("getting transcript: %w", err)
	}
	if transcript.Text == "" {
		return nil, nil
	}

	extracted, err := e.analyzeEvents(ctx, transcript.Text)
	if err != nil {
		return nil, fmt.Errorf("analyzing events: %w", err)
	}
	if len(extracted) == 0 {
		return nil, nil
	}

	files := e.getFiles(ctx, sessionID)
	now := e.clock()

	events := make([]MemoryEvent, 0, len(extracted))
	for i, ee := range extracted {
		eventID := fmt.Sprintf("ext-%s-%d", sessionID, now.UnixNano()+int64(i))
		veracity := NormalizeVeracity(string(ee.Veracity))
		confidence := ee.Confidence
		if confidence <= 0 {
			confidence = 0.5
		}
		// Apply Bayesian update for veracity-weighted confidence.
		confidence = BayesianUpdate(confidence, veracity)
		events = append(events, MemoryEvent{
			ID:      eventID,
			Scope:   ee.Scope,
			Kind:    ee.Kind,
			Content: ee.Content,
			Summary: ee.Summary,
			Source: MemorySourceRef{
				SessionID:  sessionID,
				MessageIDs: transcript.MessageIDs,
				Files:      files,
			},
			Confidence: confidence,
			Importance: ee.Importance,
			Veracity:   veracity,
			CreatedAt:  now,
			UpdatedAt:  now,
			Tags:       ee.Tags,
			Triples:    ee.Triples,
		})
	}

	return events, nil
}
