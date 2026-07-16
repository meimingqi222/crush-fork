# Product Requirements

## Goals

Crush MUST provide a backend suitable for a Codex-style desktop application:
fast streaming, reconnectable session state, complete session management,
interactive terminals, unsaved editor buffers, attachments, provider login,
and session-isolated MCP management.

## Functional requirements

| ID | Requirement |
|---|---|
| GUI-COMPAT-001 | Standard ACP clients MUST continue to work without `crush/*` negotiation. |
| GUI-COMPAT-002 | Private extensions MUST be versioned and capability-negotiated. |
| GUI-EVENT-001 | Each session MUST expose a monotonic event sequence, bounded replay, and snapshot fallback. |
| GUI-EVENT-002 | Completion, cancellation, permission, and snapshot-required events MUST never be dropped. |
| GUI-EVENT-003 | Mutations MUST accept `clientRequestId` and return the original result on retry. |
| GUI-SESS-001 | The API MUST support get, subscribe, snapshot, sync, pagination, rename, archive, delete, fork, pin, and search. |
| GUI-SESS-002 | Session snapshots MUST be bounded and MUST NOT include full message history. |
| GUI-SESS-003 | Inference configuration MUST be session-scoped with turn-level overrides. |
| GUI-TURN-001 | The API MUST support start, wait, cancel, queue inspection/mutation, steer, and retry. |
| GUI-TERM-001 | The API MUST provide lifecycle-complete interactive terminals with resize, retained output, and reconnect snapshots. |
| GUI-FS-001 | Client filesystem access MUST preserve unsaved-buffer revisions and reject stale writes. |
| GUI-BLOB-001 | Large/binary payloads MUST use owned, expiring blob handles. |
| GUI-AUTH-001 | Provider discovery, model discovery, auth status, login, cancellation, and logout MUST be available without exposing secrets. |
| GUI-MCP-001 | Session creation/loading MUST NOT block on MCP startup. |
| GUI-MCP-002 | Dynamic MCP tools, instructions, status, caches, and invocation MUST remain isolated by root session. |
| GUI-MCP-003 | MCP reconnect, disable, status, and bounded redacted logs MUST be exposed. |
| GUI-OBS-001 | Request, event, persistence, queue, terminal, blob, and active-resource metrics MUST be emitted. |
| GUI-SEC-001 | All workspace paths MUST be root-confined with symlink escape prevention. |

## Performance requirements

All latency SLOs are measured on a supported local machine with a warm backend,
excluding provider network latency unless explicitly stated.

| ID | SLO |
|---|---|
| GUI-PERF-001 | Prompt acknowledgement p95 MUST be below 20 ms. |
| GUI-PERF-002 | Provider chunk to GUI write-ready latency p95 MUST be below 50 ms and SHOULD be below 33 ms. |
| GUI-PERF-003 | Session first-screen snapshot p95 MUST be below 150 ms for a 10,000-message session. |
| GUI-PERF-004 | Cancellation acknowledgement p95 MUST be below 100 ms. |
| GUI-PERF-005 | The live path MUST perform zero SQLite reads or writes per text delta. |
| GUI-PERF-006 | A blocked client MUST have bounded memory and MUST NOT stall provider consumption or persistence. |
| GUI-PERF-007 | Ten active sessions at 1,000 aggregate chunks/second MUST remain responsive and race-free. |

## Configuration precedence

The effective inference configuration MUST use this precedence:

```text
turn override > session override > workspace default > global default
```

Changing a session override MUST NOT mutate workspace or global defaults.

## Non-goals for version 1

- Replacing or forking standard ACP.
- Remote multi-tenant hosting or Internet-facing authentication.
- Cross-device durable event journals.
- Collaborative multi-writer document editing.
- A single backend process supervising multiple workspaces. This MAY be added
  after transport and workspace boundaries are proven.
- Embedding large binary data directly in normal JSON-RPC messages.

## Product acceptance

Version 1 is releasable only when phases 0 through 5 in
[the delivery plan](07-delivery-and-work-packages.md) pass race tests, protocol
contract tests, performance gates, and the specified soak test. Phase 6 is not
required for the initial desktop release.

