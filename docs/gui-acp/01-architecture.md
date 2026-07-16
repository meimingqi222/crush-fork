# Architecture

## Component boundaries

```text
GUI / standard ACP client
          |
  JSON-RPC transport abstraction
          |
  +-------+-------------------+
  |                           |
internal/acp              internal/guiapi
standard handlers         crush/* handlers
  |                           |
  +----------+----------------+
             |
      application services
             |
  +----------+-----------+-------------+
  |                      |             |
sessionevent hub     session/DB     MCP manager
  |                      |
live subscribers     durable history
```

Recommended packages:

```text
internal/acp/                 Standard ACP compatibility only.
internal/guiapi/              Extension routing, DTOs, validation.
internal/sessionevent/
  event.go                    Event envelope and kinds.
  hub.go                      Per-session publish/subscribe.
  journal.go                  Bounded sequence replay.
  snapshot.go                 Bounded state projection.
  coalescer.go                Subscriber-specific delta batching.
internal/terminal/            PTY lifecycle and retained output.
internal/blob/                Owned temporary binary objects.
internal/clientfs/            Revision-aware client FS bridge.
internal/providerauth/        App-owned catalog/auth lifecycle and flow adapters.
```

DTOs MUST remain separate from persistence models. Shared services SHOULD use
domain types and adapters so standard ACP and `crush/*` can evolve separately.

## Streaming path

The current `internal/agent/stream_message_flusher.go` uses a 150 ms persistence
interval. GUI text MUST bypass that interval:

```text
provider chunk
  +-> session event hub -> 16-33 ms subscriber coalescer -> transport
  |
  +-> in-memory message draft -> 150-500 ms flusher -> SQLite
```

The event hub MUST be published from the agent's canonical stream callbacks,
not reconstructed by watching database updates. Tool start/result, reasoning,
usage, permissions, cancellation, and terminal output follow the same principle.

## Ownership and concurrency

- One backend process SHOULD own one workspace in version 1.
- A session runtime owns its active turn, queue, subscriptions, event journal,
  terminals, blobs, and session inference overrides.
- A negotiated client filesystem belongs to one ACP connection and root
  session. Its revision cache is injected into the root execution context and
  inherited by nested agents; it is never a process-global singleton.
- Provider credentials are workspace/process state, so one App-owned provider
  auth manager serializes login and logout. Each active login still belongs to
  one opaque ACP connection owner; only that owner receives its challenge and
  connection close or renegotiation cancels it through a completion barrier.
- A session MUST have one monotonic `sequence` allocator. Publishing an event
  and appending it to the replay journal MUST be one ordered critical section.
- Subscriber writes MUST NOT hold the hub lock.
- Slow subscribers receive coalesced events or `snapshot.required`; they MUST
  NOT backpressure the agent loop.
- Session teardown MUST cancel turns, close subscriptions, terminals, and
  blobs, revoke MCP access, then release runtime state.

## State sources

| State | Authoritative source | Recovery source |
|---|---|---|
| Live deltas/current turn | session runtime/event hub | snapshot + replay; incomplete draft may be DB-backed after restart |
| Completed messages | SQLite | paginated SQLite query |
| Session metadata | session service/SQLite | SQLite |
| MCP authorization | Handler/session access registry | rebuilt session scope; dynamic transports reconnect explicitly |
| Terminal live state | App-owned terminal manager with connection/session ownership | retained ring snapshot and bounded exit metadata |
| Attachments | App-owned blob registry with connection/client and session ownership plus durable metadata | blob or original URI according to lifetime |
| Unsaved files | Negotiated connection/root-session client FS plus opaque client revision | client-owned durable file or editor buffer; local FS only when no private scope is installed |
| Provider catalog and credentials | ConfigStore snapshot plus App-owned provider auth manager | persisted global provider config; interactive login state is intentionally not recovered |

## Transport

Version 1 MUST support stdio. JSON-RPC framing and handler dispatch MUST depend
on a transport interface so Named Pipe on Windows, Unix domain socket, or
WebSocket can be added without changing methods or domain services.

Transport responsibilities: bounded frame parsing, serialized writes,
deadlines, peer-close detection, and byte metrics. Transport MUST NOT own
session semantics. No logs may be written to protocol stdout.

`internal/acp.Transport` is the connection boundary: `ReadFrame`, `WriteFrame`,
`Close`, and a bounded metric name. `LineTransport` implements newline-delimited
stdio framing, exact-limit handling, complete short writes, oversize drain, and
context cancellation by closing blocked stdio/pipe handles. `Server` owns
JSON-RPC classification and domain dispatch above that interface, so a future
pipe/socket/WebSocket adapter cannot acquire session semantics.

Outbound work uses three bounded lanes. JSON-RPC responses and reverse calls
are critical; reliable GUI/standard notifications are next; best-effort ACP
notifications are last. One writer serializes all lanes. Count and byte budgets
bound queued frames, while a physical-write deadline closes a client that stops
reading. Incoming responses bypass the bounded request worker pool so a flood of
ordinary requests cannot starve permission or client-FS replies.

## MCP lifecycle

`session/new` and `session/load` return after durable session validation and
runtime creation. MCP connections start asynchronously. Events communicate
`starting`, `connected`, `degraded`, `failed`, `reconnecting`, and `stopping`.
New code MUST preserve generation-based internal transport names, owner-first
revocation, tombstones, revision-aware caches, and invocation-time
authorization.

The App owns one `mcplifecycle.Service`; an ACP Handler only schedules a
replacement and binds its connection owner. The process-global MCP package
continues to own transports, reconnect logic, tool caches, prompts, and
resources. The lifecycle service supplies the live root-session access object
used by both standard ACP prompts and GUI turn/retry execution. That one access
object is the authorization boundary for discovery, instructions, cached state,
and every actual tool invocation; the Coordinator MUST NOT keep another
session-to-MCP map.

Dynamic public IDs are stable within a root session as `session:<client-name>`.
Static configured servers use `static:<config-name>`. Each dynamic connect or
reconnect uses a fresh opaque process-internal generation name. Internal names,
commands, arguments, environment, headers, URLs, OAuth data, and tombstones
never cross the GUI boundary.

Replacement performs these state changes in order:

1. cancel and revoke every old generation;
2. publish `stopping` and mark each old internal name as a permanent tombstone;
3. disable the old transports and remove their ephemeral ConfigStore entries;
4. create fresh generation names and publish `starting`;
5. connect in the background and grant access only for `connected` or
   `degraded` state.

A reconnect or disable of one dynamic server has its own cancellation scope and
MUST NOT cancel sibling servers that are still starting. Static mutations are
serialized per configured name so a stale reconnect cannot persist `enabled`
after a newer disable. Session deletion and connection-owner shutdown revoke
access before waiting for transport cleanup.
