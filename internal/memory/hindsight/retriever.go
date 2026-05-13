package hindsight

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/memory/engine"
)

// Retriever implements engine.Retriever using only the remote Hindsight
// service. It intentionally does not merge local memory summaries so the
// selected backend remains unambiguous.
type Retriever struct {
	client *Client
}

// NewRetriever creates a remote-only Retriever backed by Hindsight.
func NewRetriever(client *Client) *Retriever {
	return &Retriever{client: client}
}

// Recall implements engine.Retriever by asking Hindsight for broad project and
// user context suitable for automatic prompt injection.
func (r *Retriever) Recall(ctx context.Context, opts map[string]any) (string, error) {
	req := RecallRequest{
		Query:     "project context, recent work, decisions, pitfalls, procedures, and user preferences",
		Budget:    "mid",
		MaxTokens: 1024,
	}
	if tags, match := recallTags(opts); len(tags) > 0 {
		req.Tags = tags
		req.TagsMatch = match
	}

	remoteResults, err := r.client.Recall(ctx, req)
	if err != nil {
		return "", err
	}

	if len(remoteResults) == 0 {
		return "", nil
	}

	return formatRemoteRecall(remoteResults), nil
}

// Retrieve implements engine.Retriever by querying Hindsight recall directly.
func (r *Retriever) Retrieve(ctx context.Context, query string, opts map[string]any) ([]engine.MemoryEvent, error) {
	query = strings.TrimSpace(query)
	if query == "" && !hasRecallFilters(opts) {
		return nil, nil
	}
	if query == "" {
		query = "project context, recent work, decisions, pitfalls, procedures, and user preferences"
	}

	req := RecallRequest{
		Query:     query,
		Budget:    recallBudget(opts),
		MaxTokens: recallMaxTokens(opts),
	}
	if tags, match := recallTags(opts); len(tags) > 0 {
		req.Tags = tags
		req.TagsMatch = match
	}
	remoteResults, remoteErr := r.client.Recall(ctx, req)
	if remoteErr != nil || len(remoteResults) == 0 {
		return nil, remoteErr
	}

	// Convert results preserving API order (results are already sorted by relevance).
	events := make([]engine.MemoryEvent, 0, len(remoteResults))
	for _, result := range remoteResults {
		events = append(events, remoteResultToEvent(result))
	}
	// Apply limit if specified, preserving API's relevance order.
	if limit, ok := opts["limit"].(int); ok && limit > 0 && len(events) > limit {
		events = events[:limit]
	}
	return events, nil
}

// Reflect implements engine.Retriever by asking the remote Hindsight reflect
// endpoint. It intentionally does not fall back to local summaries.
func (r *Retriever) Reflect(ctx context.Context, query string, opts map[string]any) (string, error) {
	ctxStr, _ := opts["context"].(string)
	budget, _ := opts["budget"].(string)

	remoteText, err := r.client.Reflect(ctx, query, ctxStr, budget)
	if err == nil && remoteText != "" {
		return remoteText, nil
	}
	return "", err
}

func formatRemoteRecall(results []RecallResult) string {
	var b strings.Builder
	b.WriteString("<hindsight_memories>\n")
	for _, r := range results {
		fmt.Fprintf(&b, "- %s\n", r.Text)
	}
	b.WriteString("</hindsight_memories>")
	return b.String()
}

func remoteResultToEvent(result RecallResult) engine.MemoryEvent {
	updatedAt := result.MentionedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now()
	}
	kind := engine.MemoryKindReference
	if result.Type != "" {
		kind = engine.MemoryKind(result.Type)
	}
	id := result.ID
	if id == "" {
		id = fmt.Sprintf("%d", updatedAt.UnixNano())
	}
	return engine.MemoryEvent{
		ID:         "hindsight:" + id,
		Scope:      engine.MemoryScopeGlobal,
		Kind:       kind,
		Content:    result.Text,
		Summary:    truncateSummary(result.Text),
		Confidence: 0.8,
		Importance: 0.5,
		CreatedAt:  updatedAt,
		UpdatedAt:  updatedAt,
		Tags:       []string{"source:hindsight"},
	}
}

func recallBudget(opts map[string]any) string {
	if budget, ok := opts["budget"].(string); ok && budget != "" {
		return budget
	}
	return "mid"
}

func recallMaxTokens(opts map[string]any) int {
	if maxTokens, ok := opts["max_tokens"].(int); ok && maxTokens > 0 {
		return maxTokens
	}
	return 1024
}

func hasRecallFilters(opts map[string]any) bool {
	if opts == nil {
		return false
	}
	for _, key := range []string{"scope", "kind", "session_id", "tags"} {
		switch value := opts[key].(type) {
		case string:
			if strings.TrimSpace(value) != "" {
				return true
			}
		case []string:
			if len(value) > 0 {
				return true
			}
		}
	}
	return false
}

func recallTags(opts map[string]any) ([]string, string) {
	if opts == nil {
		return nil, ""
	}

	tags := make([]string, 0, 4)
	requireAll := false
	if configuredTags, ok := opts["tags"].([]string); ok {
		tags = append(tags, configuredTags...)
	}
	if scope, ok := opts["scope"].(string); ok && strings.TrimSpace(scope) != "" {
		tags = append(tags, "scope:"+strings.TrimSpace(scope))
		requireAll = true
	}
	if kind, ok := opts["kind"].(string); ok && strings.TrimSpace(kind) != "" {
		tags = append(tags, "kind:"+strings.TrimSpace(kind))
		requireAll = true
	}
	if sessionID, ok := opts["session_id"].(string); ok && strings.TrimSpace(sessionID) != "" {
		tags = append(tags, "session:"+strings.TrimSpace(sessionID))
		requireAll = true
	}
	if len(tags) == 0 {
		return nil, ""
	}
	if requireAll {
		return tags, "all"
	}
	return tags, "any"
}

func truncateSummary(text string) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= 120 {
		return string(runes)
	}
	return string(runes[:120]) + "..."
}
