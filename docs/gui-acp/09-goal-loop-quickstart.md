# Goal/Loop Quick Start

This is the copy-and-run entry point for implementing the complete desktop
backend specification. The normative execution rules remain in
[08-autonomous-execution.md](08-autonomous-execution.md), and durable progress
remains in [IMPLEMENTATION-STATUS.md](IMPLEMENTATION-STATUS.md).

## Crush TUI: shortest supported workflow

Open this repository in Crush and enter:

```text
/goal set Implement the complete release scope in docs/gui-acp/ by following docs/gui-acp/08-autonomous-execution.md exactly. Use docs/gui-acp/IMPLEMENTATION-STATUS.md as the durable checkpoint, resume the active package or complete exactly one dependency-ready package per turn, preserve unrelated worktree changes and standard ACP/TUI behavior, use deterministic tests with no real LLM or VCR recordings, and complete the goal only after WP-17 and every release gate pass.
```

Do not add `--budget` unless a hard token ceiling is intentional. An unlimited
goal is safer for this multi-package implementation. Setting a goal records the
objective but does not itself send an implementation prompt, so send this once
afterward:

```text
Continue implementing docs/gui-acp/ according to docs/gui-acp/08-autonomous-execution.md. Read docs/gui-acp/IMPLEMENTATION-STATUS.md, verify the worktree, select exactly one dependency-ready incomplete work package or resume the active one, implement and verify it, then update the checkpoint with evidence. Do not claim global completion until WP-17 and every required completion gate pass. If no package can progress, record the repeated blocker precisely and report it.
```

Crush goal mode may chain continuation turns automatically. If execution
returns to the prompt while the goal is still active, send the same compact
prompt again. If reopening the session paused the goal, run `/goal resume`
first. Use `/goal show` to inspect state and `/goal pause` for an intentional
stop. Do not use `/goal replace` unless the release scope genuinely changes.

## Repeated loop runner

Crush currently has no `/loop` slash command or shell builtin. Here, “loop”
means an external runner, GUI client, test harness, or user repeatedly sending
the compact prompt above to the same session.

The runner must inspect the checkpoint after every iteration and stop only
when one of these conditions is true:

1. `Overall status` is `complete` and WP-17 contains release evidence.
2. `Overall status` is `blocked`, the same blocker has been recorded on three
   consecutive iterations, and no other dependency-ready package can proceed.
3. The user explicitly stops the run.

Suggested runner pseudocode:

```text
while true:
    status = read docs/gui-acp/IMPLEMENTATION-STATUS.md
    if release_complete(status): stop_success
    if repeated_global_blocker(status, count=3): stop_blocked
    send LOOP_PROMPT to the same Crush session
    wait for the turn to finish or for a bounded runner deadline
```

A transport disconnect must not be interpreted as cancellation. Reconnect,
sync the session, and inspect the checkpoint before deciding whether to resend.
Because every iteration selects one package and writes evidence, resending the
prompt after an uncertain response is safe at the workflow level.

## Agent tool invocation

An Agent with direct access to the `goal` tool can create the same goal using:

```json
{
  "op": "create",
  "objective": "Implement the complete release scope in docs/gui-acp/ by following docs/gui-acp/08-autonomous-execution.md exactly. Use docs/gui-acp/IMPLEMENTATION-STATUS.md as the durable checkpoint, resume the active package or complete exactly one dependency-ready package per turn, preserve unrelated worktree changes and standard ACP/TUI behavior, use deterministic tests with no real LLM or VCR recordings, and complete the goal only after WP-17 and every release gate pass."
}
```

Use `op: "resume"` only for a paused or budget-limited goal. If a goal is
already active, query it with `op: "get"`; do not create a competing goal.
`op: "complete"` is valid only after the checkpoint and WP-17 release gates
prove that no required work remains.

## One-command release verification

After the implementation checkpoint reaches WP-17, run the complete local gate:

```powershell
task test:gui-release
```

This runs the reduced profile under `-race`, deterministic fault/reconnect
coverage, the full 100-session by 10,000-message non-race soak, and the six
canonical performance benchmarks. It does not record VCR cassettes and does not
call a real LLM, provider, or MCP server. The individual entry points are:

```powershell
task test:gui-release-race
task test:gui-release-fault
task test:gui-release-full
```

The full profile intentionally takes longer and blocks one simulated GUI for
10 seconds. CI runs the reduced race profile on Windows, macOS, and Linux; the
full non-race profile runs on `main`, schedule, and manual dispatch.

## Recovery checklist

When a run stops unexpectedly:

1. Open the same session and repository worktree.
2. Run `/goal show`.
3. Read `IMPLEMENTATION-STATUS.md` and `git status --short`.
4. Resume the package marked `in_progress`; do not start a second package.
5. If no package is active, select one whose dependencies are all complete.
6. Run `/goal resume` only if the goal status is paused or budget-limited.
7. Send the compact loop prompt.

The checkpoint is authoritative if chat history, goal state, and the worktree
appear inconsistent. Never discard dirty-worktree changes merely to make the
checkpoint look clean.
