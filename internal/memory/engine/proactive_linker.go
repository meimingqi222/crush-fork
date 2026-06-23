package engine

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

type linkCandidate struct {
	evt        MemoryEvent
	similarity float64
}

const (
	proactiveLinkMaxSimilar = 5
	proactiveLinkThreshold  = 0.15
	proactiveLinkMaxEdges   = 3
)

// ProactiveLinker automatically discovers semantic relationships between
// newly added memory events and existing memories, adding related_to edges
// in the background. This builds the memory graph incrementally without
// requiring an explicit consolidation pass.
type ProactiveLinker struct {
	store            EventStore
	tripleStore      *TripleStore
	embedder         Embedder
	embeddingPipeline *EmbeddingPipeline
	now              func() time.Time
}

// NewProactiveLinker creates a new ProactiveLinker. When embedder is nil,
// lexical (term-overlap) similarity is used instead of vector similarity.
func NewProactiveLinker(store EventStore, ts *TripleStore, embedder Embedder) *ProactiveLinker {
	return &ProactiveLinker{
		store:       store,
		tripleStore: ts,
		embedder:    embedder,
		now:         time.Now,
	}
}

// SetEmbedder updates the embedder used for vector similarity calculation.
// Pass nil to fall back to lexical similarity.
func (pl *ProactiveLinker) SetEmbedder(embedder Embedder) {
	if pl == nil {
		return
	}
	pl.embedder = embedder
}

// SetEmbeddingPipeline attaches an EmbeddingPipeline for cached embedding lookups.
func (pl *ProactiveLinker) SetEmbeddingPipeline(p *EmbeddingPipeline) {
	if pl == nil {
		return
	}
	pl.embeddingPipeline = p
}

// LinkNewEvent analyzes a newly appended event and creates related_to edges
// to the most semantically similar existing events. It is a best-effort
// background operation: errors are logged but not surfaced to callers.
func (pl *ProactiveLinker) LinkNewEvent(ctx context.Context, newEvent MemoryEvent) {
	if pl == nil || pl.store == nil || pl.tripleStore == nil {
		return
	}
	// Skip transient kinds that don't benefit from cross-linking.
	switch newEvent.Kind {
	case MemoryKindWorkingMemory, MemoryKindTaskState, MemoryKindRequest:
		return
	}
	// Skip if the event already has explicit edges (triples/links set during extraction).
	// We still run proactive linking in case there are additional implicit connections.

	// Fetch a candidate pool of recent memories to compare against.
	cutoff := pl.now().AddDate(0, -3, 0) // look back up to 3 months
	cutoffUnix := cutoff.Unix()
	events, err := pl.store.Query(ctx, EventFilter{
		Limit:        200,
		AfterTime:    &cutoffUnix,
		OrderDesc:    true,
	})
	if err != nil {
		slog.Debug("Proactive link: candidate query failed", "error", err)
		return
	}

	var candidates []linkCandidate

	newText := pl.eventText(newEvent)
	var newVec []float64
	if pl.embeddingPipeline != nil {
		vec, err := pl.embeddingPipeline.EmbedEvent(ctx, newEvent)
		if err == nil && vec != nil {
			newVec = vec
		}
	} else if pl.embedder != nil {
		vec, err := pl.embedder.Embed(ctx, newText)
		if err == nil {
			newVec = vec
		}
	}

	now := pl.now()
	for _, evt := range events {
		// Don't link to self.
		if evt.ID == newEvent.ID {
			continue
		}
		// Skip very old transient memories.
		age := now.Sub(evt.UpdatedAt)
		params := weibullParamsForKind(evt.Kind)
		if params.Decay(age.Hours()) < 0.05 {
			continue
		}

		var sim float64
		if newVec != nil {
			var evtVec []float64
			if pl.embeddingPipeline != nil {
				v, verr := pl.embeddingPipeline.EmbedEvent(ctx, evt)
				if verr == nil {
					evtVec = v
				}
			} else if pl.embedder != nil {
				v, verr := pl.embedder.Embed(ctx, pl.eventText(evt))
				if verr == nil {
					evtVec = v
				}
			}
			if evtVec != nil {
				sim = CosineSimilarity(newVec, evtVec)
			}
		}
		if sim == 0 {
			// Fallback: lexical term-overlap Jaccard similarity.
			sim = lexicalSimilarity(newText, pl.eventText(evt))
		}
		if sim >= proactiveLinkThreshold {
			candidates = append(candidates, linkCandidate{evt: evt, similarity: sim})
		}
	}

	if len(candidates) == 0 {
		return
	}

	// Sort by similarity descending.
	sortCandidates(candidates)
	if len(candidates) > proactiveLinkMaxSimilar {
		candidates = candidates[:proactiveLinkMaxSimilar]
	}

	// Determine edge type based on similarity strength and shared attributes.
	edgesAdded := 0
	for _, c := range candidates {
		if edgesAdded >= proactiveLinkMaxEdges {
			break
		}
		edgeType := EdgeRelatedTo
		weight := c.similarity

		// Check for potential refinement: if the new event supersedes or refines
		// the old one (same scope/kind and higher watermark), mark as refines.
		if c.evt.Scope == newEvent.Scope && c.evt.Kind == newEvent.Kind {
			if newEvent.Supersedes != nil && *newEvent.Supersedes == c.evt.ID {
				edgeType = EdgeRefines
				weight = 1.0
			} else if c.similarity > 0.5 {
				edgeType = EdgeRefines
			}
		}
		// Contradiction detection (simple heuristic: same kind/scope with opposite language).
		if c.evt.Kind == newEvent.Kind && c.evt.Scope == newEvent.Scope {
			if containsContradiction(newText, pl.eventText(c.evt)) {
				edgeType = EdgeContradicts
				weight = 0.9
			}
		}

		err := pl.tripleStore.AddEdge(ctx, Edge{
			SourceID: newEvent.ID,
			TargetID: c.evt.ID,
			EdgeType: edgeType,
			Weight:   weight,
		})
		if err != nil {
			slog.Debug("Proactive link: AddEdge failed", "error", err, "target", c.evt.ID)
			continue
		}
		edgesAdded++
	}

	if edgesAdded > 0 {
		slog.Debug("Proactive linking complete",
			"event_id", newEvent.ID,
			"candidates_considered", len(events),
			"edges_added", edgesAdded)
	}
}

// LinkEvents runs proactive linking for a batch of events (e.g., after extraction).
func (pl *ProactiveLinker) LinkEvents(ctx context.Context, events []MemoryEvent) {
	for _, evt := range events {
		pl.LinkNewEvent(ctx, evt)
	}
}

func (pl *ProactiveLinker) eventText(evt MemoryEvent) string {
	return strings.Join([]string{
		evt.Summary,
		evt.Content,
		string(evt.Kind),
		strings.Join(evt.Tags, " "),
	}, " ")
}

// lexicalSimilarity computes Jaccard similarity between two texts based on
// expanded query terms. Used as a fallback when no embedder is available.
func lexicalSimilarity(a, b string) float64 {
	termsA := expandedQueryTerms(a)
	termsB := expandedQueryTerms(b)
	if len(termsA) == 0 || len(termsB) == 0 {
		return 0
	}
	setA := make(map[string]struct{}, len(termsA))
	for _, t := range termsA {
		setA[t] = struct{}{}
	}
	intersection := 0
	setB := make(map[string]struct{}, len(termsB))
	for _, t := range termsB {
		if _, ok := setA[t]; ok {
			intersection++
		}
		setB[t] = struct{}{}
	}
	union := len(setA) + len(setB) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

// containsContradiction does a simple heuristic check for contradictory
// language (negations, preference reversals) between two texts.
func containsContradiction(a, b string) bool {
	lowerA := strings.ToLower(a)
	lowerB := strings.ToLower(b)
	// Look for negation + preference reversals
	negations := []string{"not", "don't", "doesn't", "no", "never", "avoid", "instead", "rather", "不再", "不要", "别", "不"}
	preferenceWords := []string{"prefer", "use", "want", "like", "应该", "使用", "用", "喜欢", "偏好"}

	// Check if one text asserts something the other negates.
	for _, neg := range negations {
		if strings.Contains(lowerA, neg) != strings.Contains(lowerB, neg) {
			for _, pref := range preferenceWords {
				if strings.Contains(lowerA, pref) && strings.Contains(lowerB, pref) {
					return true
				}
			}
		}
	}
	return false
}

func sortCandidates(c []linkCandidate) {
	// Simple insertion sort; small N.
	for i := 1; i < len(c); i++ {
		for j := i; j > 0 && c[j].similarity > c[j-1].similarity; j-- {
			c[j], c[j-1] = c[j-1], c[j]
		}
	}
}
