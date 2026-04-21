Collects structured user decisions when the agent needs explicit confirmation.

<when_to_use>
Use this tool when:
- A user confirmation or preference materially affects the next step.
- A product or implementation preference materially affects the plan.
- The ambiguity cannot be resolved by exploring the repo.
- You need the user to choose between 2-3 meaningful tradeoffs.
</when_to_use>

<when_not_to_use>
Skip this tool when:
- The answer can be discovered from local files or system state.
- The question is low impact and a reasonable default is obvious.
</when_not_to_use>

<requirements>
- Ask 1 to 3 questions.
- Each question must provide 2 or 3 mutually exclusive options.
- Put the recommended option first.
- Keep labels short.
- Use the `header` field for a compact section title.
- The UI will also allow custom input when needed.
- Prefer using this from the primary agent; subagents should avoid blocking on
  user input unless explicitly designed to do so.
</requirements>
