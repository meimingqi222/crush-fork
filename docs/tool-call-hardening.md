# Tool-Call Hardening: Reference Survey & Gap Analysis

Date: 2026-07-12 (revised 2026-07-12 to reflect P0 completion and the
pre-existing Harmony dialect recovery). Sources surveyed:
`D:\code\copilot-refs\opencode` (TypeScript, AI SDK based) and
`D:\code\copilot-refs\oh-my-pi` (TypeScript, custom provider layer). Goal:
catalog what they do to survive malformed tool calls from weak models,
compare against crush/fantasy's current pipeline, and list concrete
reinforcement work.

---

## 1. What the reference projects do

### 1.1 opencode

- **`experimental_repairToolCall` hook** (`packages/opencode/src/session/llm.ts:296-312`):
  on any failed tool call, first try lowercasing the tool name against the
  registered tool map; if that fails, **rewrite the call to a registered
  `invalid` tool** with input `{tool, error}`.
- **The `invalid` tool trick** (`packages/opencode/src/tool/invalid.ts`): a
  hidden tool (excluded from `activeTools` so the model can never call it
  directly) whose execute returns "The arguments provided to the tool are
  invalid: <error>". The failed call thus becomes a *well-formed*
  tool_use/tool_result pair — the conversation stays protocol-valid for strict
  providers, and the model gets a structured, self-correctable error.
- **Code mode** (`packages/codemode`): an alternative paradigm where the model
  writes code that calls tools as functions. Out of scope for crush; noted for
  completeness.

### 1.2 oh-my-pi

A layered defense, deepest of the two:

1. **Tolerant JSON parsing** (`packages/utils/src/json-parse.ts`): a full
   relaxed parser for tool arguments — string-level `repairJson` (raw control
   chars, invalid escapes), single quotes, Python `True`/`False`/`None`,
   bareword keys, and **streaming-partial parse with rollback** (incomplete
   trailing values roll back to the last valid prefix instead of committing
   junk). Unparseable input flows into validation as a `__parseError` marker
   so the error message can include the raw JSON excerpt.
2. **Argument coercion pipeline**
   (`packages/ai/src/utils/validation.ts:1399-1506`): normalize
   double-JSON-encoded keys (`{"\"op\"": ...}`), strip null/`"null"`/empty
   placeholders on optional fields, substitute schema defaults, re-shape
   JSON-stringified arrays in `string|string[]` unions, then up to 5
   issue-driven coercion passes (numeric strings, JSON-string containers,
   unrecognized keys).
3. **Error-message engineering**: per-path issue list plus the received
   arguments (`original` + `normalized`), with **per-field truncation at 256
   chars** so a failed 300KB `write` call doesn't round-trip its payload back
   through the model (`truncateArgsForError`, validation.ts:1374-1389).
4. **Per-tool escape hatch**: `tool.lenientArgValidation` executes with raw
   args when validation fails (`packages/agent/src/agent-loop.ts:1832`).
5. **Leaked-thinking stream healing**
   (`packages/ai/src/utils/leaked-thinking-stream.ts`): a live projection
   applied to every non-first-party provider stream that splits leaked
   `<think>` / ```` ```thinking ```` / Harmony-channel markup out of the
   visible text channel into proper structured thinking blocks, idempotently.
   Fixes the whole class of "reasoning text pollutes content" bugs at the
   stream layer instead of patching consumers.
6. **In-band tool dialects** (`packages/ai/src/dialect/`): per-model-family
   textual tool-call grammars (qwen3, kimi, glm, minimax, hermes, harmony,
   gemma, deepseek, generic XML…) with prompt rendering, few-shot examples,
   argument coercion, and history re-encoding — so models with weak or absent
   native function calling still drive tools reliably. The stream is parsed
   and **executed**, not discarded.

---

## 2. crush / fantasy current state

Already in place (some added 2026-07-12):

| Capability | Where | Parity |
|---|---|---|
| Case-insensitive + alias tool-name repair, conservative param-key rename | `internal/agent/tool_call_repair.go` via `fantasy.WithRepairToolCall` | ≥ opencode's lowercase repair |
| "tool not found" error lists available tools (≤20) | `fantasy/agent.go` `toolNotFoundMessage` | ≈ opencode's invalid-tool message |
| Failed calls still produce error tool results (protocol-valid pairing) | `fantasy` executeSingleTool / `Invalid` flag | ≈ opencode invalid tool |
| Schema-driven arg repair: optional-null normalization, defaults, issue-driven coercion passes, JSON-string containers (with `jsonrepair` on nested values) | `fantasy/tool_validation.go` | ≈ oh-my-pi pipeline (largely ported) |
| **Top-level JSON repair fallback** (single quotes, bareword keys, Python literals, truncated JSON) | `fantasy/tool_validation.go` `parseToolCallInput` (P0-a, 2026-07-12) | ≈ oh-my-pi `repairJson` (added) |
| **Arg-echo truncation** (256-char per-field cap) in validation errors | `fantasy/tool_validation.go` `truncateArgsForError` (P0-b, 2026-07-12) | ≈ oh-my-pi `truncateArgsForError` (added) |
| Harmony textual tool-call protocol: **parse + execute + retry**, not just strip | `internal/agent/agent_recovery.go` (`parseTextualToolCallsFromAssistant`, `recoverTextualToolCallProtocol`, retry/repeat caps) | ≈ oh-my-pi Harmony dialect (parity for Harmony; other dialects still parse-and-execute-on-emit) |
| Textual tool-call protocol **stripping** from persisted content/reasoning (defense-in-depth after recovery executes) | `internal/message/content.go` `StripTextualToolCallProtocol`; `internal/agent/agent_recovery.go` `stripTextualToolCallProtocolFromAssistant` | crush-specific cleanup layer |
| `<think>` stripping for titles/classifier/guard/coordinator/summary | `thinkTagRegex` call sites; `summary_text_accumulator.go` | Point fixes — oh-my-pi heals at stream layer |
| Role-reminder rewrite of `tool not found: edit/write/bash` | `internal/agent/agent_tool_result.go:101-104` | crush-specific, keep |

---

## 3. Gaps and recommended reinforcements

### P0-a: Repair top-level argument JSON before rejecting [small]

`fantasy/agent.go` `validateToolCall` does a strict
`json.Unmarshal(toolCall.Input)` and fails with "invalid JSON input"
(~line 1103; same for provider tools ~1093). The in-repo
`fantasy/jsonrepair` package is only used for *nested* string values inside
arguments (`tool_validation.go:279,378`) — never on the top-level input.
Weak models emit single quotes, trailing commas, unescaped newlines, Python
literals; all die here today.

**Do**: on top-level unmarshal failure, retry with
`jsonrepair.RepairJSON(input)`; only fail if repair also doesn't parse, and
include a truncated raw-input excerpt in the error (oh-my-pi's
`__parseError` pattern) so the model can self-correct.

### P0-b: Truncate argument echos in validation errors [small]

`formatToolValidationError` (`fantasy/tool_validation.go:124-149`) marshals
the **full** original+normalized arguments into the error, which becomes a
tool result sent back to the model. A failed `write`/`edit` call with a large
`content` field round-trips hundreds of KB — a token bomb, and on this fork
it also fights the step tool-result budget. oh-my-pi caps each string field
at 256 chars.

**Do**: port `truncateArgsForError` (per-field cap ~256 chars, recursive)
before marshaling.

### P1-a: Leaked-thinking healing at the stream layer [medium]

crush strips `<think>` in five separate consumers and still leaks elsewhere
(the summary fix was the fifth patch of the same bug). oh-my-pi solves the
class once: wrap non-first-party provider streams in a projector that moves
leaked thinking markup from text deltas into reasoning deltas, live and
idempotently.

**Do**: implement in fantasy's stream normalization (or a crush-side stream
wrapper around `OnTextDelta`): detect `<think>`/```thinking fences at delta
granularity, re-emit as reasoning callbacks, buffer across delta boundaries
(tags split between deltas). Gate off for providers known to return
structured reasoning. Then the five call-site strips become defense-in-depth
instead of the only line.

### P1-b: Feed repaired-name telemetry back into prompts [small]

The new repair hook logs `Repaired misnamed tool call`. If a session repairs
the same model repeatedly, one line in the *next* request's system suffix
("Note: tool names are lowercase: bash, grep, glob, view…") is cheaper than
repairing every call. Requires only a per-session counter + prompt-suffix
injection, both mechanisms already exist (`PromptSuffix` in request state).

### P2-a: Per-tool lenient-validation escape hatch [small, low-ROI]

oh-my-pi's `lenientArgValidation` lets forgiving tools (e.g. bash — a single
string arg) execute on raw args when validation fails instead of bouncing.
Worth adding to `fantasy.ToolInfo` as an opt-in flag only when concrete
evidence shows a tool whose executor already validates defensively but
whose schema rejects common model output. No crush tool currently declares
this need; P0-a's top-level JSON repair already absorbs most malformed-input
scenarios. Skip until a real case appears.

### P2-b: Additional in-band tool dialects [large, partial-coverage]

**Status:** Harmony dialect is already implemented (see §2 table —
`parseTextualToolCallsFromAssistant` parses and executes, with retry/repeat
caps). Remaining gap is coverage of other model-family dialects
(qwen3, kimi, glm-non-Harmony, minimax, hermes, gemma, deepseek, generic
XML) that oh-my-pi ships.

oh-my-pi's dialect system is a provider-layer subsystem (grammars +
examples + history re-encoding per model family). Building the rest
speculatively is not justified. Trigger to revisit: if logs show
`parseTextualToolCallsFromAssistant` returning empty for models that *are*
emitting textual tool-call protocol (i.e., a non-Harmony dialect that the
regex misses), or `shouldRetryForTextualToolCallProtocol` retries spiking
for a specific model, start by adding that single dialect's grammar behind
a per-model config flag. The Harmony implementation in
`internal/agent/agent_recovery.go` is the template to extend.

### Not adopted

- **opencode code mode**: different execution paradigm (model writes code
  calling tools); orthogonal to crush's architecture.
- **opencode `invalid` tool routing**: crush already achieves the same
  protocol-validity via fantasy's `Invalid` flag + error tool results; the
  remaining delta (message quality) was closed by `toolNotFoundMessage`.

---

## 4. Suggested order

| Priority | Item | Layer | Size | Status |
|---|---|---|---|---|
| P0-a | jsonrepair on top-level tool args | fantasy | S | Done (2026-07-12) |
| P0-b | truncate arg echos in validation errors | fantasy | S | Done (2026-07-12) |
| P1-a | leaked-thinking stream healing | fantasy (or crush stream wrapper) | M | Open |
| P1-b | repeated-repair prompt nudge | crush | S | Open |
| P2-a | lenient-validation flag | fantasy + tools | S | Skip until evidence (low ROI) |
| P2-b | additional in-band dialects (Harmony done; qwen/kimi/glm/… remain) | fantasy | L (deferred) | Partial (Harmony); others on-demand |

### P0 verification (done)

Unit tests in `fantasy/default_repair_test.go` cover:

- P0-a `parseToolCallInput`: strict JSON passthrough, single-quoted JSON,
  bareword keys, Python `True`/`False` literals, truncated JSON, unrepairable
  input (raw excerpt in error), long-input excerpt truncation. Integration:
  `validateToolCall` propagates repaired JSON for single-quoted and bareword-key
  inputs.
- P0-b `truncateArgsForError`: short strings unchanged, long strings capped at
  256 runes + `…`, recursive into nested maps and arrays, non-string values
  preserved. Integration: `formatToolValidationError` with a 300KB `content`
  field produces an error under 4KB.

Existing tests remain green, including `TestToolCallRepair/Invalid JSON in tool
call` (`{invalid json}` still correctly rejected — not repairable).

Verification for P1-a: replay a `<think>`-leaking proxy model and assert
reasoning lands in reasoning parts across split-delta boundaries.
