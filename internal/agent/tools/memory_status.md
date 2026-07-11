Shows a one-line summary of the active memory backend: which backend is in
use, whether it is enabled, degraded, and rough activity counters.

<usage>
- Call without parameters. There is nothing to configure.
</usage>

<notes>
- Memory status is read-only. It does not modify any state.
- Degraded mode is reported when the backend has paused extraction or
  consolidation (e.g. the background model is unavailable, or a hindsight
  backend is missing its remote configuration).
- For deeper pipeline diagnostics (per-view watermarks, background job
  leases), use the Commands panel's Memory: Status entry instead — this
  tool is intentionally terse to keep it cheap to call from the model.
</notes>
