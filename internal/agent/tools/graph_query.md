Queries the memory knowledge graph by traversing edges from seed IDs.

Starting from one or more seed memory IDs, follows semantic edges
(related_to, contradicts, refines, depends_on) up to a configurable
number of hops. Returns both connected events and structured triples.

<usage>
- `seed_ids` is required: provide one or more memory event or triple IDs.
- `max_hops` controls traversal depth (default 2). Increase for broader
  exploration, decrease for focused results.
- `edge_types` filters which edge types to follow. Omit to follow all.
</usage>

<notes>
- Graph query is read-only. It does not modify any memory state.
- Seed IDs can be found via the recall tool's output (each event shows its ID).
- Results include both events and triples reachable from the seeds.
- Use graph_query to discover contradictions (contradicts edges) or
  dependencies (depends_on edges) between memories.
</notes>
