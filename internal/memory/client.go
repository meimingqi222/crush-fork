package memory

// client.go — MemoryClient is the single abstraction that crush uses to
// interact with its memory backend.
//
// The interface is intentionally narrow: it contains only the operations that
// crush actually needs at its three hook points:
//
//   Hook A  onUserMessage  → Recall(query) to inject <auto_recall>
//   Hook B  onAssistantTurnEnd → Extract(transcript) to persist new memories
//   Hook C  onSessionIdle (Dream) → Consolidate() to merge/prune memories
//
// Plus the LLM-callable tool surface (Store / Get / Delete / Search / List).
//
// Concrete implementations:
//   - NullMemoryClient        — no-op, used when memory is disabled
//   - ManagedRuntimeClient    — recommended, manages universal-memory runtime via stdio JSON-RPC
//   - HTTPMemoryClient        — DEPRECATED, delegates to HTTP sidecar (use ManagedRuntimeClient instead)
//   - ServiceAdapter          — wraps the original file-backed memory.Service for backward compatibility

import "context"

// MemoryClient is the minimal interface crush calls for all memory operations.
type MemoryClient interface {
	// ---- Hook 0: append messages to journal ----

	// AppendMessages appends conversation messages to the memory journal.
	// This should be called after each user/assistant turn to maintain a complete
	// conversation history for memory extraction and journal-based recall.
	AppendMessages(ctx context.Context, sessionID string, messages []AppendMessage) error

	// ---- Hook A: recall (called before each LLM request) ----

	// Recall returns lines of relevant memory text for the given query.
	// The caller wraps the result in <auto_recall>…</auto_recall> and injects
	// it into the system prompt.  Returns nil/empty when nothing is relevant.
	Recall(ctx context.Context, query string, scope string, sessionID string) ([]string, error)

	// ---- Hook B: extract (called after assistant turn ends) ----

	// Extract asks the memory backend to analyse the conversation transcript
	// and persist any durable memories it finds.  Fire-and-forget: crush calls
	// this in a goroutine and ignores the error in production.
	Extract(ctx context.Context, req ExtractRequest) error

	// ---- Hook C: consolidate / dream ----

	// Consolidate asks the memory backend to merge, deduplicate and prune
	// existing memories.  May be a no-op if the backend decides it is too
	// soon or if no background model is available.
	Consolidate(ctx context.Context, req ConsolidateRequest) error

	// FreshnessStatus returns a human-readable freshness warning string and
	// whether any memories exist.  Empty warning = memories are fresh.
	FreshnessStatus(ctx context.Context) (FreshnessResult, error)

	// ---- LLM tool surface ----

	Store(ctx context.Context, params StoreParams) error
	Get(ctx context.Context, key string) (Entry, error)
	Delete(ctx context.Context, key string) error
	Search(ctx context.Context, params SearchParams) ([]Entry, error)
	List(ctx context.Context, params ListParams) ([]Entry, error)
}

// AppendMessage represents a single message to append to the journal.
type AppendMessage struct {
	Role      string // "user", "assistant", or "system"
	Content   string
	Timestamp string // ISO 8601 timestamp, optional
}

// ExtractRequest carries the conversation context needed for memory extraction.
type ExtractRequest struct {
	SessionID  string
	Scope      string   // memory scope (global/project/session)
	Agent      string   // agent name
	Prompt     string   // the original user prompt that started the turn
	Transcript []string // ["USER: …", "ASSISTANT: …", …]
}

// ConsolidateRequest carries context for the consolidation/dream operation.
type ConsolidateRequest struct {
	SessionID string
	Force     bool
}

// FreshnessResult holds the outcome of FreshnessStatus.
type FreshnessResult struct {
	HasMemories bool
	Warning     string // non-empty when memories are stale
}
