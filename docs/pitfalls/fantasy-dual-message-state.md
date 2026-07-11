# Fantasy Dual Message State (Callback Mutations Don't Reach the Wire)

## The Problem

Modifying messages inside crush's agent callbacks (`OnToolResult`, etc.) or in the DB
does **not** change what is sent to the provider for the rest of the current run.
Within a run, the fantasy framework's internal message accumulator — not crush's
message DB — is the source of truth for every subsequent request.

## Root Cause

There are two copies of the conversation, and they only agree at run start:

1. **fantasy's in-run accumulator** — `fantasy/agent.go` appends each step's raw
   output (including **full, unmodified tool results** from `executeSingleTool`)
   to `responseMessages`. Each step's request is built from
   `initialPrompt + responseMessages` and handed to crush via
   `PrepareStepFunctionOptions.Messages`.
2. **crush's message DB** — populated by callbacks like `OnToolResult`, which
   receive a *copy* of the content and can only persist it; the callback signature
   returns `error` and cannot write back into fantasy's accumulator.

So a "truncate/compress/redact in the callback, then save" pattern silently produces
a fork: the DB (and the UI, and all history-based token estimates) show the modified
version, while the wire carries the original — until the run ends and the next run
rebuilds from the DB, at which point everything looks consistent again.

```
tool runs → fantasy keeps FULL result in responseMessages ──► next step request (full)
         └─ OnToolResult(copy) → crush truncates → DB      ──► UI / estimates / next run (truncated)
```

## Symptoms

- Context usage explodes within a single run but looks normal after restart.
- Provider-reported prompt tokens vastly exceed the history-based local estimate
  (`Built chat request token estimate` small, `Prepared provider prompt usage
  estimate` and provider usage huge).
- Logs claim mitigation happened ("Truncated tool result…") yet have no effect
  on request size.
- Unit tests pass, because tests exercise the DB-rebuild path, never the
  multi-step in-run path.

## Known Occurrences (same root cause, two disguises)

| Occurrence | Where | What happened |
|---|---|---|
| Tool-result truncation ineffective in-run | `agent.go` `OnToolResult` + `context_window.go` | Truncated only the DB copy; full results resent every step until auto-compaction fired at ~200K tokens |
| Plugin in-step compaction undone every step | `agent.go` `inStepCompactionBase` workaround | fantasy rebuilds `initialPrompt+responseMessages` each iteration, wiping the plugin's compaction; required reconstructing prepared messages from a saved base |

## Aliasing Fine Print (what `options.Messages` actually is)

`options.Messages` is built fresh each step as
`stepInputMessages := append(initialPrompt, responseMessages...)`
(`fantasy/agent.go:825`). Consequences:

- **The slice itself is per-step**; `Message` structs are copied by value.
  Top-level field writes (e.g. the existing
  `prepared.Messages[i].ProviderOptions = nil` loop) hit the per-step copy,
  not fantasy's history — but only by accident of `append` reallocating. If
  `cap(initialPrompt)` ever has slack, the prefix aliases `initialPrompt` and
  such writes persist across steps. Don't rely on this.
- **`Message.Content []MessagePart` backing arrays are shared** with the
  originals in `responseMessages`/`initialPrompt`. Writing
  `msg.Content[j] = part` or mutating through a part pointer writes straight
  into fantasy's history for all later steps. This is the aliasing that
  actually bites.

## The Fix Pattern

Any content rewrite that must affect outgoing requests has exactly two safe homes:

1. **Before fantasy sees it** — rewrite the tool's return value itself, so the
   accumulator stores the rewritten content (sent == stored == displayed).
2. **In `PrepareStep`, every step** — rewrite `prepared.Messages` after
   `prepared.Messages = options.Messages` (and after the `inStepCompactionBase`
   branch), **before** token estimation and plugin transforms. Use copy-on-write
   at both levels: copy the message slice AND the affected `Content` slices
   before replacing parts (see `applyTruncatedToolResults` in
   `context_window.go`) — part-level writes alias fantasy's history (see fine
   print above). Remember `PrepareStep` runs from scratch each step, so the
   rewrite must be idempotent and re-applied every time.

The truncation fix uses pattern 2: `OnToolResult` records truncated content in a
run-scoped `truncatedToolResults` map (ToolCallID → content, mutex-guarded — fantasy
invokes `OnToolResult` from up to 5 parallel goroutines), and `PrepareStep` swaps it
in via copy-on-write.

## Prevention Checklist

When adding any message/tool-result rewriting feature:

1. ✅ Ask: does this need to affect the **current run's** requests? If yes, DB-side
   changes are not enough — wire it through `PrepareStep` or the tool return path.
2. ✅ Never mutate message **parts** (`msg.Content[j]`) in place — part slices
   alias fantasy's internal `responseMessages`. Copy the `Content` slice first
   (copy-on-write). Top-level `Message` field writes on `prepared.Messages`
   happen to be safe today but are fragile — prefer copying there too.
3. ✅ Anything done in `PrepareStep` is redone from the full original accumulator
   next step — make it idempotent and cheap, or persist a base like
   `inStepCompactionBase` does.
4. ✅ Guard shared run-closure state touched from `OnToolResult` with a mutex
   (parallel tool execution).
5. ✅ Verify with a real multi-step run: compare `Prepared provider prompt usage
   estimate` / provider usage across steps, not just unit tests — the DB-rebuild
   path will always look correct.

## Discovery

Found while investigating runaway context growth (2026-07): four `read` calls took a
session from 35K to 199K prompt tokens and triggered auto-compaction, while the
history-based estimate said ~45K. Per-step token increments matched the raw
(untruncated) tool outputs exactly, proving the truncation logs were describing
only the DB copy.
