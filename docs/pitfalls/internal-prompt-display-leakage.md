# Internal Prompt Display Leakage

## The Problem

Internal control prompts can leak into the chat transcript as if they were user
or assistant content. Examples include Guided Goal's
`<guided_goal>...</guided_goal>`, memory reminders, `<think>...</think>`,
`<task-notification>...</task-notification>`, and orchestration caveats. These
wrappers and model-only rules are useful to the agent, but are implementation
details and should not be shown verbatim to the user.

## Root Cause

The prompt has two responsibilities that must not be confused:

1. **Agent input** — the full prompt, including the control envelope and rules,
   must be sent to the model.
2. **Chat display** — the user should see a concise representation such as the
   rough goal, not the internal protocol.

The coordinator persists the raw prompt as a `message.User`, while the chat
layer has to decide how that raw content is displayed. Detecting the
`<guided_goal>` prefix for continuation control does not automatically establish
a display policy.

## Symptoms

- `<guided_goal>`, `<think>`, or memory/orchestration wrappers appear in the chat
  window.
- Model-only rules and routing instructions become part of the visible user or
  assistant transcript.
- Attempts to remove all XML tags risk hiding legitimate XML that the user
  intentionally entered.
- Tool result wrappers are accidentally removed from the dedicated tool output.

## The Fix Pattern

Keep the raw prompt unchanged for model execution and history semantics. Apply
a narrow, display-only transformation at the user/assistant text rendering
boundaries:

- Recognize only an exact, known internal prompt envelope at the beginning of
  the message.
- Extract a user-facing field, such as the rough goal.
- Leave ordinary user-authored XML, Markdown, and text untouched.
- Add a regression test for both the internal envelope and ordinary XML.

The current implementation follows this pattern through
`message.DisplayText`, which is called by user and assistant chat renderers.
Tool-message renderers intentionally do not call it, so tool calls and tool
results remain available in their dedicated UI.

## Prevention Checklist

1. Keep model prompts and display text as separate concepts.
2. Do not globally strip XML or angle-bracket tags from user messages.
3. Maintain an explicit allowlist of internal tags to hide or unwrap.
4. Apply the display filter only to user/assistant text, never tool messages.
5. Test persisted raw content separately from rendered user-facing content.
6. When a new internal prompt is added, decide explicitly whether it should be
   hidden, summarized, or shown verbatim.

## Affected Paths

- `internal/ui/model/ui.go` — builds and sends the Guided Goal prompt.
- `internal/agent/coordinator.go` — persists the raw prompt as a user message.
- `internal/message/content.go` — display-only internal protocol filtering.
- `internal/ui/chat/user.go` — user-message rendering boundary.
- `internal/ui/chat/assistant.go` — assistant text/reasoning rendering boundary.
- `internal/ui/chat/tools.go` — dedicated tool-message path, intentionally not filtered.
