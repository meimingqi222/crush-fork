package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// CompactionRescue captures durable memory that should survive a session
// summarization (compaction). It is produced by PrepareCompactionRescue and
// rendered as a prompt-friendly Markdown block via Rendered().
type CompactionRescue struct {
	SessionID   string
	Events      []MemoryEvent
	Notes       []string
	Rendered    string
	GeneratedAt time.Time
}

// PrepareCompactionRescue builds a CompactionRescue for the given session.
// It is intentionally cheap: it queries the EventStore for the most
// important durable events, applies the configured Reranker when present,
// then truncates to the configured byte budget.
//
// The current session's pending events have already been recorded via
// AfterTurnIdle by callers, so this method does not invoke the Extractor.
func (e *Engine) PrepareCompactionRescue(ctx context.Context, sessionID string, opts CompactionRescueOptions) (*CompactionRescue, error) {
	if !e.enabled || e.store == nil {
		return nil, nil
	}
	topK := opts.TopK
	if topK <= 0 {
		topK = 5
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 2048
	}

	// Pool: session-scoped working memory + cross-session durable events.
	pool := make([]MemoryEvent, 0, 256)

	if sessionID != "" {
		sessionScope := MemoryScopeSession
		sessionEvents, err := e.store.Query(ctx, EventFilter{
			Scope:     &sessionScope,
			SessionID: &sessionID,
			Limit:     50,
		})
		if err == nil {
			pool = append(pool, sessionEvents...)
		}
	}

	durable, err := e.store.Query(ctx, EventFilter{
		Limit: 200,
	})
	if err != nil {
		return nil, fmt.Errorf("querying durable events for compaction rescue: %w", err)
	}
	for _, evt := range durable {
		if !IsMaterializableEvent(evt) {
			continue
		}
		pool = append(pool, evt)
	}
	pool = dropSupersededEvents(pool)
	if len(pool) == 0 {
		return nil, nil
	}

	// Score: invoke reranker if configured, otherwise rank by importance × recency.
	ranked := pool
	if opts.UseReranker && e.reranker != nil {
		out, rerErr := e.reranker.Rerank(ctx, "session summary, key decisions, preferences, pitfalls, procedures, recent task state", pool)
		if rerErr == nil && len(out) > 0 {
			ranked = out
		}
	} else {
		sort.SliceStable(ranked, func(i, j int) bool {
			if ranked[i].Importance == ranked[j].Importance {
				return ranked[i].Watermark > ranked[j].Watermark
			}
			return ranked[i].Importance > ranked[j].Importance
		})
	}
	if len(ranked) > topK {
		ranked = ranked[:topK]
	}

	rescue := &CompactionRescue{
		SessionID:   sessionID,
		Events:      ranked,
		GeneratedAt: time.Now(),
	}
	rescue.Rendered = renderCompactionRescue(rescue, maxBytes)
	return rescue, nil
}

// CompactionRescueOptions configures PrepareCompactionRescue.
type CompactionRescueOptions struct {
	TopK        int
	MaxBytes    int
	UseReranker bool
}

func renderCompactionRescue(rescue *CompactionRescue, maxBytes int) string {
	if rescue == nil || len(rescue.Events) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<memory_rescue>\n")
	b.WriteString("The following durable memories MUST be preserved through compaction. ")
	b.WriteString("They are ordered by relevance; copy or paraphrase them into the new summary.\n\n")
	for i, evt := range rescue.Events {
		summary := strings.TrimSpace(evt.Summary)
		if summary == "" {
			summary = truncateContent(evt.Content, 200)
		}
		fmt.Fprintf(&b, "%d. [%s/%s] %s\n", i+1, evt.Scope, evt.Kind, summary)
		if content := strings.TrimSpace(evt.Content); content != "" && content != summary {
			fmt.Fprintf(&b, "   - %s\n", truncateContent(content, 400))
		}
	}
	b.WriteString("</memory_rescue>\n")
	out := b.String()
	if maxBytes > 0 && len(out) > maxBytes {
		cut := maxBytes
		if cut < len("<memory_rescue>\n</memory_rescue>") {
			return ""
		}
		for cut > 0 && !utf8.ValidString(out[:cut]) {
			cut--
		}
		out = out[:cut]
		out += "\n…(truncated)\n</memory_rescue>\n"
	}
	return out
}

// SetReranker installs an optional Reranker used by Retrieve and by
// PrepareCompactionRescue when UseReranker is true.
func (e *Engine) SetReranker(r Reranker) {
	e.reranker = r
}

// Reranker returns the installed Reranker, or nil if none.
func (e *Engine) Reranker() Reranker { return e.reranker }
