# GUI ACP Implementation Status

This file is the durable checkpoint for autonomous implementation. Agents MUST
update it as work proceeds. Evidence means exact file paths, test/benchmark
commands, and observed outcomes; statements such as "tests pass" without a
command are insufficient.

Overall status: `blocked`  
Release scope: Phases 0-5, WP-01 through WP-17  
Active work package: WP-17  
Last updated: 2026-07-15

Allowed status values: `pending`, `in_progress`, `complete`, `blocked`.

## Work-package checklist

| Package | Status | Dependencies | Requirements | Evidence summary |
|---|---|---|---|---|
| WP-01 | complete | none | GUI-OBS-001, GUI-PERF baseline | Fixed-label metrics contract, ACP/persistence hooks, six named benchmarks, race tests. |
| WP-02 | complete | WP-01 | GUI-EVENT-001/002 | Per-session sequence hub, bounded journal, coalescing, reliable overflow recovery, race tests. |
| WP-03 | complete | WP-02 | GUI-PERF-002/005/006 | App-owned Hub wired to all Agents; direct callbacks, bounded payloads, persistence isolation, race tests. |
| WP-04 | complete | WP-02 | GUI-COMPAT-001/002 | Versioned feature negotiation, independent crush/* router, structured gating errors, compatibility tests. |
| WP-05 | complete | WP-02, WP-04 | GUI-EVENT-001/002/003 | Reliable replay/live subscription, response ordering, reconnect, overflow recovery, and race tests. |
| WP-06 | complete | WP-05 | GUI-SESS-002, GUI-PERF-003 | Two-query bounded projection, indexed 20-message tail, snapshot-mode sync, and 10k-message SLO evidence. |
| WP-07 | complete | WP-04 | GUI-SESS-001 | Indexed scoped keyset cursors, bounded redacted pages/search, mutation stability, and large-fixture evidence. |
| WP-08 | complete | WP-05, WP-09 | GUI-SESS-001 | Bounded get/CRUD projections, persisted archive/pin, pre-delete teardown, constant-memory transactional fork, idempotency and race coverage. |
| WP-09 | complete | WP-03, WP-05 | GUI-TURN-001, GUI-EVENT-003 | Serialized turn runtime, queue/steer/retry handlers, bounded exact-outcome idempotency, cancellation milestones, race and p95 evidence. |
| WP-10 | complete | WP-09 | GUI-SESS-003 | Persisted CAS overrides, five-level precedence, frozen turns, explicit child-session policy, bounded GUI projection, and race coverage. |
| WP-11 | complete | WP-04, WP-05 | GUI-TERM-001, GUI-SEC-002 | PTY/ConPTY lifecycle, Bash-equivalent permissions, bounded offset snapshots/events, reconnect, dual ownership, cleanup, and race coverage. |
| WP-12 | complete | WP-04 | GUI-BLOB-001, GUI-SEC-005 | App-owned bounded Blob registry, dual ownership, expiring handles, ranged IO, deferred attachments, cleanup, metrics, and race coverage. |
| WP-13 | complete | WP-04, WP-12 | GUI-FS-001, GUI-SEC-001/008 | Connection/root-session scoped reverse client FS, revision CAS, Blob transfer, physical root confinement, tool/event metadata, lifecycle and race coverage. |
| WP-14 | complete | WP-04, WP-05 | GUI-AUTH-001, GUI-SEC-004 | App-owned provider catalog/auth manager, connection-owned async flows, safe DTOs/errors, atomic credential lifecycle, idempotency and race coverage. |
| WP-15 | complete | WP-05, WP-06 | GUI-MCP-001/002/003 | App-owned asynchronous MCP lifecycle, root-session capability isolation, GUI control/status/logs, cleanup and race coverage. |
| WP-16 | complete | WP-04 | GUI-PERF-006, GUI-SEC-006/007 | Framed transport abstraction, three-lane bounded writer, request/deadline limits, recovery and contract race coverage. |
| WP-17 | blocked | WP-01..WP-16 | All release gates | All implementation and Windows gates pass; actual Unix SLO execution requires an unavailable runtime or commit/push authority for CI. |

## Active package record

Copy and fill this block whenever a package becomes active:

```text
Package: WP-17
Started: 2026-07-15
Baseline inspected: WP-01 through WP-16 had package evidence but no integrated
  release harness, release Taskfile/CI entry point, full fault matrix or current
  root/Fantasy failure classification.
Applicable pitfalls read: WP-17 exercises but does not rewrite Fantasy message
  state or MCP media encoding. Harnesses must use deterministic fake streams,
  transports and services; no real LLM/provider/MCP call or VCR recording.
Feature-research codebase search was attempted and the service returned 429;
repository-wide search then covered existing stress/fault/resource tests,
Taskfile/CI conventions, hooks, Windows paths and Fantasy VCR fixtures.
Files changed: new internal/releasegate/release_test.go and
  .github/workflows/gui-acp.yml; Taskfile.yaml; internal/acp/{handler.go,
  metrics_test.go,server.go,transport_test.go}; internal/guiapi/turns_test.go;
  docs/gui-acp/09-goal-loop-quickstart.md and this checkpoint.
Implementation: Added reduced/full profiles to one deterministic release
  harness. The full profile executes 100 sessions by 10,000 retained-history
  events, ten concurrent active sessions paced at approximately 1,000 aggregate
  chunks/second, one unread GUI for ten seconds, three reconnect/replay cycles,
  concurrent fake-terminal output and Blob create/read/release, provider timeout
  and real mcplifecycle replacement/reconnect over an injected backend. It
  asserts Hub publish p95 below 5 ms, bounded post-GC heap/goroutines, zero
  active subscriptions, zero active terminals/terminal bytes and zero Blob
  count/bytes. A real SQLite 10,000-message fixture samples 100 warm snapshots
  and enforces p95 below 150 ms.
Fault/security: Executable faults cover SQLite busy, cancelled slow query and
  SQLITE_FULL; blocked GUI/crash and backend restart; expired replay; malformed/
  oversized Blob; duplicate request IDs; provider/missing-response timeout;
  blocked transport read/write; missing permission response; request deadline/
  nesting; MCP hang/reconnect/revocation; provider cancellation/error redaction;
  and oversized terminal input. The missing-permission test found and fixed a
  shutdown edge where a cancellable custom transport's context error was
  reported as an ACP read failure instead of clean connection cancellation.
  ActivePromptCount is now emitted on prompt install, replacement-safe cleanup
  and Handler close. All thirteen required metrics have production emitters and
  fixed labels. Production stdout search found only the intentional
  NewLineTransport(os.Stdin, os.Stdout) protocol boundary.
Entry points: task test:gui-release runs reduced race, deterministic fault and
  full non-race/benchmark gates. Separate race/fault/full tasks are available.
  Task race commands explicitly enable CGO because the Go race runtime requires
  it while normal Crush builds remain CGO-disabled. CI runs reduced race on
  Windows/macOS/Linux and full non-race soak on main, schedule or manual
  dispatch. Workflow actions are commit-pinned and YAML/task keys have a test.
Commands and results:
  go test -race ./internal/releasegate ./internal/acp/...
    ./internal/guiapi/... ./internal/sessionevent/... ./internal/terminal
    ./internal/blob ./internal/mcplifecycle ./internal/providerauth
    ./internal/idempotency ./internal/turn -count=1 -timeout=10m
    PASS: releasegate 8.278s, ACP 10.354s, GUI API 7.314s,
    sessionevent 2.051s, terminal 1.902s, blob 1.482s, MCP 10.457s,
    provider auth 2.590s, idempotency 1.497s and turn 1.377s.
  go test -race ./internal/agent ./internal/session -count=1 -timeout=10m
    PASS: Agent 44.444s; Session 1.819s.
  CRUSH_GUI_SOAK_FULL=1 go test ./internal/releasegate -run
    '^TestGUIACP(ReleaseSoak|WarmSnapshotP95)$' -count=1 -timeout=20m -v
    PASS, 13.015s. Exactly 100x10,000 history, ten active sessions, ten-second
    blocked GUI and 9,997 chunks; publish p95 below the Windows clock quantum,
    heap growth 687,376 bytes, goroutine growth one, all owned resources zero;
    real-SQLite warm snapshot p95 516.6 us.
  go test -race ./internal/guiapi -run
    '^TestTurnCancellationAcknowledgementP95BelowHundredMilliseconds$'
    -count=10 -timeout=5m
    PASS, 2.236s. One thousand cancellation acknowledgement samples.
  go test -race ./internal/turn -run
    '^TestStartAcknowledgementP95BelowTwentyMilliseconds$' -count=20
    PASS, 1.306s. Two thousand acknowledgement samples.
  go test ./internal/guiapi -run '^$' -bench
    '^BenchmarkTurnStartAcknowledgement$' -benchmem -benchtime=100x
    PASS: 25,999 ns/op, p95 39.30 us, 9,258 B/op, 42 allocs/op.
  go test ./internal/acp -run '^$' -bench
    'Benchmark(ACPTextDelta|SessionSnapshot|SessionMessagePage|TerminalOutputCoalescing|LongSessionLoad|ConcurrentSessions)$'
    -benchmem -benchtime=1000x -count=1
    PASS: ACP delta 1,523 ns/op; 20-message snapshot 3,497 ns/op;
    200-message page 28,596 ns/op; terminal coalescing 67.70 ns/op;
    10,000-message full-history comparison 1.334 ms/op; concurrent sessions
    9.900 ns/op. The production bounded snapshot SLO is the real-SQLite harness
    above, not the full-history comparison benchmark.
  go test ./... -count=1 -timeout=20m from repository root with
    FANTASY_VCR_RECORD unset: PASS for every root package; Agent 36.804s,
    ACP 9.659s, GUI API 5.024s, hooks 4.158s and releasegate 6.562s.
  go test ./... -count=1 -timeout=20m from fantasy/ with
    FANTASY_VCR_RECORD unset: PASS for all Fantasy/provider/providertests
    packages; providertests 3.237s. newVCRRecorder defaults to ReplayOnly and
    records only when FANTASY_VCR_RECORD=1, so normal tests require no network,
    provider credential or real LLM. The offline protocol cassettes retain
    provider request/response compatibility value and were not deleted.
  go vet ./... in root and fantasy/: PASS, no output.
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build selected GUI/ACP packages;
    GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build the same packages:
    PASS. Native Windows full/race/SLO commands above pass.
  git diff --check: PASS, no output. git diff --stat and git status --short
    inspected; the cumulative dirty WP-01..WP-17 worktree and unrelated user
    docs were preserved, with no commit/push/PR.
Full-repository classification: No current failures. The previously observed
  Windows temporary-log contention, hook timeout and Fantasy OpenAI cassette
  mismatch were investigated by fresh complete runs and did not reproduce;
  they are not being hidden as accepted failures and no introduced failure
  remains.
Status: blocked. All local implementation and Windows gates pass. The one
  remaining release-signoff item is an actual Unix SLO execution. Linux and
  Darwin production cross-builds pass and the committed-shape CI definition
  requires Unix execution, but this Windows host has no WSL distribution or
  Docker and the uncommitted workflow cannot be dispatched without separate
  commit/push authority. Do not mark WP-17/global complete until that CI or an
  equivalent Unix run passes.
```

## Completed-package evidence

Move completed active-package records here. Do not delete evidence when a later
package changes the same files; append regression verification to both affected
records.

### WP-16

```text
Package: WP-16
Started/completed: 2026-07-15
Baseline inspected: internal/acp/server.go already had newline framing with a
  4 MiB cap, oversized-line drain/recovery, one writer goroutine, bounded sync/
  async channel counts, sync preference, context-aware calls, and non-blocking
  duplicate-response delivery. Framing, physical I/O, dispatch and queue
  lifecycle were still embedded directly in Server; writer shutdown could not
  reliably interrupt blocked generic I/O, frame queues had no byte budget,
  request goroutines were unbounded, responses shared a lane with reliable GUI
  events, and no reusable transport contract suite proved the invariants.
Applicable pitfalls read: WP-16 did not change Fantasy message accumulation or
  MCP media encoding, so neither documented pitfall path was modified. The
  feature-research semantic search covered ACP stdio parsing/dispatch/writes and
  existing ACP tests; repository search also covered guiapi reliable NotifySync,
  sessionevent overflow/backpressure, fixed-label metrics and server integration
  tests.
Files changed: new internal/acp/transport.go and transport_test.go;
  internal/acp/server.go, handler.go, metrics_test.go and
  performance_bench_test.go; internal/guiapi/service.go; docs/gui-acp/
  {01-architecture.md,02-protocol-spec.md,03-server-implementation.md,
  05-performance-and-reliability.md,06-security.md}.
Requirements: GUI-PERF-006 and GUI-SEC-006/007; completes the transport portion
  of GUI-COMPAT-001/002 while retaining standard ACP wire schemas.
Transport/framing: Added the exported acp.Transport boundary with Name,
  ReadFrame, WriteFrame and Close. LineTransport implements newline-delimited
  stdio/pipe framing with a 4 MiB payload cap, exact-limit acceptance, CRLF
  input, LF output, complete short-write loops, invalid UTF-8 rejection,
  embedded-delimiter rejection, oversize drain and cancellable blocked I/O via
  underlying io.Closer handles. Oversized, invalid-UTF8, malformed and nesting-
  excessive input produce small id:null JSON-RPC errors and the following frame
  remains usable. Outbound responses above the cap become a small same-ID error
  instead of a partial frame.
Writer/backpressure: Server owns three serialized bounded lanes: critical
  responses/reverse calls, reliable NotifySync messages, and best-effort Notify
  messages. Capacities are 32/32/256 with independent 8 MiB queued-byte budgets
  and one in-progress frame. Critical traffic precedes reliable and best-effort
  traffic. A 10-second physical write deadline closes a non-reading client;
  enqueue shutdown is atomic, waiting calls receive failure, and every queued
  frame reference/counter is drained. GUI reliable delivery still blocks only
  its connection-local subscription consumer; Hub publish/provider/persistence
  paths retain their bounded non-blocking overflow behavior.
Dispatch/security: JSON nesting is capped at 64. Application dispatch is capped
  at 128 handlers and 16 MiB aggregate request-frame bytes. Capacity errors are
  bounded critical responses; incoming responses bypass request capacity so
  permission/client-FS replies cannot be starved. Duplicate responses remain
  non-blocking. Fixed request classes enforce 30-second default, 30-minute
  prompt, 5-minute permission, and 5-minute-plus-margin turn/wait deadlines;
  caller/connection cancellation applies earlier. Active handlers are tracked
  through disconnect. Response lifecycle callbacks observe exactly one write
  outcome. Panic/marshal/deadline/frame errors use fixed messages without raw
  payloads. Transport metric labels normalize to a fixed five-value set.
Stdout/compatibility: NewServer constructs exactly one LineTransport over
  os.Stdin/os.Stdout. Static production search found no fmt.Print/Fprint stdout
  path in internal/acp or internal/cmd/acp.go. All logs remain slog calls outside
  the framed writer. NewServerWithIO remains compatible, while
  NewServerWithTransport lets a pipe/socket adapter reuse the identical
  JSON-RPC router. Standard ACP best-effort Notify behavior and every standard
  request/result schema are unchanged; reliable GUI events still use NotifySync.
Commands and results:
  go test -race ./internal/acp -count=10 -timeout=300s
    PASS, 76.841s. Covers the complete standard ACP suite plus exact-limit/
    oversized/malformed/UTF-8/nesting recovery, short writes, blocked read/write
    cancellation, critical/reliable/best-effort priority, byte/count cleanup,
    request capacity, response fast path, duplicate response, graceful EOF,
    response lifecycle failure and protocol-only output contracts.
  go test -race ./internal/guiapi -count=10 -timeout=300s
    PASS, 48.038s. Reliable subscription response ordering, blocked writer
    isolation, reconnect and all negotiated GUI APIs remain green.
  go test -race ./internal/sessionevent -count=20 -timeout=180s
    PASS, 3.226s. Blocked subscriber publication, overflow recovery and bounded
    reliable/coalesced queues remain green.
  go test -race ./internal/agent -count=1 -timeout=300s
    PASS, 44.208s. Provider consumption, persistence and Fantasy message-state
    compatibility remain green.
  go test -race ./internal/acp ./internal/guiapi -count=1 -timeout=300s
    PASS after final error-semantics audit: ACP 9.462s; GUI API 6.602s.
  go test ./internal/acp -run '^$' -bench '^BenchmarkACPTextDelta$'
    -benchmem -benchtime=10000x -count=5
    PASS: 1,470-1,582 ns/op, 753-754 B/op, 4 allocs/op on windows/amd64.
    This improves on the WP-01 recorded 1,911 ns/op, 805 B/op baseline and is
    far below the 10 ms encode/enqueue budget.
  go test ./internal/... -run '^$' -count=1 -timeout=300s
    PASS; every internal package compiles, 25.282s wall time.
  go vet ./internal/acp/... ./internal/guiapi ./internal/sessionevent
    ./internal/agent ./internal/cmd
    PASS, no output.
  rg production stdout writers found only NewLineTransport(os.Stdin, os.Stdout);
    no print/log bypass. gofumpt applied to all changed Go files and
    git diff --check PASS.
Compatibility/migration: No database migration, persisted format, feature bit,
  standard ACP schema or GUI DTO changed. Stdio remains the only version-1
  production adapter; Named Pipe/Unix Socket/WebSocket implementations remain
  optional Phase 6 work but now have a tested framing interface. Automated
  tests use in-memory deterministic transports only; no LLM, provider, MCP,
  VCR, socket or subprocess is required.
Remaining risk: WP-17 must run the integrated reduced/full soak, fault matrix,
  full-repository classification, cross-platform framing build/tests, complete
  requirement audit and release evidence. Alternative production transports
  and workspace supervision remain optional Phase 6.
Blockers: None.
```

### WP-15

```text
Package: WP-15
Started/completed: 2026-07-15
Baseline inspected: Handler-owned synchronous dynamic MCP replacement,
  generation/tombstone/revision capability isolation, process-global MCP
  transport state/events/cache/reconnect/circuit breaker, ConfigStore ephemeral
  entries, GUI routing/reliable session events/snapshots and App shutdown.
Applicable pitfalls read: No Fantasy message-state or MCP media payload path was
  changed. The existing execution-scoped MCP access contract remains the only
  discovery/invocation/instructions/cache authorization boundary. Every test
  used an injected deterministic backend; no MCP process, network, provider,
  LLM, or VCR cassette was used.
Files changed: new internal/mcplifecycle/{types.go,backend.go,service.go} and
  deterministic tests; new internal/guiapi/mcp.go and mcp_test.go plus service,
  turn, sync and snapshot integration; internal/agent/tools/mcp/init.go;
  internal/config/store.go and provider_credentials_test.go; internal/acp/
  {handler.go,adapter.go} and ACP tests; internal/sessionevent/{event.go,hub.go}
  and hub tests; internal/app/app.go and app_test.go; internal/cmd/acp.go;
  docs/gui-acp/{01-architecture.md,02-protocol-spec.md,
  03-server-implementation.md,04-client-state-model.md,06-security.md}.
Requirements: GUI-MCP-001, GUI-MCP-002, GUI-MCP-003, and the MCP portions of
  GUI-SEC-003/004.
Architecture/lifecycle: The App owns one mcplifecycle.Service over the existing
  process-global transport backend. ACP new/load only validates and schedules
  ReplaceAsync, so the response never waits for startup. Handler close binds to
  an opaque connection owner; owner selection and immediate revocation are
  atomic, preventing an old connection teardown from closing a replacement
  owner's generation. Session deletion and App shutdown revoke before cleanup.
  Dynamic entries have independent cancellation scopes, so reconnect/disable of
  one server cannot cancel a sibling that is still starting. Static operations
  are generation-checked and chained in request order, preventing a stale
  reconnect config write from undoing a newer disable.
Isolation/capabilities: Dynamic public IDs are session:<client-name>; configured
  static IDs are static:<config-name>. Every connect/reconnect uses a fresh
  collision-resistant internal generation and permanent scoped tombstone.
  Replacement first revokes old authorization, then disables/removes ephemeral
  config, and grants only after connected/degraded success. The same live Access
  object protects tool discovery, instructions/cache signatures and invocation;
  standard unscoped TUI/CLI still see configured static MCP. GUI turns/retries,
  ACP prompts, root agents, subagents and nested subagents inherit the root
  scope. No Coordinator session-to-MCP map, discovery-only authorization, or
  MCP-triggered UpdateModels call was introduced.
GUI wire/events: Negotiated mcpControl adds bounded list/status/reconnect/
  disable/logs handlers. Mutations require connection-local UUID idempotency and
  exact retries replay without repeating transport work. Reliable mcp.status
  events advance session revision; snapshots contain only bounded public
  resource summaries. Internal generation names, config, commands, args, env,
  headers, URLs, OAuth values and raw transport errors do not enter GUI DTOs.
  Raw MCP logging notification data is published in-process without entering
  slog, then builtin and exact config-secret redaction plus field/count/byte
  bounds are applied before retention. MCP configs now use one full deep-copy
  helper including OAuth token, registration, auth-server slices and scopes.
Commands and results:
  go test -race ./internal/mcplifecycle -count=10 -timeout=300s
    PASS, 83.983s after final owner, per-server cancellation and ordered static
    operation fixes. Earlier focused 10-run race coverage for the two lifecycle
    races passed in 25.200s.
  go test -race ./internal/config ./internal/mcplifecycle -count=1
    -timeout=300s
    PASS: Config 9.134s; lifecycle 10.839s after the final OAuth deep-copy and
    credential-bearing URL redaction changes.
  go test -race ./internal/guiapi -count=10 -timeout=300s
    PASS, 53.163s. Covers safe DTOs, cross-session denial, mutation
    idempotency/conflict, reliable status events, snapshots, bounded redacted
    logs, turn scope propagation and immediate stale-generation revocation.
  go test -race ./internal/acp/... ./internal/guiapi -count=1
    -timeout=300s
    PASS: ACP 9.632s; GUI API 8.312s after final integration changes. Full ACP
    new/load timing, standard compatibility, isolation, replacement and shutdown
    paths remain green.
  go test -race ./internal/agent/tools/mcp ./internal/sessionevent
    ./internal/config ./internal/app -count=1 -timeout=300s
    PASS: MCP transport 7.648s; sessionevent 2.249s; Config 11.737s; App
    10.123s. Covers transport projection, revision advancement, detached config,
    App ownership, deletion and shutdown ordering.
  go test -race ./internal/agent/tools -count=1 -timeout=300s
    PASS, 12.979s. Discovery and invocation-time authorization plus stale tool
    denial remain green.
  go test -race ./internal/agent -count=1 -timeout=300s
    PASS, 49.447s. Fantasy dual-message-state and Agent compatibility remain
    green.
  go test ./internal/... -run '^$' -count=1 -timeout=300s
    PASS; every internal package compiles with the final lifecycle integration.
  go vet ./internal/mcplifecycle ./internal/guiapi ./internal/acp/...
    ./internal/agent/tools ./internal/agent/tools/mcp ./internal/agent
    ./internal/config ./internal/app ./internal/sessionevent ./internal/cmd
    PASS, no output, 7.179s.
  gofumpt applied to every changed Go file; git diff --check PASS.
Compatibility/migration: No database migration or persistent schema change.
  Standard ACP schemas are unchanged; clients that do not negotiate mcpControl
  retain standard behavior. TUI/CLI keep process-global static MCP visibility.
  Internal generation IDs are intentionally unstable and never persisted as a
  public identifier. No real MCP/provider/LLM call, VCR fixture, global model
  refresh, unbounded GUI payload or protocol-stdout log was added.
Remaining risk: WP-16 owns framing, blocked-writer and transport hardening;
  WP-17 owns integrated soak/fault/security release gates. Existing non-GUI MCP
  diagnostics outside the new retained GUI log boundary remain part of the
  final integrated security audit rather than a GUI DTO surface.
Blockers: None.
```

### WP-14

```text
Package: WP-14
Started: 2026-07-13
Completed: 2026-07-15
Baseline inspected: Catwalk/configured provider and model catalogs, ConfigStore
  OAuth/API-key persistence and refresh, CLI Hyper/Copilot device flows, ACP
  reliable notification writer, guiapi negotiation/idempotency/lifecycle,
  session events, App ownership/shutdown, and secret-redaction facilities.
Applicable pitfalls read: No Fantasy message-state or TUI rendering path was
  changed. Provider authentication is an independent App service; all tests
  inject mock catalogs, credential stores and login flows without real provider
  credentials, HTTP calls, LLM calls, or VCR cassettes.
Files changed: new internal/providerauth/{types.go,manager.go,
  config_backend.go,default_flows.go} and deterministic domain/projection tests;
  new internal/guiapi/providers.go and providers_test.go plus service lifecycle;
  internal/config/store.go and provider_credentials_test.go; internal/app/app.go
  and app_test.go; internal/cmd/acp.go; docs/gui-acp/{01-architecture.md,
  02-protocol-spec.md,03-server-implementation.md,06-security.md}.
Requirements: GUI-AUTH-001 and GUI-SEC-004.
Architecture/wire: Added an App-owned providerauth.Manager. ConfigBackend reads
  detached ConfigStore snapshots, merges known Catwalk and configured/custom
  providers by ID, prefers configured models, sorts stable results, and projects
  only provider display/auth state and documented model capabilities. API keys,
  OAuth tokens, endpoint/userinfo, default/extra headers, costs, and arbitrary
  provider/model options never enter provider/model/auth-status DTOs.
  GUI methods implement provider/list, models, auth_status, login, login_cancel
  and logout. Login/cancel/logout require bounded UUID idempotency. Replay stores
  retain SHA-256 payload hashes and safe results only. Login allocates an opaque
  connection-owned login ID during the handler and starts its flow through
  ResponseLifecycle only after the response write succeeds; an exact retry
  returns the same ID and Start is once-only. Cancel uses the same response-first
  ordering. Reliable crush/provider/auth_event notifications expose only browser
  or device user-code challenges and safe terminal states.
  Hyper and GitHub Copilot reuse existing device-flow clients behind injected
  Flow factories. A generic browser flow contract and API-key flow are available
  without adding provider-specific HTTP in guiapi. Private device codes and all
  credentials remain inside the flow/backend boundary. API keys are bounded to
  64 KiB valid UTF-8 without NUL bytes and cleared from the transient flow after
  use. Challenge URLs are bounded HTTP(S) without userinfo.
Ownership/cleanup: One active login per provider is globally serialized, while
  every login is bound to an unguessable ACP connection owner. Cross-connection
  cancel collapses to CRUSH_PROVIDER_LOGIN_NOT_FOUND. Connection close and every
  renegotiation fail closed, replace the connection replay epoch, cancel matching
  flows, cancel event contexts and wait through a callback completion barrier;
  no event can appear after teardown returns. App shutdown closes the provider
  manager before other App-owned event resources.
Credential consistency/security: Provider flow failures, persistence failures
  and raw HTTP/provider bodies are discarded and become only
  CRUSH_PROVIDER_LOGIN_FAILED plus a generic message; no underlying error is
  logged. ConfigStore validates provider existence before persistence, rejects
  unsupported/empty credentials, atomically writes API-key/OAuth pairs, clears
  both persisted credentials in one read/modify/write before changing memory,
  and preserves provider/model configuration. OAuth refresh now compares the
  original credential under the write lock before persistence, so concurrent
  logout cannot be undone by a late refresh. String API-key replacement removes
  stale persisted OAuth state.
Commands and results:
  go test -race ./internal/providerauth -count=50 -timeout=180s
    PASS, 3.166s. Covers browser/device/API-key flows, response-deferred start,
    cancellation and owner isolation, connection callback barriers, logout
    ordering, generic failure projection, bounds/retention and concurrent login.
  go test -race ./internal/providerauth ./internal/guiapi -count=10
    -timeout=300s
    PASS: providerauth 2.729s; guiapi 11.961s. Covers stable discovery/model
    DTOs, login response-before-event ordering, exact retry, altered-ID conflict,
    no API-key echo, cross-connection denial, renegotiation replay revocation,
    close/no-late-event behavior, cancel/logout and raw provider error removal.
  go test -race ./internal/app ./internal/config -count=1 -timeout=300s
    PASS: App 8.195s; Config 9.504s. Covers App ownership/shutdown, detached
    snapshots, atomic clear, persistence-failure rollback, validation before
    persistence, encrypted OAuth/API-key persistence and stale OAuth removal.
  go test -race ./internal/acp/... -count=1 -timeout=300s
    PASS, 7.476s. Standard ACP behavior and existing MCP isolation regressions
    remain green.
  go test -race ./internal/agent -count=1 -timeout=300s
    PASS, exit 0 after 53.7s. No Agent/Fantasy message-state path changed.
  go test ./internal/cmd -count=1 -timeout=180s
    PASS, 1.882s. ACP command wiring compiles and command tests remain green.
  go test ./internal/... -run '^$' -count=1 -timeout=300s
    PASS, exit 0 after 28.862s; every internal package compiles. The first run
    hit the pre-existing Windows package-enumeration race while an Agent test
    removed internal/agent/crush-test-tmp/TestRunDequeuesBeforeTitleGenerationCompletes;
    the immediate quiescent rerun passed and no WP-14 stack was involved.
  go vet ./internal/providerauth ./internal/guiapi ./internal/config
    ./internal/app ./internal/cmd ./internal/acp/...
    PASS, no output, 7.021s.
  gofumpt applied to every changed Go file; git diff --check PASS.
Compatibility/migration: No database migration or persistent schema change.
  Standard ACP clients and TUI/CLI never negotiate providerAuth and retain their
  existing catalog/login paths. Existing global provider config remains the
  credential source. No model refresh, Agent tool change, MCP capability change,
  real-provider test, VCR cassette, stdout logging or unbounded resource was
  introduced.
Remaining risk: Final framing/fault/soak and integrated security gates remain
  WP-16/WP-17. WP-15 must expose asynchronous MCP control while preserving the
  existing root-session capability/revision isolation.
Blockers: None.
```

### WP-13

```text
Package: WP-13
Started/completed: 2026-07-13
Baseline inspected: standard ACP reverse fs capabilities, connection-scoped
  crush/* negotiation/router and outgoing calls, GUI turn dispatch, Blob
  ownership/lifecycle, Coordinator root/subagent context propagation, and the
  read/write/edit file tool paths and metadata projection.
Applicable pitfalls read: docs/pitfalls/fantasy-dual-message-state.md. Client
  FS changes tool I/O before Fantasy accumulates the result; it does not rely
  on a callback-only message rewrite. Standard ACP and unscoped TUI/CLI retain
  their existing local filesystem path.
Files changed: new internal/clientfs/{clientfs.go,path.go,path_windows.go,
  path_unix.go,scope.go} and deterministic platform/domain tests; new
  internal/guiapi/clientfs.go plus service, turn, session-delete, event-wire
  integration and tests; internal/turn/service.go and tests; internal/acp/
  {handler.go,server.go,types.go} and tests; internal/agent/tools/{read.go,
  write.go,edit.go} and clientfs_test.go; internal/agent/{agent.go,
  clientfs_metadata.go} and tests; internal/sessionevent/event.go;
  internal/cmd/acp.go; docs/gui-acp/{01-architecture.md,02-protocol-spec.md,
  03-server-implementation.md,06-security.md}.
Requirements: GUI-FS-001 and GUI-SEC-001/008.
Wire/implementation: Defined crush/fs/read, crush/fs/stat and crush/fs/write as
  negotiated Agent-to-GUI reverse calls, not inbound local-disk handlers.
  Read/stat return an opaque revision and original sourceUri; missing stat
  states also return a revision so creation is CAS-authorized. Write requires
  expectedRevision and UUID clientRequestId, sends UTF-8 inline only through
  64 KiB, and uses connection/session-owned WP-12 Blobs for larger or binary
  content. Temporary write Blobs are released after the response; read handles
  retain normal Blob ownership/lifetime checks. Responses validate size,
  metadata limits, requested path identity and handle ownership.
  guiapi owns a separate scope map per ACP connection. Each entry is bound to
  one root session and workspace. Standard session/prompt and queued GUI turns
  inject it into the Coordinator context; subagents and retries inherit it.
  Session deletion, connection close, workspace replacement and every
  renegotiation close old scopes and clear revisions. There is no package or
  process-global capability singleton.
  Read, write and every edit mode use clientfs Stat/ReadFile/WriteFile. The
  scope remembers the exact revision used to compute output and does not
  refresh it immediately before a write, so concurrent editor changes produce
  CRUSH_REVISION_CONFLICT without an overwrite. Source URI/revision are added
  to redacted file-tool metadata, standard ACP tool updates, and reliable GUI
  tool events; old/new file content is excluded from event metadata.
  ResolvePath lexically confines absolute/relative requests, resolves the
  nearest existing ancestor for new paths, and checks the physical target
  below the physical workspace. Windows uses GetFinalPathNameByHandle so
  junction/reparse escapes are rejected; Unix resolves symlinks. The client is
  required to repeat authorization at its actual write boundary.
Commands and results:
  go test -race ./internal/clientfs -count=50 -timeout=180s
    PASS, 11.168s. Covers unsaved content, missing-state creation revisions,
    stale writes, URI preservation, inline/Blob limits, lifecycle, concurrent
    access, lexical traversal, absolute injection, and real Windows junction
    escape for existing/new targets.
  go test -race ./internal/guiapi -count=10 -timeout=240s
    PASS, 11.261s. Covers negotiation, independent session scopes, real Blob
    bridge ownership, cross-session denial, temporary release, session close,
    renegotiation revocation, connection cleanup and event projection.
  go test -race ./internal/turn ./internal/sessionevent -count=10
    -timeout=240s
    PASS, 1.426s and 3.087s. Scope survives dispatch/retry cloning; reliable
    events preserve bounded source URI/revision metadata.
  go test -race ./internal/acp/... -count=3 -timeout=240s
    PASS, 20.104s. Standard prompt context injection, structured reverse-call
    errors, ordinary ACP compatibility and existing ACP isolation regressions.
  go test -race ./internal/agent/tools -count=3 -timeout=240s
    PASS, 42.501s. Full tool suite plus client-backed read/write/edit, no local
    write bypass, stale-edit rejection and metadata assertions.
  go test -race ./internal/agent -count=1 -timeout=300s
    PASS, 60.546s. An earlier broad run exposed the existing intermittent
    streamMessageFlusher versus Message.AddFinish race in
    TestSessionAgentRunSkipsTitleGenerationForNonInteractiveCalls; its stack
    did not enter WP-13 code. The exact test then passed with -count=20 in
    4.296s and the complete Agent race rerun passed as recorded above.
  go test -race ./internal/app -count=1 -timeout=180s
    PASS, 21.286s.
  go test ./internal/... -run '^$' -timeout=240s
    PASS; all internal packages compile with the client FS integration.
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -o
    C:/tmp/crush-clientfs-linux.test ./internal/clientfs
    PASS; temporary binary removed. Native Windows junction tests ran in the
    repeated race suite.
  go vet ./internal/clientfs ./internal/guiapi ./internal/turn
    ./internal/acp/... ./internal/agent/tools ./internal/sessionevent
    PASS.
  gofumpt applied to every changed Go file; git diff --check PASS.
Compatibility/migration: No database migration or persistent format change.
  Clients that omit private clientFS negotiation never receive reverse
  crush/fs calls. The standard ACP fs capability fields remain parsed without
  changing their wire contract; revision-sensitive writes are never silently
  downgraded to standard fs/write_text_file.
Remaining risk: Final end-to-end framing/fault/soak coverage remains WP-16/17.
  The unrelated intermittent Agent flusher race is separately classified above
  and was not reproduced in its 20-run isolation or final full rerun.
```

### WP-11

```text
Package: WP-11
Started/completed: 2026-07-13
Baseline inspected: shell/background execution, Bash permission request and
  AutoPermission exec-policy type contracts, available PTY dependencies,
  Windows ConPTY support, GUI routing/idempotency, terminal event coalescing,
  snapshot resource projection, metrics, and App/session/connection cleanup.
Applicable pitfalls read: No Fantasy message state or TUI rendering code was
  changed. Terminal execution is an independent App service and standard ACP,
  Bash tool, background shell, and TUI/CLI execution paths remain intact.
Files changed: new internal/terminal/{manager.go,ring.go,environment.go,
  backend.go,backend_windows.go,backend_unix.go} and domain/native platform
  tests; new internal/guiapi/terminals.go and terminals_test.go plus service,
  session-sync, snapshot, routing and integration tests; internal/sessionevent/
  event.go; internal/app/app.go and app_test.go; internal/cmd/acp.go; go.mod and
  go.sum; docs/gui-acp/01-architecture.md and 02-protocol-spec.md.
Requirements: GUI-TERM-001 and GUI-SEC-002.
Implementation: Added an App-owned Terminal Manager with independent
  connection-client and session ownership checks on every operation. Unix uses
  github.com/creack/pty with process-group signals; Windows uses
  github.com/UserExistsError/conpty with escaped CreateProcess arguments,
  ConPTY resize/input, and deterministic termination cleanup. Open/input/
  resize/kill/snapshot validate bounded commands, args, cwd, environment,
  dimensions, input and offsets. Default dimensions are 80x24; dimensions cap
  at 1000; decoded input caps at 1 MiB; terminal count caps at 256.
  Output uses an O(1) circular byte ring: 2 MiB retained by default and policy
  clamped to 4 MiB. Snapshot is offset based, reports truncation and more, and
  returns at most 2 MiB per block so base64 JSON stays below the normal frame
  limit. terminal.output events preserve starting offsets and merge only when
  contiguous. Reliable terminal.exited events contain state, code, signal,
  timestamp and final offset. A 250 ms post-exit drain grace closes PTY/ConPTY,
  waits for the reader, then publishes exit, guaranteeing final output precedes
  exit while promptly releasing live process/display handles. Completed output
  and metadata remain bounded for reconnect for ten minutes; no process object
  remains after completion.
  Open, input and kill call permission.Service with the exact
  tools.BashPermissionsParams type, toolName=bash, action=execute, same session
  and cwd, and RunInBackground=true. Direct executable/args are safely quoted
  only for policy analysis; the actual process is exec'd directly without a
  wrapper shell. Input bytes and environment values never enter permission
  metadata. Connection-local idempotency ensures exact retries replay success
  or denial without repeating permission prompts or process/input/kill side
  effects. Ownership probes collapse to CRUSH_TERMINAL_NOT_FOUND and backend
  errors are redacted.
  Connection close kills/removes only that client's terminals. Session delete
  closes turns, prevents new terminal callbacks, waits in-flight callbacks,
  kills terminals across all clients, releases Blobs, then closes the session
  event state so no late callback can recreate a journal. App shutdown closes
  terminals before the event Hub. Snapshot/session-get resource projections
  expose only bounded terminal IDs and states. Retained-byte gauge uses the
  existing fixed-label metric.
Commands and results:
  go test -race ./internal/terminal -run
    'Test(Manager|SessionCleanup|WindowsConPTY)' -count=50 -timeout=180s
    PASS, 26.287s after paginated snapshot projection. Covers lifecycle,
    dual ownership, input, resize, kill, exit, O(1) offsets/truncation,
    reconnect pages, capacity/expiry, callback cleanup barrier, and real
    Windows ConPTY process/display release.
  go test -race ./internal/guiapi -count=20 -timeout=180s
    PASS, 22.164s. Covers permission allow/deny and exact DTO type, denial and
    mutation replay, oversized pre-decode rejection, open/input/resize/kill,
    output-before-exit event order, snapshots, cross-client denial, connection
    cleanup, and bounded snapshot resource metadata.
  go test -race ./internal/terminal -count=20 -timeout=180s
    PASS, 9.802s.
  go test -race ./internal/sessionevent -count=20 -timeout=120s
    PASS, 3.646s.
  go test -race ./internal/app -run
    TestNewOwnsSessionEventHubAndCleansDeletedSession -count=10 -timeout=120s
    PASS, 18.370s. App/session deletion releases terminal handles and state.
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -o
    C:/tmp/crush-terminal-linux.test ./internal/terminal
    PASS; Unix backend and native Unix PTY test compile. Temporary binary was
    removed. Windows native ConPTY tests ran on the host as part of the race
    repetitions.
  go test ./internal/... -run '^$' -timeout=180s
    PASS; every internal package compiles with Terminal integration.
  go test -race ./internal/acp/... -count=1
    PASS, 7.495s.
  go test -race ./internal/agent -count=1
    PASS, 56.882s.
  go test -race ./internal/app -count=1
    PASS, 7.305s. A later combined verification encountered the known Windows
    TempDir cleanup failure because logs/crush.log was temporarily in use;
    immediate isolated rerun passed in 7.739s, classifying it as pre-existing
    file-handle contention rather than a Terminal regression.
  go test -race ./internal/terminal ./internal/guiapi
    ./internal/sessionevent -count=1
    PASS: 2.193s, 2.711s, and 1.889s; latest post-pagination run with App also
    passed Terminal 2.463s, GUI 2.739s, SessionEvent 1.967s, while App's isolated
    rerun passed as above.
  go test ./internal/agent/tools ./internal/cmd ./internal/config -run '^$'
    PASS; standard compatibility packages compile.
  go test ./internal/terminal -run '^$' -bench
    BenchmarkRetainedOutputRingAppend -benchmem -benchtime=10000x
    PASS: 602.4 ns/op, 54,399 MB/s, 0 B/op, 0 allocs/op on windows/amd64.
  go mod verify
    PASS: all modules verified. go.sum retains its pre-existing entries and
    adds only the two ConPTY checksums; creack/pty checksums already existed.
  go vet ./internal/terminal ./internal/guiapi ./internal/app
    PASS, no output.
  git diff --check
    PASS, no output.
Compatibility/security review: Standard ACP, TUI/CLI, Bash/background jobs,
  MCP isolation, and Fantasy state are unchanged. GUI commands do not use a
  hidden wrapper shell. Permission policy receives complete quoted command and
  args but no terminal input or environment values. Wire projections omit cwd,
  command, args and env. Every count, byte buffer, snapshot page, input,
  dimension, metadata field, idempotency cache and retention period is bounded.
  No real LLM, VCR cassette, provider credential, stdout log, or SQLite live
  streaming was introduced.
Remaining work: WP-13 completes Phase 4 with revision-aware client FS using
  Blob handles for large/binary data.
Subsequent status: WP-13 is now complete; Phase 4 is complete.
Blockers: None.
```

### WP-12

```text
Package: WP-12
Started/completed: 2026-07-13
Baseline inspected: GUI feature routing and per-connection lifecycle, App
  session deletion callbacks, turn retained-input/idempotency behavior,
  message.Attachment conversion, resource cleanup order, bounded metrics, and
  GUI-BLOB-001/GUI-SEC-005 protocol/security requirements.
Applicable pitfalls read: No Fantasy message accumulation or provider protocol
  state was changed. Blob attachments are converted to the existing
  message.Attachment shape only at actual turn dispatch/steer, before entering
  Coordinator.Run; standard ACP attachment extraction remains unchanged.
Files changed: new internal/blob/service.go and service_test.go;
  internal/guiapi/blobs.go and blobs_test.go plus service.go, sessions.go,
  turns.go, and turns_test.go integration; internal/turn/service.go and tests;
  internal/app/app.go and app_test.go; internal/cmd/acp.go;
  docs/gui-acp/01-architecture.md and 02-protocol-spec.md.
Requirements: GUI-BLOB-001 and GUI-SEC-005.
Implementation: Added an App-owned, mutex-protected in-memory Blob registry
  with a 10-minute default TTL, 64 MiB object limit, 256 MiB process-retained
  limit, 1,024-object limit, and 1 MiB ranged-read limit. Create verifies
  independently base64-decoded chunks or content against declared size and
  SHA-256 before granting a UUID handle; allocation is rejected before decode
  when declared size exceeds configured policy. Metadata strings are bounded,
  valid UTF-8, NUL-free, and filenames cannot contain path separators.
  Every create/read/release/resolve verifies an unguessable GUI-connection
  owner ID plus session ID and current expiry. Cross-client and cross-session
  probes intentionally collapse to CRUSH_BLOB_NOT_FOUND. Create and release
  use a connection-local bounded 10-minute idempotency store, so request IDs
  cannot replay another client's result. Ranged reads return bounded base64
  content with offset/nextOffset/eof. Blob retained bytes emit the existing
  fixed-label gauge on operations.
  Connection close releases all objects for that client without closing the
  shared registry. Session deletion releases the session across every client,
  including deletion initiated by GUI, standard ACP, TUI, or CLI through the
  App callback. App shutdown closes the registry. All cleanup paths are
  idempotent and clear retained byte slices.
  Turn content now accepts {type:blob,blobId}. Queued/retried turns retain only
  a lightweight resolver handle, so a >1 MiB attachment does not duplicate
  bytes inside the 1 MiB retained-input budget. Bytes are ownership/expiry
  checked and copied only at dispatch or steer. Resolution failure publishes a
  reliable attachment_unavailable terminal event and advances the queue; a
  publish failure uses local finish+advance fallback. Inline image/audio/
  resource content over the mandatory 4 MiB threshold is rejected before
  base64 decode.
Commands and results:
  go test -race ./internal/blob -count=50
    PASS, 2.565s (latest equivalent repeat also passed in 2.283s). Covers hash,
    ranges, expiry, release, dual ownership, size/count/byte capacity, cleanup,
    and concurrent read/release.
  go test -race ./internal/guiapi -run
    'Test(BlobHandlersHashRangesOwnershipIdempotencyAndCleanup|BlobValidationExpiryAndConnectionClose|TurnStartResolvesOwnedBlobAttachment|BlobConnectionAndSessionDeletionCleanup)'
    -count=20 -timeout=180s
    PASS, 8.640s. Covers chunk upload, exact replay/conflict, read offsets,
    cross-client/session denial, metrics, expiry, pre-decode policy rejection,
    >1 MiB deferred turn attachment, connection cleanup, and cross-connection
    session deletion cleanup.
  go test -race ./internal/turn -run
    TestAttachmentResolutionFailureAdvancesQueue -count=50
    PASS, 1.633s.
  go test -race ./internal/app -run
    TestNewOwnsSessionEventHubAndCleansDeletedSession -count=10 -timeout=120s
    PASS, 33.720s. App ownership and standard session deletion clear Blobs.
  go test -race ./internal/blob ./internal/guiapi ./internal/turn -count=1
    PASS: 2.261s, 3.280s, and 1.644s.
  go test -race ./internal/acp/... -count=1
    PASS, 12.805s.
  go test -race ./internal/agent -count=1
    PASS, 64.987s.
  go test -race ./internal/app -count=1
    PASS, 8.642s.
  go test ./internal/agent/tools ./internal/cmd ./internal/config -run '^$'
    PASS; selected compatibility packages compile.
  go test ./internal/... -run '^$' -timeout=180s
    PASS on rerun; all internal packages compile. The first concurrent compile
    attempt reported an isolated App watchdog kill while other packages were
    heavily compiling. It did not reproduce: go test ./internal/app -run '^$'
    -count=3 -timeout=120s passed in 1.639s, the full internal rerun passed,
    and the App race suite passed.
  git diff --check
    PASS, no output.
Compatibility/security review: Standard ACP schemas and handlers are
  unchanged. TUI/CLI and Fantasy message flow receive the same resolved
  message.Attachment values as before. Blob IDs do not confer authority;
  client+session+expiry are revalidated on every operation. Errors do not echo
  content, hash, URI, filename, ownership details, or storage internals. All
  object, byte, chunk, metadata, read, TTL, replay, and turn-input resources are
  bounded. No real LLM, provider credential, VCR fixture, local path read, or
  stdout log was introduced.
Remaining work: WP-13 consumes Blob handles for large/binary client-FS data.
WP-11 supplies terminal resources before Phase 4 can complete.
Subsequent status: WP-13 and WP-11 are complete; Phase 4 is complete.
Blockers: None.
```

### WP-10

```text
Package: WP-10
Started/completed: 2026-07-13
Baseline inspected: global selected-model slots, Agent profile and Plan-mode
  resolution, runtime model/call-option refresh, enhanced prompt cache,
  session persistence, GUI turn dispatch/retry, snapshot projection, and
  parent/child session context boundaries.
Applicable pitfalls read: Fantasy message accumulation was not changed. Runtime
  inference is projected into a per-run config and context scope; it does not
  mutate ConfigStore model maps, recent-model state, provider credentials,
  prompts, endpoints, or context-window configuration.
Files changed: additive migration
  internal/db/migrations/20260713020000_add_session_inference_config.sql;
  internal/db/sql/sessions.sql and generated internal/db files;
  internal/session/session.go and desktop_test.go;
  internal/agent/inference_config.go, inference_config_test.go, agent.go,
  agent_config.go, and coordinator.go; internal/turn/service.go and tests;
  internal/guiapi/sessions.go, turns.go, snapshot.go and tests;
  internal/app/app.go; internal/cmd/acp.go;
  docs/gui-acp/02-protocol-spec.md.
Requirements: GUI-SESS-003.
Implementation: Added persisted session inference overrides with a monotonic
  revision and SQL compare-and-swap update. The supported bounded field set is
  model/provider, maxOutputTokens, temperature, topP, topK,
  frequencyPenalty, presencePenalty, and think. Validation requires paired
  model/provider selection and fixed numeric ranges; arbitrary provider
  options, credentials, endpoints, prompts, and context-window mutation are
  excluded. Effective resolution is side-effect free with precedence global
  selected slot < Agent profile < Plan mode < session < turn. Coordinator Run
  freezes the merged session/turn scope after loading the session, and each
  SessionAgent run retains its own runtime Model and initial call options even
  if another session refreshes the shared Agent. Child task sessions do not
  inherit a parent's frozen override; they resolve their own profile and child
  session state. Enhanced prompt cache identity includes base prompt, model,
  image support, tools/date, and MCP scope/revision under one mutex-protected
  logical cache entry. GUI config get/update expose revision, overrides, and
  full effective projection; update is idempotent and CAS protected. Turn start
  validates and deep-copies turn overrides, and retry retains the captured
  inference. Clearing with an empty object restores lower precedence without
  changing workspace/global defaults.
Commands and results:
  go test -race ./internal/agent -run
    'Test(EffectiveInferencePrecedenceAndGlobalImmutability|TurnInferenceIsFrozenAndDoesNotLeakToSubagents|RuntimeModelIsPerRunDespiteSharedAgentModelChanges)'
    -count=50 -timeout=180s
    PASS, 74.211s.
  go test -race ./internal/session -run
    TestInferenceOverridesPersistAndUseCompareAndSwapRevision -count=50
    PASS, 6.185s.
  go test -race ./internal/guiapi -run
    'Test(SessionInferenceHandlersResolvePrecedenceAndRejectStaleRevision|TurnStartCarriesValidatedInferenceOverrides)'
    -count=50
    PASS, 5.324s.
  go test -race ./internal/agent -run <WP-10 focused tests> -count=10;
    go test -race ./internal/session -run <WP-10 CAS test> -count=10;
    go test -race ./internal/guiapi -run <WP-10 handler/snapshot tests>
    -count=10; go test -race ./internal/turn -run TestRetryAndSteer -count=10
    PASS: agent 15.141s, session 2.542s, guiapi 2.356s, turn 1.417s.
    Covers override clearing, structured missing-session errors, full snapshot
    fields/revision, and turn/message retry inference retention.
  go test ./internal/... -run '^$'
    PASS; every internal package compiles.
  go test -race ./internal/acp/... -count=1
    PASS, 8.115s.
  go test -race ./internal/agent -count=1
    PASS, 59.920s.
  go test -race ./internal/app -count=1
    PASS, 8.410s.
  go test -race ./internal/session ./internal/guiapi ./internal/sessionevent
    ./internal/turn ./internal/idempotency -count=1
    PASS: 2.143s, 2.668s, 2.095s, 1.481s, and 1.636s.
  git diff --check
    PASS, no output.
Compatibility/security review: Standard ACP, TUI/CLI defaults, MCP access
  scoping, and Fantasy dual-message-state behavior remain unchanged. Overrides
  are session data, not provider/global state. Active runs are immutable,
  pointer fields are deep-copied, child sessions fail closed against parent
  leakage, and invalid or stale mutations return structured errors. No real
  LLM, provider credential, or VCR fixture is used.
Remaining work: None for WP-10. WP-12 is the next recommended dependency-ready
  package; WP-11 is also dependency-ready.
Blockers: None.
```

### WP-08

```text
Package: WP-08
Started/completed: 2026-07-13
Baseline inspected: session schema/service narrow updates, transactional delete
  callback, existing TUI fork behavior, App runtime cleanup, bounded snapshot
  projection, guiapi route/idempotency wiring, and GUI-SESS-001 contracts.
Applicable pitfalls read: No Fantasy message-state code was changed. Existing
  TUI/CLI session.Service and fork entry points remain shared with the GUI;
  desktop-only archive/pin methods use a separate MutationService extension so
  unrelated service mocks and consumers retain the stable interface.
Files changed: additive migration
  internal/db/migrations/20260713010000_add_session_desktop_flags.sql;
  internal/db/sql/sessions.sql and generated internal/db files;
  internal/session/session.go and desktop_test.go;
  internal/sessionevent/{snapshot.go,hub.go};
  internal/guiapi/{sessions.go,sessions_test.go,service.go,turns.go} plus router
  compatibility tests; internal/app/app.go; internal/agent/coordinator.go;
  internal/cmd/acp.go; docs/gui-acp/02-protocol-spec.md.
Requirements: CRUD/fork/get portion of GUI-SESS-001 and destructive mutation
  idempotency/teardown requirements inherited from GUI-EVENT-003.
Implementation: Added persisted archived/pinned booleans with reversible schema
  migration and narrow RETURNING updates. Rename, archive, and pin return a
  bounded public metadata projection and publish atomic monotonic-revision
  session.updated events. Session get returns metadata, runtime status, active
  turn, queue summary, effective config, latest sequence, and revision while
  omitting history/resource arrays. Titles are valid UTF-8, trimmed, non-empty,
  and capped at 256 bytes. Delete authorizes inside idempotent execution, closes
  GUI turn state, cancels a standard Agent run through App, then uses the shared
  transactional session deletion/callback path. Exact duplicate delete requests
  replay after the row is gone; new request IDs fail as not found. The App to
  Coordinator deletion hook is now exported across the package boundary, so
  transcript/background-agent/memory cleanup actually runs.
  Fork supports an optional boundary. It locates the next user turn with the
  stable (created_at,rowid) ordering and copies all selected rows in one
  transaction using INSERT SELECT with fresh UUIDs. This replaces the previous
  full-history Go slice plus per-message insert loop. Empty messageId copies the
  complete persisted history; a selected message copies through its completed
  assistant turn. Forks inherit workspace/collaboration/permission/plan state
  but start with fresh goal/archive/pin/runtime state and return a bounded
  snapshot. Session mutations continue after request-context disconnection and
  share WP-09's bounded 10-minute exact-outcome store.
Commands and results:
  sqlc generate
    PASS; session models/queries include archived/pinned fields and narrow
    RETURNING updates.
  go test -race ./internal/guiapi ./internal/session ./internal/sessionevent
    -count=20 -timeout=120s
    PASS: guiapi 14.828s, session 6.322s, sessionevent 4.307s. Covers exact
    duplicate/conflict results, disconnected mutation completion, metadata and
    revision projection, active-runtime teardown before delete, replay after
    deletion, new-ID not-found behavior, completed-turn/full-history fork
    boundaries, fresh message UUIDs, persisted flags, and router concurrency.
  go test -race ./internal/acp/... -count=1
    PASS, 7.795s.
  go test -race ./internal/agent -count=1
    PASS, 57.959s.
  go test -race ./internal/app -count=1
    PASS, 8.457s; repeated after the constant-memory fork change and passed in
    8.577s.
  go test -race ./internal/session ./internal/guiapi ./internal/sessionevent
    ./internal/idempotency ./internal/turn -count=1
    PASS: 2.587s, 2.634s, 2.324s, 1.999s, and 1.428s.
  go test ./internal/session -run '^$' -bench '^BenchmarkSessionFork10K$'
    -benchmem -benchtime=1x
    PASS: 167,542,700 ns/op, 329,152 B/op, 7,276 allocs/op for a real
    10,000-message SQLite fork. Copying is one INSERT SELECT and allocation does
    not scale as a retained 10,000-message Go projection.
  go test ./internal/... -run '^$'
    PASS; every internal package, including TUI and ACP fakes, compiles.
  go test ./internal/agent/tools ./internal/cmd ./internal/config
    ./internal/ui/model -run '^$'
    PASS.
  git diff --check
    PASS, no output.
Compatibility/security review: Standard ACP still bypasses guiapi; TUI/CLI use
  the same Rename/Delete/Fork methods and their existing semantics remain
  additive. No prompt, attachment bytes, goal text, todo content, tool internals,
  provider state, or database error text enters CRUD projections. Mutation
  authorization runs inside the first idempotent execution. Fork boundaries are
  source-scoped and invalid boundaries return a redacted symbolic error. The
  migration defaults both flags false and is reversible.
Additional stress classification: `go test -race ./internal/app -count=10`
  triggered the existing TestSetupSubscriber_NoTimerLeak detector on long-lived
  net/http HTTP/2 and lumberjack millRun goroutines. The stacks contain no WP-08
  session/gui code, while two independent full App race runs with count=1 pass.
  This is recorded as broader-test noise rather than hidden as package evidence.
Remaining work: WP-10 completes Phase 3 with session inference overrides.
Blockers: None.
```

### WP-09

```text
Package: WP-09
Started/completed: 2026-07-13
Baseline inspected: Agent request queue/cancellation/steer paths, canonical
  session events, Coordinator run ownership, message persistence/query APIs,
  guiapi negotiated routing, App lifecycle, and turn protocol requirements.
Applicable pitfalls read: docs/pitfalls/fantasy-dual-message-state.md. Turn IDs
  are propagated as execution context metadata; no Fantasy accumulated message
  slice or provider message flow was mutated. internal/ui/AGENTS.md was read
  before updating the Coordinator test mock required by the interface change.
Files changed: internal/agent/{agent.go,agent_queue.go,queue.go,coordinator.go,
  turn_context.go} and focused tests; internal/sessionevent/event.go;
  internal/idempotency/store.go and tests; internal/turn/{service.go,runner.go,
  retry.go} and tests; internal/guiapi/turns.go, turns_test.go, service wiring;
  internal/db/sql/messages.sql and generated internal/db files;
  internal/message/message.go and pagination_test.go; internal/app/app.go and
  tests; internal/cmd/acp.go; internal/ui/model/queue_pause_test.go;
  docs/gui-acp/02-protocol-spec.md.
Requirements: GUI-TURN-001, GUI-EVENT-003, and GUI-PERF-001.
Implementation: Added an App-owned turn service that serializes GUI turns per
  session and carries stable UUID turn identity through Coordinator and Agent
  queues. Start acknowledges before execution; wait cancellation affects only
  the waiter; cancel publishes a reliable acknowledgement before the terminal
  cancellation milestone. Queue list/remove/reorder use monotonic revisions,
  bounded previews, stable IDs, and partial-order semantics. Steer joins the
  active run when supported or creates an explicit queued turn. Retry accepts a
  failed/cancelled retained turn or an assistant message; message retry uses an
  indexed nearest-preceding-user query and reconstructs attachments.
  Mutation results and structured errors are replayed exactly for 10 minutes by
  method/session/clientRequestId and conflicting payload reuse fails closed.
  The idempotency store and turn retention are each bounded at 4,096 entries;
  each session allows 128 active-plus-pending turns and each retained input is
  capped at 1 MiB. Capacity failures clean newly created empty runtimes. App
  shutdown/session deletion close turn resources before event ownership ends.
Commands and results:
  go test -race ./internal/idempotency ./internal/turn ./internal/guiapi
    -count=20
    PASS: idempotency 1.795s, turn 1.525s, guiapi 12.981s after cancellation
    ordering and asynchronous-dispatch synchronization fixes. A later bounded
    resource regression run also passed: turn 1.499s, idempotency 1.561s,
    guiapi 13.127s.
  go test -race ./internal/agent -run
    'Test(SessionAgentPublishesCanonicalLiveEventOrder|RemoveQueuedTurnAndEnqueueSteerUseStableTurnIdentity)'
    -count=20
    PASS, 3.710s.
  go test -race ./internal/message -run
    TestGetRetrySourceReturnsNearestPrecedingUserMessage -count=20
    PASS, 2.645s; a later assistant-only retry regression passed in the
    message/turn/guiapi race suite for 10 repetitions.
  go test -race ./internal/acp/... -count=1
    PASS, 7.707s.
  go test -race ./internal/agent -count=1
    PASS, 57.128s.
  go test -race ./internal/app -count=1
    PASS, 8.172s.
  go test -race ./internal/message ./internal/sessionevent ./internal/turn
    ./internal/idempotency ./internal/guiapi -count=1
    PASS: 2.019s, 2.007s, 1.431s, 1.635s, and 2.259s.
  go test ./internal/guiapi -run '^$' -bench
    '^BenchmarkTurnStartAcknowledgement$' -benchmem -benchtime=1000x
    PASS: 30,883 ns/op average, 55.50 p95-us, 3,280 B/op, 39 allocs/op.
    The measured p95 is about 360 times below the 20 ms requirement.
  go test ./internal/... -run '^$'
    PASS after adding the new Coordinator methods to the TUI test mock; every
    internal package and interface fake compiles.
  git diff --check
    PASS, no output.
Compatibility/security review: Standard ACP handlers still bypass guiapi and
  existing TUI/CLI queue behavior is unchanged. GUI turns rely on the existing
  Agent one-active-request invariant plus their own per-session dispatcher, so
  unrelated standard ACP/TUI runs are not cancelled as queued GUI work. All
  mutation scopes verify session ownership, UUID payload hashes disclose no
  content, errors are structured/redacted, and no real provider or VCR fixture
  is used. Every queue, input, retained turn, idempotency entry, wait duration,
  and preview is bounded.
Remaining work: WP-08 can now use the idempotency service for session CRUD and
  fork. WP-10 depends on this turn runtime for immutable running-turn session
  overrides.
Blockers: None.
```

### WP-07

```text
Package: WP-07
Started/completed: 2026-07-13
Baseline inspected: offset/recent-tail message queries, composite index and
  equal-timestamp compatibility ordering, message/history services, guiapi
  route surface, content-part security boundaries, and pagination specs.
Applicable pitfalls read: No Fantasy message-state mutation or TUI code was
  changed. Existing List/ListPage/history equal-timestamp ordering remains
  separate from GUI keyset ordering. A focused history regression caught the
  first attempt to reuse one query for both contracts; dedicated keyset search
  now preserves the legacy SearchMessages behavior.
Files changed: internal/db/sql/messages.sql and generated
  internal/db/{messages.sql.go,db.go,querier.go}; refined migration
  20260713000000_add_recent_message_index.sql; internal/message/message.go and
  pagination_test.go; internal/history/file.go and search.go; message/history
  test fakes; internal/guiapi/messages.go, messages_test.go, service.go;
  internal/cmd/acp.go; docs/gui-acp/02-protocol-spec.md.
Requirements: Pagination and search portion of GUI-SESS-001.
Implementation: Added reverse-chronological `(createdAt, messageId)` keyset
  pagination with an indexed `(session_id, created_at DESC, id DESC)` query.
  Opaque base64url JSON cursors are versioned and bound to method/session; search
  cursors are additionally bound to a normalized query/filter hash. Default
  message pages contain 50 entries and clamp at 200; search defaults to 20 and
  clamps at 100. Both fetch one extra row for exact hasMore/nextCursor results.
  Newer insertions cannot shift an established boundary, and deleting the
  boundary row does not invalidate the keyset. Equal timestamps use message ID
  deterministically. GUI pages cap text at 64 KiB per message and 1 MiB per
  page; search previews cap at 512 bytes. Wire projections omit attachment
  bytes/paths/URLs, reasoning content/signatures, tool input, tool result
  content/metadata, and provider-internal state. Missing sessions and storage
  failures return structured, redacted errors. Standard history search retains
  its prior rowid tie behavior through a separate query.
Commands and results:
  go test ./internal/guiapi ./internal/message ./internal/history -count=1
    PASS: guiapi 3.398s, message 2.812s, history 2.109s. Covers default/max
    limits, equal timestamps, insertion/deletion stability, malformed and
    cross-session/query cursors, payload budget/UTF-8 behavior, sensitive-field
    redaction, missing sessions, source failure redaction, search continuation,
    and composite-index query-plan assertions.
  go test -race ./internal/guiapi -count=10
    PASS, 6.569s.
  go test -race ./internal/message ./internal/history -count=20
    PASS, 2.890s and 3.011s.
  go test ./internal/message -run '^$' -bench
    '^BenchmarkSessionMessagePage$' -benchmem -benchtime=50x
    PASS with a real 10,000-message SQLite fixture and a 201-row fetch:
    623,942 ns/op, 315,780 B/op, 5,474 allocs/op.
  go test -race ./internal/acp/... -count=1
    PASS, 7.557s.
  go test -race ./internal/agent -count=1
    PASS, 53.691s.
  go test -race ./internal/app -count=1
    PASS, 8.098s.
  go test ./internal/... -run '^$'
    PASS; every internal package and test fake compiles, including TUI paths.
  git diff --check
    PASS, no output.
Compatibility/security review: Standard ACP session/load and the TUI offset
  ListPage API are unchanged. Legacy message/history ordering has explicit
  rowid ties while private GUI methods use a dedicated ID-keyset contract.
  Cursors disclose no prompt data or secrets and fail closed across scopes.
  Query/storage errors are redacted as retryable CRUSH_QUERY_FAILED. Every page,
  preview, text budget, and result count is bounded, and no real provider or VCR
  fixture is used. The index migration remains additive and reversible.
Remaining work: WP-08 supplies session CRUD/fork after WP-09 idempotency. WP-09
  is the next recommended package and supplies turn/queue semantics plus the
  mutation idempotency required by WP-08.
Blockers: None.
```

### WP-06

```text
Package: WP-06
Started/completed: 2026-07-13
Baseline inspected: session/message service contracts and SQL queries,
  Coordinator busy/queue/model projection, event Hub sequence/revision,
  guiapi snapshot/sync routes, and the synthetic snapshot benchmark.
Applicable pitfalls read: No Fantasy message-state mutation or TUI code was
  changed. The broader Agent gate caught and drove correction of a message
  tie-order regression introduced by the first index shape.
Files changed: internal/sessionevent/snapshot.go, snapshot_test.go, hub.go;
  internal/guiapi/snapshot.go, snapshot_test.go, service.go, session_sync.go;
  internal/message/message.go; internal/db/sql/messages.sql, generated
  internal/db/{messages.sql.go,db.go,querier.go}, and migration
  20260713000000_add_recent_message_index.sql; message-service test fakes;
  internal/cmd/acp.go; docs/gui-acp/02-protocol-spec.md.
Requirements: GUI-SESS-002 and GUI-PERF-003.
Implementation: Added a bounded domain snapshot with session metadata, runtime
  status/active-turn slot, queue state, effective model/provider, independently
  capped MCP/terminal resource slots, at most 20 message summaries, latest
  sequence, and monotonic session revision. Message previews are valid UTF-8
  capped at 512 bytes; attachments are counts only and binary bytes/MIME data
  are never inlined. Production construction performs exactly one session
  metadata query plus one indexed message-tail LIMIT 20 query, independent of
  history length. ListRecent returns the selected tail chronologically. The
  new composite index preserves legacy equal-timestamp insertion order for
  existing List/ListPage queries by explicitly ordering on rowid; this fixed
  four Agent regressions exposed by the first full gate. GUI snapshot requests
  map missing sessions and redacted retryable source failures structurally.
  Expired crush/session/sync now returns mode=snapshot.
Commands and results:
  go test ./internal/sessionevent ./internal/guiapi ./internal/message -count=1
    PASS: sessionevent 2.155s, guiapi 3.433s, message 2.568s.
  go test -race ./internal/sessionevent ./internal/guiapi ./internal/message
    -count=20
    PASS: 4.032s, 1.939s, and 3.234s.
  go test ./internal/sessionevent -run '^$' -bench
    '^BenchmarkSessionSnapshot$' -benchmem -benchtime=50x
    PASS on Windows/amd64 with a real 10,000-message SQLite session:
    150,912 ns/op, 33,127 B/op, 662 allocs/op. This is about 0.151 ms/op,
    roughly 993x below the 150 ms first-screen target. The rejected OFFSET
    baseline was 25,495,900 ns/op, 18,438,215 B/op, 269,260 allocs/op.
  go test ./internal/agent -run
    'TestRunNormalSummarizeUsesSummarizePurpose|TestRunSubAgent/does_not_fall_back_to_earlier_assistant_text_when_latest_assistant_is_empty|TestLoadEnhancePromptHistorySkipsToolsAndHonorsSummaryBoundary|TestLatestUserRequestForHandoff'
    -count=1
    PASS, 2.118s after preserving equal-timestamp message ordering.
  go test -race ./internal/acp/... -count=1
    PASS, 9.459s.
  go test -race ./internal/agent -count=1
    PASS, 53.336s.
  go test -race ./internal/app -count=1
    PASS, 7.795s.
  go test ./internal/agent/tools ./internal/cmd ./internal/config
    ./internal/db -run '^$'
    PASS; selected packages compile.
  go test -race ./internal/guiapi -count=20
    PASS, 1.824s after redacted structured snapshot-error coverage.
  git diff --check
    PASS, no output.
Compatibility/security review: Standard ACP session/load remains unchanged and
  retains its protocol-required replay. Existing message list/page ordering is
  explicit and the full Agent suite verifies compatibility. Snapshot history,
  queries, previews, resource arrays, and output are bounded; no binary content,
  prompt-sized queue text, database error text, secret, or real-provider data
  reaches the wire. Snapshot construction does no provider calls and no full
  history scan. The migration is additive and reversible.
Remaining work: WP-07 adds stable cursor pagination. WP-09, WP-11, and WP-15
  enrich queue, terminal, and MCP snapshot slots through bounded sources.
Blockers: None.
```

### WP-05

```text
Package: WP-05
Started/completed: 2026-07-13
Baseline inspected: sessionevent replay/subscription/overflow behavior, guiapi
  feature router, ACP synchronous writer and dispatch response ordering.
Applicable pitfalls read: No Fantasy message-state or TUI code was changed.
Files changed: internal/sessionevent/hub.go; internal/acp/server.go;
  internal/guiapi/service.go, session_sync.go, session_sync_test.go, and
  server_integration_test.go; internal/cmd/acp.go;
  docs/gui-acp/02-protocol-spec.md.
Requirements: GUI-EVENT-001/002 and the replay/retry portion of GUI-EVENT-003.
Implementation: Added connection-scoped subscribe/unsubscribe/sync handlers
  backed by the bounded sessionevent journal. Subscribe returns a lifecycle
  result whose writer starts only after the JSON-RPC response is synchronously
  written. Replay and live events use crush/session/event over NotifySync, not
  the best-effort ACP notification queue. Wire envelopes and typed payloads use
  camelCase and omit internal delivery/coalescing metadata. Reconnect resumes
  after a known sequence; expired ranges return CRUSH_SEQUENCE_EXPIRED with
  retryable and snapshotRequired details until WP-06 can return snapshot mode.
  Slow writers remain outside Hub publication; overflow drains every accepted
  reliable event followed by snapshot.required. Response failure, unsubscribe,
  connection close, and repeated cleanup all detach subscriptions safely.
Commands and results:
  go test ./internal/guiapi ./internal/acp ./internal/sessionevent
    ./internal/cmd -run '^$'
    PASS; all selected packages compile.
  go test ./internal/guiapi -count=1
    PASS, 2.044s. Covers ordered sync wire schema, reconnect, expired sequence,
    lifecycle ordering, idempotent unsubscribe/close, response-write failure,
    blocked writer overflow, reliable-event preservation, and concurrent
    publish/unsubscribe/close.
  go test -race ./internal/guiapi -count=20
    PASS, 1.768s.
  go test -race ./internal/sessionevent -count=20
    PASS, 1.883s.
  go test -race ./internal/guiapi ./internal/sessionevent -count=1
    PASS, 1.392s and 1.488s.
  go test -race ./internal/acp/... -count=1
    PASS, 7.422s.
  go test -race ./internal/agent -count=1
    PASS, 58.290s.
  go test -race ./internal/app -count=1
    PASS, 8.176s.
  go test ./internal/agent/tools ./internal/cmd ./internal/config -run '^$'
    PASS; selected packages compile.
  git diff --check
    PASS, no output.
Compatibility/security review: Standard ACP methods still bypass guiapi and
  session/load compatibility replay is unchanged. Reliable GUI events use the
  synchronous writer only after feature negotiation. Every journal and
  subscription remains bounded; no Agent callback waits for client IO; no full
  history, binary expansion, secret, stdout log, or real-provider fixture was
  introduced. Service close cancels blocked writes and bounds cleanup waiting.
Remaining work: WP-06 constructs bounded snapshots and upgrades expired sync
  from structured snapshot-required errors to mode=snapshot results. WP-09
  supplies mutation idempotency for the remaining GUI-EVENT-003 contract.
Blockers: None.
```

### WP-01

```text
Package: WP-01
Started/completed: 2026-07-13
Baseline inspected: internal/acp/server.go, internal/acp/handler.go,
  internal/agent/stream_message_flusher.go, internal/event/event.go, existing
  repository benchmarks and go.mod dependencies.
Applicable pitfalls read: No message-state mutation was made; Fantasy message
  flow remained out of scope.
Files changed: internal/guimetrics/metrics.go and tests;
  internal/acp/handler.go, server.go, metrics_test.go,
  performance_bench_test.go; internal/agent/stream_message_flusher.go and test.
Requirements: GUI-OBS-001 foundation and executable GUI-PERF baselines.
Implementation: Added an exporter-neutral, context-injected Recorder with the
  complete required metric-name catalog and a closed four-field label shape.
  Unknown ACP method labels normalize to "other". Added request/session-load,
  write-queue latency/depth, and SQLite-flush duration hooks. Added all six
  prescribed deterministic benchmarks with allocation reporting.
Commands and results:
  go test -race ./internal/guimetrics ./internal/acp/... ./internal/agent
    -run <WP-01 focused tests> -count=1
    PASS (guimetrics, acp, and agent focused runs; agent repeated separately).
  go test ./internal/acp -run '^$' -bench
    'Benchmark(ACPTextDelta|SessionSnapshot|SessionMessagePage|TerminalOutputCoalescing|LongSessionLoad|ConcurrentSessions)$'
    -benchmem -benchtime=100x
    PASS. Windows/amd64 baseline: ACP delta 1,911 ns/op, 805 B/op,
    4 allocs/op; snapshot 3,955 ns/op, 3,243 B/op, 2 allocs/op;
    200-message page 42,569 ns/op, 28,197 B/op, 2 allocs/op;
    terminal coalescing 85 ns/op, 0 allocs/op; 10,000-message full projection
    1,591,207 ns/op, 1,440,739 B/op, 3 allocs/op; concurrent sessions
    96 ns/op, 87 B/op, 0 allocs/op.
  go test -race ./internal/acp/... -count=1
    PASS, 8.380s.
  go test -race ./internal/agent -count=1
    PASS, 61.204s.
  go test ./internal/agent/tools ./internal/cmd ./internal/config -run '^$'
    PASS; no tests selected, packages compile.
  git diff --check
    PASS, no output.
Compatibility/security review: No JSON-RPC schemas, notification drop policy,
  or 150 ms persistence interval changed. Recorder defaults to no-op. Label
  keys are closed and tests cover arbitrary-method normalization; no session ID,
  path, prompt, or raw error is recorded.
Remaining work: Later packages must attach event, snapshot, terminal, blob,
  subscription, prompt, and sequence metrics and replace representative
  snapshot/page benchmarks with service-level implementations while retaining
  these baseline names.
Blockers: None.
```

### WP-04

```text
Package: WP-04
Started/completed: 2026-07-13
Baseline inspected: ACP initialize DTOs/handler, JSON-RPC dispatch, cmd/acp
  wiring, App-owned sessionevent Hub, and existing ACP in-memory IO tests.
Applicable pitfalls read: No Fantasy message or TUI code was changed.
Files changed: new internal/guiapi/service.go and unit/integration tests;
  internal/acp/types.go, handler.go, server.go; internal/cmd/acp.go;
  docs/gui-acp/README.md and 02-protocol-spec.md.
Requirements: GUI-COMPAT-001/002.
Implementation: Added a connection-scoped GUI service with protocol version 1
  and feature groups sessionSync, sessionControl, terminal, blob, clientFS,
  providerAuth, and mcpControl. Standard initialize now has generic optional
  experimental request/result fields. The GUI service advertises capabilities,
  validates a client-selected subset, rejects unsupported versions/features,
  and clears old authority before every renegotiation. JSON-RPC errors now allow
  optional structured data with symbolic code, retryable, and details fields.
  ACP Server dispatches only the crush/* namespace to an independent extension
  router; standard methods continue through the original Handler. The router
  predeclares the complete planned method surface, enforces feature selection
  before handler invocation, and provides a registration boundary for later
  work packages. crush/protocol/status reports the current selection. cmd/acp
  creates one GUI service per client connection and shares only the App-owned
  event Hub.
Commands and results:
  go test -race ./internal/guiapi -count=20
    PASS, 1.425s. Covers capability catalog, subset selection, unsupported
    version/feature, fail-closed renegotiation, feature gating, registration,
    negotiation reset, concurrent negotiation/routing, standard ACP initialize,
    namespace dispatch, and structured unnegotiated errors.
  go test ./internal/guiapi -run '^$' -bench BenchmarkNegotiatedRoute
    -benchmem -benchtime=10000x
    PASS: 50.41 ns/op, 24 B/op, 1 alloc/op.
  go test -race ./internal/guiapi ./internal/acp/... -count=1
    PASS: guiapi 1.404s; ACP 7.640s.
  go test -race ./internal/agent -count=1
    PASS, 54.220s.
  go test -race ./internal/app -count=1
    PASS, 7.928s.
  go test -race ./internal/sessionevent ./internal/guimetrics -count=1
    PASS, 1.751s and 1.793s.
  go test ./internal/agent/tools ./internal/cmd ./internal/config -run '^$'
    PASS; selected packages compile.
  git diff --check
    PASS, no output.
Compatibility/security review: Existing ACP tests run without an extension and
  retain the previous response shape. With GUI support installed, standard
  clients may ignore the optional experimental advertisement and still execute
  standard session/cancel. Private methods never enter the ACP Handler. Invalid
  renegotiation is fail-closed, feature checks occur before application handler
  calls, negotiation state is connection-local and race-protected, and metrics
  use bounded feature labels.
Remaining work: WP-05 registers session subscribe/sync/replay handlers; later
  packages register their feature methods through this router. WP-16 extracts
  framing/transport implementations without changing this contract.
Blockers: None.
```

WP-02 follow-up during WP-03: `Journal` was corrected from slice shifting to an
O(1) circular ring after the live-path benchmark exposed full-buffer copy cost.
The WP-02 race suite passed 20 repetitions after the correction.

### WP-03

```text
Package: WP-03
Started/completed: 2026-07-13
Baseline inspected: sessionAgent construction and canonical Fantasy callbacks,
  Coordinator agent/subagent construction, App lifecycle, message persistence
  flusher, cancellation path, and existing stub-model test patterns.
Applicable pitfalls read: docs/pitfalls/fantasy-dual-message-state.md. The bridge
  observes callbacks and does not mutate Fantasy's accumulated message slices.
Files changed: internal/agent/agent.go, agent_queue.go,
  agent_session_events.go and tests, coordinator.go; internal/app/app.go and
  app_test.go; sessionevent payload/coalescer/journal refinements and tests.
Requirements: GUI-PERF-002/005/006 and WP-03 event ordering requirements.
Implementation: App now owns one sessionevent Hub and closes session journals on
  deletion/shutdown. Coordinator injects the same Hub into the primary Agent and
  every subagent. Canonical callbacks directly publish turn start, message
  create/delta/reset/complete, reasoning delta, tool progress/result, usage,
  turn completion/failure/cancellation, and cancellation acknowledgement.
  Text/reasoning events publish before updating the in-memory draft dirty flag;
  tool and step terminal events publish before SQLite persistence. Message
  persistence retains the existing 150 ms flusher and remains independent.
  Tool input/result previews are redacted and bounded to 64 KiB. Oversized UTF-8
  text deltas are losslessly split into <=64 KiB events. Coalescing is limited to
  32 events, 33 ms from the first event, and 64 KiB, preventing rolling-window
  string-copy amplification.
Commands and results:
  go test -race ./internal/agent -run
    'Test(SessionAgentPublishes|SessionAgentTextDelta|SessionAgentCancellation|SessionAgentWithoutEventHub|SplitLiveText|BoundedLiveToolText)'
    -count=5
    PASS, 2.219s. Covers canonical event order, persistence failure isolation,
    zero synchronous persistence writes per text delta, cancel ack ordering,
    nil-Hub compatibility, and UTF-8 payload bounds.
  go test -race ./internal/app -run
    TestNewOwnsSessionEventHubAndCleansDeletedSession -count=3
    PASS, 7.163s.
  go test -race ./internal/sessionevent -count=20
    PASS, 1.786s after circular-ring and coalescer-bound changes.
  go test -race ./internal/sessionevent ./internal/guimetrics -count=1
    PASS, 1.750s and 1.748s.
  go test -race ./internal/acp/... -count=1
    PASS, 7.405s.
  go test -race ./internal/agent -count=1
    PASS, 53.299s.
  go test -race ./internal/app -count=1
    PASS, 7.922s.
  go test ./internal/agent/tools ./internal/cmd ./internal/config -run '^$'
    PASS; selected packages compile.
  go test ./internal/agent -run '^$' -bench
    'BenchmarkLiveTextDeltaPublish' -benchmem -benchtime=10000x
    PASS. Journal-only: 296.5 ns/op, 203 B/op, 5 allocs/op. With a blocked
    subscriber/coalescer: 709.2 ns/op, 607 B/op, 6 allocs/op. Both are far
    below the 50 ms callback-to-write-ready budget; transport time is measured
    separately by WP-01/WP-04.
  git diff --check
    PASS, no output.
Compatibility/security review: Standard ACP handlers and schemas are unchanged;
  nil SessionEvents preserves isolated Agent behavior. No live callback mutates
  Fantasy history. No raw provider error enters events. Tool fields are redacted
  and bounded. Hub publication never waits for SQLite or a subscriber consumer.
Remaining work: WP-04 must negotiate and route the GUI protocol to the Hub;
  WP-05 supplies reconnect/sync handlers. Blob handles replace bounded tool
  previews in WP-12.
Blockers: None.
```

### WP-02

```text
Package: WP-02
Started/completed: 2026-07-13
Baseline inspected: docs/gui-acp event/backpressure specifications,
  internal/pubsub, internal/csync, internal/guimetrics, and current worktree.
Applicable pitfalls read: No Fantasy message-state or TUI code was changed.
Files changed: internal/sessionevent/event.go, journal.go, coalescer.go, hub.go
  and their tests; docs/gui-acp/02-protocol-spec.md and
  04-client-state-model.md for the coalesced sequence-range contract.
Requirements: GUI-EVENT-001/002 foundation.
Implementation: Added isolated monotonic sequence spaces, atomic journal append
  and publish ordering, count/age-bounded replay, bounded pull subscriptions,
  33 ms text/reasoning/terminal coalescing, latest-wins entity updates,
  reliable-event preservation, explicit snapshot-required overflow pause,
  metric hooks, payload copying for terminal bytes, and idempotent teardown.
  Added firstSequence to the protocol: a coalesced event covers the inclusive
  [firstSequence, sequence] range so a client does not mistake intentional
  coalescing for packet loss.
Commands and results:
  go test -race ./internal/sessionevent -count=50
    PASS, 2.113s. Covers 500 concurrent publishers, ordered concurrent
    subscription delivery, count/age replay, coalescing windows and payloads,
    reliable and recoverable overflow, snapshot marker, terminal payload
    ownership, close/unblock, and bounded metric kinds.
  go test -race ./internal/sessionevent ./internal/guimetrics -count=1
    PASS, sessionevent 1.944s; guimetrics 1.727s.
  go test -race ./internal/acp/... -count=1
    PASS, 8.557s.
  go test -race ./internal/agent -count=1
    PASS, 62.437s.
  go test ./internal/agent/tools ./internal/cmd ./internal/config -run '^$'
    PASS; selected packages compile.
  git diff --check
    PASS, no output.
Compatibility/security review: No ACP handler, agent, TUI, CLI, persistence,
  or standard ACP wire behavior was changed. The new package is not connected
  to production paths until WP-03/WP-04. Every queue and journal has count/age
  bounds. Metric kinds normalize unknown values to "other". Subscriber delivery
  does not perform transport writes or wait for consumers while publishing.
Remaining work: WP-03 now bridges canonical Agent callbacks. WP-04 must expose
  negotiated GUI routing; WP-05 must build protocol sync/replay handlers on the
  journal and snapshot-required contract.
Blockers: None.
```

## Blocker history

For each occurrence record date, package, exact condition, evidence, attempted
alternatives, and whether another package can progress. A global `blocked`
status requires the same impasse on three consecutive autonomous iterations and
no dependency-ready alternative.

2026-07-15, WP-17, occurrence 1: all implementation and Windows gates pass,
but GUI-PERF release signoff requires an actual Unix SLO run. Linux/Darwin
CGO-disabled cross-builds pass and `.github/workflows/gui-acp.yml` defines the
required jobs. This host has no WSL distribution (`wsl.exe --list --quiet`
returned no entries) and no Docker executable. The workflow contains uncommitted
worktree changes and cannot be dispatched without separate commit/push authority.
No other required package remains that can progress locally.

2026-07-15, WP-17, occurrence 2: re-audited local Unix execution alternatives.
`Get-Command` found only the Windows WSL launcher and legacy bash launcher; no
Podman, nerdctl, qemu-x86_64 or qemu-x86_64-static executable is installed, and
recursive inspection found no QEMU binary under Program Files. WSL still has no
registered distribution. Installing/importing a distribution or pushing the
uncommitted workflow would mutate external state beyond current authority. The
same Unix runtime evidence remains the only unproven release requirement; all
safe in-worktree implementation and verification work is exhausted.

2026-07-15, WP-17, occurrence 3: external state is unchanged. WSL again reports
no registered distribution, and `Get-Command` again finds no Docker, Podman,
nerdctl or QEMU user-mode executable. The Unix SLO cannot be executed on this
host, while dispatching the uncommitted three-platform workflow still requires
commit/push authority. This is the third consecutive turn with the identical
blocking condition, no dependency-ready package or safe local alternative
remains, and the runbook threshold for marking WP-17/global status blocked is
satisfied. Resume after either providing a Unix runtime or authorizing CI.

## Phase-gate evidence

| Phase | Status | Commands/results | SLO/compatibility evidence |
|---|---|---|---|
| Phase 0 | complete | WP-01 baselines plus WP-17 full metric-emitter audit, canonical benchmarks and SLO harness recorded above. | All thirteen fixed-label metrics have production emitters; Windows service-level SLOs pass. |
| Phase 1 | complete | WP-02/WP-03 race suites and live publish benchmarks recorded above. | Direct callback path is separate from persistence; zero synchronous DB writes per text delta; standard ACP unchanged. |
| Phase 2 | complete | WP-04 routing, WP-05 reliable sync, WP-06 bounded snapshot, and WP-07 cursor pagination/search race/SLO gates pass. | Reconnect/gap, 10k first-screen, indexed pagination, and standard ACP compatibility are evidenced. |
| Phase 3 | complete | WP-08 CRUD/fork, WP-09 turn/idempotency, and WP-10 inference override race/compatibility gates pass. | Turn execution and session lifecycle are serialized, bounded, reconnect-safe, revision-aware, and per-run inference is immutable. |
| Phase 4 | complete | WP-11 Terminal, WP-12 Blob, and WP-13 client-FS ownership, reconnect, cleanup, platform, permission, path-security, revision and race gates pass. | PTY/ConPTY, large/binary handles, and unsaved-buffer I/O are bounded, dual-owner/root authorized, revision-aware, and standard ACP/TUI compatible. |
| Phase 5 | complete | WP-14 provider auth and WP-15 asynchronous MCP race/security evidence recorded above. | Provider credentials and dynamic MCP capabilities are owner/session isolated, asynchronous, bounded, redacted and standard-ACP compatible. |
| Phase 6 optional | pending | — | Not required for version 1 |

## Final requirement traceability

Populate with implementation files and test names before release. The work
package mapping in `07-delivery-and-work-packages.md` is a plan, not completion
evidence.

| Requirement group | Implementation evidence | Test/benchmark evidence | Status |
|---|---|---|---|
| GUI-COMPAT | Optional ACP experimental fields, independent connection-scoped guiapi router, and reusable framed Transport preserving identical JSON-RPC dispatch | WP-04 standard/desktop integration plus WP-16 stdio/custom-transport contract and full ACP race suites | complete |
| GUI-EVENT | internal/sessionevent hub, reliable guiapi sync, and bounded exact-outcome mutation idempotency | WP-02/WP-05 replay and overflow suites plus WP-09 duplicate/conflict/race tests | complete |
| GUI-SESS | Bounded snapshot/sync, indexed pagination/search, persisted CRUD flags, runtime-safe delete, transactional fork, and session/turn inference overrides | WP-06/WP-07 query/SLO suites; WP-08 CRUD/fork evidence; WP-10 precedence, CAS, immutable-run, child-policy, snapshot, retry, and race tests | complete |
| GUI-TURN | internal/turn serialized runtime plus guiapi turn, queue, steer, and retry handlers | WP-09 state-machine, cancellation-order, retry, duplicate, capacity, race, and p95 tests | complete |
| GUI-TERM | internal/terminal PTY/ConPTY manager, guiapi terminal handlers/events/snapshots, App/session/connection lifecycle wiring | WP-11 domain, GUI, App and native Windows race tests; Linux CGO-disabled cross-compile; O(1) ring benchmark | complete |
| GUI-FS | internal/clientfs execution scope and physical path resolver; guiapi reverse-call/Blob adapter; turn/ACP context injection; read/write/edit adapters; bounded tool/event metadata | WP-13 domain, Windows junction/Unix symlink, GUI lifecycle/ownership, turn retry, ACP, file-tool and Agent race suites | complete |
| GUI-BLOB | internal/blob registry, guiapi Blob handlers, deferred turn attachment adapter, App/session/connection lifecycle wiring | WP-12 domain, GUI, Turn, App race suites and >1 MiB deferred attachment coverage | complete |
| GUI-AUTH | internal/providerauth manager/config adapter/default flows plus guiapi provider handlers/events and App ownership | WP-14 domain, projection, GUI lifecycle/idempotency, ConfigStore and App race suites | complete |
| GUI-MCP | internal/mcplifecycle App service, live root-session Access, async ACP scheduling, guiapi control/log handlers, reliable status events and snapshots | WP-15 repeated lifecycle/GUI race suites; ACP immediate new/load, A/B isolation, generation, partial failure, reconnect/disable/shutdown, tombstone, config cleanup, log redaction and stale invocation tests | complete |
| GUI-OBS | Context-injected fixed-label Recorder; ACP request/load, event queue/coalescing/gap/snapshot, chunk/write, SQLite, terminal/blob and active subscription/prompt emitters | WP-01 metric/cardinality tests plus WP-17 production-emitter audit and ActivePromptCount install/cleanup/close regression | complete |
| GUI-SEC | Terminal permission parity, Blob ownership/expiry, client-FS confinement/CAS, provider/MCP isolation, bounded idempotency, redacted errors/logs, method deadlines/nesting/frame limits and protocol-only stdout | WP-11 through WP-16 suites plus WP-17 SQLite/attachment/duplicate/replay/backend/provider/MCP/permission/blocked-pipe fault gates, full race/root/Fantasy runs and stdout/bounds audit | complete |
| GUI-PERF | Agent callback bridge, bounded event hot path, connection-local blocked-writer isolation, bounded transport/dispatch, indexed snapshots and release soak/resource stabilization | Start p95 test and 39.30 us benchmark; cancellation p95 test; real-SQLite snapshot p95 516.6 us; 1,523 ns enqueue; full 100x10k/10-session/9,997-chunk Windows soak; reduced race; Unix CI and cross-build | in_progress: actual Unix SLO run pending |

## Final release evidence

```text
Full targeted race suite: PASS. WP-17 combined concurrent packages plus full
  Agent/Session race commands and timings are recorded in the active record.
Full repository test classification: Root go test ./... PASS; nested Fantasy
  go test ./... PASS with VCR recording disabled. No failures to waive.
Benchmarks and SLO results: Windows prompt p95, cancellation p95, real-SQLite
  10k snapshot p95, Hub publish p95 and six canonical benchmarks all pass; exact
  outputs recorded above. Actual Unix SLO execution remains required.
Soak result and resource stabilization: Full non-race profile PASS with
  100x10,000 history, ten active sessions, 9,997 chunks over ten seconds,
  blocked GUI/recovery, terminal/Blob/MCP/provider activity, 687,376-byte heap
  growth, one goroutine growth and zero retained owned resources. Reduced race
  profile PASS.
Fault-injection result: PASS for SQLite busy/slow/full, blocked pipe/GUI,
  GUI/backend restart, MCP hang/reconnect, provider/missing-response timeout,
  malformed/oversized attachments, duplicate IDs and expired replay.
Security result: PASS. All queues/buffers/registries have configured bounds;
  fixed error/redaction/ownership/path/revision tests pass; only protocol framing
  owns production stdout; all thirteen metrics have bounded-label emitters.
Standard ACP compatibility result: Full ACP race and root suites PASS; standard
  schemas/routing remain separate from negotiated crush/* methods.
git diff --check: PASS, no output. Diff stat/status inspected; dirty cumulative
  worktree preserved.
Known optional Phase 6 work: Named Pipe/Unix Socket/WebSocket production
  adapters and workspace supervision remain optional and are not release scope.
Pre-existing unrelated failures: None reproduced. Prior Windows temp-log lock,
  hook timeout and Fantasy cassette mismatch all pass in fresh full runs.
Final reviewer/date: Pending actual Unix SLO execution; 2026-07-15 local audit.
```
