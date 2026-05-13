Synthesizes across memory events to answer a question about past sessions,
decisions, or project history.

Reflect queries the EventStore across sessions and returns a formatted
synthesis. Unlike recall which returns materialized summaries, reflect
performs cross-memory analysis.

<usage>
- `query` is a natural language question about past memory (required).
- `scope`, `kind`, and `session_id` narrow the search space.
</usage>

<notes>
- Reflect is read-only. It does NOT write to long-term memory.
- Results are sorted by importance (highest first, top 10).
- If the memory engine is disabled or the retriever is not configured,
  reflect will return an error message.
</notes>
