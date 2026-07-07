Globs files and directories via fast pattern matching, any codebase size.

<instruction>
- `path`: a glob, file, or directory. Search several at once by passing a semicolon-delimited list (`src/**/*.ts; test/**/*.ts`).
- `gitignore` (default `true`) hides `.gitignore` matches. Set `gitignore: false` to find `.env*`, `*.log`, fresh build outputs, or anything your repo ignores.
- `hidden` (default `true`); combine with `gitignore: false` to surface dotfiles also gitignored.
- `limit` (default `200`, maximum `200`) caps the number of results.
- Use Glob for uncertain file-name lookups before read.
</instruction>

<output>
Matching paths sorted by mtime (newest first), one path per line.
</output>

<avoid>
Open-ended searches needing multiple rounds of globbing/searching: you MUST use the Agent tool instead.
</avoid>
