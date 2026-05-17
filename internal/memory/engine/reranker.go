package engine

import (
	"context"
	"sort"
	"strings"
	"time"
)

// Reranker re-orders a candidate slice of MemoryEvent based on relevance to
// the given query. Implementations may use heuristic signals (term overlap,
// recency, scope, importance) or invoke an LLM for deeper relevance scoring.
//
// The contract is intentionally minimal: Rerank may return a different slice
// length than its input as long as the order reflects descending relevance,
// but implementations SHOULD NOT silently drop events unless reranking
// confidently classified them as irrelevant.
type Reranker interface {
	Rerank(ctx context.Context, query string, candidates []MemoryEvent) ([]MemoryEvent, error)
	// Name returns a short human-readable identifier (e.g. "heuristic", "llm")
	// for observability and tool reporting.
	Name() string
}

// HeuristicReranker is a zero-LLM Reranker that combines term-overlap with
// recency, importance, scope priority, and kind priority. It is safe to use
// by default because it never blocks on network calls.
type HeuristicReranker struct {
	now func() time.Time
}

// NewHeuristicReranker constructs a HeuristicReranker.
func NewHeuristicReranker() *HeuristicReranker {
	return &HeuristicReranker{now: time.Now}
}

// Name implements Reranker.
func (h *HeuristicReranker) Name() string { return "heuristic" }

// Rerank implements Reranker.
func (h *HeuristicReranker) Rerank(_ context.Context, query string, candidates []MemoryEvent) ([]MemoryEvent, error) {
	if len(candidates) == 0 {
		return candidates, nil
	}
	terms := tokenize(query)
	rawTerms := rawQueryTerms(query)
	now := h.now()
	type scored struct {
		evt   MemoryEvent
		score float64
	}
	scoredCandidates := make([]scored, 0, len(candidates))
	for _, evt := range candidates {
		score := h.scoreEvent(now, terms, evt) + exactTermBoost(rawTerms, evt)
		scoredCandidates = append(scoredCandidates, scored{evt: evt, score: score})
	}
	sort.SliceStable(scoredCandidates, func(i, j int) bool {
		if scoredCandidates[i].score == scoredCandidates[j].score {
			return scoredCandidates[i].evt.Watermark > scoredCandidates[j].evt.Watermark
		}
		return scoredCandidates[i].score > scoredCandidates[j].score
	})
	out := make([]MemoryEvent, 0, len(scoredCandidates))
	for _, sc := range scoredCandidates {
		out = append(out, sc.evt)
	}
	return out, nil
}

// scoreEvent computes a single weighted score combining term overlap with
// importance, confidence, recency, scope priority, and kind priority.
func (h *HeuristicReranker) scoreEvent(now time.Time, terms []string, evt MemoryEvent) float64 {
	score := 0.0
	if len(terms) > 0 {
		text := strings.ToLower(strings.Join([]string{
			evt.Summary,
			evt.Content,
			string(evt.Scope),
			string(evt.Kind),
			strings.Join(evt.Tags, " "),
		}, " "))
		for _, term := range terms {
			if strings.Contains(text, term) {
				score += 1.0
			}
		}
	}
	score += evt.Importance * 2.0
	score += evt.Confidence * 0.5
	score += kindPriority(evt.Kind)
	score += scopePriority(evt.Scope)

	// Recency decay: full weight within 7 days, halves each additional 7d.
	if !evt.UpdatedAt.IsZero() {
		age := now.Sub(evt.UpdatedAt)
		if age < 0 {
			age = 0
		}
		weeks := age.Hours() / (24 * 7)
		if weeks < 0 {
			weeks = 0
		}
		decay := 1.0
		for w := 0.0; w < weeks && decay > 0.0625; w += 1.0 {
			decay *= 0.7
		}
		score += decay
	}
	return score
}

func exactTermBoost(terms []string, evt MemoryEvent) float64 {
	if len(terms) == 0 {
		return 0
	}
	text := strings.ToLower(strings.Join([]string{
		evt.Summary,
		evt.Content,
		string(evt.Scope),
		string(evt.Kind),
		strings.Join(evt.Tags, " "),
	}, " "))
	boost := 0.0
	for _, term := range terms {
		if strings.Contains(text, term) {
			boost += 2.0
		}
	}
	return boost
}

func tokenize(s string) []string {
	return expandedQueryTerms(s)
}

func kindPriority(k MemoryKind) float64 {
	switch k {
	case MemoryKindDecision:
		return 1.0
	case MemoryKindPreference:
		return 0.8
	case MemoryKindPitfall:
		return 0.7
	case MemoryKindProcedure:
		return 0.5
	case MemoryKindReference:
		return 0.3
	default:
		return 0.0
	}
}

func scopePriority(s MemoryScope) float64 {
	switch s {
	case MemoryScopeProject:
		return 0.6
	case MemoryScopeUser:
		return 0.5
	case MemoryScopeGlobal:
		return 0.2
	default:
		return 0.0
	}
}
