# Performance and Reliability

## Latency budgets

| Segment | p95 budget |
|---|---:|
| Decode, validate, enqueue prompt | 20 ms total acknowledgement |
| Provider callback to hub publish | 5 ms |
| Adjacent-event merge eligibility | 16-33 ms; not an added wait window |
| Event encode and local transport enqueue | 10 ms |
| Cancellation request to acknowledgement | 100 ms total |
| Warm session snapshot | 150 ms total |

Instrumentation MUST distinguish provider latency from Crush-added latency.
No lock held by SQLite, a terminal writer, or a client transport may be acquired
on the provider callback hot path.

The coalescer evaluates events that are already adjacent in a connection-local
subscriber queue. It MAY merge a compatible successor within the configured
16-33 ms eligibility range (33 ms from the first event in a merged range), but
it MUST NOT hold an event waiting for that range to expire. A measured
callback-to-transport delay therefore must not be modelled as two serial timer
windows merely because the GUI bridge also batches its own events.

## Backpressure and bounds

- Every queue, journal, buffer, blob, terminal history, and idempotency cache
  MUST have byte/count and lifetime bounds.
- Coalescing MUST preserve UTF-8 correctness, content-part identity, terminal
  byte offsets, and event order.
- Persistence may lag live events but MUST converge at tool/turn boundaries.
- A blocked GUI for 10 seconds MUST not stall provider reads.
- Oversized frames MUST produce a request error and framing recovery; they MUST
  NOT terminate the server.
- Duplicate/late JSON-RPC responses MUST never block a dispatch goroutine.

The stdio implementation enforces these concrete transport bounds: 32 critical
frames, 32 reliable frames, 256 best-effort frames, 8 MiB queued per lane, 128
concurrent request handlers, 16 MiB aggregate in-flight request frames, and one
additional physical frame. A blocked physical write is terminated after 10
seconds. Reliable GUI delivery may block only its connection-local subscription
consumer; Hub publication, provider consumption, and persistence remain
non-blocking and use their existing overflow-to-snapshot behavior.

## Benchmarks

Add stable benchmarks named:

```text
BenchmarkACPTextDelta
BenchmarkSessionSnapshot
BenchmarkSessionMessagePage
BenchmarkTerminalOutputCoalescing
BenchmarkLongSessionLoad
BenchmarkConcurrentSessions
```

Benchmarks MUST report allocations and bytes. CI may compare against a checked-in
baseline with a documented tolerance; release builds MUST run the SLO harness on
Windows and at least one Unix platform.

## Soak test

The release soak scenario is:

- 100 sessions with 10,000 messages each;
- 10 active concurrent sessions;
- 1,000 aggregate chunks per second;
- one GUI blocked for 10 seconds;
- repeated disconnect/reconnect and sequence recovery;
- concurrent terminal output and large-file/blob activity;
- injected provider timeout and MCP reconnect.

After warm-up, heap, goroutine count, active subscriptions, retained terminal
bytes, and retained blob bytes MUST stabilize within configured bounds. The test
MUST run under `-race` in a reduced profile and without race in the full profile.

## Fault injection

Tests MUST cover SQLite busy/slow, blocked pipe, GUI crash, MCP hang, provider
timeout, missing permission response, backend restart, disk full, malformed or
oversized attachments, duplicate request IDs, and expired replay sequences.

## Metrics

Implement at least:

```text
acp_request_duration
gui_event_queue_depth
gui_event_coalesced_total
gui_snapshot_total
gui_sequence_gap_total
session_load_duration
stream_chunk_to_event_duration
stream_event_to_write_duration
sqlite_flush_duration
terminal_retained_bytes
blob_retained_bytes
active_subscription_count
active_prompt_count
```

Metric labels MUST NOT contain prompt text, file contents, secrets, arbitrary
session IDs, or other unbounded cardinality.
