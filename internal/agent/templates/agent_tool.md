Launch a subagent to handle a bounded task autonomously.

Available subagent types:
{agents}

All subagent types are selected via the `subagent_type` parameter of this `agent` tool — they are NOT standalone tools.

## When to use the Agent tool

- **`explore`** — evidence gathering only, not final judgment. Open-ended codebase search, pattern hunting, implementation lookup, dependency tracing, diff summarization. Runs on a smaller/faster model. Read-only (restricted git-inspection bash); do NOT assign build/test/lint/reproduction tasks to it.
- **`plan`** — architecture planning before complex multi-file work. Read-only; returns concrete files, sequence, edge cases, verification steps.
- **`review`** — final code review. Runs on a large/review-capable model, read-only, **blocking**. Returns patch-anchored findings. Use for final review — never delegate correctness decisions to `explore`.
- **`librarian`** — source-verified dependency/framework/API research; cites local sources or official docs.
- **`designer`** — UI/UX implementation and review; full tool access.
- **`quick_task`** — small, mechanical, unambiguous tasks where a low-cost worker fits.
- **`general`** — normal implementation and verification: independent tasks, test reproduction, refactors that don't block your next step.

Delegation rules:
- If 2+ substantial independent tasks can run in parallel, use a single `tasks` array (shared concurrency limit, consolidated result) instead of separate calls or serial work.
- Start delegated work immediately when an `explore` can gather context while you implement.
- Don't claim you're delegating unless this response actually includes the `agent` tool calls.
- Verify after each batch (type checks/tests/lint on changed files); don't trust subagent self-reports without gates.

## When NOT to use the Agent tool

- **NEVER spawn a subagent to read files** — single or multiple, with or without analysis. Direct `read`/`glob`/`grep` calls are always cheaper (parallel-capable, no output round-trip). This applies equally to `explore`: it is not a file-content relay.
- Don't delegate tiny, tightly-coupled edits, lightweight isolated file ops, or work whose result you need immediately. Prefer direct tool calls when cheaper.
- Don't keep broad implementation in the main thread just because you know the files — delegate separable chunks.

## Usage notes

1. Each subagent call is stateless and returns a single final report.
2. State clearly whether the subagent may only research or may modify code.
3. Specify the exact output you need (files, findings, verification commands).
4. Subagent output isn't shown to the user automatically — summarize it yourself.
5. Prefer early delegation for bounded work that unblocks or parallelizes the main task.

## Task schema

Each entry in `tasks` accepts:
- `name` (required): short identifier used in logs and result mapping.
- `description` (required): one-line summary of what the task accomplishes.
- `assignment` (required): full self-contained instructions — file paths, acceptance criteria, exact deliverable.
- `subagent_type` (optional): subagent kind (`explore`/`general`/`review`/...). Defaults to the orchestrator's default.
- `role` (optional): specialist identity injected into the subagent's system prompt.

The model and structured-output schema are determined by the subagent type's static configuration, not per-invocation parameters.

## Execution model

Tasks run in parallel under a concurrency limit. There is no dependency graph and no automatic retry — you are the orchestrator: decide when to spawn, how to interpret results, and whether to retry with revised instructions.

## Continuing a subagent

`run_in_background: true` returns an agent ID instead of blocking. That agent stays addressable after it finishes: send it more work with `send_message(agent_id, message)` and it resumes its **existing conversation**, keeping every file it read and conclusion it reached.

Prefer continuing over re-spawning when the new work builds on what that subagent already learned — a fresh subagent starts from a handoff summary and has to rediscover the details. Re-spawn only when the task is genuinely independent or you want an untainted second opinion.

## Completion contract

Every subagent MUST complete by calling the `yield` tool exactly once (it terminates the subagent). The result has:
- `status`: `completed` / `completed_with_warnings` / `failed` / `canceled` / `blocked`.
- `data`: the complete result text (required for successful statuses).
- `error`: error details on `failed`/`blocked`.
- `payload`: optional structured JSON per the agent type's output schema.

On failure/block, inspect `error`/`data`, then decide: retry with revised instructions, skip, or restructure the plan. No automatic retry or timeout — you own those decisions.

## IRC coordination between subagents

Subagents in the same `tasks` array can coordinate via the `irc` tool (always available to subagents). Each gets its own agent ID and visible peer list.

- **Run in parallel when B needs a small piece from A** (a type signature, config key, file path) — B asks A over IRC instead of waiting. **Still sequence** when one task produces a large contract the other consumes wholesale.
- Keep IRC to quick questions, not long-form transfer; subagents should answer what their own tools (read/grep/glob) can.
- **Verification**: after a batch completes, verify combined output (type checks/tests/lint). Fix minor issues directly; dispatch a fix-up subagent for significant gaps. Never mark complete on self-reports alone.
