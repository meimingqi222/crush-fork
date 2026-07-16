# ACP Implementation: Gap Analysis & Hardening Plan

Date: 2026-07-12. Scope: `internal/acp/` (types.go, server.go, handler.go,
client.go, adapter.go, timeline.go) plus its wiring in `internal/cmd/acp.go`.
Goal: catalog the gaps found in a full review of the ACP server against the
Agent Client Protocol spec and against crush's own tool surface, with
concrete, independently implementable work items for each.

Each item below is self-contained: symptom, root cause with file:line
references, and a suggested fix. Items are ordered by priority. An
implementing agent can pick them off one at a time; none of them depend on
each other unless explicitly noted.

Verification baseline: `go test -race ./internal/acp/...` currently passes
(1230-line test file, `acp_test.go`, uses in-memory fakes — see
`fakeSessionService` / `fakeMessageService` there for the mock pattern).
Every fix should come with a test in that style.

---

## P0 — Advertised capabilities the implementation does not honor

These are protocol violations: `initialize` promises a capability, clients
rely on it, and the data is silently dropped.

### P0.1 Image and EmbeddedContext prompt blocks are discarded

- **Symptom**: a client sends an image block or an embedded-resource block in
  `session/prompt`; the content silently vanishes — no error, no fallback.
- **Cause**: `handleInitialize` advertises
  `PromptCapabilities{Image: true, EmbeddedContext: true}`
  (`internal/acp/handler.go:136-139`), but the prompt path flattens content
  through `extractText` (`internal/acp/handler.go:758-769`), which only
  handles `type == "text"` blocks. Note `ContentBlock` (types.go:17) has
  `Data`/`MIMEType`/`URI` fields for image/audio, but there is **no
  `Resource` field at all**, so embedded-context blocks (`type: "resource"`
  per ACP spec) don't even survive JSON decoding beyond the `Type` tag.
- **Fix**:
  1. Add a `Resource` field to `ContentBlock` matching the ACP spec shape
     (`resource: { uri, mimeType, text }`).
  2. In `handleSessionPrompt`, map image blocks to crush attachments —
     `coordinator.Run` already accepts `...message.Attachment`
     (`internal/acp/handler.go:323` currently passes none). Base64 `Data` +
     `MIMEType` translate directly.
  3. Map resource blocks to prompt text (e.g. fenced block with the URI as
     header), which is how embedded context is conventionally inlined.
  4. Only if attachment plumbing turns out to be blocked: flip the advertised
     capabilities to `false` instead — an honest denial is spec-conformant,
     silent dropping is not.

### P0.2 `mcpServers` from session/new and session/load is ignored

- **Symptom**: client passes MCP server configs when creating/loading a
  session (Zed does this for project-configured MCP servers); they never get
  connected.
- **Cause**: `initialize` advertises `MCP: {HTTP: true, SSE: true}`
  (`handler.go:140-143`); `SessionNewParams.MCPServers` and
  `SessionLoadParams.MCPServers` (types.go:128-145) are decoded but never
  read — `handleSessionNew` (handler.go:158-184) and `handleSessionLoad`
  (handler.go:187-216) do not reference `params.MCPServers` at all.
- **Fix**: wire the configs into the MCP client manager
  (`internal/agent/tools/mcp`). The manager already supports stdio/http/sse
  transports and runtime (re)connection — see `mcp.WaitForInit` usage in
  `internal/cmd/acp.go:50` and the tools refresh that follows via
  `UpdateModels`. After registering the client-supplied servers, trigger the
  same refresh path so their tools appear. If the session ends, these
  session-scoped servers should be disconnected (track them per session in
  the Handler). If this is deemed out of scope for now, remove the `MCP`
  capability from initialize instead.

### P0.3 Diff rendering misses crush's own edit tool (dead adapter code)

- **Symptom**: IDE clients never receive `diff` content for crush's primary
  `edit` tool — file edits render as plain text output. Meanwhile the diff
  code that does exist targets tools crush doesn't have.
- **Cause**: `getToolDiffContent` (`handler.go:1190-1257`) matches tool names
  `replace_file_content`, `multi_replace_file_content`, `write_to_file` and
  params `TargetContent`/`ReplacementContent`/`CodeContent` — these are
  another agent's (Windsurf-style) names, ported here and now dead code.
  Crush's real tools: `edit` with `old_string`/`new_string`/`file_path`
  (`internal/agent/tools/edit.go:31-32,69`) and `write` with
  `content`/`file_path` (`internal/agent/tools/write.go:45`). Only `write`
  accidentally works via the `"content"` key fallback; `edit` falls through
  and returns nil.
- **Fix**:
  1. Add an `edit` branch: `OldText` ← `old_string`, `NewText` ←
     `new_string`, path ← `file_path`. (Verified: crush's `edit` tool has no
     multi-edit variant, so a single old/new pair suffices.)
  2. Delete the Windsurf-style branches (`replace_file_content`,
     `multi_replace_file_content`, `TargetContent` extraction) — they can
     never match crush tool calls.
  3. Same cleanup applies to the foreign tool names sprinkled through
     `GetToolKind` (`client.go:40-61`) and `GetBeautifulTitle`
     (`client.go:100-247`): `view_file`, `run_terminal_command`, `warp_grep`,
     `search_context`, etc. Keep only names crush emits (built-in tools +
     `fs/`-prefixed ACP names if used) — or, if MCP tools with arbitrary
     names must map too, say so in a comment instead of carrying a silent
     foreign-agent list.

---

## P1 — Protocol-conformance bugs

### P1.1 `stopReason` never reflects the actual finish reason

- **Symptom**: clients always get `end_turn` (or `cancelled`); `max_tokens`,
  `refusal`, `max_turn_requests` are defined (types.go:36-41) but unreachable.
- **Cause**: in `handleSessionPrompt`, the run goroutine delivers
  `runResult{result, err}` (handler.go:322-325) and the receive path checks
  only `r.err` — **`r.result` is never read** (handler.go:332-339).
- **Fix**: map `r.result` (a `*fantasy.AgentResult`) finish reason to the ACP
  StopReason before returning `PromptResult`. Check what
  `fantasy.AgentResult` exposes (`fantasy/agent.go`) — it carries the final
  step's finish reason; map `length`→`max_tokens`, refusal→`refusal`,
  max-steps→`max_turn_requests`, default→`end_turn`.

### P1.2 History replay emits an invalid ToolKind and skips title beautification

- **Symptom**: after `session/load`, replayed tool calls carry
  `"kind": "tool"` — not a member of the ACP kind enum — and raw tool names
  as titles. Strict clients may reject; lenient ones render inconsistently
  with live streaming (which uses `GetToolKind`/`GetBeautifulTitle`).
- **Cause**: `replayHistory` hardcodes `Kind: "tool"` (handler.go:252) and
  `Title: tc.Name` (handler.go:251), bypassing the helpers the live path
  uses (compare handler.go:478-480).
- **Fix**: parse `tc.Input`, then use `GetToolKind(tc.Name)` and
  `GetBeautifulTitle(tc.Name, "", inputParams)` exactly as
  `handleMessageEvent` does. Consider extracting a small shared helper so
  replay and live can't drift again.

### P1.3 `RawInput` is sent as a JSON string, not an object

- **Symptom**: clients receive `"rawInput": "{\"file_path\": ...}"` (a
  string) from the message-event path, but a real object from the permission
  path — inconsistent, and the spec expects an object.
- **Cause**: `handleMessageEvent` passes `RawInput: tc.Input`
  (handler.go:500,513) where `tc.Input` is the raw JSON string; the parsed
  `inputParams` is already available right above (handler.go:474-477).
- **Fix**: pass `inputParams` (fall back to the string only if parsing
  failed).

### P1.4 Plan updates (`SessionUpdatePlan`) are never emitted

- **Symptom**: IDEs that render agent plans get nothing; the update type and
  `PlanEntry` exist (types.go:253, 343-348) with zero senders repo-wide.
- **Cause**: no bridge from crush's todos state to ACP plan entries.
- **Fix**: the todos tool persists per-session todo state
  (`internal/agent/tools/todos.go`, backed by the session service). In the
  prompt event loop, when a todos tool result is observed (tool name match in
  `handleMessageEvent`), translate the todo list into `[]PlanEntry`
  (`pending`/`in_progress`/`completed` map directly) and send a
  `SessionUpdatePlan` update. Keep it best-effort: failure to parse todos
  must not disturb the normal tool-call updates.

---

## P2 — Resource leaks & performance on the streaming hot path

### P2.1 `activeToolParams` leaks on cancelled/failed turns

- **Symptom**: long-lived ACP process (editors keep it running for days)
  slowly accumulates orphaned entries.
- **Cause**: entries are added on every tool call observed
  (handler.go:481) but removed only in `sessionUpdateFromToolResult`
  (handler.go:670) — which requires a tool *result* to arrive. Cancelled
  runs, provider errors, or dropped subscriptions orphan the entry forever.
  `sessionCWD` (handler.go:42) similarly only grows, though it's small.
- **Fix**: sweep per turn — `handleSessionPrompt` already knows every
  tool-call ID it saw (`readBytes` keys with the `:tc:` infix); in its defer,
  delete those IDs from `activeToolParams`. That bounds the map by the
  currently-running turns regardless of how they end.

### P2.2 Uncached DB lookups per streaming event

- **Symptom**: with an active subagent, every message/runtime event for the
  child session triggers a `sessions.Get` DB query; with unrelated concurrent
  sessions, every one of their events also triggers a query. On a chatty
  stream this is hundreds of queries per turn.
- **Cause**: two call sites, no caching:
  - `getSubagentPrefix` (handler.go:1156-1177) — queries the session and
    string-parses its *title* on every event to derive a display prefix.
  - `shouldForwardSessionEvent` (handler.go:735-755) — caches positive
    matches in `trackedSessionIDs` but re-queries for every negative event.
- **Fix**: both are per-prompt-turn concerns; `handleSessionPrompt` already
  owns per-turn maps (`readBytes`, `runtimeSnapshotHashes`). Add a
  `prefixCache map[string]string` and a `rejectedSessionIDs
  map[string]struct{}` with the same lifetime and thread them through. Note
  the title-parsing in `getSubagentPrefix` (matching `"(@name subagent)"`)
  is fragile — if the subagent session type is available on the session row
  (check `session.Session` fields), prefer structured data over parsing
  display strings.

### P2.3 A >4MB message kills the whole server

- **Symptom**: one oversized JSON-RPC line (large embedded file, big base64
  image) makes `bufio.Scanner` return `ErrTooLong`, `Serve` returns, the
  process exits, and the editor's agent connection dies.
- **Cause**: `server.go:143-144` caps the scanner buffer at 4MB and
  `Serve` treats any scanner error as fatal (server.go:159-161).
- **Fix**: switch to `bufio.Reader.ReadBytes('\n')`-style framing with an
  explicit per-message cap; on an oversized message, drain to the next
  newline, respond with a `CodeInvalidRequest` error (id unknown → notify
  via log only), and continue serving. Raising the cap alone is not a fix —
  any hard cap with fatal semantics has the same failure mode.

### P2.4 Duplicate response IDs leak a goroutine

- **Symptom**: a buggy/malicious client answering the same outgoing call
  twice permanently blocks a dispatch goroutine.
- **Cause**: `dispatch` does a blocking send `ch <- &resp`
  (server.go:216) into a buffer-1 channel; the second send has no receiver
  (Call consumed the first and deleted the pending entry — but the Load
  happened before the delete, so the second sender holds a live channel).
- **Fix**: non-blocking send (`select { case ch <- &resp: default: }`).

---

## P3 — Correctness niceties / decisions needed

### P3.1 ClientCapabilities are parsed and dropped

`handleInitialize` (handler.go:124-155) reads the client's
`fs.readTextFile/writeTextFile` and `terminal` capabilities and stores
nothing. The valuable one is client FS: routing file reads through the
client lets the agent see **unsaved editor buffers**. That is a deep change
(the read tool would need a per-session FS indirection) — treat as a
separate project, but *store* the capabilities on the Handler now so smaller
features (e.g. P0.1 fallbacks) can consult them.

### P3.2 Mixed content shapes in `SessionUpdate.Content`

The `Content any` field carries either a bare `*ContentBlock` (most paths)
or a `[]any` of mixed `TextBlock`+`DiffBlock` (diff path,
handler.go:651-657). Clients must special-case both shapes. Check the ACP
spec's `ToolCallContent` array form and normalize — likely everything should
be a one-element array in the single-block case. Coordinate with whatever
client this was tested against (the shape may be load-bearing for Zed).

### P3.3 Config-option redundancy with legacy modes

`session/set_mode` (handler.go:867-900) and
`session/set_config_option{configId: "mode"}` (handler.go:1033-1064)
duplicate the same mode-transition logic ~40 lines each. Extract a shared
`applyPermissionMode(ctx, sessionID, modeID)` helper; the two handlers
should differ only in response shape.

---

## Suggested execution order

| Order | Item | Size | Independent? |
|---|---|---|---|
| 1 | P0.3 edit-tool diff + dead-code cleanup | S | yes |
| 2 | P1.1 stopReason mapping | S | yes |
| 3 | P1.2 replay kind/title | S | yes |
| 4 | P1.3 RawInput object | XS | yes |
| 5 | P2.4 duplicate-response guard | XS | yes |
| 6 | P2.1 activeToolParams sweep | S | yes |
| 7 | P2.2 per-turn caches | M | yes |
| 8 | P2.3 oversized-message resilience | M | yes |
| 9 | P0.1 image/resource block support | M | needs attachment plumbing check |
| 10 | P1.4 plan updates from todos | M | yes |
| 11 | P0.2 session-scoped MCP servers | L | needs MCP manager API review |
| 12 | P3.x | — | decisions needed first |

Ground rules for the implementing agent:

- Run `go test -race ./internal/acp/...` before and after each item; add a
  test per fix using the fakes in `acp_test.go`.
- `gofumpt -w` anything touched; log messages start with a capital letter.
- Do not change wire-visible behavior beyond what each item specifies —
  editors (Zed et al.) are live consumers of this protocol surface.
- If a fix requires touching `internal/agent` or `fantasy/`, read
  `docs/pitfalls/fantasy-dual-message-state.md` first.
