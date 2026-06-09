Inspect Crush instance status and recent logs.

## Actions

| action | Description |
|---|---|
| `info` | Show current Crush instance status: paths, models, providers, LSP, MCP, memory, permissions, options |
| `logs` | Show recent log entries from the Crush log file |

## Parameters

- `action` (required): One of `info`, `logs`
- `lines` (optional, logs only): Number of recent log entries to return. Default 50, max 100.

## Usage

- Use `info` to understand the current Crush configuration and runtime state.
- Use `logs` to debug issues by examining recent log entries.
