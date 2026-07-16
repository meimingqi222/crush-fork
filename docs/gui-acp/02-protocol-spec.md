# `crush/*` Protocol Specification

## Common envelope and types

Every event uses:

```json
{
  "sessionId": "session-123",
  "firstSequence": 1840,
  "sequence": 1842,
  "sessionRevision": 31,
  "eventId": "0190...",
  "timestamp": "2026-07-13T10:00:00.123Z",
  "kind": "message.delta",
  "payload": {}
}
```

`sequence` is strictly increasing within a session runtime. `firstSequence`
defaults to `sequence`. A subscriber-specific coalesced event covers the
inclusive range `[firstSequence, sequence]`; its payload is equivalent to
applying every event in that range. A client may apply it only when
`firstSequence == localSequence + 1`. Any other gap requires
`crush/session/sync`. `sessionRevision` increments on externally observable
session configuration or ownership changes, including MCP replacement.

All mutation requests MUST contain a UUID-like `clientRequestId`. The server
MUST retain the completed result or error for at least 10 minutes and return it
for duplicates. Reusing an ID with different parameters returns
`CRUSH_IDEMPOTENCY_CONFLICT`.

Common error data contains `code`, `message`, `retryable`, and optional
`details`. Defined codes include:

```text
CRUSH_FEATURE_NOT_NEGOTIATED  CRUSH_SESSION_NOT_FOUND
CRUSH_SESSION_BUSY            CRUSH_REVISION_CONFLICT
CRUSH_SEQUENCE_EXPIRED        CRUSH_PERMISSION_DENIED
CRUSH_SNAPSHOT_FAILED         CRUSH_INVALID_PATH
CRUSH_QUERY_FAILED            CRUSH_PAYLOAD_TOO_LARGE
CRUSH_BLOB_NOT_FOUND          CRUSH_TERMINAL_NOT_FOUND
CRUSH_FS_NOT_FOUND
CRUSH_PROVIDER_NOT_FOUND      CRUSH_PROVIDER_AUTH_UNSUPPORTED
CRUSH_PROVIDER_LOGIN_IN_PROGRESS  CRUSH_PROVIDER_LOGIN_NOT_FOUND
CRUSH_PROVIDER_AUTH_UNAVAILABLE  CRUSH_PROVIDER_CAPACITY
CRUSH_PROVIDER_AUTH_REQUIRED  CRUSH_MCP_NOT_FOUND
CRUSH_IDEMPOTENCY_CONFLICT    CRUSH_DEADLINE_EXCEEDED
CRUSH_UNSUPPORTED_PROTOCOL_VERSION  CRUSH_UNSUPPORTED_FEATURE
```

## Transport and framing contract

Version 1 stdio uses one JSON-RPC object per newline-delimited UTF-8 frame. The
payload limit is 4 MiB excluding the line delimiter. A frame exactly at the
limit is valid. CRLF is accepted on input; output always uses one LF. Embedded
raw CR/LF bytes, partial/short writes, and multiple writers may never create a
second framing path.

Invalid UTF-8 returns a bounded parse error without ending the connection. An
oversized frame is drained through its next delimiter, then the server sends
a bounded `-32600` response with `id: null` and continues with the following
frame. Malformed JSON similarly returns `-32700`; JSON nesting over 64 levels
returns `-32600`. None of these errors echoes input. An outbound response over
4 MiB is replaced with a small same-ID `-32603` error, leaving the connection
usable.

Incoming application requests are bounded to 128 concurrent handlers and 16
MiB aggregate frame bytes. A request beyond either limit receives a same-ID
`-32603` capacity error; notifications may be discarded. Incoming responses to
server-originated calls bypass that worker budget and duplicate/late responses
are non-blocking.

Default handler deadlines are 30 seconds, `session/prompt` is 30 minutes,
`session/request_permission` is 5 minutes, and `crush/turn/wait` is 5 minutes
plus a 5-second response margin around its own bounded wait. Caller cancellation
and connection close apply earlier. Every physical frame write has a 10-second
deadline; a timeout closes the connection because a partially written frame
cannot be recovered safely.

The serialized writer has three strict classes: JSON-RPC responses/reverse
calls, reliable notifications, and best-effort notifications. Queue capacities
are 32, 32, and 256 frames respectively; each lane also has an 8 MiB queued-byte
budget. Shutdown atomically stops enqueue, interrupts transport I/O, fails
waiting calls, and releases all queued frames.

## Extension negotiation

The server advertises `initialize.result.experimental.crush` with
`protocolVersion` and the supported feature list. A desktop client selects the
same version and a subset of features in `initialize.params.experimental.crush`.
Omitting that object leaves the connection in standard ACP-only mode. Every
initialize replaces prior negotiation state; malformed or unsupported
renegotiation MUST clear the previous selection.

Version 1 features are `sessionSync`, `sessionControl`, `terminal`, `blob`,
`clientFS`, `providerAuth`, and `mcpControl`. `crush/protocol/status` returns the
version and features selected for the current connection. Calls belonging to an
unselected feature return `CRUSH_FEATURE_NOT_NEGOTIATED` without entering an
application handler.

## Session synchronization methods

| Method | Required request fields | Result/semantics |
|---|---|---|
| `crush/session/get` | `sessionId` | Bounded metadata, status, effective config, sequence and revision. |
| `crush/session/subscribe` | `sessionId`, optional `afterSequence` | Creates subscription; replays if available, otherwise emits `snapshot.required`. |
| `crush/session/unsubscribe` | `subscriptionId` | Idempotently closes subscription. |
| `crush/session/snapshot` | `sessionId` | Current bounded projection; no full history. |
| `crush/session/sync` | `sessionId`, `afterSequence` | `{mode: replay, events}` or `{mode: snapshot, snapshot}`. |
| `crush/session/messages` | `sessionId`, `limit`, optional `beforeCursor` | Reverse-chronological page plus stable cursor; default 50, maximum 200. |
| `crush/session/rename` | `sessionId`, `title`, `clientRequestId` | Updated session metadata. |
| `crush/session/archive` | `sessionId`, `archived`, `clientRequestId` | Updated archive state. |
| `crush/session/delete` | `sessionId`, `clientRequestId` | Cancels runtime and deletes according to retention policy. |
| `crush/session/fork` | `sessionId`, optional `messageId`, `clientRequestId` | New session ID and snapshot. |
| `crush/session/pin` | `sessionId`, `pinned`, `clientRequestId` | Updated pin state. |
| `crush/session/search` | query, filters, cursor, limit | Bounded ranked session/message summaries. |
| `crush/session/config/get` | `sessionId` | Persisted overrides, revision, and effective inference configuration. |
| `crush/session/config/update` | `sessionId`, `expectedRevision`, `overrides`, `clientRequestId` | Compare-and-swap replacement of session inference overrides. |

Session metadata returned directly by `rename`, `archive`, and `pin`, and in
the `session` field of `get`, is bounded and contains only public session state:

```json
{
  "id": "session-id",
  "parentSessionId": "parent-id",
  "kind": "normal",
  "title": "Desktop work",
  "workspaceCwd": "D:/workspace",
  "collaborationMode": "default",
  "permissionMode": "auto",
  "messageCount": 42,
  "promptTokens": 1000,
  "completionTokens": 250,
  "archived": false,
  "pinned": true,
  "createdAt": 1783872000,
  "updatedAt": 1783872060
}
```

`get` additionally returns `status`, optional `activeTurn`, bounded queue
summary, `effectiveConfig`, `latestSequence`, and `sessionRevision`. It omits
message history and resource arrays; clients use `snapshot` for the complete
bounded first-screen projection.

## Session and turn inference configuration

Inference fields are:

```json
{
  "model": "gpt-5",
  "provider": "openai",
  "maxOutputTokens": 8192,
  "temperature": 0.2,
  "topP": 0.95,
  "topK": 40,
  "frequencyPenalty": 0,
  "presencePenalty": 0,
  "think": true
}
```

`model` and `provider` must be supplied together. Numeric constraints are:
`maxOutputTokens` 1..200000, `temperature` 0..2, `topP` 0..1, non-negative
`topK`, and penalties -2..2. Provider-specific option maps, credentials,
endpoints, system prompts, context windows, and arbitrary request bodies are
not accepted through this API.

The precedence order from lowest to highest is:

1. workspace/global selected model slot and its configured sampling values;
2. the selected Agent profile model slot;
3. collaboration-mode selection, including the Plan model;
4. persisted session overrides;
5. ephemeral `crush/turn/start.inference` overrides.

An update replaces the complete persisted override object. Sending `{}` clears
all session overrides and exposes the lower precedence levels again. The
request's `expectedRevision` must equal the current session inference revision;
successful updates increment it exactly once. Stale concurrent requests return
`CRUSH_REVISION_CONFLICT`. Like other mutations, exact retries replay the
original result and conflicting payload reuse returns
`CRUSH_IDEMPOTENCY_CONFLICT`.

Example response:

```json
{
  "revision": 3,
  "overrides": {"temperature": 0.2},
  "effective": {
    "model": "gpt-5",
    "provider": "openai",
    "maxOutputTokens": 8192,
    "temperature": 0.2,
    "revision": 3
  }
}
```

At turn admission, the Coordinator freezes the merged session and turn
overrides in the root execution context. Updates during a running turn affect
only later turns; per-step tool/config refresh cannot change that turn's model
or sampling values. Retry retains the original turn overrides. A parent
session's overrides do not implicitly flow into subagents: a child uses its
Agent profile and its own child-session overrides. This preserves review,
explore, designer, librarian, and quick-task model policies. Direct turns on a
child session may still use that child's persisted or ephemeral overrides.

Effective inference values appear in session get/snapshot projections without
making a provider request. Updating them never modifies ConfigStore model maps,
workspace files, global defaults, recent-model lists, or provider credentials.

Titles are trimmed, must be non-empty valid UTF-8, and are capped at 256
bytes. Archive and pin are independent persisted booleans and do not modify
inference, permission, goal, or workspace configuration. These mutations emit
coalescible `session.updated` events containing the same bounded projection.

Delete accepts `sessionId` and `clientRequestId` and returns
`{"sessionId":"...","deleted":true}`. Before persistence is removed, the
desktop runtime closes queued/running GUI turns and cancels any standard Agent
run owned by that session. The shared session service then transactionally
deletes messages/files and invokes its normal App cleanup callback. Repeating
the same request ID replays the successful result even though the session no
longer exists; a new request against the deleted ID returns
`CRUSH_SESSION_NOT_FOUND`.

Fork accepts an optional `messageId`. When omitted, the complete persisted
message history is copied. When supplied, the fork contains history through
the selected message plus the remaining assistant messages in that completed
turn, stopping before the next user message. The boundary must belong to the
source session or the server returns `CRUSH_FORK_BOUNDARY_INVALID`. A fork gets
a new session ID, fresh archive/pin/goal/runtime state, inherited workspace and
session modes, independently generated message IDs, and no live resources.
The result is:

```json
{
  "sessionId": "new-session-id",
  "snapshot": {"session": {"id": "new-session-id"}, "messages": []}
}
```

All five session mutations use the same method/session-scoped 10-minute
`clientRequestId` replay contract as turn mutations. Authorization and source
existence checks occur inside the first idempotent execution, allowing an exact
delete replay without weakening access checks for a new request.

`crush/session/subscribe` returns its response before any replay or live event
for the new subscription can be written:

```json
{
  "subscriptionId": "0190...",
  "latestSequence": 1842
}
```

Events are delivered as reliable `crush/session/event` notifications. They
MUST use the synchronous/reliable transport path rather than the best-effort
ACP notification queue:

```json
{
  "subscriptionId": "0190...",
  "event": {
    "sessionId": "session-123",
    "firstSequence": 1840,
    "sequence": 1842,
    "sessionRevision": 31,
    "eventId": "0190...",
    "timestamp": "2026-07-13T10:00:00.123Z",
    "kind": "message.delta",
    "payload": {
      "messageId": "message-7",
      "partId": "text-1",
      "text": "hello"
    }
  }
}
```

`crush/session/sync` replay results contain ordered wire envelopes and the
latest sequence observed by the server:

```json
{
  "mode": "replay",
  "latestSequence": 1842,
  "events": []
}
```

When `afterSequence` predates the retained journal,
`crush/session/sync` returns `mode: "snapshot"` with the same bounded projection
as `crush/session/snapshot`. A subscriber that overflows after creation first
drains every accepted reliable event and then receives `snapshot.required`;
publication into the session journal MUST NOT wait for the client writer.

`crush/session/unsubscribe` returns `{ "unsubscribed": true }` when it closes
an active subscription and `{ "unsubscribed": false }` for an already absent
subscription. Both outcomes are successful and cleanup is idempotent.

Snapshot MUST include session metadata, status, active turn summary, queued
turn summaries, effective inference configuration, MCP summaries, terminal
summaries, last 20 message summaries, latest sequence, and session revision.
Attachments are metadata plus blob handles; a snapshot MUST NOT inline binaries.
Message summaries contain only a bounded UTF-8 preview (currently 512 bytes),
attachment/tool counts, model/provider identity, finish state, and timestamps.
The backend reads them with an indexed newest-first `LIMIT 20` query and returns
them in chronological order; it MUST NOT use a history-sized `OFFSET` or load
all messages before truncating.

Representative snapshot result:

```json
{
  "session": {
    "id": "session-123",
    "title": "Fix reconnect",
    "kind": "normal",
    "collaborationMode": "default",
    "permissionMode": "auto",
    "messageCount": 10000,
    "promptTokens": 1200,
    "completionTokens": 400,
    "createdAt": 1783910000,
    "updatedAt": 1783910300
  },
  "status": "running",
  "activeTurn": { "state": "running" },
  "queue": { "count": 1, "paused": false },
  "effectiveConfig": { "model": "model-id", "provider": "provider-id" },
  "mcpServers": [],
  "terminals": [],
  "messages": [],
  "latestSequence": 1842,
  "sessionRevision": 31
}
```

Resource summary arrays are independently capped at 50 entries. Services added
in later work packages enrich the queue, MCP, and terminal slots without
changing the bounded message query or snapshot envelope.

## Message pagination and search

`crush/session/messages` returns messages newest first. `limit` defaults to 50
and is capped at 200. `beforeCursor` is an opaque versioned cursor bound to the
method and session. It encodes the last returned `(createdAt, messageId)` key;
clients MUST NOT construct or reuse it for another session.

```json
{
  "messages": [
    {
      "id": "message-123",
      "role": "assistant",
      "text": "bounded display text",
      "textTruncated": false,
      "hasReasoning": true,
      "attachments": [{ "kind": "binary", "mimeType": "application/pdf" }],
      "toolCalls": [{ "id": "tool-1", "name": "read", "finished": true }],
      "toolResultCount": 1,
      "finishReason": "end_turn",
      "model": "model-id",
      "provider": "provider-id",
      "usage": { "inputTokens": 100, "outputTokens": 20 },
      "createdAt": 1783910300,
      "updatedAt": 1783910301
    }
  ],
  "nextCursor": "opaque-base64url",
  "hasMore": true
}
```

Text is capped at 64 KiB per message and 1 MiB per page while preserving valid
UTF-8. Attachment bytes, attachment paths and source URLs, reasoning content or
signatures, tool input, tool result content/metadata, and provider-internal
state are never returned. Attachment and tool lifecycle metadata remain
bounded public projections.

The database query uses keyset predicates over the indexed composite key
`(session_id, created_at DESC, id DESC)`, fetching `limit + 1` rows to derive
`hasMore`. Inserting a newer message after page one cannot shift subsequent
pages, and deleting the boundary row does not invalidate its cursor.

`crush/session/search` returns bounded message hits newest first. Its limit
defaults to 20 and is capped at 100. Search cursors use the same composite key
and are additionally bound to the normalized query and optional session
filter, so changing any scope field invalidates the cursor. Search previews are
capped at 512 bytes. Malformed or cross-scope cursors return invalid params;
storage failures return redacted, retryable `CRUSH_QUERY_FAILED`.

## Turn and queue methods

| Method | Semantics |
|---|---|
| `crush/turn/start` | Accepts content blocks, attachments, turn overrides, and `clientRequestId`; returns `turnId`, queue position, and accepted sequence immediately. |
| `crush/turn/wait` | Waits until terminal turn state or deadline; disconnect does not cancel the turn. |
| `crush/turn/cancel` | Requests cancellation and returns acknowledgement before cleanup completes. |
| `crush/session/queue/list` | Returns ordered queued turns without full content. |
| `crush/session/queue/remove` | Idempotently removes a queued turn. |
| `crush/session/queue/reorder` | Applies a complete/partial ordered ID list with expected revision. |
| `crush/session/steer` | Adds high-priority user direction to the active turn when supported; otherwise queues it with explicit result mode. |
| `crush/session/retry` | Starts a new turn from a selected failed/cancelled turn or assistant message. |

Turn content uses typed blocks: `text`, `image`, `audio`, `resource`, and
`blob`. Binary data over 64 KiB SHOULD use `blob`; data over 4 MiB MUST use
`blob`.

All turn and queue mutations require a UUID `clientRequestId`. The server
retains the exact result or structured error for 10 minutes in a bounded
store, scoped by method and session. Reusing the same ID with the same
canonical JSON payload replays that outcome without repeating the mutation;
reusing it with a different payload returns `CRUSH_IDEMPOTENCY_CONFLICT`.

Version 1 uses these concrete shapes (timestamps are Unix milliseconds):

```json
{
  "sessionId": "session-id",
  "content": [
    {"type": "text", "text": "Implement the change"},
    {"type": "image", "mimeType": "image/png", "filename": "ref.png", "data": "base64"}
  ],
  "inference": {"temperature": 0.2, "maxOutputTokens": 8192},
  "clientRequestId": "uuid"
}
```

`crush/turn/start` returns:

```json
{
  "turnId": "uuid",
  "sessionId": "session-id",
  "status": "queued",
  "queuePosition": 0,
  "acceptedSequence": 42,
  "createdAt": 1783872000000
}
```

The same bounded `Turn` projection is returned by `crush/turn/wait` and
included in cancel results. Status is `queued`, `running`,
`cancel_requested`, `completed`, `failed`, or `cancelled`. Wait accepts
`turnId` and optional `timeoutMs`; the server clamps a positive timeout to five
minutes. Client disconnection or wait timeout stops only that wait and returns
retryable `CRUSH_DEADLINE_EXCEEDED`; it never cancels the turn.

Cancel accepts `sessionId`, `turnId`, and `clientRequestId`, and returns
`{"acknowledged":true,"turn":{...}}`. The server publishes reliable
`cancel.acknowledged` before requesting runner cancellation. A later reliable
`turn.cancelled` is the terminal milestone. Cancelling a pending turn removes
it from the queue and publishes both milestones before `queue.updated`.

Queue list returns `sessionId`, monotonic `revision`, `paused`, and bounded
turn entries containing only `turnId`, `status`, `position`, and a 256-byte
preview. Reorder accepts `expectedRevision`, a complete or partial ordered
`turnIds` list, and `clientRequestId`; omitted pending IDs retain their relative
order after the listed IDs. A stale revision returns
`CRUSH_REVISION_CONFLICT`. Remove accepts `sessionId`, `turnId`, and
`clientRequestId` and returns the new queue projection.

Steer accepts the same `sessionId`, typed `content`, and `clientRequestId`
shape as start. It returns `mode: "steered"` with `acceptedSequence` when the
active Agent accepts high-priority direction, or `mode: "queued"` with a new
`turn` when no active run can accept it. Retry accepts exactly one of `turnId`
or `messageId` plus `sessionId` and `clientRequestId`, and returns a new Turn.
A turn retry is valid only after failure or cancellation; message retry
reconstructs the nearest preceding user message and its attachments.

Inline `image`, `audio`, and `resource` data is standard base64 and contributes
to the 1 MiB retained-input limit. `blob` blocks are rejected until WP-12's
blob service is negotiated. Each session holds at most 128 active-plus-pending
GUI turns. The service retains at most 4,096 terminal turns for 10 minutes and
never permits two GUI-owned active Agent runs for one session.

## Terminal methods

Terminal storage is App-owned. Every operation checks an unguessable internal
connection owner and the request `sessionId`; ownership failures collapse to
`CRUSH_TERMINAL_NOT_FOUND`. Unix uses a real PTY and Windows uses ConPTY.

All terminal mutations require `clientRequestId`. `crush/terminal/open`
accepts:

```json
{
  "sessionId": "session-id",
  "command": "pwsh",
  "args": ["-NoLogo"],
  "cwd": "D:/workspace",
  "env": {"TERM": "xterm-256color"},
  "cols": 120,
  "rows": 40,
  "clientRequestId": "uuid"
}
```

The result contains `terminalId`, `sessionId`, state, dimensions, current byte
offset, and creation time. Command, arguments, cwd, and environment are not
returned by terminal DTOs or resource snapshots. Open validates bounded UTF-8
metadata, dimensions, argument/environment counts, and NUL exclusion before
asking permission or starting a process.

`crush/terminal/input` accepts `sessionId`, `terminalId`, exactly one of UTF-8
`text` or base64 `bytes`, and `clientRequestId`; decoded input is capped at
1 MiB and the result reports bytes written. `crush/terminal/resize` accepts
`cols`, `rows`, and the ownership/idempotency fields. Dimensions are 1..1000.
`crush/terminal/kill` accepts an optional `signal` (`interrupt`, `terminate`,
or `kill`) and returns an immediate acknowledgement. Windows maps all signals
to ConPTY termination while preserving the requested terminal exit reason.

Open, input, and kill pass through the same permission service identity as the
Bash tool (`toolName=bash`, `action=execute`, same session and cwd). Input bytes
and environment values are never placed in permission metadata. Exact mutation
retries replay without repeating permission prompts or process/input/kill side
effects.

`crush/terminal/snapshot` accepts `sessionId`, `terminalId`, and optional
`afterOffset`. It returns terminal metadata plus `startOffset`, `endOffset`,
`truncated`, `more`, and base64 `data`. `terminal.offset` is the current total
end while `endOffset` is the returned block end; clients continue from
`endOffset` while `more=true`. Snapshot blocks default to at most 2 MiB so
base64 JSON remains below the normal frame limit. Output events carry the starting byte offset and
base64 bytes; adjacent ranges may be coalesced only when contiguous. The server
retains an O(1) 2 MiB byte ring by default and permits policy configuration up
to 4 MiB. If `afterOffset` predates retained output, snapshot starts at the
oldest retained offset with `truncated:true`; an offset beyond current output
is invalid.

Reliable `terminal.exited` events include state, code, signal, timestamp, and
final offset. Completed terminals immediately close PTY/ConPTY process and
display handles while retaining bounded metadata/output for ten minutes.
Connection close and session deletion kill and remove owned terminals; App
shutdown closes every remaining terminal. Session snapshots list only bounded
terminal IDs and states.

## Blob methods

Blob storage is App-owned so session deletion can clean resources across all
connections. Each connection receives an unguessable internal client owner ID,
and every method also requires `sessionId`. Both owner dimensions are checked
on every operation. Cross-connection and cross-session access return
`CRUSH_BLOB_NOT_FOUND` without revealing which ownership check failed.

`crush/blob/create` is an idempotent mutation with this shape:

```json
{
  "sessionId": "session-id",
  "mimeType": "application/pdf",
  "filename": "spec.pdf",
  "sourceUri": "file:///workspace/spec.pdf",
  "size": 12345,
  "sha256": "lowercase-hex-sha256",
  "content": "base64",
  "chunks": ["independently-base64-encoded", "chunks"],
  "clientRequestId": "uuid"
}
```

`content` and `chunks` are mutually exclusive; a zero-byte Blob may omit both.
Each chunk is independently base64 encoded. The server verifies decoded size
and SHA-256 before granting a handle. Results contain `blobId`, `sessionId`, size, hash, MIME type, filename,
optional source URI, and `expiresAt` as Unix milliseconds. Filenames are
metadata only and cannot contain path separators.

`crush/blob/read` accepts `sessionId`, `blobId`, `offset`, and `limit`. It
returns the same metadata plus `offset`, `nextOffset`, `eof`, and base64
`content`. Offset/limit ranges are validated before copying. Version 1 defaults
to 1 MiB maximum decoded bytes per read.

`crush/blob/release` accepts `sessionId`, `blobId`, and `clientRequestId`, and
returns `{blobId, released: true}`. Exact retries replay the successful result.
Blobs are also released on owner-session deletion and connection shutdown.

Version 1 defaults to a 10-minute TTL, 64 MiB per Blob, 256 MiB retained per
backend process, and 1,024 retained objects. Policy may configure lower limits.
Inline turn binaries over 4 MiB are rejected with
`CRUSH_PAYLOAD_TOO_LARGE`. A `blob` turn block contains only `type` and
`blobId`; queued turns retain the handle rather than copying bytes, and resolve
the owned Blob only when dispatched or steered. Expired or released handles
fail the turn with an attachment-unavailable terminal state.

## Revision-aware client filesystem

`crush/fs/read`, `crush/fs/write`, and `crush/fs/stat` are reverse JSON-RPC
calls from the Agent to the GUI client. They are available only when the
connection selected `clientFS`; they are not GUI-to-Agent local-disk methods.
Standard ACP clients that do not select the private feature retain the existing
local file tools. Crush MUST NOT route a revision-sensitive write through the
standard `fs/write_text_file` method because that method has no compare-and-swap
token.

Every reverse request contains the root `sessionId` and an absolute path that
Crush has already confined to the session workspace. The client MUST repeat
the same workspace authorization because it owns the actual buffer/file. Both
sides reject lexical traversal, absolute paths outside the root, and existing
or newly created paths whose symlink/junction ancestor escapes the root.

`crush/fs/stat` and `crush/fs/read` accept:

```json
{"sessionId":"session-id","path":"D:/workspace/src/main.go"}
```

Stat returns metadata only. Read returns the same metadata plus exactly one of
`content` or `blobId` for a non-empty file:

```json
{
  "path": "D:/workspace/src/main.go",
  "sourceUri": "vscode-notebook-cell:///workspace/src/main.go",
  "revision": "opaque-client-token",
  "mimeType": "text/plain",
  "size": 123,
  "exists": true,
  "isDirectory": false,
  "content": "current unsaved text"
}
```

`sourceUri` is the client's original document identity and MUST survive
unchanged into file-tool metadata, standard ACP tool updates, and reliable GUI
tool events. `revision` is opaque to Crush and identifies either an unsaved
buffer or durable file state. Stat for a missing path returns `exists:false`
and still supplies a non-empty revision token representing that missing state;
this token authorizes creation without a time-of-check/time-of-use overwrite.

`crush/fs/write` accepts:

```json
{
  "sessionId": "session-id",
  "path": "D:/workspace/src/main.go",
  "expectedRevision": "opaque-client-token",
  "content": "replacement text",
  "clientRequestId": "uuid"
}
```

`content` and `blobId` are mutually exclusive. UTF-8 writes up to 64 KiB use
`content`; larger or binary writes use a connection/session-owned WP-12 Blob.
The client atomically compares `expectedRevision`, applies the write or file
creation, and returns the new metadata. A mismatch returns
`CRUSH_REVISION_CONFLICT` without changing the buffer or file. The client
applies the common `clientRequestId` replay contract so a transport retry does
not duplicate a successful write.

Reads may likewise return a Blob handle for large/binary content. The same
connection/client and root-session ownership checks apply when Crush resolves
it. Temporary write Blobs are released after the reverse response; read handles
remain client-owned and are released explicitly or on connection/session
cleanup. Version 1 caps a file at 64 MiB and rejects malformed metadata,
oversized revisions/source URIs, mismatched sizes, cross-session handles, and
returned paths that differ from the authorized request.

## Provider methods

```text
crush/provider/list
crush/provider/models
crush/provider/auth_status
crush/provider/login
crush/provider/login_cancel
crush/provider/logout
```

`crush/provider/list` has no required parameters and returns providers sorted by
`providerId`:

```json
{
  "providers": [{
    "providerId": "copilot",
    "name": "GitHub Copilot",
    "type": "openai",
    "authMethods": ["device_code"],
    "configured": true,
    "authenticated": false,
    "disabled": false,
    "modelCount": 10
  }]
}
```

`crush/provider/models` takes `providerId`. Results are sorted by `modelId` and
contain capability metadata only:

```json
{
  "models": [{
    "providerId": "copilot",
    "modelId": "gpt-5",
    "name": "GPT-5",
    "contextWindow": 200000,
    "maxOutputTokens": 8192,
    "canReason": true,
    "reasoningLevels": ["low", "medium", "high"],
    "defaultReasoningEffort": "medium",
    "supportsImages": true
  }]
}
```

Provider endpoints, API-key templates, tokens, headers, costs, provider/model
options, and arbitrary configuration are not part of either discovery result.
`crush/provider/auth_status` takes `providerId` and returns only
`{providerId, authenticated}`.

`crush/provider/login` is an idempotent asynchronous mutation:

```json
{
  "providerId": "copilot",
  "authMethod": "device_code",
  "clientRequestId": "UUID"
}
```

API-key providers instead use `authMethod: "api_key"` and add `apiKey`. The key
is accepted only in this request, is bounded to 64 KiB, and is never echoed.
The result is written before any authentication work or event starts:

```json
{"loginId":"UUID","providerId":"copilot","status":"starting"}
```

The server then emits reliable `crush/provider/auth_event` notifications:

```json
{
  "loginId": "UUID",
  "providerId": "copilot",
  "status": "waiting_code",
  "verificationUri": "https://github.com/login/device",
  "userCode": "ABCD-EFGH",
  "expiresAt": 1780000000000
}
```

Statuses are `waiting_browser`, `waiting_code`, `authenticated`, `failed`, and
`cancelled`. A failed event contains only error code
`CRUSH_PROVIDER_LOGIN_FAILED` and the generic message `Provider authentication
failed`; raw provider errors are discarded. A device-flow `userCode` is the
only intentionally disclosed code and is sent only to the owning connection.
The private OAuth device code, exchange authorization codes, access/refresh
tokens, headers, response bodies, and URLs containing userinfo never appear in
results, notifications, snapshots, replay entries, errors, or logs.

`crush/provider/login_cancel` takes `loginId` and `clientRequestId`, returns
`{loginId, status: "cancelling"}`, and emits `cancelled` only after the response
is written. A different connection receives the same
`CRUSH_PROVIDER_LOGIN_NOT_FOUND` as an unknown ID. `crush/provider/logout` takes
`providerId` and `clientRequestId`, cancels any active flow, atomically removes
the persisted API key and OAuth token, and returns
`{providerId, authenticated: false}`. Exact mutation retries replay their
original safe result and do not restart flows or repeat credential changes.

## MCP methods

```text
crush/mcp/list
crush/mcp/status
crush/mcp/reconnect
crush/mcp/disable
crush/mcp/logs
```

All requests are session-scoped. Status is one of `disabled`, `starting`,
`connected`, `degraded`, `failed`, `reconnecting`, `stopping`. Logs are
bounded, redacted, and identify the public server name rather than reusable
internal generation IDs. Reconnect/replacement revokes the old owner before
disconnect and grants the new generation only after successful connection.

`session/new` and `session/load` schedule any supplied `mcpServers` and return
without waiting for transport startup. Clients learn the result through
`mcp.status`, `crush/mcp/status`, or `crush/mcp/list`; an acknowledgement is not
evidence that a server connected.

Public IDs have exactly two namespaces: `static:<config-name>` for workspace
configuration and `session:<client-name>` for the current root session's
dynamic configuration. Opaque `acp-*` generation names MUST NOT appear on the
wire. A server DTO has this shape:

```json
{
  "serverId": "session:docs",
  "name": "docs",
  "scope": "session",
  "status": "connected",
  "tools": 7,
  "prompts": 1,
  "resources": 2,
  "revision": 12,
  "updatedAt": 1784058000000
}
```

`errorCode` is optional and contains only a stable symbolic code such as
`CRUSH_MCP_CONNECTION_FAILED`, `CRUSH_MCP_AUTH_REQUIRED`,
`CRUSH_MCP_CIRCUIT_OPEN`, or `CRUSH_MCP_LIVE_CONNECTION_UNAVAILABLE`. Raw
transport errors are never returned.

List and status requests and responses are:

```json
{"sessionId":"session-id"}
```

```json
{"revision":12,"servers":[{"serverId":"session:docs","name":"docs","scope":"session","status":"connected","tools":7,"prompts":1,"resources":2,"revision":12,"updatedAt":1784058000000}]}
```

```json
{"sessionId":"session-id","serverId":"session:docs"}
```

`crush/mcp/status` returns one server DTO. A server belonging to another root
session is indistinguishable from an unknown server and returns
`CRUSH_MCP_NOT_FOUND`.

Reconnect and disable are asynchronous mutations:

```json
{"sessionId":"session-id","serverId":"session:docs","clientRequestId":"4e480db2-05c4-46d6-a71a-5cd46a24da6f"}
```

They immediately return a server DTO in `reconnecting` or `stopping` state.
`clientRequestId` MUST be a UUID. An exact retry on the same connection returns
the original result without repeating the operation; reuse with different
parameters returns `CRUSH_IDEMPOTENCY_CONFLICT`.

`crush/mcp/logs` accepts:

```json
{"sessionId":"session-id","serverId":"session:docs","afterSequence":41,"limit":100}
```

It returns at most 1,000 entries:

```json
{
  "entries": [{"sequence":42,"timestamp":1784058000000,"level":"info","logger":"server","message":"Indexed 10 documents"}],
  "latestSequence": 42,
  "truncated": false
}
```

Retention is globally bounded by count and bytes; `truncated` is true when the
requested prefix was evicted or more matching entries remain. Messages and
logger names are bounded and redacted before retention. No config, credential,
credential-bearing URL, raw error, or internal generation ID is included.

Every lifecycle transition is a reliable `mcp.status` session event. Its
payload is the server DTO without `updatedAt`. The event advances the session
revision, and the MCP `revision` changes even for a connected-to-connected
generation replacement so clients invalidate derived tool/instruction state.

## Event kinds and delivery policy

Required event families include `session.*`, `turn.*`, `message.*`,
`reasoning.*`, `tool.*`, `permission.*`, `usage.updated`, `queue.updated`,
`terminal.*`, `mcp.status`, and `snapshot.required`, plus the connection-scoped
`crush/provider/auth_event` notification.

| Event | Backpressure strategy |
|---|---|
| Text/reasoning delta | Merge adjacent compatible deltas for 16-33 ms. |
| Terminal output | Coalesce by byte count and time, preserving offsets. |
| Tool progress | Latest wins per tool call. |
| Usage/title/status update | Latest wins per entity. |
| Permission request | Never drop. |
| Tool/turn completion | Never drop. |
| Cancellation acknowledgement | Never drop. |
| `snapshot.required` | Never drop. |

When a reliable subscriber queue cannot accept a non-droppable event, the
server MUST replace pending coalescible state with `snapshot.required` and
close or pause that subscription after the marker is written. It MUST NOT
silently continue with an invalid client projection.
