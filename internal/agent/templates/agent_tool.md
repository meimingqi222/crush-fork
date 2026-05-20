Launch a subagent to handle a bounded task autonomously.

Available subagent types:
{agents}

When to use the Agent tool:
- **Use `explore` only for evidence gathering, not final judgment.** The `explore` subagent runs on a smaller, faster, cheaper model by default and is purpose-built for parallel context discovery. Treat it like Claude Code/opencode-style research subagents: ask it to map files, locate symbols, summarize diffs, collect local git facts, and return file:line evidence for the primary model to analyze.
- Open-ended codebase exploration, pattern hunting, implementation lookup, dependency tracing, and diff summarization should usually use the `explore` subagent.
- **Do not delegate final code review, correctness approval, or bug triage decisions to `explore`.** Use `review` for final code review so the work runs on a large/review-capable model with a read-only review prompt. `explore` can collect the relevant diff snippets, history, references, and candidate concerns for the primary agent or review subagent to verify.
- Use `plan` for architecture planning before complex multi-file work; it is read-only and should return concrete files, sequence, edge cases, and verification steps rather than implementation. `plan` can spawn `explore` for parallel evidence gathering.
- Use `review` for final code review; it runs on a large/review-capable model, is read-only, and returns patch-anchored findings. `review` is **blocking** — the main flow waits for it to complete. `review` can spawn `explore` for parallel evidence gathering.
- Use `librarian` for source-verified dependency, framework, or external API research; it should cite local dependency source or official documentation instead of relying on model memory.
- Use `designer` for UI/UX implementation and review; it has full tool access and can edit files and run verification.
- Use `quick_task` only for small, mechanical, unambiguous tasks where a low-cost worker is appropriate; use `general` for normal implementation and verification work.
- The `explore` subagent is read-only and has a restricted `bash` tool for direct local read-only git inspection only. It is suitable for `git diff`, `git status`, `git log`, `git show`, `git blame`, `git rev-parse`, `git merge-base`, and `git ls-files`, but not for mutating git commands, wrapper shells, build/test commands, package managers, linters, or general shell work.
- **Do not assign build, test, lint, or reproduction tasks to `explore`.** Commands such as `go test`, `go build`, `npm test`, `pytest`, `cargo test`, `make`, `task`, or any command needing `2>&1`/general shell execution require the primary agent or a `general` subagent.
- Independent implementation tasks, test reproduction, final code-review passes, or file-local refactors that can proceed without blocking your immediate next step should usually use the `general` subagent or remain with the primary agent.
- If 2 or more substantial independent tasks can proceed in parallel, you should usually delegate them instead of doing them serially in the main thread.
- **When there are multiple substantial independent tasks, use a single Agent call with the `tasks` array** so they run in parallel under a shared concurrency limit, with a single consolidated result.
- If an `explore` subagent can gather context while you or another subagent handles implementation, start that delegated work immediately instead of waiting to do the search yourself first.
- For user requests that ask you to review code, inspect security-sensitive logic, or decide whether a change is correct, use `explore` only for bounded evidence collection; perform the actual review and decision in the primary thread.
- Do not claim that you are delegating, spinning up subagents, or parallelizing work unless this response actually includes the corresponding `agent` tool calls.

When NOT to use the Agent tool:
- **CRITICAL: NEVER spawn a subagent for file reading tasks.** This includes:
  - Reading single or multiple files and returning their contents
  - Reading files without analysis, transformation, or synthesis
  - Tasks that can be accomplished with direct `read`, `glob`, or `grep` tool calls
  - Example violations: "Read file X", "Get contents of files A, B, C", "Read these files and return them"
- **This rule applies equally to `explore`.** Do NOT use `explore` as a file-content relay. Prompts like "read these N files completely and return their contents" sent to `explore` are pure waste: the subagent reads the files, burns its output tokens echoing them back, and the primary agent still has to process them. `explore` exists to *search, locate, and summarize* — not to act as a proxy `read` call.
- **Why this matters:** Spawning a subagent just to read files wastes an entire LLM turn and session context. The subagent will spend output tokens returning file contents that you could have read directly.
- **Correct approach:** Call `read`/`glob`/`grep` directly in your current response. These tools support parallel invocation, so you can read multiple files simultaneously without subagent overhead.
- If the next step depends immediately on the result, do the work directly instead of delegating and waiting.
- Do not delegate tiny, tightly-coupled edits that are faster to do in the current thread.
- Do not delegate lightweight isolated single-file operations when direct tool calls are likely cheaper in tokens and just as fast.
- If several independent lightweight file operations can proceed in parallel, prefer multiple direct tool calls in one response instead of subagents.
- Do not use the main thread for broad implementation work just because you already know which files are involved. If those file changes are still separable, delegate them.

Usage notes:
1. Each subagent call is stateless and returns a single final report.
2. Your prompt must clearly state whether the subagent should only research or is allowed to modify code.
3. Tell the subagent exactly what output you need back, including relevant files, findings, and verification commands for the primary agent to run when the chosen subagent cannot run them itself.
4. The subagent result is not shown to the user automatically; summarize the result yourself if needed.
5. The subagent's outputs should generally be trusted unless they conflict with stronger evidence in the current thread.
6. Do not treat this tool as a last resort. Prefer early delegation for bounded work that can unblock or parallelize the main task.
7. If you choose delegation, make the tool call first rather than narrating a future intention to delegate.
8. **Use the `tasks` array for 2+ tasks.** All tasks in the array run in parallel with a concurrency limit. Each task must be self-contained with its own complete instructions in the `assignment` field. Use the `context` field to share background information across all tasks. The `tasks` array provides concurrency limiting and a unified result — always prefer it over launching multiple separate Agent calls.

## Task schema

Each entry in `tasks` accepts:
- `name` (required): short identifier used in logs and result mapping.
- `description` (required): one-line summary of what the task accomplishes.
- `assignment` (required): full self-contained instructions for the subagent, including file paths, acceptance criteria, and the exact deliverable.
- `subagent_type` (optional): subagent kind to spawn (e.g. `explore`, `general`, `review`). Defaults to the orchestrator's configured default when omitted.
- `output_schema` (optional): JSON Schema describing the structured payload the subagent should return in its `data` field.

## Execution model

Tasks are executed in parallel with a concurrency limit. All tasks in a batch start as soon as a slot is available; there is no automatic dependency graph and no automatic retry. The parent agent (you) is the orchestrator: you decide when to spawn tasks, how to interpret their results, and whether to retry by spawning a new task with revised instructions.

## Completion contract

Every subagent MUST complete by calling the `subagent_finish` tool exactly once. The structured result returned to the parent has these fields:
- `status`: terminal status — one of `completed`, `completed_with_warnings`, `failed`, `canceled`, or `blocked`.
- `summary`: short human-readable summary of what was done.
- `data`: structured payload (conforms to `output_schema` when one was supplied).
- `error`: error details when `status` is `failed` or `blocked`.
- `files_touched`: list of files the subagent created or modified.

If a task fails or is blocked, inspect the `error` and `summary`, then decide whether to retry by spawning a fresh task with revised instructions, skip the work, or restructure the plan. There is no automatic retry, no failure budget, and no graph timeout — the orchestrator owns those decisions.

## Model priority override

Use the `model_priority` field on the top-level params to override the default model selection order for all tasks in this invocation. Each entry is a model identifier; the system tries them in order until one is available.

## IRC coordination between subagents

Subagents launched in the same `tasks` array can communicate with each other and with you via the `irc` tool. Each subagent receives its own agent ID and a list of visible peers at startup.

**When IRC enables more parallelism:**
- If task B depends on a small piece of information from task A (a type signature, a config key, a file path), you can often run A and B in parallel. B can ask A for the missing piece over IRC instead of waiting for A to finish first.
- **Still sequence** when one task produces a large, evolving contract (generated types, schema migration, core module API) that the other consumes wholesale — IRC round-trips do not replace a finished artifact.

**How to write assignments that use IRC:**
- Tell each subagent its role and what peers it can reach: "If you need information from another agent, use `irc` with op=send to ask."
- Keep IRC usage to quick questions and coordination — not long-form content transfer.
- Subagents should not use IRC to ask questions that their own tools (read, grep, glob) can answer.

**Verification after parallel work:**
- After a batch of subagents completes, verify their combined output before proceeding. Run type checks, tests, or lint on the union of changed files.
- If a subagent's work has minor issues, you may fix them directly if the fix is small and obvious. For significant gaps, dispatch a fix-up subagent.
- Do not mark work complete based solely on subagent self-reports — verify with gates.
