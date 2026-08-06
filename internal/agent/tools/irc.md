Send messages to or list other live agents via IRC.

This tool lets you communicate with other running agents in real time. Use it to coordinate work, ask quick questions, or share findings without waiting for the orchestrator to relay information.

## Operations

### `send` — Send a message to a peer or broadcast

Parameters:
- `to` (required): The agent ID of the recipient, or `"all"` to broadcast to every visible peer.
- `message` (required): The message body. Keep it concise and focused.
- `await_reply` (optional): Whether to wait for a reply. Omit it to get the default (`true` for DMs, `false` for broadcasts); pass `true` or `false` explicitly to override that default in either direction — for example, `await_reply: false` on a DM sends a fire-and-forget notification with no reply.
- `reply_to` (optional): The message ID you are replying to, if you're answering a message you received. Include it when responding to an inbound peer message.

**A completed peer can still be reached — do not respawn it.** A subagent that has finished (idle) or gone dormant (parked, shown in `irc list`/roster with a "message revives" note) is still addressable: sending it a message wakes it up for a real turn using its own history and tools. Prefer this over spawning a fresh subagent for related follow-up work — a new subagent starts cold with only a handoff summary, while messaging the original keeps everything it already knows.

**How a reply actually reaches you depends on what the recipient is doing:**
- **Idle or dormant (parked) peer**: your message wakes it for a real turn — its own inference model, full session history, and tools. If you set `await_reply` (or leave the DM default), you get that real reply back synchronously.
- **Busy peer**: your message is queued for its attention at the next safe point in its current turn — it does **not** interrupt whatever it's doing (no reply is delivered synchronously today; you may see a placeholder, best-effort acknowledgment instead of the real answer, or nothing back until it replies from its own turn later using `irc send` with `reply_to`).
- **Idle primary/orchestrator agent**: your message is queued for the user's next turn, not answered immediately — the primary agent's turns belong to the user, not to peers.

Do not rely on any synchronous acknowledgment you get back from a busy peer to confirm file changes, task completion, or the recipient's actual progress — verify those independently (e.g. by reading files or checking `irc list` status) when it matters. A real reply from an idle/dormant peer's actual turn is authoritative; anything else is best-effort.

### `list` — List currently visible peers

Returns all agents that are currently running, idle, or parked (dormant but revivable by a message — excluding yourself). Parked entries are annotated so you know addressing them triggers a revive rather than an instant delivery. Use this to discover who you can reach before sending a message.

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
