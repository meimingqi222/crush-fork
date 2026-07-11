Queries the memory knowledge graph: traverse connected memories or look up
structured facts.

This tool has two modes over the same triple store:

- `mode: "path"` (default) traverses semantic edges (related_to, contradicts,
  refines, depends_on) outward from one or more seed IDs, up to a
  configurable number of hops. Returns both connected events and triples.
- `mode: "triples"` performs a direct subject/predicate/object lookup —
  precise structured search, unlike recall's free-form text search.

<usage>
- `mode="path"`: `seed_ids` is required (one or more memory event or triple
  IDs, found via the recall tool's output). `max_hops` controls traversal
  depth (default 2). `edge_types` filters which edges to follow (omit for
  all).
- `mode="triples"`: `subject` and/or `predicate` filter results (exact match,
  omit either for wildcard). `limit` caps results (default 50). Provide at
  least one of subject or predicate for meaningful results.
</usage>

<notes>
- Both modes are read-only; neither modifies memory state.
- Triples are extracted automatically during memory extraction when
  structured relationships are detected in conversations.
- Common predicates include: "uses", "prefers", "depends_on", "belongs_to",
  "located_in", "version_of", etc.
- Use `mode="path"` to discover contradictions (contradicts edges) or
  dependencies (depends_on edges) between memories.
- Use `mode="triples"` for specific factual lookups like "what framework
  does project X use?" or "what does the user prefer for Y?"
</notes>
