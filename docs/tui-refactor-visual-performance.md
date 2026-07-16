# TUI Refactor: Visual & Performance Issues

Assessment date: 2026-07-12. Last implementation pass: 2026-07-12. Scope:
`internal/ui` rendering pipeline, with the agent-side event flow where it drives
UI behavior. Findings are marked **[confirmed]** (verified against code,
file:line cited) or **[suspected]** (mechanism identified, needs a repro before
fixing).

---

## 5. Implementation status (2026-07-12)

| Priority | Item | Status | Notes |
|---|---|---|---|
| P0 | 1.1 suppress thinking on summary messages | **Done** | `assistant.go`: skip thinking UI when `IsSummaryMessage`. |
| P0 | 1.2 strip `<think>` from summary accumulation | **Done** | `summary_text_accumulator.go` + summarize `OnTextDelta`. |
| P0 | 3.1.1 batch delta persistence + pubsub | **Done** | `stream_message_flusher.go` in main run + summarize streams. |
| P1 | 3.1.2 content-delta throttle | **Done** | `contentStreamThrottle` in `SetMessage`; first delta of a run detected via `lastInvalidation.IsZero()` so pure-content models (which never set `wasThinking`) are throttled after the first render instead of bypassing the throttle on every delta. |
| P1 | 1.3 `OnReasoningStart` in summarize | **Done** | Summarize stream callbacks. |
| P1 | 3.2 estimated-usage display | **Done** | `~` prefix via `session.EstimatedUsage` + `FormatContextUsage`. |
| P2 | 2.1 region-based hit-testing | **Done** | `clickRegions` + `HandleMouseClick`; legacy height fields reset in `invalidateCache` to prevent stale geometry when a block disappears. |
| P2 | 2.2 two-stage render pipeline | **Closed** | Not started. After review, the two-stage split already exists for thinking (`renderThinking` Phase A/B) and summary (`summaryRenderCache`); the only gap is `renderMarkdown` lacking a glamour cache for main content. Deferred to a small on-demand patch (add `contentFullRender/contentHash/contentWidth` mirroring the thinking Phase A) if profiling shows it as a hot spot. Full L refactor is not worth the risk given P0/P1 already bound the cost. |
| P2 | 3.3 cache-miss divider timing | **Done** | `maybeInsertCacheMissDivider` on finish when item already exists. |
| P1 | 3.1.3 scope sidebar/pills per delta | **Closed** | Not implemented; obsoleted by 3.1.1. `stream_message_flusher` already coalesces `UpdatedEvent` to ~7/s, `invalidateSidebarCache` is O(1) flag-set, `renderPills` early-returns when no queue, and `shouldRefreshSessionUsage` already gates on `finish.Time > 0` (part boundaries). Incremental gain ≈ 0. |

Verification: run compaction with a reasoning / `<think>`-emitting model; profile
with `task dev` to confirm fewer DB writes per stream. Follow `internal/ui/AGENTS.md`.

---

## 1. Confirmed bugs — context-compaction (summary) display

These explain the reported symptoms: "thinking shows outside the collapsed
compaction block" and "compaction content displays wrong".

### 1.1 Summarizer reasoning renders outside the summary box [confirmed]

`AssistantMessageItem.renderMessageContent`
(`internal/ui/chat/assistant.go:303-330`): for `IsSummaryMessage` messages,
reasoning content is rendered as a regular thinking indicator/box **before and
outside** the `◈ CONTEXT SUMMARY` box. The summarize stream persists reasoning
via `OnReasoningDelta` (`internal/agent/agent.go:2945-2948`), so any reasoning
model produces a stray "Thought for Xs (Ctrl+O to view)" line — or a full
thinking box — floating above the summary block.

**Fix direction**: for `IsSummaryMessage`, suppress the thinking
indicator/box entirely (the summarizer's reasoning has no user value), or
fold it into the summary box as a collapsed section. Suppression is simpler
and removes the `summaryBoxStart` bookkeeping (see 2.1).

### 1.2 Inline `<think>` tags leak into summary content [confirmed]

The summarize stream appends raw text deltas to the summary message content
(`OnTextDelta`, `internal/agent/agent.go:2958-2961`). Models served through
OpenAI-compatible proxies often emit thinking as inline `<think>…</think>`
text rather than structured `reasoning_content`; fantasy has **no** inline
think-tag extraction (`fantasy/providers/openaicompat/` handles only the
structured field). Crush strips think tags for titles, classifier, guard, and
coordinator output (`thinkTagRegex`, `internal/agent/agent.go:76`, applied in
`agent_title.go:72`, `auto_classifier.go:284`, `auto_guard.go:202`,
`coordinator.go:3846,4040`) — but **not** for summary content, and the UI
renderer strips only textual tool-call protocol
(`internal/ui/chat/assistant.go:294-299`). Result: raw thinking text renders
inside the summary box as if it were the summary.

**Fix direction**: strip/extract `<think>` blocks in the summarize
`OnTextDelta` accumulation path (streaming-safe: buffer until the closing tag
or end of stream), mirroring the existing `thinkTagRegex` usage. A UI-side
strip is a acceptable stopgap but leaves polluted content in the DB (and in
the next run's context, since the summary message becomes conversation
history — this is also a token-waste bug, not just cosmetic).

### 1.3 Summarize stream drops `OnReasoningStart` text [confirmed]

The main run loop handles `OnReasoningStart` (initial reasoning text,
`internal/agent/agent.go:1273-1276`), but the summarize stream registers only
`OnReasoningDelta`/`OnReasoningEnd` (`agent.go:2945-2957`). Providers that
deliver reasoning as a single block at start lose that text; combined with
1.2 this changes which of the two failure modes appears.

**Fix direction**: add the same `OnReasoningStart` handler.

### 1.4 Summary click hit-zone off by two lines when thinking expanded [confirmed]

`renderThinking` sets `thinkingBoxHeight` **before** appending the
`"\n\n" + footer` ("Thought for Xs") lines (`assistant.go:400-416`), and
`renderMessageContent` computes `summaryBoxStart = thinkingBoxHeight + 1`
(`assistant.go:318-325`). With expanded thinking + footer the real offset is
`thinkingBoxHeight + 3`, so the click zone in `HandleMouseClick`
(`assistant.go:765-772`) is shifted two lines up: the top of the summary box
doesn't toggle, and two lines above it do. Fixing 1.1 (no thinking on summary
messages) removes the interaction; otherwise include the footer in
`thinkingBoxHeight` or track region offsets structurally (see 2.1).

---

## 2. Structural issues (bug factories)

### 2.1 Manual Y-offset bookkeeping for mouse hit-testing [confirmed pattern]

Click regions are reconstructed from fields mutated as **side effects of the
last render** (`thinkingBoxHeight`, `summaryBoxStart`, `summaryBoxHeight` —
`assistant.go:50-53`), then compared against click Y in `HandleMouseClick`.
This is inherently fragile: 1.4 is one instance; the fields also go stale
when a block disappears between renders (they are never reset to zero), and
every new clickable block must re-derive offsets by hand.

**Refactor**: have render produce a small region list
(`[]struct{name string; start, height int}`) alongside the string — one
source of truth for both layout and hit-testing. This is a contained change
inside `AssistantMessageItem` and generalizes to tool items.

### 2.2 Six overlapping caches with three invalidation combos [confirmed pattern]

`AssistantMessageItem` carries: `cachedMessageItem` (base render),
`prefixedCache`, thinking glamour cache, `viewportLines`, stripped-regex
cache, and summary render cache (`assistant.go:40-107`), invalidated via
`invalidateCache` / `invalidateContentCache` / individual helpers
(`assistant.go:517-575`) in different combinations per event. Any new state
that affects output must be threaded through the right combo; a miss shows
stale content, an over-invalidation costs a glamour re-render. The caches are
also all single-entry width-keyed, so any resize throws everything away.

**Refactor**: collapse to a two-stage pipeline per item — (a) expensive
glamour render keyed by `xxh3(content) + width`, (b) cheap composition
(truncation, boxing, prefix, highlight) recomputed every frame from (a).
Stage (b) at ~1-5ms/item for visible items only (the list is lazy) is
affordable and deletes most invalidation logic.

---

## 3. Performance issues

### 3.1 Per-token DB write + pubsub event + full re-render [confirmed]

Every streaming delta calls `a.messages.Update(...)`
(`internal/agent/agent.go:1309-1314` text, `1277-1279` reasoning; same in the
summarize stream) → SQLite write + pubsub `UpdatedEvent` **per token**. In the
UI each event runs `updateSessionMessage` → `SetMessage` →
`invalidateCache()` (`internal/ui/model/ui.go:2103+`,
`internal/ui/chat/assistant.go:708-743`), plus `invalidateSidebarCache()` and
`renderPills()` per event (`ui.go:944-979`).

Only **thinking** deltas are throttled (200ms, `thinkingStreamThrottle`);
**content** deltas invalidate unconditionally (`assistant.go:719-722`), and
`renderMarkdown` has no cache (`assistant.go:488-493`), so a streaming
response glamour-renders its full accumulated text on every delta — O(n²)
work over the stream. The same applies to summary streaming via
`renderSummary` (hash changes every delta).

**Refactor** (in order of leverage):
1. Batch delta persistence agent-side: accumulate and flush on a ~100-200ms
   ticker and at part boundaries, collapsing DB writes and pubsub events.
   This fixes cost at the source for every subscriber.
2. Extend the existing thinking throttle to content deltas (UI-side quick
   win; one condition in `SetMessage`).
3. Scope per-event side effects: sidebar/pills invalidation only on part
   boundaries or finish, not per delta.

### 3.2 Estimated usage displayed as real usage [confirmed, partially mitigated]

`PrepareStep` writes `estimatedPromptTokens` into the assistant message usage
before the request (`internal/agent/agent.go:1265-1270`); the context meter
shows the estimate, then snaps to provider-reported values at step finish.
Char/4 estimates overshoot by 10-40%, so the meter visibly jumps every step.
(The historical 5x jumps were the truncation bug, fixed 2026-07 — see
`docs/pitfalls/fantasy-dual-message-state.md`.)

**Refactor**: mark estimate-sourced usage in the message (flag exists:
`usage_estimated` in logs) and render it distinctly (e.g. `~` prefix or muted
style) in `internal/ui/model/context_usage.go`, or don't move the meter until
provider-confirmed.

### 3.3 Cache-miss divider only appears on history reload [suspected]

The divider is inserted in `updateSessionMessage` only when
`existingItem == nil && msg.IsFinished()` (`ui.go:2133-2145`), but in live
streaming the assistant item is created at `CreatedEvent`
(`appendSessionMessage`, `ui.go:2010+`; empty assistant messages pass
`ShouldRenderAssistantMessage`), so `existingItem != nil` and the divider
never inserts live. Verify with a live cache-miss, then move detection to the
finish transition regardless of item existence.

---

## 4. Suggested execution order

See **§5 Implementation status** for what is already landed. Original plan:

| Priority | Item | Where | Size |
|---|---|---|---|
| P0 | 1.1 suppress thinking on summary messages | `assistant.go` | S |
| P0 | 1.2 strip `<think>` from summary accumulation | `agent.go` summarize | S-M |
| P0 | 3.1.1 batch delta persistence + pubsub | `agent.go` | M |
| P1 | 3.1.2 content-delta throttle (if 3.1.1 deferred) | `assistant.go` | S |
| P1 | 1.3 `OnReasoningStart` in summarize | `agent.go` | S |
| P1 | 3.2 estimated-usage display | `context_usage.go` | S |
| P2 | 2.1 region-based hit-testing (subsumes 1.4) | `assistant.go`, `chat/` | M |
| P2 | 2.2 two-stage render pipeline | `chat/` items | L |
| P2 | 3.3 cache-miss divider timing | `ui.go` | S (after repro) |

Verification: for P0 items, drive a real compaction with a reasoning model
(and once with a `<think>`-emitting proxy model) and confirm the summary
block renders self-contained; for 3.1, compare per-delta CPU/DB-write counts
before/after with `task dev` profiling. Follow
`internal/ui/AGENTS.md` conventions for any UI change.
