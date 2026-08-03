[CRITICAL WARNING]
Do NOT call this tool unless you have actually spawned background subagents using the `agent` tool first (`run_in_background: true`). It CANNOT be used to communicate with the user — write your response in the text body instead.

Sends a control message to a running task graph, or gives a background agent more work.

A follow-up sent with `agent_id` continues that agent's existing conversation: it keeps every file it read and every conclusion it reached. Refer to its earlier findings directly rather than restating them. This works whether the agent is still running (the prompt is queued) or has already finished (it starts a new turn).

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
- Prefer `agent_id` for resumable background agents, `mailbox_id` for task graph coordination.
- Delivery is best-effort while the mailbox is active; if an agent is still running the message is queued for its next turn.
- Unknown IDs return an error, as does an agent that was stopped or whose parent session ended. The subagent should use `yield` to submit its complete result before finishing.
- Continuing one agent beats spawning a fresh one for related work: a new subagent starts from a handoff summary, while a follow-up keeps the original's full context.
