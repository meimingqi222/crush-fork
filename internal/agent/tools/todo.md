Manages the active goal's task list. Tasks are subtasks of the session's
current goal, separate from session.Todos which track subagent progress.

Operations:
- `init`: Create the initial task list. Only allowed when the goal has no
tasks and the session has an active goal. `items` is a list of task strings
and must be non-empty.
- `add`: Append one or more new tasks to the existing list. Duplicate content
is not allowed.
- `start`: Mark a task as `in_progress` by `index`. Any other in-progress
task is moved back to `pending`.
- `done`: Mark a task as `completed` by `index`. Provide `evidence` to
record how you verified the result (required to complete the goal).
Calling `done` on an already-completed task is idempotent and can be used
to add or update evidence.
- `block`: Mark a task as `blocked` by `index` with a `reason`.
- `unblock`: Restore a `blocked` task to `pending` by `index`.
- `drop`: Mark a task as `dropped` by `index` with a `reason`. Dropped tasks
do not block goal completion but count against stall detection. You cannot
drop a completed task.
- `view`: Read the current task list.

Use `init` once after creating a goal to break the objective into concrete
steps. Use `start` to focus on one task at a time, and `done` with evidence
when it is truly complete.

When called from a subagent: `view` is read-only (may return stale state if
queued updates have not yet been applied by the parent); `start` and `done`
submit a queued update that the parent agent applies on its next turn — the
response says "task update submitted", not "done", because the change is not
yet in effect. `init`, `add`, `block`, `unblock`, and `drop` are not allowed
from subagents.
