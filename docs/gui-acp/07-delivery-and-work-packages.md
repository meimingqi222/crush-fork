# Delivery Plan and Agent Work Packages

## Phase plan

### Phase 0: instrumentation and baselines

Entry: current ACP and agent race tests pass. Implement metrics, benchmark
harnesses, and reproducible long-session fixtures. Exit: current 150 ms stream
coupling and full-load cost are measured; all future SLOs have executable gates.
No wire behavior changes.

### Phase 1: direct live event path

Entry: phase 0. Implement event types, hub, coalescer, and agent callback bridge
separate from persistence. Exit: GUI delta p95 meets GUI-PERF-002 and zero DB
operations occur per delta. Standard ACP behavior is unchanged and can fall back
independently.

### Phase 2: reliable synchronization

Entry: phase 1. Add sequence journal, subscription overflow semantics, snapshot,
sync, and message pagination. Exit: reconnect/gap tests and 10,000-message
first-screen SLO pass. Rollback: disable advertised `sessionSync`; do not change
standard ACP `session/load` semantics.

### Phase 3: session and turn completeness

Entry: phase 2. Add CRUD/fork/search, queue/steer/retry, idempotency, and
session-scoped inference overrides. Exit: state machine, conflict, and duplicate
request tests pass.

### Phase 4: terminal, client FS, and blob

Entry: phase 2; session ownership is stable. Add terminal lifecycle, retained
output, blob service, typed attachments, and revision-aware client FS. Exit:
resource/security tests and terminal/blob stress pass.

### Phase 5: provider auth and asynchronous MCP

Entry: phases 2-4. Add provider APIs and async MCP status/control while
preserving session isolation. Exit: new/load acknowledgement never waits for MCP;
replacement, shutdown, partial failure, secret-redaction, and stale-tool tests
pass.

### Phase 6: supervisor and alternative transports

Entry: version 1 is stable on stdio. Extract process/workspace supervisor and
add Named Pipe/Unix Socket or WebSocket behind the transport interface. Exit:
identical protocol contract tests pass across transports. This phase is optional
for the first desktop release.

## Work-package contract

Each Agent MUST read this directory, `docs/acp-hardening-plan.md`, applicable
`docs/pitfalls/*`, and local `AGENTS.md` files before editing. It MUST add tests,
run `gofumpt` on Go changes, run targeted `-race` tests, and report requirement
IDs satisfied. Agents MUST NOT silently change public wire schemas; propose spec
amendments first.

### WP-01: Performance baseline and observability

- Files: new ACP/GUI benchmark files; metrics hooks in `internal/acp`, agent
  streaming, and persistence.
- Dependencies: none.
- Delivers: GUI-OBS-001 and executable GUI-PERF baselines.
- Tests: all named benchmarks, metric cardinality test.
- Must not change: protocol results or flush intervals.

### WP-02: Event hub foundation

- Files: new `internal/sessionevent/{event,hub,journal,coalescer}.go` and tests.
- Dependencies: WP-01 metrics interfaces.
- Delivers: GUI-EVENT-001/002 core ordering and backpressure.
- Acceptance: concurrent publishers have a strict sequence; slow subscribers
  are bounded and receive `snapshot.required`; race tests pass.
- Must not change: agent or ACP handlers.

### WP-03: Direct model stream bridge

- Files: `internal/agent/agent.go`, stream callback/runtime integration, adapter
  into `internal/sessionevent`.
- Dependencies: WP-02.
- Delivers: GUI-PERF-002/005/006.
- Tests: text, reasoning, tool, completion, cancellation order; persistence
  failure does not block live events.
- Must not change: Fantasy dual message-state semantics; read the documented
  pitfall before edits.

### WP-04: GUI routing and negotiation

- Files: new `internal/guiapi`, transport dispatch integration, initialize
  extension DTOs/tests.
- Dependencies: WP-02.
- Delivers: GUI-COMPAT-001/002.
- Acceptance: unnegotiated calls fail predictably; standard ACP golden tests
  are unchanged.
- Must not change: standard ACP method schemas.

### WP-05: Reliable sequence, replay, and sync

- Files: `internal/sessionevent/journal.go`, GUI subscription/sync handlers.
- Dependencies: WP-02, WP-04.
- Delivers: GUI-EVENT-001/002/003 (sync portion).
- Tests: reconnect, duplicate delivery, expired sequence, blocked client, gap
  recovery.
- Must not change: `session/load` compatibility replay.

### WP-06: Snapshot service

- Files: new `internal/sessionevent/snapshot.go`, session/runtime projections,
  GUI snapshot handler.
- Dependencies: WP-05.
- Delivers: GUI-SESS-002 and GUI-PERF-003.
- Acceptance: bounded query count and response size for 10,000 messages.
- Must not include: complete message bodies/history.

### WP-07: Message pagination and search

- Files: session DB SQL/query generation, session service, GUI handlers.
- Dependencies: WP-04.
- Delivers: part of GUI-SESS-001.
- Tests: stable cursors during insert/delete, default/max limit, indexed query
  plan, large fixtures.
- Must not expose: raw attachment binaries or secret/tool-internal metadata.

### WP-08: Session CRUD and fork

- Files: session service/DB and GUI handlers for rename/archive/delete/fork/pin.
- Dependencies: WP-05, idempotency support from WP-09.
- Delivers: GUI-SESS-001.
- Tests: active-turn delete, fork boundaries, duplicate requests, teardown.
- Must not change: existing CLI/TUI session semantics without shared-service
  compatibility tests.

### WP-09: Turn queue, steer, retry, and idempotency

- Files: coordinator/session runtime command queue, GUI turn handlers, bounded
  idempotency cache/store.
- Dependencies: WP-03, WP-05.
- Delivers: GUI-TURN-001 and GUI-EVENT-003.
- Tests: reorder conflicts, cancel milestones, wait disconnect, retry source,
  duplicate ID same/different payload.
- Must not allow: two active agent runs for one session.

### WP-10: Session inference overrides

- Files: session configuration model/storage, coordinator effective-config
  resolver, GUI handler.
- Dependencies: WP-09.
- Delivers: GUI-SESS-003.
- Tests: all precedence levels, concurrent revision conflict, running-turn
  immutability, subagent inheritance policy.
- Must not mutate: workspace/global model defaults.

### WP-11: Terminal service and protocol

- Files: new/reused `internal/terminal`, GUI handlers/events, platform tests.
- Dependencies: WP-04, WP-05.
- Delivers: GUI-TERM-001 and GUI-SEC-002.
- Tests: open/input/resize/kill/exit, offsets, truncation, reconnect, Windows and
  Unix behavior, permission denial, cleanup.
- Must not retain: live PTY/display objects after completion.

### WP-12: Blob and attachment service

- Files: new `internal/blob`, attachment DTO adapters, GUI blob handlers.
- Dependencies: WP-04.
- Delivers: GUI-BLOB-001 and GUI-SEC-005.
- Tests: hashing, chunk ranges, expiry/release, cross-session denial, size
  limits, cleanup.
- Must not inline: payloads over 4 MiB.

### WP-13: Revision-aware client filesystem

- Files: new `internal/clientfs`, per-session capability injection, file tool
  adapters, GUI/ACP client calls.
- Dependencies: WP-04, WP-12 for large/binary files.
- Delivers: GUI-FS-001 and GUI-SEC-001/008.
- Tests: unsaved reads, stale writes, URI preservation, traversal and symlink/
  junction escape.
- Must not create: a global mutable client FS singleton.

### WP-14: Provider discovery and authentication

- Files: provider service adapters, GUI handlers/events, redaction tests.
- Dependencies: WP-04, WP-05.
- Delivers: GUI-AUTH-001 and GUI-SEC-004.
- Tests: browser/code flows, cancel/logout, provider errors, no-secret snapshots
  and logs.
- Must not use: real LLM/provider credentials in automated tests; inject mock
  clients as documented in `AGENTS.md`.

### WP-15: Asynchronous session MCP lifecycle

- Files: `internal/acp/handler.go` only for shared extraction/compatibility;
  MCP service and GUI handlers/events elsewhere.
- Dependencies: WP-05, WP-06.
- Delivers: GUI-MCP-001/002/003.
- Tests: immediate new/load response, status transitions, A/B isolation,
  replacement generation, partial failure, reconnect, shutdown, tombstones,
  ephemeral config cleanup, stale invocation denial.
- Must not reintroduce: global `UpdateModels`, a second Coordinator session-to-MCP
  map, or authorization based only on tool discovery.

### WP-16: Transport abstraction and framing hardening

- Files: `internal/acp/server.go`, new transport interfaces/adapters, protocol
  contract suite.
- Dependencies: WP-04.
- Delivers: GUI-PERF-006 and GUI-SEC-006/007.
- Tests: oversized frame recovery, blocked writer, duplicate responses,
  disconnect, write priority, stdio cleanliness.
- Must not weaken: reliable GUI event delivery semantics.

### WP-17: Soak, fault-injection, and release gate

- Files: integration/performance test harness and CI/task definitions.
- Dependencies: WP-01 through WP-16 for full profile.
- Delivers: release evidence for all performance/reliability requirements.
- Acceptance: specified soak stabilizes; faults recover or fail explicitly;
  `go test -race ./internal/acp/... ./internal/guiapi/... ./internal/sessionevent/...`
  and relevant agent/session packages pass.
- Must not rely on: real LLM calls or VCR cassettes; use deterministic fake
  provider streams and mock transports.

## Requirement traceability

| Requirement | Work packages |
|---|---|
| GUI-COMPAT-001/002 | WP-04, WP-16 |
| GUI-EVENT-001/002/003 | WP-02, WP-05, WP-09 |
| GUI-SESS-001/002/003 | WP-06, WP-07, WP-08, WP-10 |
| GUI-TURN-001 | WP-09 |
| GUI-TERM-001 | WP-11 |
| GUI-FS-001 | WP-13 |
| GUI-BLOB-001 | WP-12 |
| GUI-AUTH-001 | WP-14 |
| GUI-MCP-001/002/003 | WP-15 |
| GUI-OBS-001 | WP-01, WP-17 |
| GUI-SEC-001..010 | WP-11 through WP-16, WP-17 |
| GUI-PERF-001..007 | WP-01, WP-03, WP-06, WP-09, WP-16, WP-17 |

## Agent handoff template

Every implementation handoff SHOULD state:

```text
Work package:
Requirements satisfied:
Files changed:
Wire/schema changes:
Migration/rollback notes:
Tests and benchmark results:
Known limits or follow-up work:
```

