Send messages to or list other live agents via IRC.

This tool lets you communicate with other running agents in real time. Use it to coordinate work, ask quick questions, or share findings without waiting for the orchestrator to relay information.

## Operations

### `send` — Send a message to a peer or broadcast

Parameters:
- `to` (required): The agent ID of the recipient, or `"all"` to broadcast to every visible peer.
- `message` (required): The message body. Keep it concise and focused.
- `await_reply` (optional): Whether to wait for a reply. Defaults to `true` for DMs, `false` for broadcasts.

When `await_reply` is `true`, the recipient generates a brief reply based on its current context. The reply does not trigger any tool calls — it is a direct text response.

### `list` — List currently visible peers

Returns all agents that are currently running or idle (excluding yourself). Use this to discover who you can reach before sending a message.

## When to use IRC

- **Unexpected state**: You encounter a missing file, contradictory config, or ambiguity not mentioned in your assignment — DM `0-Main` (the orchestrator) for guidance.
- **Blocked by another agent**: A peer holds a file, branch, or resource you need — DM that peer directly.
- **Out-of-scope decision point**: You hit a genuine fork in the road that your assignment did not predetermine — ask the orchestrator.
- **Coordination opportunity**: You realize a peer's work would benefit from knowing about yours — broadcast or DM.

## When NOT to use IRC

- Do not use IRC to ask questions that your own tools (`read`, `grep`, `glob`) can answer.
- Do not send structured JSON status payloads — use plain prose.
- Do not ask "did you receive my message?" — one round-trip is sufficient.
- Do not use IRC for long-form content transfer. Reference files via paths, not by pasting contents.

## Etiquette

- Address peers by their exact ID (as returned by `list`).
- Use `"all"` to broadcast when you need to share information with everyone.
- Keep messages short and focused — one question per DM.
- Use file paths to reference files rather than pasting content.
- One round-trip is enough; do not follow up asking whether the message was received.
