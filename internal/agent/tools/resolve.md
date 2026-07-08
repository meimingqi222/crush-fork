Use this tool when the plan is complete, written to the active plan file, and ready for user review.

Rules:
- Only use this tool in Plan Mode.
- Before calling this tool, the active plan file MUST contain the final, decision-complete plan.
- Do not use this tool to ask free-form approval questions in text.
- If requirements or implementation details are still unresolved, use `request_user_input` instead of `resolve`.
- Call this tool with `action: "apply"`, a short `reason`, and `extra.title` set to a short kebab-case slug naming this task.

The UI will handle the approval flow after this tool succeeds.
