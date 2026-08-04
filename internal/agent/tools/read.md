Reads file contents, directories, or URL content.

<usage>
- Provide path (required): file path, directory path, skill:// URL, or URL
- For files: append selectors for line ranges or raw mode (see below)
- Line selectors auto-enable hashline anchors (LINE#HASH) for line-addressable editing
</usage>

<selectors>
Append `:<selector>` to the path for file reads:

- `file.ts` — read from start (default limit: 2000 lines)
- `file.ts:50` — from line 50 onward
- `file.ts:50-100` — lines 50–100 inclusive
- `file.ts:50+10` — 10 lines starting at line 50
- `file.ts:50-` — from line 50 to end of file
- `file.ts:raw` — verbatim output (no line numbers, no anchors, no wrapping)
- `file.ts:50-100:raw` — combined (order-independent)

When a line selector is present, output includes LINE#HASH anchors for use with the edit tool's operations[] parameter.
</selectors>

<features>
- Unified interface for files, directories, and URLs
- Displays file contents with line numbers
- Handles large files with selector-based pagination
- Auto-truncates very long lines for display
- Suggests similar filenames when file not found
- Renders image files directly in terminal
- Automatically lists directory contents when given a directory path
</features>

<url_reading>
When path is a URL (http:// or https://):
- Auto-converts HTML to markdown by default
- Max response size: 1MB
</url_reading>

<skill_urls>
When path is a skill:// URL:
- `skill://<name>` — reads the skill's SKILL.md file
- `skill://<name>/<path>` — reads a relative path within the skill's directory (e.g. `skill://pdf/scripts/convert.py`)
- Supports the same line selectors as regular file paths (e.g. `skill://pdf/SKILL.md:10-50`)
- Use skill:// URLs to access skill resources (scripts, references, assets) without knowing their filesystem location
</skill_urls>

<directory_support>
When path is a directory:
- Returns a tree view of the directory contents
- Truncates at 1000 files with a notice if too large
- For filtered or deeper listing, use the Glob tool instead
</directory_support>

<limitations>
- Max file size: 1MB for URLs, 5MB for local files
- Default limit: 2000 lines for files
- Lines >2000 chars truncated
- Binary files (except images) cannot be displayed
- URL reads cannot handle authentication or cookies
</limitations>

<tips>
- Relative paths resolve from the current session working directory, not from a repository name prefix.
- Use with Glob to find files first
- For code exploration: Grep to find relevant files, then read to examine
- Do not use read to probe guessed paths. First ground uncertain paths with Glob, Grep, a directory read, or a path returned by another tool.
- If a missing path has exactly one matching suffix inside the working directory, read may recover it and will report the original and resolved paths explicitly.
- Multiple matching suffixes are never guessed; follow the returned glob, parent-directory, or grep suggestion before retrying read.
- When a file is not found, follow the returned glob, parent-directory, or grep suggestion before retrying read.
- When output says `Use path="file:N" to continue`, pass that exact path in the next read call. Do not reuse a continuation path for a different file.
- For line-addressable edits: read with a line selector (e.g. `path="file.ts:50-100"`) to get LINE#HASH anchors, then use edit with operations[].
- For URLs, markdown format is usually best for AI processing
</tips>
