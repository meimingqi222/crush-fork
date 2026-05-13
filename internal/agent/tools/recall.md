Reads materialized memory summaries and retrieves memory events.

Without a query, returns the full materialized summary block for the current
session (user + project summaries and working memory). With a query or
filters, performs a targeted retrieve across all memory events.

<usage>
- Omit `query` to get the full materialized summary for prompt injection.
- Provide `query` for targeted retrieval across memory events.
- `scope`, `kind` filter results by scope or kind.
- `limit` caps the number of returned events (default 20).
</usage>

<notes>
- The recall tool is read-only. It does not modify any memory state.
- Results are ordered by watermark (insertion order).
- Materialized summaries come from memory_summary.md (if available).
- Targeted retrieval queries the EventStore directly.
</notes>
