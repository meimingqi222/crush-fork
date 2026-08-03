Edits files by replacing text, creating new files, or deleting content. For moving/renaming use Bash 'mv'. For large edits use the Write tool.

<prerequisites>
1. Read the file first to understand its contents and context.
2. For new files: use bash to check the directory exists.
3. Note whitespace, indentation, and formatting from read output.
</prerequisites>

<parameters>
1. file_path: Absolute path to file (required).
2. old_string: Text to replace (must match exactly, including whitespace/indentation).
3. new_string: Replacement text.
4. replace_all: Replace all occurrences (default false).
5. edits: Array of {old_string, new_string, replace_all} for multiple changes (when provided, old_string/new_string/replace_all are ignored).
6. operations: Array of hashline operations using LINE#HASH references from `read` with a line selector (e.g. `read(path="file.ts:50-200")`). When provided, all other parameters except file_path are ignored. Each operation: {operation, content, line/start/end}. Operations: replace_line, replace_range, prepend, append.
</parameters>

<special_cases>
- Create file: file_path + new_string, leave old_string empty.
- Delete content: file_path + old_string, leave new_string empty.
- Multiple changes: file_path + edits array.
</special_cases>

<whitespace_tolerance>
Trailing whitespace and minor indentation variations are handled via fuzzy matching fallback: trailing-whitespace trim, full-line whitespace trim, then indentation-flexible matching.
</whitespace_tolerance>

<hashline_mode>
When exact text matching is brittle (heavy escaping, repeated snippets), read with a line selector to get LINE#HASH references, then pass `operations` instead of old_string/new_string. Operation types:
- replace_line: replace the referenced line with `content`.
- replace_range: replace lines `start`..`end` (inclusive) with `content`.
- prepend / append: insert `content` before / after the referenced line.

On hash mismatch, re-read with a line selector and retry.
</hashline_mode>

<uniqueness>
When replace_all=false, old_string MUST uniquely identify the target:
- Include 3-5 lines of context before AND after the change point.
- Include exact whitespace and surrounding code.
- If the text appears multiple times, add more context.
- One call changes ONE instance; use replace_all=true or separate calls for multiple instances.
</uniqueness>

<recovery>
If "old_string not found":
1. Re-read the file at the exact location.
2. Copy MORE context (include the whole function if needed).
3. Check whitespace: indentation count, blank lines, trailing spaces.
4. If the target has heavy escaping or repeated text, use a line selector + `operations` (hashline mode) instead.
5. Never guess — always read the file to get exact text.
</recovery>

<best_practices>
- Use absolute paths with forward slashes.
- Multiple edits to one file: use the edits[] array for atomic multi-edit.
- When in doubt, include MORE context rather than less.
- Match the existing code style exactly.
</best_practices>

<windows_notes>
- Forward slashes work throughout (C:/path/file).
- Line endings converted automatically (\n ↔ \r\n).
</windows_notes>
