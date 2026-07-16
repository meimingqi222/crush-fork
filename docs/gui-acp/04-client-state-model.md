# Client State Model

This document is normative for synchronization behavior even when the GUI is
implemented in another repository.

## Session status

```text
idle -> queued -> running -> waiting_permission -> running
                    |       -> waiting_user -> running
                    +-> canceling -> completed
                    +--------------> failed
```

Wire values are `idle`, `queued`, `running`, `waiting_permission`,
`waiting_user`, `canceling`, `completed`, and `failed`. `completed` describes
the latest turn; a session may return to `queued` or `running` on a new turn.

## Initial load

1. Negotiate `crush` protocol version/features.
2. Request `crush/session/snapshot`.
3. Install the snapshot atomically and record `latestSequence`.
4. Subscribe with `afterSequence = latestSequence`.
5. Apply events only when `firstSequence == localSequence + 1`, treating a
   missing `firstSequence` as equal to `sequence`, then advance local state to
   `sequence`.
6. Load older message pages lazily as the user scrolls.

The client MUST deduplicate by `eventId` and sequence. It MUST NOT append a
delta twice after reconnect.

## Gap recovery

On a gap, parse error, or `snapshot.required`, the client stops projecting new
events and calls `crush/session/sync` using its last committed sequence. Replay
is applied transactionally. Snapshot mode replaces the bounded session
projection but SHOULD preserve client-only UI state such as scroll position and
draft input.

## Message model

A message has stable `messageId`, role, timestamps, completion state, ordered
content parts, tool-call references, usage, and attachments. Delta events target
a stable message/content-part ID. Completion freezes the message except for
explicit correction events. Paginated completed history and live drafts are
merged by ID, never by array position.

## Optimistic mutations

The GUI MAY optimistically render rename, pin, archive, queue reorder, and
terminal input. It MUST retain `clientRequestId` across retries. A revision
conflict causes rollback or snapshot refresh; it MUST NOT be silently retried
with a new ID.

## MCP projection

The GUI installs the bounded MCP summaries from the session snapshot, then
applies reliable `mcp.status` events by public `serverId`. `starting`,
`reconnecting`, and `stopping` are non-terminal and must not block the rest of
session initialization. A higher MCP revision invalidates any client-derived
tool, instruction, prompt, or resource display even when status remains
`connected`. The GUI never retains or addresses an internal generation name.

Reconnect and disable buttons retain one `clientRequestId` until the mutation
is acknowledged or definitively rejected. Their immediate result updates the
optimistic status; later `mcp.status` events are authoritative. On a replay gap,
the normal session snapshot/sync recovery replaces the MCP projection before
new events are applied.

## Disconnect behavior

Transport disconnect does not cancel turns or terminals. On reconnect the GUI
renegotiates, obtains a snapshot/sync result, restores terminal output by offset,
and resumes provider-auth/MCP status. The backend MAY expire unattached session
runtimes after a configured idle period, but active turns remain alive.
