[CRITICAL WARNING]
Do NOT call this tool unless you have actually spawned background subagents using the `agent` tool first (`run_in_background: true`). It CANNOT be used to communicate with the user — write your response in the text body instead.

Sends a control message to a running task graph, or gives a background agent more work.

A follow-up sent with `agent_id` continues that agent's existing conversation: it keeps every file it read and every conclusion it reached. Refer to its earlier findings directly rather than restating them. This works whether the agent is still running (the prompt is queued), has already finished (idle — it starts a new turn), or has gone dormant after a period of inactivity (parked — it is rebuilt from its saved history and starts a new turn). **A finished or dormant subagent is not gone — address it here instead of spawning a new one** for related work; a fresh subagent starts from a handoff summary and has to rediscover context the original already has.

Parameters:
- agent_id (optional): Background agent ID (e.g. "a-abc123") or name (e.g. "researcher-xyz") to continue with a follow-up prompt.
- mailbox_id (optional): Mailbox identifier to deliver to; defaults to the current task graph mailbox from context.
- message (required): Content to enqueue.
- task_id (optional): Target task ID. Omit to broadcast to all tasks in the mailbox.

Targeting precedence:
1. `agent_id` set → targets that background agent; `mailbox_id`/`task_id` ignored.
2. `mailbox_id` + `task_id` → that task in that mailbox.
3. `mailbox_id` only → broadcast in that mailbox.
4. Neither `agent_id` nor `mailbox_id` → the current delegation mailbox from context.
5. `task_id` only → that task in the context default mailbox.

Notes:
- Prefer `agent_id` for resumable background agents and foreground subagents (running, idle, or dormant/parked), `mailbox_id` for task graph coordination.
- Delivery is best-effort while the mailbox is active; if an agent is still running the message is queued for its next turn.
- Unknown IDs return an error, as does an agent that failed/was canceled (aborted — spawn a new one instead) or was stopped. The subagent should use `yield` to submit its complete result before finishing.
- Continuing one agent beats spawning a fresh one for related work: a new subagent starts from a handoff summary, while a follow-up keeps the original's full context — this applies equally to a dormant (parked) subagent, not just a running or freshly-idle one.
