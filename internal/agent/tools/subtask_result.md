Fetches the complete assistant response from a child session created by a previous Agent tool call.

When an Agent tool returns a truncated response with a `task_ref` such as `0-review` or `subtask://0-review`, use that reference to retrieve the full output. Prefer `task_ref` over long child session IDs when it is available.

Parameters:
- task_ref (optional): Short task reference from an earlier Agent tool result, such as `0-review` or `subtask://0-review`
- session_id (optional): The actual child session ID from an earlier Agent tool result. Usually omit this and the tool will use the most recent child session in the current conversation.
- agent_id (optional): The agent ID from a background agent (alternative to session_id)
- offset (optional): Character offset to start from for pagination (0-based)
- limit (optional): Maximum characters to return (default 80000, max 200000)

Usage notes:
- Use this when you need more detail from a subagent's work than the summary provided in the Agent tool response
- Prefer `task_ref` when the Agent result includes one; it is stable and short
- Do not pass the literal placeholder text `"messageID$$toolCallID"`; that is only the session ID format, not a real value
- The response may still be truncated if very long; use offset/limit to paginate
- This retrieves the final assistant response and falls back to structured finish metadata when no assistant text is available
- For background agents: if the agent is still running, returns "still running" message
- Either `agent_id`, `task_ref`, or `session_id` must be provided or inferable from the current conversation
