Launch a subagent to handle a bounded task autonomously.

Available subagent types:
{agents}

When to use the Agent tool:
- **Use `explore` only for evidence gathering, not final judgment.** The `explore` subagent runs on a smaller, faster, cheaper model by default and is purpose-built for parallel context discovery. Treat it like Claude Code/opencode-style research subagents: ask it to map files, locate symbols, summarize diffs, collect local git facts, and return file:line evidence for the primary model to analyze.
- Open-ended codebase exploration, pattern hunting, implementation lookup, dependency tracing, and diff summarization should usually use the `explore` subagent.
- **Do not delegate final code review, correctness approval, or bug triage decisions to `explore`.** The primary model must own final review conclusions because `explore` commonly uses a weaker small model and may miss subtle defects. `explore` can collect the relevant diff snippets, history, references, and candidate concerns for the primary agent to verify.
- The `explore` subagent is read-only and has a restricted `bash` tool for direct local read-only git inspection only. It is suitable for `git diff`, `git status`, `git log`, `git show`, `git blame`, `git rev-parse`, `git merge-base`, and `git ls-files`, but not for mutating git commands, wrapper shells, build/test commands, package managers, linters, or general shell work.
- **Do not assign build, test, lint, or reproduction tasks to `explore`.** Commands such as `go test`, `go build`, `npm test`, `pytest`, `cargo test`, `make`, `task`, or any command needing `2>&1`/general shell execution require the primary agent or a `general` subagent.
- Independent implementation tasks, test reproduction, final code-review passes, or file-local refactors that can proceed without blocking your immediate next step should usually use the `general` subagent or remain with the primary agent.
- If 2 or more substantial independent tasks can proceed in parallel, you should usually delegate them instead of doing them serially in the main thread.
- **When there are multiple substantial independent tasks, use a single Agent call with the `tasks` array** so they run in parallel with unified tracking, shared budget control, and a single consolidated result.
- If an `explore` subagent can gather context while you or another subagent handles implementation, start that delegated work immediately instead of waiting to do the search yourself first.
- For user requests that ask you to review code, inspect security-sensitive logic, or decide whether a change is correct, use `explore` only for bounded evidence collection; perform the actual review and decision in the primary thread.
- Do not claim that you are delegating, spinning up subagents, or parallelizing work unless this response actually includes the corresponding `agent` tool calls.

When NOT to use the Agent tool:
- **CRITICAL: NEVER spawn a subagent for file reading tasks.** This includes:
  - Reading single or multiple files and returning their contents
  - Reading files without analysis, transformation, or synthesis
  - Tasks that can be accomplished with direct `view`, `glob`, or `grep` tool calls
  - Example violations: "Read file X", "Get contents of files A, B, C", "Read these files and return them"
- **Why this matters:** Spawning a subagent just to read files wastes an entire LLM turn and session context. The subagent will spend output tokens returning file contents that you could have read directly.
- **Correct approach:** Call `view`/`glob`/`grep` directly in your current response. These tools support parallel invocation, so you can read multiple files simultaneously without subagent overhead.
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
8. **Use the `tasks` array for 2+ tasks.** Tasks with no `depends_on` run in parallel; tasks with dependencies run after their prerequisites complete. The `tasks` array provides budget control, concurrency limiting, retry support, and a unified result — always prefer it over launching multiple separate Agent calls.
