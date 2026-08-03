Creates and manages a structured task list for tracking progress when explicit, manual task tracking is useful.

<usage>
- Backward compatible: pass `todos` with no `action` to replace the full task list.
- `action` is one of: `replace`, `create`, `update`, `delete`, `get`, `list`.
- `create` appends new tasks; `update` modifies exactly one task by ID; `delete` and `get` require `id`.
- Tasks support stable IDs plus `progress`, `created_at`, `updated_at`, `started_at`, `completed_at`.
</usage>

<when_to_use>
Use when: the user explicitly requests todo management, a long-running task needs a persistent checklist, or you are already using the tool and need accurate status updates.
</when_to_use>

<when_not_to_use>
Skip when: single/trivial tasks, purely conversational requests, or independent work better delegated to subagents.
</when_not_to_use>

<task_states>
- `pending` → `in_progress` → `completed`. Keep exactly ONE task in_progress at a time.
- Each task requires two forms: `content` (imperative, e.g. "Run tests") and `active_form` (present continuous, e.g. "Running tests").
- Mark tasks complete immediately after finishing; remove tasks no longer relevant; use `progress` for partial completion.
- ONLY mark completed when fully accomplished: not with failing tests, partial implementation, or unresolved errors. If blocked, keep in_progress and add a task describing what to resolve.
- Break complex tasks into specific, actionable items with clear names.
</task_states>

<output_behavior>
NEVER print or list todos in your response text — the user sees the todo list in real-time in the UI.
</output_behavior>

<tips>
- Use `list` or `get` to inspect current task IDs before single-task updates/deletes.
- Structured metadata is returned in the response for programmatic consumers.
</tips>
