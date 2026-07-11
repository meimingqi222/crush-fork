Manually retains a memory event into the event-sourced memory log.

This is the only direct write path for manual memory capture. All other
memory writes go through the automatic extraction and consolidation pipeline.

<usage>
- Requires `scope`, `kind`, and `content` to create a memory event.
- `scope` determines the visibility: `session`, `project`, `user`, or `global`.
- `kind` classifies the memory: `preference`, `decision`, `procedure`, `pitfall`, `reference`, or `task_state`.
- `summary` is a short human-readable description (recommended).
- `importance` from 0.0 to 1.0 influences ranking. Defaults to 0.5.
- `tags` are optional labels for filtering and retrieval.
</usage>

<when_to_use>
Use retain when:
- The user explicitly states a preference or constraint that should be remembered.
- You discover a non-obvious project pattern or architecture decision.
- A workflow is confirmed to work and should be preserved across sessions.
- The user corrects your behavior in a way that should be remembered.
- Important context that would be costly to re-discover.

retain vs. CRUSH.md memory files: retain is for cross-session, non-repo-appropriate
knowledge (user preferences, environment quirks, decisions and their rationale).
CRUSH.md is for project-level conventions meant to be committed and read by humans
(build/test/lint commands, code style, project patterns). Pick one per fact --
never write the same fact to both.
</when_to_use>

<notes>
- Retained events flow through the extraction and consolidation pipeline to
  produce materialized views (memory_summary.md, MEMORY.md, skills/).
- This tool writes to crush's own data directory, not the user's workspace,
  so it runs without a permission prompt. Never retain secrets, API keys,
  passwords, or other credentials -- treat this as a durable, searchable
  store, not scratch space for sensitive data.
- Events are immutable once written. Use supersede semantics if an event
  should replace an earlier one.
</notes>
