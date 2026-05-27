[CRITICAL WARNING]
Do NOT call this tool unless you have actually spawned subagents using the `agent` tool first and need to cancel them. This tool is ONLY for stopping running subagents in a mailbox. It CANNOT be used if no subagents are currently running.

Requests cancellation of a task graph task.

Use this tool to stop one task or all tasks in an active mailbox.

Parameters:
- task_id (optional): Specific task name/ID to stop (e.g., 'linter', 'compiler'). Omit to request stop for all active sub-tasks in current session.
- reason (optional): Human-readable cancellation reason.

Notes:
- Stop is cooperative and applied by the task graph runtime.
- Unknown mailbox or task IDs return an error response.