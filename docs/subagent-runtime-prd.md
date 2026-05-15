# Subagent Runtime Redesign PRD

## Status

Draft for implementation planning. The P0 dependency release fix (only
`completed` releases dependents) and recursive `agent` tool denial for subagents
at registration time are already correctly implemented and are not pending work.

## Summary

Crush already has a functional `agent` tool, a `tasks[]` DAG scheduler
(`runTaskGraphDirect`), child session creation, background launch, mailbox
messaging, escalation bridges, worker identity injection, and a `subtask_result`
retrieval tool. The status enum, dependency release rule, explore agent tool
shape, and `subagent` mode gating on the `agent` tool are all correct in the
current codebase.

What does not yet exist is the cohesive layer that ties these pieces together
into a verifiable subagent runtime: a structured `subagent_finish` completion
tool, `completed_with_warnings` and `blocked` lifecycle states, a
`SubagentRuntimeContext` envelope, deny-preserving permission derivation,
runtime `ShapeToolsForSubagent` filtering, side-effect-aware retry, safe
structured parent summaries, and a `SubagentEventSink` for lifecycle
forwarding.

This PRD defines the pending product work while being precise about what
already exists so that implementation effort is not duplicated.

The architectural target remains:

1. Keep TaskGraph as the DAG orchestration layer.
2. Add a first-class `SubagentRuntimeContext` around each child session.
3. Require structured subagent completion via `subagent_finish`.
4. Derive child permissions from parent context and child profile with deny
   precedence.
5. Enforce coordinator/worker/explore/review profiles at tool-exposure time
   through a runtime `ShapeToolsForSubagent` call.
6. Treat background, retry, cancellation, and partial results as explicit
   lifecycle states.
7. Pass safe structured summaries to the parent while preserving explicit raw
   retrieval through `subtask_result`.
8. Keep sandbox as a future capability; optionally support lightweight worktree
   isolation before OS-level sandboxing.

## Problem Statement

Seven reliability and safety gaps remain after the current foundation:

1. **Completion is ambiguous.** There is no `subagent_finish` tool. Parent code
   infers child success from raw `ToolResponse` content, artifact metadata, or
   absence of error. A child session may look complete in the UI while the
   parent task graph marks it failed, or the parent may mark it completed even
   though no explicit terminal result was produced. The `taskGraphNodeResultFromResponse`
   helper reads raw tool-response content; no structured finish record exists.

2. **`completed_with_warnings` and `blocked` are missing from the public status
   enum.** `ToolResultSubtaskStatus` in `internal/message/auto_mode.go` has
   `pending`, `in_progress`, `running`, `completed`, `failed`, and `canceled`
   but lacks `completed_with_warnings` (for a child that finished but with
   transport/tool cleanup warnings) and `blocked` (for a child that was
   prevented from starting by a policy or dependency-chain constraint). These
   are needed to correctly represent lifecycle states without misusing
   `completed` or `failed`.

3. **Child permissions are partially enforced but not formally derived.**
   Explore's tool shape is set via config `AllowedTools` and
   `BashToolOptions{DisableBackground: true}` at registration time. The `agent`
   tool is already denied for subagents at `tool_registration.go:47`. Worker
   identity and escalation bridge are injected at `coordinator.go:2988-2994`.
   However, there is no deny-preserving derivation algorithm that computes
   effective child permissions from the intersection of parent rules, parent
   session denies, global denies, and child profile — before the child session
   starts.

4. **Tool profiles are partly enforced at registration time but no runtime
   `ShapeToolsForSubagent` function exists.** The explore agent's lack of
   write/edit/download tools comes from its `AllowedTools` config list, not from
   a runtime filtering function applied to the constructed tool registry. If
   config changes or a new profile is introduced, there is no single function
   that enforces the profile shape. A runtime `ShapeToolsForSubagent` is needed
   as the authoritative enforcement point.

5. **Retry can duplicate side effects.** A failed write-capable child task may
   be retried by the TaskGraph retry logic after touching files, running
   commands, or spawning background work. No `SideEffectSummary` tracking
   exists. There is no `SubagentRetryPolicy` type and no `ShouldRetrySubagent`
   function that consults side-effect records before allowing retry.

6. **Result safety is too coarse.** The current parent-visible output is raw
   response text from `subAgentResponseText`, optionally reviewed by
   `reviewHandoffText`. The `ToolResultReducer` in `auto_mode.go` has the right
   structure for structured summaries (`Artifacts`, `FilesTouched`, `PatchPlan`,
   `TestResults`, `FollowupQuestions`, `Risks`), but the reducer is populated
   from ad-hoc metadata rather than from a verified structured finish record.
   A safe reducer that is seeded from `subagent_finish` metadata and falls back
   to sanitized text does not yet exist.

7. **Approval and event routing: `EscalationBridge` exists but
   `SubagentRuntimeContext` is not yet an explicit envelope.** The pieces are
   present — `EscalationBridge`, `WorkerIdentity`, `WithWorkerIdentity`,
   `WithEscalationBridge` — but they are assembled ad-hoc in
   `runSubAgentDirect`. There is no `SubagentRuntimeContext` struct that owns
   child execution identity, tool profile, permissions, approval authority,
   retry policy, result contract, and event sink in one place. Without this
   envelope, future worktree, sandbox, or remote execution cannot be added
   without invasive coordinator changes.

## Goals

### Product Goals

- Make subagent execution reliable enough that the parent agent can reason about
  child status without guessing.
- Let users run parallel research, implementation, and verification workstreams
  with unambiguous status and recoverable results.
- Prevent subagents from bypassing parent restrictions or profile constraints
  through a formal deny-preserving derivation rule.
- Preserve useful child summaries in parent context without injecting raw
  untrusted child output into the parent model.
- Support background agents with clear `launched`, `running`, and terminal
  states.
- Make future isolation features, especially worktree and sandbox, pluggable
  rather than invasive.

### Engineering Goals

- Keep `internal/agent/taskgraph` as the graph validation and scheduling model.
- Keep `runTaskGraphDirect` as the main execution entry while moving child-agent
  concerns into focused runtime helpers.
- Introduce explicit runtime structs for child session identity, permissions,
  tools, lifecycle, result contract, retry, and event forwarding.
- Add tests for status semantics, structured completion, permission derivation,
  and retry safety.
- Maintain backward compatibility for existing `agent` tool calls while making
  structured completion the preferred path for built-in agents.

## Non-Goals

- Redesigning Auto Mode permission policy.
- Replacing TaskGraph with a flat task runner.
- Implementing OS-level sandboxing in the first phase.
- Requiring every subagent to run in a separate git worktree immediately.
- Requiring LLM review for every subagent result.
- Removing `subtask_result`; it remains the explicit raw retrieval tool.
- Renaming or relocating `internal/message/auto_mode.go` types in this phase.

## Users and Use Cases

### Primary Users

- Developers delegating research to `explore` subagents.
- Developers splitting implementation work across independent `general`
  subagents.
- Developers using background agents for long-running analysis or verification.
- Future agent/profile authors defining custom subagents.

### Core Use Cases

1. **Parallel read-only research.** A coordinator launches multiple `explore`
   workers and receives structured findings, evidence, risks, and confidence via
   `subagent_finish`.
2. **Parallel implementation.** A coordinator launches disjoint `general`
   workers with explicit scope, receives files touched and tests run, and avoids
   unsafe retries after side effects.
3. **Background execution.** A child is launched in background, parent receives
   a non-terminal `running` status, and later polls or resumes through
   `subtask_result`.
4. **Review and verification.** A `review` subagent reads diffs and reports
   structured findings without write tools.
5. **Approval forwarding.** If a child needs a sensitive action, the request is
   routed through the parent authority rather than being locally decided by the
   child.
6. **Future isolated execution.** The same runtime envelope can later select
   `none`, `worktree`, `external_sandbox`, or `managed_sandbox` capabilities.

## Current Project Baseline

The following are already correctly implemented and must not be re-implemented:

- **`agent` tool recursive denial:** `tool_registration.go:47` checks
  `config.NormalizeAgentMode(agent.Mode) != config.AgentModeSubagent` before
  registering the `agent` tool. Subagents do not receive the `agent` tool.
- **Dependency release rule:** `coordinator.go:2321` releases dependents only
  when `dependencyResult.Status != message.ToolResultSubtaskStatusCompleted` is
  false — i.e., only `completed` unlocks dependents. Background `running` tasks
  correctly do not release their dependents.
- **Background `running` status:** Background tasks are finalized with
  `ToolResultSubtaskStatusRunning` and do not release dependents.
- **`subtask_result` tool:** `internal/agent/tools/subtask_result.go` handles
  session-based lookup, background agent lookup via
  `toolruntime.BackgroundAgentLookupFromContext`, inference of latest child
  session, and character-level pagination with offset/limit.
- **Worker identity injection:** `coordinator.go:2988` creates a
  `permission.WorkerIdentity` and injects it via `permission.WithWorkerIdentity`
  and `permission.WithEscalationBridge` into the child execution context.
- **Explore agent tool shape:** `tool_registration.go:80-89` sets
  `BashToolOptions{DisableBackground: true}` for the explore agent and relies on
  the agent's `AllowedTools` config list to exclude write/edit/download.
- **`ToolResultSubtaskStatus` values:** `pending`, `in_progress`, `running`,
  `completed`, `failed`, `canceled` exist in `internal/message/auto_mode.go`.
- **`ToolResultReducer`, `ToolResultReducerChildSession`,
  `ToolResultSubtaskResult`:** All three types are defined in
  `internal/message/auto_mode.go` with full serialization helpers.
- **`EscalationBridge`:** Exists as a coordinator field (`c.escalationBridge`)
  and as a type in `internal/permission`, with `NewEscalationBridge()` called
  during coordinator construction.
- **`buildDelegationPromptPrefix`:** `internal/agent/delegation.go:136` injects
  the coordinator prompt prefix only for non-subagent sessions that have the
  `agent` tool, cleanly separating coordinator and subagent prompt contexts.
- **`isSubAgent bool` flag:** Threaded through `buildAgent`,
  `buildAgentModels`, `buildProvider`, HTTP client construction, and persistence
  decisions throughout `coordinator.go`.

The following were previously in `internal/autopermission/service.go` as
candidates for subagent permission derivation but have been removed as dead code:
`safeNullRedirectPattern`, `isSafeReadOnlyBashSegment`,
`isSafeReadOnlyGitCommand`, `classifyPluginDecision`.

## Reference Design Inputs

### Codex

- Child/delegate execution inherits execution policy from the parent context.
- Approval requests are forwarded to the parent session rather than decided by
  the child.
- Delegated work uses filtered/forked context instead of arbitrary prompt
  concatenation.
- Parallel work is framed as independent sidecar work with disjoint write sets.

Crush implication: child runtime should carry parent authority, execution
policy, filtered context, and write-scope information.

### Claude Code

- Coordinator mode has restricted tools so the coordinator primarily delegates.
- Worker tool sets are shaped by role.
- Worktree isolation is a lightweight isolation option before a full sandbox.
- Forked worker contexts can preserve prompt-cache efficiency.

Crush implication: add runtime-enforced `coordinator`, `worker`, `explore`, and
`review` tool profiles, and treat worktree as an optional capability.

### opencode

- Subagent permissions are derived by copying parent session/agent deny rules
  into the child session.
- Agent definitions include `mode: subagent | primary | all` and permission
  rulesets.
- Task creation shapes the child session before the child starts.

Crush implication: implement deny-preserving permission derivation before child
session execution, not after tool calls arrive.

### oh-my-pi

- Subagents must finish through a structured `yield` tool.
- Missing `yield` triggers reminder loops before final warning/failure.
- Output schemas make parent consumption more reliable than natural-language
  final messages.

Crush implication: add a subagent-only finish tool and make TaskGraph prefer its
structured data over raw `ToolResponse` text.

## Proposed Product Design

### Product Pillars

1. **TaskGraph remains the scheduler.** Do not throw away the DAG design.
2. **Subagent runtime becomes explicit.** Every child run has a runtime
   envelope.
3. **Completion is structured.** The parent does not guess child success from
   raw text.
4. **Permission inheritance is deny-preserving.** Children cannot widen parent
   authority.
5. **Tool profiles are enforced at runtime.** Prompt rules are secondary
   defense.
6. **Lifecycle states are precise.** `running` is not `completed`.
   `completed_with_warnings` is not `failed`.
7. **Retry is side-effect aware.** Write tasks are not blindly replayed after
   touching files.
8. **Parent-visible output is safe by default.** Useful summaries are retained;
   raw transcripts are explicit retrieval only.

### SubagentRuntimeContext

Every child run should be created with a runtime envelope before execution
begins:

```text
SubagentRuntimeContext
  parent_session_id
  child_session_id
  parent_message_id
  parent_tool_call_id
  task_id
  task_description
  agent_profile        -- SubagentProfile
  tool_profile         -- SubagentToolProfile
  permissions          -- DerivedSubagentPermissions
  approval_authority   -- ApprovalAuthority
  workspace_policy     -- SubagentWorkspacePolicy
  isolation            -- SubagentIsolation
  retry                -- SubagentRetryPolicy
  result_contract      -- SubagentResultContract
  events               -- SubagentEventSink
```

The context owns child execution identity, is built before execution starts,
and must not be mutated in ways that widen child authority after execution
begins.

### Structured Completion

Add a subagent-only completion tool named `subagent_finish`. Expose it only to
child sessions. This is the preferred completion path for all built-in
subagents.

The completion payload must include:

- `status` — one of `SubagentTerminalStatus` (see Lifecycle Model below)
- `summary` — human-readable completion summary
- `files_touched` — workspace-relative paths mutated during the task
- `test_results` — test run summaries
- `artifacts` — referenced output files
- `patch_plan` — a list of applied/proposed changes for coordinator review
- `risks` — identified risks or caveats
- `followups` — questions or tasks for the coordinator
- `confidence` — qualitative confidence string
- `error` — required for `failed` and `blocked` statuses
- `data` — optional typed structured data for schema-validated profiles

Parent TaskGraph should prefer structured completion metadata over raw assistant
text or generic `ToolResponse.IsError` inference.

### Lifecycle Model

Subagent status must be explicit. The new statuses `completed_with_warnings`
and `blocked` must be added to the public `ToolResultSubtaskStatus` enum:

| Status | Meaning | Terminal | Satisfies Dependencies |
|---|---|---|---|
| `pending` | Not started | No | No |
| `in_progress` | Foreground child is running | No | No |
| `running` | Background child still executing | No | No |
| `completed` | Child produced successful structured result | Yes | Yes |
| `completed_with_warnings` | Child completed but transport/tool cleanup warned | Yes | Yes |
| `failed` | Child produced or encountered failure | Yes | No |
| `canceled` | Child was canceled | Yes | No |
| `blocked` | Policy/profile prevented execution | Yes | No |

The internal `SubagentRuntimeStatus` type in `subagent_runtime.go` may include
additional granularity (e.g., `launched`) without requiring corresponding public
enum changes.

### Permission and Tool Profiles

Built-in profiles must be enforced at runtime through `ShapeToolsForSubagent`:

| Profile | Purpose | Tool Shape |
|---|---|---|
| `coordinator` | Orchestrate workers | agent/task_stop/send_message/subtask_result + safe reads |
| `general` | Implement scoped tasks | read/write/edit/bash under parent-derived permissions |
| `explore` | Gather evidence | read-only tools, no build/test/write/package-manager commands |
| `review` | Analyze code/diffs | read-only tools, no writes |
| `guardian` | Future approval reviewer | minimal read context, strict structured output |

The `explore` profile must be technically read-only even if the prompt is
ignored. The current config-based `AllowedTools` enforcement remains, and P2
adds `ShapeToolsForSubagent` as the authoritative runtime filter on top.
`general` profiles should not recursively spawn agents unless the
parent/profile explicitly allows it.

### Child Permission Derivation

Effective child permission must be computed before child execution, using
deny-wins semantics:

```text
effective_child = child_profile_allows
                ∩ parent_session_allows
                ∩ parent_agent_allows
                ∩ runtime_capabilities
                − parent_session_denies
                − parent_agent_denies
                − global_denies
                − profile_denies
```

Deny always wins. This prevents plan/review/read-only constraints, external
path restrictions, and parent agent restrictions from being bypassed by
spawning a child agent.

### Approval and Event Forwarding

This PRD does not redesign Auto Mode policy, but subagents must route approval
and runtime events correctly:

- Child approval requests use the parent authority session identifier.
- Approval prompts include child task ID and child session ID.
- Child sessions cannot approve their own privilege elevation.
- Child progress, finish, cancellation, and failure events flow through a parent
  `SubagentEventSink`.
- The parent can display or summarize child progress without reading raw child
  transcripts.

### Safe Parent Summaries

The parent should receive a safe structured summary by default:

- status, child session ID
- summary (from `subagent_finish`)
- files touched, tests run, artifacts, risks, follow-ups, confidence

Raw child transcript and raw tool output remain available through `subtask_result`
explicit retrieval. They are never injected automatically into parent reasoning
context.

### Retry Policy

Retries must consider side effects:

- `explore` and `review` can retry transient errors freely (no write side
  effects possible).
- `general` can retry before any side effects are recorded, or inside an
  isolated workspace.
- A write-capable task that touched files must not be automatically retried in
  the same workspace without isolation or idempotency guarantee.
- Background task retries must preserve visible attempt history.
- Retry summary must include attempt count and prior error reasons.

### Isolation Position

Sandbox is not required for P0–P3. The runtime envelope should expose an
isolation capability field so future modes can be added without invasive
changes:

- `none` (P0–P3 default)
- `worktree` (P4)
- `external_sandbox` (future)
- `managed_sandbox` (future)

Worktree isolation is the first useful extension and directly helps write-task
retry safety.

## User Experience

### Agent Tool Result

A foreground multi-task call should return a structured summary:

- task ID, description, status, child session ID
- summary (from `subagent_finish`)
- files touched, tests run
- risks and follow-ups
- how to retrieve full result if needed

A background task should return:

- `running` status
- child session ID or background agent ID
- mailbox ID when applicable
- polling instructions through `subtask_result`

### Error Messages

Failures should be precise and actionable:

- missing structured completion (missing-finish policy applied)
- child policy blocked
- dependency failed or was canceled
- child ran but result extraction failed
- retry suppressed due to side effects
- parent approval denied

### Configuration Surface

Subagent-specific configuration should live under a `subagents` (or
`subagent_runtime`) key, separate from Auto Mode policy:

```json
{
  "subagents": {
    "structured_completion_required": true,
    "missing_finish_policy": "retry_then_warn",
    "default_retry_policy": "read_only_only",
    "max_concurrency": 4,
    "allow_recursive_agents": false,
    "default_isolation": "none",
    "safe_summary": true
  }
}
```

Agent profile definitions may also expose:

```json
{
  "agents": {
    "explore": {
      "mode": "subagent",
      "profile": "explore",
      "tools": ["view", "grep", "glob", "lsp_*"],
      "can_spawn": false,
      "result_schema": "subagent_summary"
    }
  }
}
```

## Success Metrics

### Reliability

- Parent task graph status matches actual child lifecycle.
- Missing child completion is never silently treated as success.
- Background `running` is never counted as completed and never releases
  dependents.
- Dependency release only happens after terminal success (`completed` or
  `completed_with_warnings`).
- `subtask_result` can retrieve full output for every completed child session.

### Safety

- Explore/review profiles cannot mutate files through exposed tools.
- Child agents cannot widen parent permissions.
- Parent denial and approval decisions propagate to all descendants.
- Raw child output is not automatically injected into parent context.
- Write-task retries do not duplicate side effects by default.

### Performance

- Existing ready-queue DAG scheduling remains intact.
- Structured summaries avoid extra parent calls to fetch normal child results.
- Runtime permission/tool shaping is computed before execution and does not
  require LLM calls.
- No LLM call is required only to determine whether a child completed.

## Rollout Plan

### P0: Structured Completion and Status Fixes

**Already correct — no work needed:**
- Dependency release logic at `coordinator.go:2321`: only `completed` releases
  dependents. Background `running` does not.
- `agent` tool recursive denial at `tool_registration.go:47`.
- `ToolResultSubtaskStatus` base enum values.

**Remaining work:**

1. Add `internal/agent/tools/subagent_finish.go` and
   `internal/agent/tools/subagent_finish.md`.
   - Define `SubagentFinishParams` with all fields.
   - Define `SubagentTerminalStatus` values: `completed`,
     `completed_with_warnings`, `failed`, `canceled`, `blocked`.
   - Register tool only in child sessions (gate by `isSubAgent`).
   - Reject non-terminal status values, require `error` for `failed`/`blocked`.
2. Add `ToolResultSubtaskStatusCompletedWithWarnings` and
   `ToolResultSubtaskStatusBlocked` to `internal/message/auto_mode.go`.
3. Add `ToolResultSubagentFinish` metadata type and helpers
   (`ParseToolResultSubagentFinish`, `SubagentFinish()`, `WithSubagentFinish()`)
   to `internal/message/auto_mode.go` or a new `internal/message/subagent.go`.
4. In `coordinator.go`, after `params.Agent.Run(...)` returns for a foreground
   child, extract `subagent_finish` metadata before falling back to
   `subAgentResponseText`.
5. Add missing-finish reminder loop: if no finish metadata found and contract
   allows fallback, send a short reminder prompt and allow at most two retries
   before applying `MissingFinishPolicy`.
6. Update `taskGraphNodeResultFromResponse` to prefer finish metadata when
   present.

**Verification:**

```sh
go test ./internal/agent ./internal/message
```

---

### P1: SubagentRuntimeContext

**Work:**

1. Add `internal/agent/subagent_runtime.go` with:
   - `SubagentRuntimeContext` struct
   - `SubagentProfile`, `SubagentToolProfile`
   - `SubagentWorkspacePolicy`, `SubagentResultContract`, `MissingFinishPolicy`
   - `SubagentIsolation` with `SubagentIsolationKind` (only `none` needed now)
   - `SubagentEventSink` interface
   - `ApprovalAuthority` explicit struct
2. Build `SubagentRuntimeContext` for every child task in
   `runSubAgentDirect` before calling `buildAgent`.
3. Move child session creation inputs into the runtime context rather than
   threading them as individual parameters.
4. Add event sink adapter that wraps existing pubsub/timeline paths.
5. Preserve the public `agent` tool schema.

**Files:**
- `internal/agent/subagent_runtime.go` (new)
- `internal/agent/coordinator.go`
- `internal/agent/agent_tool.go`
- tests in `internal/agent`

**Verification:**

```sh
go test ./internal/agent
```

---

### P2: Permission Derivation and Tool Shaping

**Context:** Explore's `AllowedTools` and `DisableBackground` shape already
set via config and `tool_registration.go`. P2 adds the formal derivation
algorithm and a runtime `ShapeToolsForSubagent` function on top.

**Work:**

1. Add `internal/agent/subagent_permissions.go` with:
   - `ParentPermissionContext` struct
   - `DerivedSubagentPermissions` struct
   - `DeriveSubagentPermissions` function using deny-wins intersection
2. Add `internal/agent/subagent_tools.go` with:
   - `ShapeToolsForSubagent(all []tools.BaseTool, profile SubagentToolProfile) []tools.BaseTool`
   - Built-in profile defaults for `coordinator`, `explore`, `general`,
     `review`
3. Wire `ShapeToolsForSubagent` into tool registry construction.
4. Make `explore` and `review` hard read-only through derived permissions.
6. Add parent-deny propagation tests and read-only subagent tests.

**Files:**
- `internal/agent/subagent_permissions.go` (new)
- `internal/agent/subagent_tools.go` (new)
- `internal/autopermission/service.go`
- tests in `internal/agent`, `internal/permission`, `internal/config`

**Verification:**

```sh
go test ./internal/agent ./internal/permission ./internal/config
```

---

### P3: Safe Summaries, Retry Safety, and Event Forwarding

**Work:**

1. Build safe reducer from structured finish metadata in `coordinator.go`.
   Keep raw retrieval through `subtask_result` unchanged.
2. Add `SubagentRetryPolicy` and `ShouldRetrySubagent` function in
   `subagent_runtime.go`.
3. Add `SideEffectSummary` type and population from file tracker, finish
   metadata, and approval bridge records.
4. Integrate side-effect-aware retry decisions into the TaskGraph execution loop.
5. Route child approvals through parent authority session with task metadata
   in approval request payload.
6. Emit `SubagentEventSink` events for start, progress, finish, canceled,
   and failed lifecycle transitions.

**Files:**
- `internal/agent/coordinator.go`
- `internal/agent/subagent_runtime.go`
- `internal/autopermission/service.go`
- tests in `internal/agent`, `internal/autopermission`, `internal/message`

**Verification:**

```sh
go test ./internal/agent ./internal/autopermission ./internal/message
```

---

### P4: Isolation Capabilities

**Work:**

1. Add `SubagentIsolation` provider interface in
   `internal/agent/subagent_isolation.go`.
2. Implement `none` provider (no-op).
3. Optionally implement `worktree` provider using a lightweight git worktree.
4. Use isolation status in `ShouldRetrySubagent` decisions.
5. Leave `managed_sandbox` as a future provider behind the same interface.

**Files:**
- `internal/agent/subagent_isolation.go` (new)
- optional worktree implementation files

**Verification:**

```sh
go test ./internal/agent ./...
```

## Acceptance Criteria

- PRD and SPEC focus on subagent runtime, not Auto Mode redesign.
- TaskGraph remains the orchestration layer.
- `subagent_finish` tool is specified and implemented.
- `completed_with_warnings` and `blocked` are added to the public status enum.
- Structured child completion is preferred over raw text inference.
- Parent/child status semantics are unambiguous.
- Dependency release rule is documented as already correct and is not
  regressed.
- Background `running` status does not release dependents — remains correct.
- Permission inheritance and tool shaping are specified and implemented with
  deny-wins semantics.
- Retry behavior is side-effect aware.
- Safe parent summaries are built from `subagent_finish` metadata.
- Raw output retrieval through `subtask_result` is preserved.
- Worktree and sandbox are future capabilities, not required for P0–P3.
- Unused autopermission helpers are wired or cleaned up in P2.
