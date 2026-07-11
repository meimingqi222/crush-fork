# Subagent Runtime (Current State)

> This document describes the **currently implemented** subagent runtime in
> Crush. It is the single source of truth; the historical
> `subagent-runtime-spec.md`, `subagent-runtime-prd.md`, and
> `subagent-system-redesign.md` (now in `docs/history/`) are outdated and must
> not be used as references. When the code and this doc disagree, the code
> wins—please update this doc.

## Overview

Crush delegates work to **subagents**: child `SessionAgent` instances spawned
by the coordinator when the parent LLM invokes the `agent` tool (or the batch
`agent` tool with a `tasks` array). Each subagent runs in its own session,
with its own model, tools, and permissions, derived from a **subagent
profile** and the parent's permission context.

The parent LLM orchestrates. There is **no DAG scheduler**: a flat parallel
executor (`internal/agent/subagent_executor.go`) runs the tasks a batch
contains, bounded by a concurrency semaphore. Dependency ordering, if any, is
the parent LLM's responsibility.

## Core types

- **`subAgentParams`** (`coordinator.go`): the parameters for a single
  subagent run—parent/child session IDs, tool-call IDs, prompt, title,
  subagent type, isolation, background flag, and flags controlling handoff
  review and structured-finish checks. Also carries `ExistingSessionID` /
  `AgentID` for warm-revive and `SessionSetup` / `OnProgress` callbacks.
- **`SubagentRuntimeContext`** (`subagent_runtime.go`): built per child run
  via `buildSubagentRuntimeContext`. Carries `ParentSessionID`,
  `ChildSessionID`, `ParentMessageID`, `ParentToolCallID`, `TaskID`, plus the
  resolved agent profile, permission context, isolation, and an event sink.
- **`subagentTask`**: `{Name, Description, Assignment}`—the per-task unit
  within a batch. `AgentTaskParams` (the tool input) has `name`,
  `description`, `assignment`, `subagent_type`, `isolation`. There are **no
  dependency fields**; the parent LLM sequences work itself.

## Child session lifecycle

1. The parent LLM calls the `agent` tool. The coordinator derives a child
   session ID via `CreateAgentToolSessionID(agentMessageID, toolCallID)` (or
   `toolCallID::taskName` for per-task children in a batch) and creates a task
   session with `CreateTaskSession`.
2. A `SubagentRuntimeContext` is built and injected into the context. A
   `WorkerIdentity` + `EscalationBridge` are also injected so the child's
   permission escalations route through the parent's authority.
3. The child `SessionAgent` runs with the enriched prompt
   (`buildSubagentHandoffSummary` prepends structured parent context).
4. On completion the result is stamped with subtask/reducer metadata
   (`withSubtaskToolResponseMetadata` / `reducer.Reduce`) carrying
   `ChildSessionID`, `TaskRef`, and `Status`.

### Status semantics

`ToolResultSubtaskStatus` values: `pending`, `in_progress`, `running`,
`completed`, `completed_with_warnings`, `failed`, `canceled`, `blocked`.
Cancellation propagates via `ctx.Done()`.

### Keep-alive / lifecycle manager

On a clean (non-failed) completion the child `SessionAgent` is **kept live**
for a warm-revive window via `subagentLifecycleManager` (`subagent_lifecycle.go`):
`Adopt(childID, agentID, TTL)` arms a 5-minute (`defaultSubagentAdoptTTL`)
timer; a follow-up `agent` call targeting the same `ExistingSessionID` can
warm-revive instead of rebuilding from disk. After the TTL the entry is
`Park`ed. `Revoke` cancels a pending timer early (used when re-spawning).

## Isolation

`SubagentIsolation` kinds:

- **`none`** (default): the subagent shares the parent's working directory.
- **`worktree`** (opt-in): the subagent runs in a git worktree.

`external_sandbox` / `managed_sandbox` are constants only and are not wired.

### Worktree merge-back

- **Patch mode** (default): the subagent's changes merge back into the parent
  working tree after it completes (`cleanupWorktreeIfNeeded` /
  `mergeBackWorktree`).
- **Branch mode** (opt-in via per-task isolation override): subagent commits
  are preserved on a branch rather than merged back.

### Batch auto-defaulting

`computeBatchIsolationDefault` auto-selects `worktree` when a batch has 2+
writer (non-read-only) tasks. Per-task isolation resolution priority
(`resolveTaskIsolation`): task override > batch default > agent static config
> global `DefaultIsolation`.

## Permissions

`DeriveSubagentPermissions` (`subagent_permissions.go`) computes the child's
allowed/denied tool set as a **deny-wins intersection**: profile ∩ parent ∩
available. `yield` is always included. Non-builtin tools are preserved. Read-only
profiles deny `edit`/`write`/`download`/`job`/`irc`/etc.

### Global denied tools

`globalSubagentDeniedTools` denies `resolve`, `request_user_input`, and `goal`
for subagents (these are parent-only coordination tools). The recursive
`agent` tool is denied for subagent-mode agents at registration time
(`tool_registration.go`), allowed only when the profile permits spawning
(`profile.CanSpawn`) and the parent allows it. Parent denies propagate to all
descendants.

## Yield protocol

The `yield` tool is exposed **only** in child sessions and is the structured
completion mechanism. It validates the terminal status, requires an `error`
field for failed/blocked outcomes, requires `data`/`payload` for success, and
validates `payload` against the profile's `OutputSchema` (one retry). Duplicate
yields are rejected. `yield` sets `StopTurn`. `ensureSubagentYield` runs the
missing-finish reminder loop (up to 2 prompts) then applies the
`MissingFinishPolicy`.

## Handoff review

`reviewHandoffText` runs **only** when the parent session is in Auto mode
(`PermissionModeAuto`) and `SkipHandoffReview` is false. It reviews both the
delegation prompt (pre-run) and the result (post-run), and may block the
handoff. `buildSubagentHandoffSummary` prepends structured parent context to
the subagent's prompt regardless of mode.

## Events

`SubagentEvent` (`subagent_runtime.go`) types: `started`, `progress`,
`finish`, `failed`, `canceled`, `blocked`. The `coordinatorSubagentEventSink`
translates these into timeline events. Note: `started`/`progress` are
currently published only terminally (`finish`/`failed`/`canceled`/`blocked`);
the live "started" signal the UI uses comes from the session service's
`CreatedEvent` → `timeline.ChildSessionStartedEvent` (`app/timeline.go`).

## Configuration

Subagent profiles are agent definitions in config. Relevant fields:
`AllowedTools`, `Memory`, `Isolation`, `Background`, `CanSpawn`, `OutputSchema`,
plus the `SubagentRuntimeConfig` (`MaxConcurrency`, default 4, consumed by the
executor semaphore).

## UI mapping (task → child session)

The UI resolves a task to its child session through (in priority order):

1. **Reducer metadata** (`childSessionIDForTaskRef`): scans the parent's tool
   results for `ToolResultReducer.ChildSessions[].SessionID` matching the
   task ref. Authoritative for completed subagents.
2. **Derived ID lookup** (`CreateAgentToolSessionID(msgID, toolCallID)`): the
   canonical child ID is derived from the parent message + tool call.
3. **Legacy prefix scan**: `childID + "::"` prefix match against
   `ListChildren`. Needed for running subagents whose reducer metadata has
   not been written yet; logs `slog.Debug("child session resolved via legacy
   prefix scan")` and is scheduled for removal once spawn-time metadata
   lands.

Subagent navigation keys: `]`/`l` open a child session (falls back to the
latest running child when nothing is selected), `[`/`h` return to the parent,
`ctrl+↑/↓` cycle siblings. The subagent footer shows role, `(n of N)` index,
token/context usage, cost, and navigation hints.
