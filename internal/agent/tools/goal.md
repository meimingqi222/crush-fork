Manages session goals for autonomous multi-turn execution.

Operations:
- `create`: Start a new goal with an objective and optional token budget
- `get`: Query current goal state, progress, and budget
- `complete`: Mark the goal as done (only after verification)
- `pause`: Temporarily pause the goal
- `resume`: Re-activate a paused goal
- `drop`: Discard the current goal
- `budget`: Adjust the token budget

Use `create` when the user sets an objective. Use `complete` only after
verifying the objective is fully met — never call complete just because the
budget is low or a turn is ending. Use `get` to check remaining budget and
progress.
