Marks a subagent session as finished with structured completion metadata and triggers immediate loop termination.

Use this tool only from subagent sessions. Call it exactly once at the end of delegated work. This is the ONLY way for a subagent to properly complete its execution.

<parameters>
- `status` (required): Terminal subagent status. One of `completed`, `completed_with_warnings`, `failed`, `canceled`, or `blocked`.
- `summary` (required): Human-readable completion summary describing what was accomplished.
- `data` (optional): Structured JSON payload. If an OutputSchema is configured for this agent, the data field is validated against it. On the first validation failure you will receive an error and may retry. On the second failure the result is force-accepted.
- `error` (required if status is `failed` or `blocked`): Description of the failure or blocking condition.
- `files_touched` (optional): Workspace-relative or absolute file paths changed by the task.
- `artifacts` (optional): Output artifacts or references.
- `patch_plan` (optional): Applied or proposed change steps.
- `test_results` (optional): Verification results.
- `followups` (optional): Questions or next tasks for the coordinator.
- `risks` (optional): Risks or caveats discovered during execution.
- `next_actions` (optional): Suggested next coordinator actions.
- `confidence` (optional): Qualitative confidence label.
</parameters>

<behavior>
- Triggers immediate agent loop termination on success (StopTurn).
- Only the first `subagent_finish` call is accepted; subsequent calls return an error.
- If an OutputSchema is defined, the `data` field is validated against it:
  - First validation failure: returns an error message allowing one retry.
  - Second validation failure: force-accepts the result to avoid infinite loops.
</behavior>

<rules>
- Do not pass non-terminal statuses.
- `error` is required for `failed` and `blocked`.
- `data` must be valid JSON when provided.
- Call this tool exactly once at the end of your work.
</rules>
