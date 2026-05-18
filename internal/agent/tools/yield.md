Submits the complete result of the current task to the parent agent.

<usage>
- Call exactly once at the end of the task, after all work is done
- Include the full result text in `data` — do not truncate or summarize
- Set `status` to reflect the task outcome
</usage>

<when_to_use>
- Use this instead of producing a long free-text response
- The parent agent will receive the full `data` without truncation
- Call `subagent_finish` after `yield` if you also need to report structured metadata (files touched, risks, etc.)
</when_to_use>

<parameters>
- `data` (required): The complete result text. Include all findings, analysis, and conclusions. Do not abbreviate.
- `status` (required): One of `completed`, `completed_with_warnings`, `failed`, `canceled`, `blocked`
</parameters>

<important>
- Do NOT call this tool multiple times for the same task
- Do NOT put a summary here — put the complete, unabridged result
- If the result is extremely long, focus on completeness over brevity
- After calling yield, call `subagent_finish` if you need to report structured metadata (files touched, risks, etc.). If no metadata is needed, you are done.
</important>
