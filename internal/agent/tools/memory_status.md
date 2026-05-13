Shows the current status of the memory engine pipeline, including event store
health, extraction and consolidation pipeline state, materialized view
statuses, and background job information.

<usage>
- Call without parameters to see the full pipeline status.
- Use `view_name` to inspect a specific materialized view.
</usage>

<notes>
- Memory status is read-only. It does not modify any state.
- The event store status indicates whether the SQLite-backed event log is
  operational, disabled, or unavailable.
- Degraded mode is displayed when the background model is unavailable and
  the pipeline has paused extraction/consolidation.
- Materialized views show per-view watermark tracking for incremental rebuild.
</notes>
