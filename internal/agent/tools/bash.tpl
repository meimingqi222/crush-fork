Executes bash commands with automatic background conversion for long-running tasks.

<cross_platform>
Uses the mvdan/sh interpreter (Bash-compatible on all platforms including Windows).
- Use forward slashes: "ls C:/foo/bar" not "ls C:\foo\bar".
- For null redirection always use /dev/null, never `nul` or `$null`.
- Never wrap in another shell: no `bash -lc`, `sh -c`, `cmd /c`, `powershell -Command`, or `pwsh -c`.
- This tool is not PowerShell; use direct POSIX-style commands or the dedicated View/Grep/Glob tools.
</cross_platform>

<execution_steps>
1. Directory verification: before creating directories/files, check the parent exists.
2. Security check: banned commands ({{ .BannedCommands }}) return an error; safe read-only commands run without prompts.
3. Execute with proper quoting; capture output.
4. Auto-background: commands exceeding their timeout are promoted to background jobs; set run_in_background=true upfront for known long-running commands.
5. Output is truncated if it exceeds {{ .MaxOutputLength }} characters.
6. Return results with errors and metadata (<cwd></cwd> tags).
</execution_steps>

<usage_notes>
- Command is required; working_dir is optional (defaults to the current directory).
- Always provide a brief `description` parameter (under 30 chars).
- Prefer Grep/Glob/Agent tools over `find`/`grep`; use View instead of `cat`/`head`/`tail`/`ls`. If you must search from bash, use `rg` (ripgrep).
- Chain with `;` or `&&`; avoid newlines except inside quoted strings.
- Each command runs in an independent shell (no state persists between calls).
- Prefer absolute paths over `cd` (use `cd` only when the user explicitly requests it).
- skill:// URLs in commands auto-resolve to filesystem paths (e.g. `python skill://pdf/scripts/run.py`).
</usage_notes>

<background_execution>
- Set run_in_background=true to run in a separate background shell; it returns a shell ID. To read output, wait, or kill it, activate the deferred `job` tool with tool_search (query "select:job").
- NEVER use `&` to background a command — use the run_in_background parameter.
- Good candidates: long-running servers, watch tasks, anything that runs indefinitely.
- Do NOT background: builds, test suites, git operations, file operations, short-lived scripts.
</background_execution>

<git_commits>
When the user asks to create a commit: gather status/diff/log in one message, stage relevant files, draft a concise "why"-focused message, commit via HEREDOC, and verify with git status. Retry once on pre-commit hook failure; amend if files were modified. Don't push, don't stage unrelated files, never update git config.
</git_commits>

<pull_requests>
Use `gh` for GitHub tasks. When creating a PR: gather status/diff/branch state in one message, create/commit/push the branch, draft a concise summary of all changes since main divergence, and create the PR with `gh pr create --title --body` using a HEREDOC. Return an empty response (the user sees gh output); never update git config.
</pull_requests>

<examples>
Good: pytest /foo/bar/tests
Bad: cd /foo/bar && pytest tests
</examples>
