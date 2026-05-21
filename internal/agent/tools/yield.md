Submits the complete result of the current task to the parent agent and terminates execution.

<usage>
- Call exactly once at the end of the task, after all work is done.
- Specify the `status` to reflect the task outcome.
- Provide either the full result text in `data`, or detailed error in `error`, or structured payload in `payload`.
</usage>

<when_to_use>
- Use this as the sole method to complete your task and submit findings back to the parent agent.
- Calling this tool will automatically terminate the subagent run loop.
</when_to_use>

<parameters>
- `status` (required): One of `completed`, `completed_with_warnings`, `failed`, `canceled`, `blocked`.
- `data` (optional): The complete result text. Include all findings, analysis, and conclusions. Required unless status is failed or blocked.
- `error` (optional): The error details. Required if status is failed or blocked.
- `payload` (optional): Optional structured JSON payload conforming to the expected schema if OutputSchema is defined.
</parameters>

<important>
- Do NOT call this tool multiple times for the same task.
- Once called, the agent execution terminates immediately. Make sure all your changes and analyses are fully completed first.
</important>
