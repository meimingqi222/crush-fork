Queries structured knowledge-graph triples from the memory store.

Triple query searches for subject-predicate-object facts extracted from
conversations. Unlike recall (which searches free-form text), triple_query
enables precise structured lookups.

<usage>
- `subject` filters by subject (exact match). Omit to match all subjects.
- `predicate` filters by predicate (exact match). Omit to match all predicates.
- `limit` caps the number of results (default 50).
- At least one of subject or predicate should be provided for meaningful results.
</usage>

<notes>
- Triple query is read-only. It does not modify any memory state.
- Triples are automatically extracted during memory extraction when
  structured relationships are detected in conversations.
- Common predicates include: "uses", "prefers", "depends_on", "belongs_to",
  "located_in", "version_of", etc.
- Use triple_query to find specific factual relationships like
  "what framework does project X use?" or "what does user prefer for Y?"
</notes>
