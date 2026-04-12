package memory

// service_adapter.go — DEPRECATED: This file wraps the file-backed memory.Service.
// Use ManagedRuntimeClient (stdio JSON-RPC) for the canonical universal-memory runtime instead.
// This adapter is kept for backward compatibility only.

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const freshnessWarnAfter = 30 * 24 * time.Hour

// ServiceAdapter adapts the file-backed Service to the MemoryClient interface.
// It wires up the three hook points using the same logic that currently lives
// in agent/recall.go and agent/memory_recall.go, but behind the clean interface.
type ServiceAdapter struct {
	svc Service
	// autoRecall is the function currently used to build recall strings.
	// Injected at construction so the adapter doesn't import the agent package.
	autoRecall func(ctx context.Context, query, scope, sessionID string) ([]string, error)
	// extract is the function currently used to run memory extraction.
	extract func(ctx context.Context, req ExtractRequest) error
	// consolidate is the function currently used to run the memory dream.
	consolidate func(ctx context.Context, req ConsolidateRequest) error
}

// ServiceAdapterOptions holds the optional hook functions.  If a function is
// nil, the corresponding MemoryClient method becomes a no-op.
type ServiceAdapterOptions struct {
	AutoRecall  func(ctx context.Context, query, scope, sessionID string) ([]string, error)
	Extract     func(ctx context.Context, req ExtractRequest) error
	Consolidate func(ctx context.Context, req ConsolidateRequest) error
}

// NewServiceAdapter creates a MemoryClient that delegates storage operations
// to the existing Service and optional higher-level hooks to the provided fns.
func NewServiceAdapter(svc Service, opts ServiceAdapterOptions) MemoryClient {
	return &ServiceAdapter{
		svc:         svc,
		autoRecall:  opts.AutoRecall,
		extract:     opts.Extract,
		consolidate: opts.Consolidate,
	}
}

func (a *ServiceAdapter) Recall(ctx context.Context, query string, scope string, sessionID string) ([]string, error) {
	if a.autoRecall != nil {
		return a.autoRecall(ctx, query, scope, sessionID)
	}
	// Fallback: search full Value content (not just truncated Description)
	// This ensures Extract()'d transcript content is searchable
	entries, err := a.svc.Search(ctx, SearchParams{
		Query: query,
		Scope: scope,
		Limit: 20,
	})
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, e := range entries {
		desc := excerptForQuery(e.Value, query)
		lines = append(lines, fmt.Sprintf("- %s: %s", e.Key, desc))
	}
	return lines, nil
}

// AppendMessages is a no-op for the legacy file-backed adapter.
// The ServiceAdapter does not maintain a journal; use ManagedRuntimeClient instead.
func (a *ServiceAdapter) AppendMessages(_ context.Context, _ string, _ []AppendMessage) error {
	// No-op: legacy file-backed adapter doesn't have journal support
	return nil
}

func (a *ServiceAdapter) Extract(ctx context.Context, req ExtractRequest) error {
	if a.extract != nil {
		return a.extract(ctx, req)
	}
	// Fallback: store transcript as a simple memory record
	// This provides basic extraction semantics without LLM
	if len(req.Transcript) == 0 {
		return nil
	}
	content := strings.Join(req.Transcript, "\n")
	return a.svc.Store(ctx, StoreParams{
		Key:   fmt.Sprintf("session-%s-extract-%d", req.SessionID, time.Now().Unix()),
		Value: content,
		Scope: req.Scope,
		Tags:  []string{"extract", req.Agent, "session"},
	})
}

func (a *ServiceAdapter) Consolidate(ctx context.Context, req ConsolidateRequest) error {
	if a.consolidate != nil {
		return a.consolidate(ctx, req)
	}
	// Fallback: basic consolidation without LLM
	// This provides basic consolidation by updating timestamp
	now := time.Now()
	if !req.Force {
		lastConsolidated, err := a.svc.ReadLastConsolidatedAt()
		if err == nil && !lastConsolidated.IsZero() {
			age := now.Sub(lastConsolidated)
			if age < 24*time.Hour {
				return nil
			}
		}
	}
	// Update last consolidated timestamp
	return a.svc.WriteLastConsolidatedAt(now)
}

func (a *ServiceAdapter) FreshnessStatus(_ context.Context) (FreshnessResult, error) {
	infos, err := a.svc.ListMemoryFiles()
	if err != nil {
		return FreshnessResult{}, err
	}
	lastAt, err := a.svc.ReadLastConsolidatedAt()
	if err != nil {
		return FreshnessResult{}, err
	}
	hasMemories := len(infos) > 0
	warning := adapterFreshnessWarning(time.Now(), lastAt, hasMemories)
	return FreshnessResult{HasMemories: hasMemories, Warning: warning}, nil
}

func adapterFreshnessWarning(now, lastAt time.Time, hasMemories bool) string {
	if !hasMemories {
		return ""
	}
	if lastAt.IsZero() {
		return "Memory stale: never consolidated — run /dream"
	}
	age := now.Sub(lastAt)
	if age < freshnessWarnAfter {
		return ""
	}
	days := int(age.Hours() / 24)
	return fmt.Sprintf("Memory stale: last consolidated %d days ago — run /dream", days)
}

func excerptForQuery(value, query string) string {
	desc := truncateForDescription(value)
	query = strings.TrimSpace(query)
	if query == "" {
		return desc
	}

	valueLower := strings.ToLower(value)
	queryLower := strings.ToLower(query)
	idx := strings.Index(valueLower, queryLower)
	if idx < 0 {
		return desc
	}

	runes := []rune(value)
	start := len([]rune(value[:idx]))
	end := start + len([]rune(value[idx:idx+len(query)]))
	from := start - 40
	if from < 0 {
		from = 0
	}
	to := end + 40
	if to > len(runes) {
		to = len(runes)
	}

	snippet := string(runes[from:to])
	if from > 0 {
		snippet = "…" + snippet
	}
	if to < len(runes) {
		snippet += "…"
	}
	return snippet
}

func (a *ServiceAdapter) Store(ctx context.Context, params StoreParams) error {
	return a.svc.Store(ctx, params)
}

func (a *ServiceAdapter) Get(ctx context.Context, key string) (Entry, error) {
	return a.svc.Get(ctx, key)
}

func (a *ServiceAdapter) Delete(ctx context.Context, key string) error {
	return a.svc.Delete(ctx, key)
}

func (a *ServiceAdapter) Search(ctx context.Context, params SearchParams) ([]Entry, error) {
	return a.svc.Search(ctx, params)
}

func (a *ServiceAdapter) List(ctx context.Context, params ListParams) ([]Entry, error) {
	return a.svc.List(ctx, params)
}
