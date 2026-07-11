# Plan Mode

Crush uses a session-level Plan Mode for drafting, reviewing, and approving
plans before implementation.

## What Changed

- Removed the inline `<proposed_plan>` block protocol from assistant messages.
- The plan is written to a dedicated plan file and surfaced through the
  `resolve` tool.
- The `PlanReview` dialog is opened from the plan file instead of parsing a
  block from the message text.

Planning and execution remain in the same session.

## Session-Level Collaboration Mode

Sessions persist a `collaboration_mode` value:

- `default`
- `plan`

Existing sessions migrate to `default`. New sessions also start in `default`.

The active mode is part of session state, flows through the existing session
pubsub path, and is rendered by the UI directly. Dialogs do not maintain a
hidden plan-state shadow copy.

## Entering and Exiting Plan Mode

The Commands panel exposes an explicit toggle:

- `Enter Plan Mode`
- `Exit Plan Mode`

When a session is in Plan Mode, the header also shows a `PLAN` indicator.

If the latest assistant response includes a `resolve` tool call with
`action: "apply"`, the UI opens the `PlanReview` dialog.

## Agent Behavior in Plan Mode

Plan Mode is enforced in two layers.

### Prompt Rules

When a session is in `plan` mode, the agent receives dedicated planning rules:

- Explore first.
- Ask clarifying questions only when they materially affect the plan.
- Do not implement changes.
- Finish by calling the `resolve` tool with `action: "apply"` once the plan is
  ready.

### Tool Gating

Plan Mode hard-gates tools instead of relying only on prompting.

Read-only and analysis tools remain available:

- `read`, `glob`, `grep`, `lsp`, `recall`, `reflect`, `memory_status`
- `agent` (read-only subagents)
- `bash` (the tool registration layer narrows it to git read-only commands and
  disables background jobs)
- `agentic_fetch` and `sourcegraph` (external research, read-only)
- `write` and `edit` (to draft the plan in the active plan file; they are
  restricted to the plan file path and use read-only/preview semantics unless
  `Writable` is explicitly set to `true`)

Mutating tools such as workspace-writing `download`, `retain`, and execution
paths that modify the repo are not exposed while planning.

## Structured Clarification via `request_user_input`

Plan Mode has one official structured clarification tool:

- `request_user_input`

This tool is used for high-impact product or implementation decisions that
cannot be resolved by reading the repo.

The tool accepts 1–3 structured questions. Each question includes:

- a short `header`
- a stable `id`
- a `question`
- 2–3 mutually exclusive options, with the recommended option first

The UI renders the request in a dedicated dialog, allows selection or custom
input, and returns structured answers back to the agent through a request
registry instead of parsing free-form user messages.

## Plan File Protocol

The output contract for Plan Mode is a plan file on disk plus a `resolve`
tool call.

1. Crush creates an active plan file path for the session when Plan Mode is
   entered.
2. The agent explores the repo and writes the final plan to the active plan
   file using `write` or `edit`.
3. When the plan is ready, the agent calls `resolve` with `action: "apply"` and
   `extra.title` matching the plan slug.
4. The UI detects the `resolve` call and opens the `PlanReview` dialog loaded
   from the plan file.

This removes the plan content from the assistant message text and makes the
plan file the single source of truth.

## Executing an Approved Plan

Approving a plan does not create a new session and does not clear history.

Instead, Crush:

1. switches the same session from `plan` back to `default`
2. keeps all existing messages, exploration output, and attachments
3. sends a concrete execution prompt built from the approved plan file content

This preserves planning context and lets implementation continue naturally in
the same conversation.

## Subagent Plan Execution

Subagents spawned in Plan Mode inherit the plan mode constraints through the
`collaboration_mode` propagation. Each task creates a child session, and the
parent can monitor task state through the child session. The task graph in the
UI derives its status from the child session's `ToolResultSubtaskStatus`, using
the child session as the single source of truth for task completion.

## Enforcement Reminders

If the assistant finishes a Plan Mode turn without calling `resolve` or
`request_user_input`, Crush injects a transient planning reminder. The reminder
is hidden from the transcript and only the model sees it. If the reminder still
does not produce a required tool call, Crush surfaces a UI status warning so the
user is not left waiting silently.
