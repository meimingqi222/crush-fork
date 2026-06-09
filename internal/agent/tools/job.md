Manage background shell jobs started by the bash tool.

## Actions

| action | Description |
|---|---|
| `output` | Get the current output snapshot of a background job |
| `wait` | Block until the background job completes, then return full output |
| `kill` | Terminate a running background job |

## Parameters

- `action` (required): One of `output`, `wait`, `kill`
- `shell_id` (required): The ID of the background shell, returned by bash when `run_in_background=true`

## Usage

- Use `output` for a non-blocking snapshot of current progress.
- Use `wait` when you need the job to finish before proceeding.
- Use `kill` to cancel a long-running or stuck job.
- Background jobs are started via the `bash` tool with `run_in_background: true`.
