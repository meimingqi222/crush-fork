package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// SummaryRetriever implements the Retriever interface by reading from
// materialized views (memory_summary.md, mental_models/*.md, rollouts/*.md)
// and the EventStore. It is the primary recall path for prompt injection,
// using polyphonic retrieval with four voices (FTS, vector, temporal,
// triple/graph) fused via weighted Reciprocal Rank Fusion.
type SummaryRetriever struct {
	store             EventStore
	db                *sql.DB
	tripleStore       *TripleStore
	embeddingPipeline *EmbeddingPipeline
	outputDir         string
	reranker          Reranker
	voiceWeights      VoiceWeights
	mentalMaxLen      int // max bytes from mental_models layer when budget unset
	maxCandidates     int
}

// NewSummaryRetriever creates a SummaryRetriever that reads materialized
// views from outputDir and falls back to EventStore queries when views
// are not yet available.
func NewSummaryRetriever(store EventStore, db *sql.DB, outputDir string) *SummaryRetriever {
	return &SummaryRetriever{
		store:         store,
		db:            db,
		outputDir:     outputDir,
		mentalMaxLen:  4096,
		maxCandidates: 30,
		voiceWeights:  DefaultVoiceWeights(),
	}
}

// WithReranker installs a Reranker on the SummaryRetriever; subsequent
// Retrieve calls re-order FTS5/keyword candidates through it before
// truncating to limit. Pass nil to disable.
func (r *SummaryRetriever) WithReranker(rr Reranker) *SummaryRetriever {
	r.reranker = rr
	return r
}

// WithTripleStore connects a TripleStore for the graph/fact retrieval voice.
func (r *SummaryRetriever) WithTripleStore(ts *TripleStore) *SummaryRetriever {
	r.tripleStore = ts
	return r
}

// WithEmbeddingPipeline connects an EmbeddingPipeline for cached vector lookups.
func (r *SummaryRetriever) WithEmbeddingPipeline(p *EmbeddingPipeline) *SummaryRetriever {
	r.embeddingPipeline = p
	return r
}

// WithVoiceWeights overrides the default polyphonic RRF voice weights.
func (r *SummaryRetriever) WithVoiceWeights(vw VoiceWeights) *SummaryRetriever {
	if vw.Vector > 0 || vw.FTS > 0 || vw.Temporal > 0 || vw.Triple > 0 {
		r.voiceWeights = vw
	}
	return r
}

// WithMaxCandidates sets the candidate cap used before reranking.
func (r *SummaryRetriever) WithMaxCandidates(maxCandidates int) *SummaryRetriever {
	if maxCandidates > 0 {
		r.maxCandidates = maxCandidates
	}
	return r
}

// Recall returns a formatted summary block for prompt injection.
// It reads from:
//  1. mental_models/*.md (stable layer: preferences, conventions, pitfalls)
//  2. memory_summary.md  (recent corpus snapshot)
//  3. EventStore for current session working memory
//
// If no materialized views are available, it falls back to querying the
// EventStore for the most important durable events.
func (r *SummaryRetriever) Recall(ctx context.Context, opts map[string]any) (string, error) {
	var parts []string

	// 1. Stable mental models — always read in a fixed order.
	if mm := r.readMentalModels(); mm != "" {
		parts = append(parts, mm)
	}

	// 2. Recent corpus snapshot.
	summaryContent, err := r.readFile("memory_summary.md")
	if err == nil && summaryContent != "" {
		parts = append(parts, summaryContent)
	}

	// 3. Working memory for the current session.
	if sessionID, ok := opts["session_id"].(string); ok && sessionID != "" {
		wmContent := r.readWorkingMemory(ctx, sessionID)
		if wmContent != "" {
			parts = append(parts, wmContent)
		}
	}

	if len(parts) == 0 {
		return r.recallFromEvents(ctx)
	}
	return strings.Join(parts, "\n\n"), nil
}

// readMentalModels concatenates the per-model Markdown files in a fixed
// order so the prompt sees a stable, deduplicated mental-model layer.
func (r *SummaryRetriever) readMentalModels() string {
	if r.outputDir == "" {
		return ""
	}
	dir := filepath.Join(r.outputDir, "mental_models")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	type entry struct {
		name string
		data []byte
	}
	files := make([]entry, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".md") || name == "index.md" {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil || len(data) == 0 {
			continue
		}
		files = append(files, entry{name: name, data: data})
	}
	if len(files) == 0 {
		return ""
	}
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })

	var b strings.Builder
	b.WriteString("<mental_models>\n")
	limit := r.mentalMaxLen
	used := 0
	for _, f := range files {
		piece := strings.TrimSpace(string(f.data))
		if piece == "" {
			continue
		}
		if limit > 0 && used+len(piece) > limit {
			remaining := limit - used
			if remaining > 256 {
				piece = piece[:remaining]
				for len(piece) > 0 && !utf8.ValidString(piece) {
					piece = piece[:len(piece)-1]
				}
				b.WriteString(piece)
				b.WriteString("\n…(mental models truncated)\n")
				used = limit
			}
			break
		}
		b.WriteString(piece)
		b.WriteString("\n\n")
		used += len(piece) + 2
	}
	b.WriteString("</mental_models>")
	return b.String()
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

// Retrieve returns the most relevant memory events for a given context using
// polyphonic retrieval with four voices fused via weighted Reciprocal Rank Fusion:
//
//  1. FTS voice    – lexical BM25 match (weight: voiceWeights.FTS)
//  2. Vector voice – semantic embedding similarity (weight: voiceWeights.Vector)
//  3. Temporal voice – Weibull-recency weighted ranking (weight: voiceWeights.Temporal)
//  4. Triple/graph voice – fact triples & linked memory graph (weight: voiceWeights.Triple)
//
// opts may include "scope", "kind", "session_id", "limit" to filter results.
func (r *SummaryRetriever) Retrieve(ctx context.Context, query string, opts map[string]any) ([]MemoryEvent, error) {
	limit := 20
	if configuredLimit, ok := opts["limit"].(int); ok && configuredLimit > 0 {
		limit = configuredLimit
	}
	maxCandidates := r.maxCandidates
	if maxCandidates <= 0 {
		maxCandidates = 30
	}
	if configuredMaxCandidates, ok := opts["max_candidates"].(int); ok && configuredMaxCandidates > 0 {
		maxCandidates = configuredMaxCandidates
	}

	sessionID, _ := opts["session_id"].(string)

	filter := EventFilter{
		Limit: maxCandidates * 2,
	}
	if scope, ok := opts["scope"].(string); ok && scope != "" {
		s := MemoryScope(scope)
		filter.Scope = &s
	}
	if kind, ok := opts["kind"].(string); ok && kind != "" {
		k := MemoryKind(kind)
		filter.Kind = &k
	}
	if sessionID != "" {
		filter.SessionID = &sessionID
	}

	if strings.TrimSpace(query) == "" {
		events, err := r.store.Query(ctx, filter)
		if err != nil {
			return nil, err
		}
		if len(events) > limit {
			events = events[:limit]
		}
		return events, nil
	}

	// Parse temporal expressions from the query (e.g. "last week", "最近3天")
	// and apply them as time-range filters.
	temporalExprs := ParseTemporalExprs(query, time.Now())
	if len(temporalExprs) > 0 {
		earliest := temporalExprs[0].After
		for _, te := range temporalExprs[1:] {
			if te.After.Before(earliest) {
				earliest = te.After
			}
		}
		ts := earliest.Unix()
		filter.AfterTime = &ts
	}

	fetchLimit := maxCandidates * 2
	var voices []RankedList

	// --- Voice 1: FTS (lexical) ---
	if r.db != nil {
		ftsEvents, err := r.ftsSearch(ctx, query, filter, fetchLimit, true)
		if err != nil || len(ftsEvents) == 0 {
			ftsEvents, _ = r.ftsSearch(ctx, query, filter, fetchLimit, false)
		}
		if len(ftsEvents) > 0 {
			voices = append(voices, RankedList{Events: ftsEvents, Weight: r.voiceWeights.FTS})
		}
	}

	// --- Voice 2: Vector (semantic) ---
	if embReranker, ok := r.reranker.(*EmbeddingReranker); ok && embReranker != nil && embReranker.embedder != nil {
		if vecEvents, verr := r.vectorSearch(ctx, query, filter, fetchLimit, embReranker.embedder); verr == nil && len(vecEvents) > 0 {
			voices = append(voices, RankedList{Events: vecEvents, Weight: r.voiceWeights.Vector})
		}
	}

	// --- Voice 3: Temporal (recency via Weibull decay) ---
	broadFilter := filter
	broadFilter.Limit = 500
	allEvents, err := r.store.Query(ctx, broadFilter)
	if err == nil && len(allEvents) > 0 {
		temporalRanked := TemporalVoiceRank(allEvents, time.Now(), fetchLimit, sessionID)
		if len(temporalRanked) > 0 {
			voices = append(voices, RankedList{Events: temporalRanked, Weight: r.voiceWeights.Temporal})
		}
	} else {
		// Fallback to keyword ranking if broad query fails
		filter.Limit = 1000
		events, kerr := r.store.Query(ctx, filter)
		if kerr == nil && len(events) > 0 {
			keywordRanked := rankMemoryEvents(query, events)
			voices = append(voices, RankedList{Events: keywordRanked, Weight: r.voiceWeights.Temporal * 0.5})
		}
	}

	// --- Voice 4: Triple / graph facts ---
	if r.tripleStore != nil && r.db != nil {
		tripleEvents := r.tripleGraphSearch(ctx, query, filter, fetchLimit)
		if len(tripleEvents) > 0 {
			voices = append(voices, RankedList{Events: tripleEvents, Weight: r.voiceWeights.Triple})
		}
	}

	// --- Fuse all voices with weighted RRF ---
	var events []MemoryEvent
	if len(voices) > 0 {
		events = WeightedReciprocalRankFusion(voices, 60)
	} else {
		// Fallback when DB is unavailable: in-memory keyword ranking.
		filter.Limit = 1000
		events, err = r.store.Query(ctx, filter)
		if err != nil {
			return nil, err
		}
		events = rankMemoryEvents(query, events)
	}

	// Apply a final light heuristic rerank to blend edge signals (scope/kind
	// priority) that RRF alone does not capture.  Skip embedding rerank since
	// vector voice already contributed its signal.
	if r.reranker != nil {
		if hr, ok := r.reranker.(*HeuristicReranker); ok && hr != nil {
			if reranked, rerr := hr.Rerank(ctx, query, events); rerr == nil && len(reranked) > 0 {
				events = reranked
			}
		}
	}

	if len(events) > limit {
		events = events[:limit]
	}
	return events, nil
}

// tripleGraphSearch retrieves events by matching query terms against stored
// triples (subject/predicate/object), then expands via graph edges up to 2 hops.
// It forms the graph/fact voice of polyphonic recall.
func (r *SummaryRetriever) tripleGraphSearch(ctx context.Context, query string, filter EventFilter, limit int) []MemoryEvent {
	if r.tripleStore == nil {
		return nil
	}
	terms := expandedQueryTerms(query)
	if len(terms) == 0 {
		return nil
	}

	// Collect matching triples (subject OR predicate OR object hit).
	seenTriples := make(map[string]Triple)
	seedIDs := make([]string, 0, 32)
	for _, term := range terms {
		// Search by subject (exact term match for now; LIKE-based is enough
		// since our triple store is relatively small).
		rows, err := r.db.QueryContext(ctx, `
			SELECT id, subject, predicate, object, confidence, veracity,
			       valid_from, source_event_id, scope, created_at, updated_at
			FROM memory_triples
			WHERE (subject LIKE ? OR predicate LIKE ? OR object LIKE ?)
			  AND superseded_by IS NULL
			  AND (valid_to IS NULL OR valid_to = 0 OR valid_to > strftime('%s','now'))
			ORDER BY confidence DESC, created_at DESC
			LIMIT ?
		`, "%"+term+"%", "%"+term+"%", "%"+term+"%", limit/2)
		if err != nil {
			slog.Debug("Triple voice search failed", "term", term, "error", err)
			continue
		}
		for rows.Next() {
			var t Triple
			var validFrom, createdAt, updatedAt int64
			var sourceEventID sql.NullString
			if err := rows.Scan(
				&t.ID, &t.Subject, &t.Predicate, &t.Object,
				&t.Confidence, &t.Veracity, &validFrom,
				&sourceEventID, &t.Scope, &createdAt, &updatedAt,
			); err != nil {
				continue
			}
			t.ValidFrom = time.Unix(validFrom, 0)
			t.CreatedAt = time.Unix(createdAt, 0)
			t.UpdatedAt = time.Unix(updatedAt, 0)
			if sourceEventID.Valid {
				t.SourceEventID = sourceEventID.String
				if t.SourceEventID != "" {
					seedIDs = append(seedIDs, t.SourceEventID)
				}
			}
			seenTriples[t.ID] = t
		}
		rows.Close()
	}

	// Expand seeds via graph edges (2 hops) to find linked memories.
	linkedEvents, _, _ := r.tripleStore.GraphQuery(ctx, seedIDs, 2, []EdgeType{EdgeRelatedTo, EdgeRefines, EdgeDependsOn})

	// Also fetch the source events that produced the matched triples.
	idSet := make(map[string]bool)
	for _, id := range seedIDs {
		idSet[id] = true
	}
	for _, evt := range linkedEvents {
		idSet[evt.ID] = true
	}
	allIDs := make([]string, 0, len(idSet))
	for id := range idSet {
		allIDs = append(allIDs, id)
	}

	// Apply scope/kind/session filters from the original EventFilter and
	// return events ordered by confidence/importance/recency.
	if len(allIDs) == 0 {
		return nil
	}

	// Fetch all source events and filter manually (simpler than building SQL).
	broad := EventFilter{Limit: 500}
	allEvents, err := r.store.Query(ctx, broad)
	if err != nil {
		return nil
	}

	type scored struct {
		evt   MemoryEvent
		score float64
	}
	scoredList := make([]scored, 0, len(allIDs))
	now := time.Now()
	for _, evt := range allEvents {
		if !idSet[evt.ID] {
			continue
		}
		if filter.Scope != nil && evt.Scope != *filter.Scope {
			continue
		}
		if filter.Kind != nil && evt.Kind != *filter.Kind {
			continue
		}
		if filter.SessionID != nil && evt.Source.SessionID != *filter.SessionID {
			continue
		}
		age := now.Sub(evt.UpdatedAt)
		if age < 0 {
			age = 0
		}
		decay := weibullParamsForKind(evt.Kind).Decay(age.Hours())
		score := evt.Confidence*2.0 + evt.Importance + decay
		score *= 0.3 + 0.7*VeracityWeightFor(evt.Veracity)
		scoredList = append(scoredList, scored{evt: evt, score: score})
	}

	sort.SliceStable(scoredList, func(i, j int) bool {
		return scoredList[i].score > scoredList[j].score
	})
	if len(scoredList) > limit {
		scoredList = scoredList[:limit]
	}
	result := make([]MemoryEvent, 0, len(scoredList))
	for _, s := range scoredList {
		result = append(result, s.evt)
	}
	return result
}

// ftsSearch uses SQLite FTS5 for full-text search with BM25 ranking.
func (r *SummaryRetriever) ftsSearch(ctx context.Context, query string, filter EventFilter, limit int, strict bool) ([]MemoryEvent, error) {
	// Build the FTS query with optional scope/kind/session filters.
	// FTS5 MATCH supports boolean operators: AND, OR, NOT, and phrase queries with quotes.
	ftsQuery := sanitizeFTSQuery(query, strict)
	if ftsQuery == "" {
		return nil, nil
	}

	var conditions []string
	var args []any

	// Add the FTS MATCH condition.
	conditions = append(conditions, "memory_events_fts MATCH ?")
	args = append(args, ftsQuery)

	// Add scope filter if specified.
	if filter.Scope != nil {
		conditions = append(conditions, "e.scope = ?")
		args = append(args, string(*filter.Scope))
	}
	if filter.Kind != nil {
		conditions = append(conditions, "e.kind = ?")
		args = append(args, string(*filter.Kind))
	}
	if filter.SessionID != nil {
		conditions = append(conditions, "e.session_id = ?")
		args = append(args, *filter.SessionID)
	}

	whereClause := strings.Join(conditions, " AND ")

	// Query FTS5 with BM25 ranking and join with memory_events for full data.
	// bm25(memory_events_fts) returns lower scores for better matches.
	querySQL := fmt.Sprintf(`
		SELECT e.id, e.session_id, e.scope, e.kind, e.content, e.summary,
		       e.source_json, e.source_hash, e.confidence, e.importance,
		       e.veracity, e.supersedes, e.tags_json, e.watermark, e.created_at, e.updated_at, e.expires_at
		FROM memory_events_fts fts
		JOIN memory_events e ON fts.id = e.id
		WHERE %s
		ORDER BY bm25(memory_events_fts)
		LIMIT ?
	`, whereClause)
	args = append(args, limit)

	rows, err := r.db.QueryContext(ctx, querySQL, args...)
	if err != nil {
		return nil, fmt.Errorf("FTS search: %w", err)
	}
	defer rows.Close()

	var events []MemoryEvent
	for rows.Next() {
		var (
			event      MemoryEvent
			sessionID  string
			sourceJSON string
			sourceHash string
			tagsJSON   string
			createdAt  int64
			updatedAt  int64
			expiresAt  sql.NullInt64
		)
		if err := rows.Scan(
			&event.ID,
			&sessionID,
			&event.Scope,
			&event.Kind,
			&event.Content,
			&event.Summary,
			&sourceJSON,
			&sourceHash,
			&event.Confidence,
			&event.Importance,
			&event.Veracity,
			&event.Supersedes,
			&tagsJSON,
			&event.Watermark,
			&createdAt,
			&updatedAt,
			&expiresAt,
		); err != nil {
			return nil, fmt.Errorf("scanning FTS result: %w", err)
		}

		if err := json.Unmarshal([]byte(sourceJSON), &event.Source); err != nil {
			slog.Warn("FTS search: skipping event with invalid source_json", "event_id", event.ID, "error", err)
			continue
		}
		if event.Source.SessionID == "" {
			event.Source.SessionID = sessionID
		}
		if err := json.Unmarshal([]byte(tagsJSON), &event.Tags); err != nil {
			slog.Warn("FTS search: skipping event with invalid tags_json", "event_id", event.ID, "error", err)
			continue
		}

		event.CreatedAt = time.Unix(createdAt, 0)
		event.UpdatedAt = time.Unix(updatedAt, 0)
		if expiresAt.Valid {
			t := time.Unix(expiresAt.Int64, 0)
			event.ExpiresAt = &t
		}

		events = append(events, event)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating FTS results: %w", err)
	}

	return events, nil
}

// sanitizeFTSQuery escapes special characters and prepares the query for FTS5.
func sanitizeFTSQuery(query string, strict bool) string {
	// FTS5 special characters: " ' ( ) * ^ ~
	query = strings.TrimSpace(query)
	if query == "" {
		return ""
	}

	// Remove problematic characters that could cause syntax errors.
	query = strings.Map(func(r rune) rune {
		if r == '"' || r == '\'' || r == '(' || r == ')' || r == '*' || r == '^' || r == '~' {
			return ' '
		}
		return r
	}, query)

	terms := rawQueryTerms(query)
	if !strict {
		terms = queryTerms(query)
	}
	if len(terms) == 0 {
		return ""
	}
	if strict {
		return strings.Join(terms, " AND ")
	}
	return strings.Join(terms, " OR ")
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
		// Veracity weight scales the score based on how the fact was established.
		score *= 0.3 + 0.7*VeracityWeightFor(evt.Veracity)
		// Weibull recency decay: prefer recent memories, with per-kind
		// decay rates so that preferences outlast working memory.
		if !evt.UpdatedAt.IsZero() {
			age := time.Since(evt.UpdatedAt)
			if age < 0 {
				age = 0
			}
			params := weibullParamsForKind(evt.Kind)
			score += params.Decay(age.Hours())
		}
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

// vectorSearch performs embedding-based retrieval: it embeds the query,
// then scores all candidate events by cosine similarity and returns the
// top-k results. Pre-computed embeddings are read from the embedding
// store when available; missing ones are computed on-demand and cached.
// This complements FTS5's lexical matching for the RRF fusion path.
func (r *SummaryRetriever) vectorSearch(ctx context.Context, query string, filter EventFilter, limit int, embedder Embedder) ([]MemoryEvent, error) {
	if embedder == nil {
		return nil, nil
	}

	queryVec, err := embedder.Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("embedding query for vector search: %w", err)
	}

	// Fetch a broad candidate set from the store.
	broadFilter := filter
	broadFilter.Limit = 500
	candidates, err := r.store.Query(ctx, broadFilter)
	if err != nil || len(candidates) == 0 {
		return nil, err
	}

	// Use embedding pipeline for batch embedding with cache lookup when available.
	var candidateVecs map[string][]float64
	if r.embeddingPipeline != nil {
		candidateVecs, err = r.embeddingPipeline.EmbedEvents(ctx, candidates)
		if err != nil {
			slog.Debug("Batch embedding via pipeline failed, falling back to direct", "error", err)
			candidateVecs = nil
		}
	}

	type vecScored struct {
		evt   MemoryEvent
		score float64
	}
	vscored := make([]vecScored, 0, len(candidates))
	for _, evt := range candidates {
		var vec []float64
		if candidateVecs != nil {
			vec = candidateVecs[evt.ID]
		}
		if vec == nil {
			// Fallback: embed directly if no cache hit.
			text := embeddingEventText(evt)
			v, verr := embedder.Embed(ctx, text)
			if verr != nil {
				continue
			}
			vec = v
		}
		sim := dotProduct(queryVec, vec)
		vscored = append(vscored, vecScored{evt: evt, score: sim})
	}

	sort.SliceStable(vscored, func(i, j int) bool {
		return vscored[i].score > vscored[j].score
	})

	if len(vscored) > limit {
		vscored = vscored[:limit]
	}

	result := make([]MemoryEvent, 0, len(vscored))
	for _, s := range vscored {
		result = append(result, s.evt)
	}
	return result, nil
}
