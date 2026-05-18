Marks a subagent session as finished with structured completion metadata.

Use this tool only from subagent sessions. Call it exactly once at the end of delegated work.

Parameters:
- status: Terminal subagent status (`completed`, `completed_with_warnings`, `failed`, `canceled`, or `blocked`)
- summary (optional): Brief completion summary. Can be empty if yield has already submitted the full result.
- artifacts: Output artifacts or references
- files_touched: Workspace-relative or absolute file paths changed by the task
- patch_plan: Applied or proposed change steps
- test_results: Verification results
- followups: Questions or next tasks for the coordinator
- risks: Risks or caveats discovered during execution
- next_actions: Suggested next coordinator actions
- confidence: Qualitative confidence label
- error: Required for `failed` and `blocked`
- data: Optional structured JSON payload

Rules:
- Do not pass non-terminal statuses.
- `summary` is optional when yield has already been called.
- Focus on structured metadata (files_touched, risks, test_results) rather than repeating the full result.
- `error` is required for `failed` and `blocked`.
- Only the first `subagent_finish` call is accepted; subsequent calls return an error.
