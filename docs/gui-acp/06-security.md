# Security Requirements

| ID | Requirement |
|---|---|
| GUI-SEC-001 | Resolve and validate all local paths under the workspace root. Existing and newly created paths MUST prevent symlink/junction escape. |
| GUI-SEC-002 | Terminal open/input/kill MUST pass the same permission policy as equivalent shell tool actions. |
| GUI-SEC-003 | MCP discovery, instructions, invocation, status, and caches MUST enforce root-session capability scope and revision. |
| GUI-SEC-004 | Provider tokens, private device/exchange codes, environment secrets, headers, raw provider bodies/errors, and MCP credentials MUST be absent from logs/events/errors. A device-flow user code may be shown only to its owning connection. |
| GUI-SEC-005 | Blob access MUST validate session/client ownership and expiry on every operation. |
| GUI-SEC-006 | Requests MUST have method-specific deadlines, cancellation, maximum nesting, and size limits. |
| GUI-SEC-007 | Protocol stdout MUST contain framed protocol messages only. Logs go to stderr or the configured logger. |
| GUI-SEC-008 | Client FS writes MUST use optimistic revision checks and workspace authorization. |
| GUI-SEC-009 | Destructive session operations MUST be idempotent and auditable. |
| GUI-SEC-010 | Error messages MUST not echo large/raw user payloads. |

Client filesystem path checks are defense in depth. Before any reverse call,
the Agent converts the requested path to an absolute workspace path, rejects a
lexical escape, resolves the workspace and the nearest existing target
ancestor, and verifies the physical result remains below the root. On Windows,
the physical check opens the directory/file and resolves its final handle path
so junctions and other reparse points cannot bypass `EvalSymlinks` behavior.
The GUI client repeats authorization at the actual write boundary. New-file
writes are authorized by the revision token returned for the missing path;
there is no unchecked create fallback.

## Initial limits

Recommended defaults, configurable downward by policy:

| Resource | Limit |
|---|---:|
| Normal JSON-RPC frame | 4 MiB |
| Inline binary recommendation threshold | 64 KiB |
| Mandatory blob threshold | 4 MiB |
| Message page | 200 items |
| Terminal retained output | 2 MiB default, 4 MiB maximum |
| Event replay | 4,096 events or 5 minutes |
| Subscriber logical queue | 256 events |
| MCP log response | 1,000 entries / 1 MiB |
| JSON nesting | 64 levels |
| In-flight request handlers | 128 / 16 MiB aggregate frames |
| Critical/reliable/best-effort writer lanes | 32 / 32 / 256 frames; 8 MiB each |
| Physical frame write | 10 seconds |

An oversized newline-delimited frame MUST be drained through the next delimiter,
reported, and followed by continued service. Authentication and provider-login
methods SHOULD use shorter retention for idempotency results than normal session
mutations.

Transport errors use bounded fixed messages and never include the rejected
frame. Oversized and excessive-nesting requests return `id: null`; an oversized
outbound result is replaced rather than partially written. Handler deadlines
are selected from a fixed method class, caller cancellation applies earlier,
and transport metric names normalize to `stdio`, `pipe`, `socket`, `websocket`,
or `other` to prevent label-cardinality injection. Only the framed transport
writes protocol stdout; `slog` remains outside that writer.

Provider login replay entries retain only a SHA-256 request hash and a safe
result. API keys are validated as bounded UTF-8 without NUL bytes, never echoed,
and cleared from the transient flow after use. Verification URLs are bounded
HTTP(S) URLs without userinfo. Provider adapters may see raw responses and
tokens, but manager/GUI boundaries collapse all failures to stable symbolic
codes and generic messages without logging the underlying error. Closing or
renegotiating a connection cancels its flows and waits until no callback can
send another challenge or terminal event.

MCP capability checks are live rather than copied allow-lists. Dynamic internal
names are permanently marked scoped, including after cleanup, and a tombstone
therefore denies an old cached tool even if no current transport entry exists.
The root session must match and the current generation must be authorized at
both discovery and invocation time. Instructions, status projections, cached
tool signatures, subagents, nested subagents, queued turns, and retries use the
same execution scope and revision.

Raw MCP logging notification data does not enter the configured process logger.
Before GUI retention, the lifecycle service redacts builtin secret patterns and
the exact values of environment variables, headers, URL userinfo/query values,
and OAuth client/access/refresh credentials. It bounds each message/logger and
the shared log store, and exposes only public server IDs plus stable symbolic
failure codes. Redaction happens before storage, not only during serialization.

## Threat-focused tests

Tests MUST attempt `..` traversal, absolute-path injection, Windows junction and
Unix symlink escape, cross-session blob access, cross-session MCP invocation,
stale tool objects after MCP replacement, terminal input without permission,
oversized frames, decompression/base64 bombs, secret-bearing provider errors,
and request-ID reuse with altered parameters.
