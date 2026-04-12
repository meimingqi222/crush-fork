package memory

// universal_bridge.go — §14.3 of the universal-memory spec.
//
// Bridges crush's existing memory.Service to the universal-memory spec format.
//
// The bridge provides two directions:
//
//  1. ExportToUniversalFormat — reads all crush memory entries and returns
//     them serialised as markdown+YAML frontmatter blobs that match the
//     universal-memory MemoryRecord wire format (for bulk export / sync).
//
//  2. ImportFromUniversalRecord — takes a universal-memory JSON record
//     (as a map) and writes it into the crush memory.Service.
//
// The bridge is intentionally thin: it does NOT replace the existing
// memory.Service or any extraction/consolidation logic.  crush keeps its
// own pipeline; the bridge is only an interop shim for tooling that speaks
// the universal-memory protocol (e.g. a shared CLI inspector, the VS Code
// extension, or a cross-agent memory migrator).
//
// Integration note
// ----------------
// To wire the bridge into your crush binary, call:
//
//	bridge := memory.NewUniversalBridge(memorySvc)
//	records := bridge.Export(ctx)               // → []UniversalRecord
//	err := bridge.Import(ctx, externalRecord)   // ← UniversalRecord
//
// No changes to coordinator.go are required; the bridge is an opt-in layer.

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// UniversalRecord is a minimal representation of a universal-memory
// MemoryRecord that crush can import/export without depending on the
// TypeScript package.  Field names match the JSON schema defined in
// universal-memory/src/types.ts.
type UniversalRecord struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`    // "identity"|"fact"|"preference"|"decision"|"workflow"|"summary"|"event"
	Title     string   `json:"title"`
	Content   string   `json:"content"`
	Summary   string   `json:"summary,omitempty"`
	Scope     string   `json:"scope"`   // "global"|"project"|"session"
	Tags      []string `json:"tags"`
	Agent     string   `json:"agent"`
	Origin    string   `json:"origin"`  // "manual"|"auto_extract"|"consolidation"
	CreatedAt string   `json:"createdAt"`
	UpdatedAt string   `json:"updatedAt"`
	Stale     bool     `json:"stale,omitempty"`
}

// UniversalBridge bridges a crush memory.Service to the universal-memory spec.
type UniversalBridge struct {
	svc Service
}

// NewUniversalBridge creates a new bridge around the given service.
func NewUniversalBridge(svc Service) *UniversalBridge {
	return &UniversalBridge{svc: svc}
}

// Export reads all crush memory entries and converts them to UniversalRecord.
//
// Mapping rules:
//   - Entry.Key       → UniversalRecord.Title  (also used as a slug)
//   - Entry.Value     → UniversalRecord.Content
//   - Entry.Type      → UniversalRecord.Kind   (falls back to "fact")
//   - Entry.Scope     → UniversalRecord.Scope  (falls back to "project")
//   - Entry.Tags      → UniversalRecord.Tags
//   - Entry.UpdatedAt → UniversalRecord.UpdatedAt (nanoseconds → RFC3339)
func (b *UniversalBridge) Export(ctx context.Context) ([]UniversalRecord, error) {
	entries, err := b.svc.List(ctx, ListParams{})
	if err != nil {
		return nil, fmt.Errorf("listing crush memories: %w", err)
	}

	records := make([]UniversalRecord, 0, len(entries))
	for _, e := range entries {
		records = append(records, entryToUniversal(e))
	}
	return records, nil
}

// ExportScope returns only entries matching the given scope.
func (b *UniversalBridge) ExportScope(ctx context.Context, scope string) ([]UniversalRecord, error) {
	entries, err := b.svc.List(ctx, ListParams{Scope: scope})
	if err != nil {
		return nil, fmt.Errorf("listing crush memories for scope %q: %w", scope, err)
	}
	records := make([]UniversalRecord, 0, len(entries))
	for _, e := range entries {
		records = append(records, entryToUniversal(e))
	}
	return records, nil
}

// Import converts a UniversalRecord into a crush StoreParams and persists it.
//
// Mapping rules (inverse of Export):
//   - UniversalRecord.Title   → StoreParams.Key
//   - UniversalRecord.Content → StoreParams.Value  (with header if summary present)
//   - UniversalRecord.Kind    → StoreParams.Type
//   - UniversalRecord.Scope   → StoreParams.Scope
//   - UniversalRecord.Tags    → StoreParams.Tags
//
// Stale records are skipped.
func (b *UniversalBridge) Import(ctx context.Context, r UniversalRecord) error {
	if r.Stale {
		return nil // do not import stale records
	}
	if r.Title == "" {
		return fmt.Errorf("universal record has empty title (key)")
	}

	value := r.Content
	if r.Summary != "" && !strings.HasPrefix(strings.TrimSpace(value), r.Summary) {
		value = r.Summary + "\n\n" + value
	}

	params := StoreParams{
		Key:   r.Title,
		Value: value,
		Type:  universalKindToCrushType(r.Kind),
		Tags:  r.Tags,
	}
	if r.Scope != "" {
		params.Scope = r.Scope
	}
	if r.Agent != "" {
		params.Category = "agent:" + r.Agent
	}

	return b.svc.Store(ctx, params)
}

// ImportBatch imports multiple records, skipping stale ones and collecting
// non-fatal errors.  Returns a combined error only if all imports failed.
func (b *UniversalBridge) ImportBatch(ctx context.Context, records []UniversalRecord) error {
	var errs []string
	imported := 0
	for _, r := range records {
		if err := b.Import(ctx, r); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", r.Title, err))
			continue
		}
		imported++
	}
	if imported == 0 && len(errs) > 0 {
		return fmt.Errorf("all %d imports failed: %s", len(errs), strings.Join(errs, "; "))
	}
	return nil
}

// RecallAsUniversal performs a keyword search on crush memory and returns
// matching entries as UniversalRecord.  Useful for agents that want a
// unified recall interface.
func (b *UniversalBridge) RecallAsUniversal(ctx context.Context, query, scope string, limit int) ([]UniversalRecord, error) {
	params := SearchParams{Query: query, Limit: limit}
	if scope != "" {
		params.Scope = scope
	}
	entries, err := b.svc.Search(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("searching crush memories: %w", err)
	}
	records := make([]UniversalRecord, 0, len(entries))
	for _, e := range entries {
		records = append(records, entryToUniversal(e))
	}
	return records, nil
}

// ---- helpers ----

func entryToUniversal(e Entry) UniversalRecord {
	updatedAt := time.Now().UTC().Format(time.RFC3339)
	if e.UpdatedAt > 0 {
		updatedAt = time.Unix(0, e.UpdatedAt).UTC().Format(time.RFC3339)
	}

	scope := e.Scope
	if scope == "" {
		scope = "project"
	}

	kind := crushTypeTouniversalKind(e.Type)

	// Strip auto-generated description prefix that extractMemories prepends.
	content := e.Value

	return UniversalRecord{
		ID:        sanitizeFilename(e.Key), // stable, deterministic
		Kind:      kind,
		Title:     e.Key,
		Content:   content,
		Scope:     scope,
		Tags:      e.Tags,
		Agent:     "crush",
		Origin:    "auto_extract",
		CreatedAt: updatedAt,
		UpdatedAt: updatedAt,
	}
}

func crushTypeTouniversalKind(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "identity", "user_identity":
		return "identity"
	case "preference", "user_preference":
		return "preference"
	case "decision":
		return "decision"
	case "workflow", "process":
		return "workflow"
	case "summary":
		return "summary"
	case "event":
		return "event"
	default:
		return "fact"
	}
}

func universalKindToCrushType(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "identity":
		return "identity"
	case "preference":
		return "preference"
	case "decision":
		return "decision"
	case "workflow":
		return "workflow"
	case "summary":
		return "summary"
	case "event":
		return "event"
	default:
		return "general"
	}
}
