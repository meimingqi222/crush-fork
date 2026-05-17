package engine

import (
	"context"
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"unicode"
)

const defaultHashingEmbeddingDimensions = 384

// Embedder converts text into a normalized embedding vector.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float64, error)
	Name() string
}

// HashingEmbedder is a zero-download local embedding backend. It uses signed
// feature hashing over normalized lexical tokens plus CJK character n-grams.
// It is not as semantic as a neural model, but it is deterministic, cheap,
// bilingual-friendly, and safe to enable without model files.
type HashingEmbedder struct {
	dimensions int
}

func NewHashingEmbedder(dimensions int) *HashingEmbedder {
	if dimensions <= 0 {
		dimensions = defaultHashingEmbeddingDimensions
	}
	return &HashingEmbedder{dimensions: dimensions}
}

func (h *HashingEmbedder) Name() string { return "hashing" }

func (h *HashingEmbedder) Embed(ctx context.Context, text string) ([]float64, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	vec := make([]float64, h.dimensions)
	features := embeddingFeatures(text)
	for _, feature := range features {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		idx, sign := hashedFeature(feature, h.dimensions)
		vec[idx] += sign
	}
	normalizeVector(vec)
	return vec, nil
}

// EmbeddingReranker reranks lexical candidates by local embedding similarity,
// then blends in the existing heuristic score to avoid overfitting to weak
// hash collisions. It only sees the candidate slice provided by Retrieve().
type EmbeddingReranker struct {
	embedder  Embedder
	heuristic *HeuristicReranker
}

func NewEmbeddingReranker(embedder Embedder) *EmbeddingReranker {
	if embedder == nil {
		embedder = NewHashingEmbedder(defaultHashingEmbeddingDimensions)
	}
	return &EmbeddingReranker{
		embedder:  embedder,
		heuristic: NewHeuristicReranker(),
	}
}

func (r *EmbeddingReranker) Name() string {
	return "embedding:" + r.embedder.Name()
}

func (r *EmbeddingReranker) Rerank(ctx context.Context, query string, candidates []MemoryEvent) ([]MemoryEvent, error) {
	if len(candidates) == 0 {
		return candidates, nil
	}
	queryVec, err := r.embedder.Embed(ctx, query)
	if err != nil {
		return nil, err
	}
	rawTerms := rawQueryTerms(query)
	terms := expandedQueryTerms(query)
	now := r.heuristic.now()
	type scored struct {
		evt   MemoryEvent
		score float64
	}
	scoredCandidates := make([]scored, 0, len(candidates))
	for _, evt := range candidates {
		content := embeddingEventText(evt)
		vec, err := r.embedder.Embed(ctx, content)
		if err != nil {
			return nil, err
		}
		similarity := dotProduct(queryVec, vec)
		heuristicScore := r.heuristic.scoreEvent(now, terms, evt) + exactTermBoost(rawTerms, evt)
		scoredCandidates = append(scoredCandidates, scored{
			evt:   evt,
			score: similarity*6.0 + heuristicScore,
		})
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

func embeddingEventText(evt MemoryEvent) string {
	return strings.Join([]string{
		evt.Summary,
		evt.Content,
		string(evt.Scope),
		string(evt.Kind),
		strings.Join(evt.Tags, " "),
	}, " ")
}

func embeddingFeatures(text string) []string {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return nil
	}
	features := make([]string, 0, 64)
	seen := make(map[string]struct{})
	add := func(feature string) {
		feature = strings.TrimSpace(feature)
		if feature == "" {
			return
		}
		if _, ok := seen[feature]; ok {
			return
		}
		seen[feature] = struct{}{}
		features = append(features, feature)
	}
	for _, term := range expandedQueryTerms(text) {
		add("term:" + term)
	}
	var cjk []rune
	flushCJK := func() {
		if len(cjk) == 0 {
			return
		}
		for i, r := range cjk {
			add("cjk1:" + string(r))
			if i+1 < len(cjk) {
				add("cjk2:" + string(cjk[i:i+2]))
			}
			if i+2 < len(cjk) {
				add("cjk3:" + string(cjk[i:i+3]))
			}
		}
		cjk = cjk[:0]
	}
	for _, r := range text {
		if isCJKRune(r) {
			cjk = append(cjk, r)
			continue
		}
		flushCJK()
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			continue
		}
	}
	flushCJK()
	return features
}

func hashedFeature(feature string, dimensions int) (int, float64) {
	h := fnv.New64a()
	_, _ = h.Write([]byte(feature))
	sum := h.Sum64()
	idx := int(sum % uint64(dimensions))
	sign := 1.0
	if (sum>>63)&1 == 1 {
		sign = -1.0
	}
	return idx, sign
}

func normalizeVector(vec []float64) {
	norm := 0.0
	for _, v := range vec {
		norm += v * v
	}
	if norm == 0 {
		return
	}
	norm = math.Sqrt(norm)
	for i := range vec {
		vec[i] /= norm
	}
}

func dotProduct(a, b []float64) float64 {
	limit := len(a)
	if len(b) < limit {
		limit = len(b)
	}
	result := 0.0
	for i := 0; i < limit; i++ {
		result += a[i] * b[i]
	}
	return result
}
