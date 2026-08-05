Manages session goals for autonomous multi-turn execution.

Operations:
- `create`: Start a new goal with an objective and optional token budget
- `replace`: Update the current goal's objective and optional budget while
  preserving tokens used and elapsed time. If no goal exists, `replace`
  behaves like `create`.
- `get`: Query current goal state, progress, and budget
- `complete`: Mark the goal as done (only after verification)
- `pause`: Temporarily pause the goal
- `resume`: Re-activate a paused goal
- `drop`: Discard the current goal
- `budget`: Adjust the token budget

Use `create` when the user sets an objective. Use `replace` when the user
wants to adjust the objective without losing progress statistics. Use `todo`
to track subtasks: `init` the task list, `start` one task, mark it `done`
with `evidence`, and `drop` impossible tasks with a `reason`. Use `complete`
only after every task is done or dropped — the completion gate requires
evidence for completed tasks, reasons for dropped tasks, and a dropped ratio
no higher than 50%. Never call complete just because the budget is low or a
turn is ending. Use `get` to check remaining budget and progress.

When called from a subagent, only `get` is allowed. Subagents cannot create,
replace, complete, pause, resume, drop, or budget the parent goal.
