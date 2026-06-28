Manage background shell jobs started by the bash tool.

## Actions

| action | Description |
|---|---|
| `output` | Get the current output snapshot of a background job (non-blocking). |
| `wait` | Wait for the background job to complete, but no longer than `wait_timeout_ms`. Returns the current snapshot whether the job finished or not. |
| `kill` | Terminate a running background job. |

## Parameters

- `action` (required): One of `output`, `wait`, `kill`.
- `shell_id` (required): The ID of the background shell, returned by bash when `run_in_background=true`.
- `wait_timeout_ms` (optional, `wait` only): Maximum milliseconds to wait before returning the current snapshot. Default `300000` (5m). Clamped to `[1000, 1800000]` (1s..30m). `0` or negative uses the default.

## `wait` semantics

`wait` is **bounded**: it never blocks forever. If the job does not finish within the wait window, the tool returns the current output snapshot with `done=false` and `timed_out=true` — the job keeps running in the background. This prevents a stuck job (e.g. `tail -f`, a server waiting on stdin, or a hung process) from deadlocking the agent.

When `timed_out=true`:
- The job is still running. Inspect progress with `action=output`, wait again with `action=wait` (optionally with a larger `wait_timeout_ms`), or stop it with `action=kill`.
- The returned text includes any output produced so far.

Use a short `wait_timeout_ms` (e.g. `5000`) to poll a long-running job periodically; use a longer one when you genuinely need to block until completion.

## Usage

- Use `output` for a non-blocking snapshot of current progress.
- Use `wait` when you need the job to finish before proceeding; tune the window with `wait_timeout_ms`.
- Use `kill` to cancel a long-running or stuck job.
- Background jobs are started via the `bash` tool with `run_in_background: true`.
