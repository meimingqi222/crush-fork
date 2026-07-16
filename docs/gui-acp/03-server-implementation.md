# Server Implementation Specification

## Shared services

Before adding handlers, extract interfaces for session query/mutation, turn
control, event subscription, terminal, blob, provider auth, client FS, and MCP
control. Handlers MUST validate and translate only; business state transitions
belong in services.

`internal/acp/server.go` may retain best-effort notification behavior for
standard ACP compatibility. The GUI reliable stream MUST use a separate bounded
writer/subscription abstraction with explicit overflow behavior.

## Framed transport runtime

`internal/acp.Transport` owns only framed connection I/O. `LineTransport` uses
a `maxFrame + 1` reader so a payload exactly at the limit is accepted, drains an
oversized line without retaining it, loops over short writes, rejects embedded
line delimiters, and closes underlying `io.Closer` handles on cancellation.
Adapters must provide equivalent `ReadFrame`, `WriteFrame`, peer-close and
`Close` semantics; they must not call ACP or GUI handlers themselves.

`Server` runs one writer over critical, reliable and best-effort lanes. Every
lane has count and byte bounds. Critical responses/reverse calls precede
reliable event notifications, which precede best-effort standard notifications.
A 10-second physical write deadline terminates the connection, and writer exit
atomically rejects new enqueue, acknowledges waiting writers, and drains queued
frame references. Graceful input EOF waits for active handlers and a critical
writer barrier so final responses are not lost.

Request dispatch has both a 128-handler semaphore and 16 MiB aggregate frame
budget. Capacity rejection is itself a bounded critical response. Incoming
responses are parsed on the reader path and delivered through a non-blocking
pending-call channel, preventing both worker starvation and duplicate-response
goroutine leaks. Connection cancellation is propagated to tracked handlers
before `Serve` waits for them.

## Session event hub

The hub MUST provide:

- atomic per-session sequence allocation and journal append;
- bounded replay by sequence and age;
- non-blocking publish with per-subscriber coalescers;
- non-droppable event classification;
- explicit `snapshot.required` overflow transition;
- subscription cancellation and leak-free teardown;
- metrics without high-cardinality session labels.

Suggested initial bounds: 4,096 events or 5 minutes per active session,
whichever expires first; 256 queued logical events per subscriber; 16-33 ms
delta coalescing. Bounds MUST be configurable and benchmarked.

## Snapshot and pagination

Snapshots are assembled from bounded runtime projections plus session metadata.
They MUST NOT query all messages. Message pagination MUST use a stable composite
cursor (for example creation time plus message ID), an indexed query, default
page size 50, and maximum 200. Deleting or inserting concurrent messages MUST
not duplicate entries across cursor pages.

Standard ACP `session/load` may retain required replay semantics. The GUI MUST
use snapshot/pagination and MUST NOT call or emulate full replay in
`internal/acp/handler.go`.

## Turns and persistence

Prompt acceptance creates a turn record/runtime command before returning.
Provider callbacks publish directly to the event hub and update an in-memory
message draft. Persistence flushes drafts every 150-500 ms, at tool boundaries,
and on terminal turn state. A persistence failure emits a durable-state warning
and retries with bounded backoff; it MUST NOT corrupt event ordering.

Cancellation has two milestones: `cancel.requested` is acknowledged immediately;
`turn.cancelled` is emitted after the agent loop stops and final persistence is
attempted. `turn/wait` waits for the latter.

## Session configuration

Store session overrides independently from workspace defaults. Updates require
an expected revision and publish `session.config.updated`. A turn captures its
effective immutable configuration at start. Mid-turn configuration changes apply
only to later turns unless a field explicitly documents live behavior.

## Client filesystem

Client FS is an injected per-session interface. Reads return content plus a
revision token representing an unsaved buffer or durable file. Writes include
`expectedRevision`; mismatches return `CRUSH_REVISION_CONFLICT`. Server-side path
tools MUST consistently choose client FS or local FS according to negotiated
capability and workspace policy. The source URI and revision MUST survive into
tool/event metadata.

The implementation owns one capability per ACP connection and root session.
Both standard `session/prompt` and queued `crush/turn/start` install it in the
Coordinator execution context, so root agents, subagents, nested subagents and
retries use the same client and revision cache. No capability is installed
when `clientFS` was not selected; TUI/CLI and standard ACP therefore keep the
local filesystem implementation. Read, write, and edit tools select the
execution-scoped bridge before Fantasy records their result.

The bridge sends reverse `crush/fs/*` calls over the connection's synchronous
request lane. It records the revision returned by read/stat and consumes that
exact token on write. It never refreshes the token immediately before applying
already-computed content. Connection close, session delete, workspace-root
replacement, or failed renegotiation closes the scope and clears revisions.

## Provider discovery and authentication

Provider discovery reads a detached ConfigStore snapshot and projects it into
domain DTOs before GUI serialization. Known Catwalk providers and configured
custom providers are merged by ID; configured models win. Projection code must
copy only display names, type, authentication state, disabled state, model
count, and documented model capabilities. It must not serialize Config,
ProviderConfig, Catwalk Provider, or raw provider errors.

The App owns one `providerauth.Manager`. Device/browser implementations satisfy
an injected Flow interface, while automated tests use deterministic mock flows.
A flow keeps private device codes and credentials inside its implementation and
returns a transient credential only at completion. The manager persists it,
clears the local reference, and emits a generic terminal event. Hyper and
GitHub Copilot use their existing device-flow clients; no GUI-specific provider
HTTP client or VCR path is introduced.

GUI services bind login entries to their opaque connection owner. Login uses a
connection-local, bounded idempotency store whose retained payload is only a
SHA-256 hash. A response lifecycle starts the prepared flow after the JSON-RPC
response write succeeds and removes it if the write fails. Cancellation uses
the same ordering. Connection close and every renegotiation cancel matching
flows, wait for callbacks to finish, and replace the replay epoch so stale
login IDs cannot be reactivated. Provider credentials remain global App state;
interactive login entries, challenges, and errors are never persisted.

## MCP lifecycle service

`internal/mcplifecycle` is App-owned and receives an injected transport backend,
ConfigStore, and session event Hub. `internal/acp/handler.go` only translates
ACP server configs, calls `ReplaceAsync`, installs the returned live access
scope for prompts, and closes resources by connection owner. GUI MCP handlers
validate/project DTOs and call the same service; they do not operate global MCP
maps directly.

`ReplaceAsync`, reconnect, and disable must revoke authorization while holding
the lifecycle mutex, then return before transport I/O. Transport work runs in
tracked goroutines. Dynamic entries have independent cancellation contexts so
one server mutation cannot cancel sibling startup. Full replacement cancels all
old entries, assigns a monotonically unique generation to each new entry, and
retains tombstones for every generated name. A successful transport result is
rechecked against the current entry and generation before authorization.

The access object reads live service state on every discovery and invocation
check. Its cache signature contains root session and lifecycle revision. This
is required for stale provider steps, deferred activation, cached tool objects,
instructions, and connected-to-connected replacement; filtering only when a
tool list is built is not sufficient.

Dynamic ConfigStore entries are ephemeral. They are added immediately before
connect and removed on failure, replacement, disable, session deletion, owner
close, and App shutdown. Static reconnect/disable operations are chained per
config name in request order before persisting the disabled flag. Tests inject
a backend that can block, fail, emit state/log events, and ignore cancellation;
they MUST NOT start real MCP processes.

The service subscribes once to process-global MCP events. State events are
mapped to public symbolic states and reliable root-session events. MCP logging
notifications are published in-process without their raw data entering `slog`;
the service applies builtin and exact config-secret redaction, field bounds,
and global count/byte retention before GUI access. Transport failure details are
collapsed to stable error codes.

## Resource cleanup

Every runtime resource MUST have one owner and an idempotent close path. Handler
shutdown order:

1. stop accepting new mutations;
2. acknowledge/issue cancellation for active prompts;
3. revoke dynamic MCP access and cancel per-server lifecycle work;
4. close subscriptions and writer loops;
5. cancel connection-owned provider logins and wait for callbacks;
6. kill/release terminals and blobs;
7. close the App provider auth and MCP lifecycle managers, flush bounded
   drafts, then close process-global MCP transports.

Tests MUST assert no residual goroutines, active prompts, subscriptions,
terminals, ephemeral MCP configs, or blobs.
