# Skill Tool

Invoke a skill by name with optional arguments.

## When to Use

Use this tool when you need to explicitly invoke a skill rather than just reading its SKILL.md file. This is useful when:

- You want to run a skill with specific arguments that you determine programmatically
- The skill defines `allowed-tools` and you need those permissions active
- The skill should be run in a specific context (e.g., `fork` mode)

## Parameters

- `name` (required): The name of the skill to invoke
- `args` (optional): Arguments to pass to the skill

## Behavior

1. Discovers all available skills from configured skill paths
2. Finds the skill matching the given name
3. Reads the skill's full instructions
4. Substitutes arguments using the skill's defined placeholders:
   - `$ARGUMENTS` - full arguments string
   - `$ARGUMENTS[0]`, `$ARGUMENTS[1]`, etc. - indexed arguments
   - `$0`, `$1`, etc. - shorthand for indexed arguments
   - Named arguments (e.g., `$foo`, `$bar`) if defined in skill's `arguments` field
5. Returns the skill's instructions with substituted arguments

## Example

```json
{
  "name": "cherry-pick-pr",
  "args": "123 main"
}
```

This would invoke the `cherry-pick-pr` skill with `pr_number=123` and `target_branch=main`.

## Related

- Skills are discovered from paths configured in `options.skills_paths`
- The skill's `when_to_use` field describes when the model should auto-invoke
- Skills with `context: fork` should be run as sub-agents (future feature)
