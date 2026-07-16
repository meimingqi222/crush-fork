# Autonomous Goal/Loop Execution Runbook

This document is the single entry point for an autonomous Agent implementing
the complete Crush desktop backend specification. It is designed for Crush's
`goal` workflow, a repeated `loop` workflow, or any runner that invokes an Agent
until a repository checkpoint reports completion.

The runner MUST treat [IMPLEMENTATION-STATUS.md](IMPLEMENTATION-STATUS.md) as
the durable execution checkpoint. Conversation history, todos, and goal state
are helpful but are not authoritative implementation records.

For exact Crush TUI commands and a copy-and-run loop entry, see
[09-goal-loop-quickstart.md](09-goal-loop-quickstart.md).

## Release scope

The required release scope is phases 0 through 5 and WP-01 through WP-17.
WP-17 validates the integrated result and therefore remains incomplete until
all required work packages pass its release gates. Phase 6 transport/supervisor
work is optional unless the invoking user explicitly includes it.

The Agent MUST NOT mark the goal complete merely because all source files were
created. Completion requires the verification and evidence rules below.

## One-shot goal objective

Copy the following text as the goal objective. Do not set a token budget unless
the user explicitly wants one; a fixed budget can stop execution before the
integration and soak phases.

```text
Implement the complete Crush desktop ACP backend specification in
docs/gui-acp/ for release scope phases 0 through 5.

Use docs/gui-acp/IMPLEMENTATION-STATUS.md as the durable source of progress.
Read AGENTS.md, every specification document in docs/gui-acp/,
docs/acp-hardening-plan.md, and every applicable docs/pitfalls/ document before
changing code. Work through WP-01 to WP-17 according to their dependency graph,
not merely numeric order. Resume partially completed packages before starting
new ones.

For every work package: inspect the current implementation first; record the
baseline; implement the smallest architecture-compatible change; add
deterministic tests without real LLM calls or VCR cassettes; format Go code;
run targeted tests including -race where applicable; run git diff --check; then
update IMPLEMENTATION-STATUS.md with files, requirement IDs, commands, results,
and remaining risks. Never mark a package complete when required tests are
failing or acceptance criteria are unverified.

Preserve standard ACP compatibility, existing TUI/CLI behavior, session MCP
capability isolation, and Fantasy dual-message-state invariants. Do not
reintroduce global model refreshes, full-history GUI load, SQLite-driven live
streaming, unbounded queues, real-provider tests, or silent reliable-event
drops. Do not overwrite or discard unrelated user changes in the dirty
worktree. Do not commit, push, create a PR, or perform unrelated cleanup unless
the user separately authorizes it.

Continue autonomously while safe in-scope work remains. When a package is
blocked, record concrete evidence and continue with another dependency-ready
package if possible. Declare the goal blocked only when no required package can
make meaningful progress without user authority or an external state change.

Complete the goal only after WP-17 passes the required race, protocol,
performance, security, reconnect, fault-injection, and soak gates; all required
requirements have traceability evidence; all required rows in
IMPLEMENTATION-STATUS.md are complete; and git diff --check is clean. In the
final handoff, report completed packages, requirement coverage, tests and
benchmarks, migrations/compatibility notes, known optional Phase 6 work, and
any pre-existing unrelated failures separately.
```

In Crush, the Agent should invoke the goal tool equivalent of:

```json
{
  "op": "create",
  "objective": "<the objective above>"
}
```

If the same goal already exists, use `resume`; use `replace` only when changing
scope while preserving accumulated usage. The Agent MUST call `complete` only
after satisfying the completion gates.

## Loop prompt

For a runner that repeatedly supplies a prompt, use this compact prompt on every
iteration:

```text
Continue implementing docs/gui-acp/ according to
docs/gui-acp/08-autonomous-execution.md. Read
docs/gui-acp/IMPLEMENTATION-STATUS.md, verify the worktree, select exactly one
dependency-ready incomplete work package or resume the active one, implement and
verify it, then update the checkpoint with evidence. Do not claim global
completion until WP-17 and every required completion gate pass. If no package
can progress, record the repeated blocker precisely and report it.
```

The loop runner SHOULD stop only when the checkpoint `Overall status` is
`complete`, or when it is `blocked` with the same unresolved blocking condition
confirmed on three consecutive iterations. A transient test failure, slow test,
or difficult implementation is not a blocker.

## Per-iteration algorithm

Each autonomous turn MUST follow this sequence:

1. Read `AGENTS.md`, this runbook, and the implementation checkpoint.
2. Inspect `git status --short` and preserve unrelated changes.
3. If one package is `in_progress`, resume it. Otherwise select a `pending`
   package whose dependencies are all `complete`.
4. Re-read that package in `07-delivery-and-work-packages.md` and the linked
   requirements. Read applicable pitfall documents before touching their code.
5. Inspect the actual code and tests; do not assume the specification's file
   suggestions still match the repository.
6. Mark the package `in_progress` in the checkpoint and record the start date.
7. Implement only the selected package, including tests and metrics required by
   its acceptance criteria.
8. Format changed Go files using `gofumpt`; fall back to `goimports`, then
   `gofmt` only if necessary.
9. Run the package's targeted tests. Run `-race` for concurrency-sensitive Go
   packages. Run broader dependent tests in proportion to risk.
10. Review the diff for standard ACP compatibility, security limits, resource
    cleanup, and known pitfalls. Run `git diff --check`.
11. Update the checkpoint with exact commands and outcomes. Mark `complete`
    only if every package acceptance criterion is evidenced.
12. Continue to the next dependency-ready package when execution time permits.

The Agent MAY complete multiple small dependency-ready packages in one turn,
but MUST preserve an independently reviewable checkpoint entry for each one.

## Dependency scheduler

Use this graph rather than assuming strict numeric execution:

```text
WP-01 -> WP-02 -> WP-03 -------------------+
             +-> WP-04 -> WP-05 -> WP-06  |
                         |       |         |
                         |       +-> WP-15 |
                         +-> WP-09 -> WP-10|
                         |       +-> WP-08|
                         +-> WP-11         |
                 WP-04 -> WP-12 -> WP-13  |
                         +-> WP-14         |
                         +-> WP-16         |
                 WP-04 -> WP-07            |
                                           v
                                      WP-17 release gate
```

WP-08 depends on WP-09's idempotency support even though its number is lower.
WP-15 requires reliable sync and snapshots. WP-17 may be developed incrementally
from Phase 0 but cannot be completed until WP-01 through WP-16 are complete.

Recommended execution batches:

1. Foundation: WP-01, WP-02.
2. Live path and routing: WP-03, WP-04.
3. Reliable state: WP-05, WP-06, WP-07.
4. Turn/session semantics: WP-09, WP-08, WP-10.
5. Desktop resources: WP-12, WP-11, WP-13.
6. External services: WP-14, WP-15.
7. Framing/integration: WP-16, WP-17.

## Verification levels

### Package gate

A package can be marked complete only when:

- its required code and deterministic tests exist;
- targeted tests pass;
- race tests pass when concurrency is involved;
- public behavior has contract coverage;
- cleanup/error paths are tested;
- its requirements and acceptance criteria are linked in the checkpoint;
- `git diff --check` is clean for the current worktree.

### Phase gate

At the end of every phase, run all directly affected packages plus:

```powershell
go test -race ./internal/acp/...
go test -race ./internal/agent -count=1
go test ./internal/agent/tools ./internal/cmd ./internal/config -run '^$'
git diff --check
```

Add `./internal/guiapi/...`, `./internal/sessionevent/...`, terminal, blob,
client-FS, session, and provider packages as they are introduced. A command that
matches no package MUST be updated to the real package path in the checkpoint.

### Release gate

Before completing the goal, the Agent MUST:

1. run all WP-17 protocol, reconnect, fault-injection, security, and soak tests;
2. run the full targeted race suite for changed concurrent packages;
3. run `go test ./...` and classify every failure as introduced, pre-existing,
   platform-specific, or external-fixture-related;
4. fix every introduced failure;
5. demonstrate all GUI-PERF SLOs with recorded benchmark/harness output;
6. verify no automated test requires a real LLM, provider credential, or VCR
   recording;
7. verify no unbounded resource and no stdout logging was introduced;
8. run `git diff --check` and inspect `git diff --stat` plus `git status`;
9. update the traceability and final evidence sections in the checkpoint.

Pre-existing Windows temporary-file contention, hook timeout, or obsolete VCR
failures MUST be investigated and classified. They MUST NOT be used to conceal a
regression. Tests that inherently require real LLM calls SHOULD be replaced with
deterministic protocol fixtures or mocks; deleting a test requires documenting
which contract replacement preserves its value.

## Blocker policy

Record a blocker with command, exact error, affected package, attempts, and why
safe alternatives cannot progress. The Agent SHOULD work on another ready
package when dependency-safe. It MUST request user authority for destructive
repository operations, external credentials, changing the release scope, or a
wire-level design decision that contradicts this specification.

Do not classify these as blockers by themselves:

- a dirty worktree with unrelated changes that can be preserved;
- a long-running test;
- an absent optional tool when a documented fallback exists;
- an initial benchmark failing its future target before the implementation;
- a failing optional Phase 6 feature.

## Spec amendment rule

If implementation proves a requirement infeasible or internally inconsistent,
the Agent MUST first update the relevant specification with rationale,
compatibility impact, migration plan, and changed acceptance criteria. The same
change MUST update requirement traceability and the checkpoint. An Agent MUST
NOT silently implement a different wire contract.
