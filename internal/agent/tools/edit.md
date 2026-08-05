Edits files by replacing text, creating new files, or deleting content. For moving/renaming use Bash 'mv'. For large edits use the Write tool.

<prerequisites>
1. Read the file first to understand its contents and context.
2. For new files: use bash to check the directory exists.
3. Note whitespace, indentation, and formatting from read output.
</prerequisites>

<parameters>
1. file_path: Absolute path to file (required, even in patch/file_operations mode - used as the fallback path).
2. old_string: Text to replace (must match exactly, including whitespace/indentation).
3. new_string: Replacement text.
4. replace_all: Replace all occurrences (default false).
5. edits: Array of {old_string, new_string, replace_all} for multiple changes to ONE file, applied in order (when provided, old_string/new_string/replace_all are ignored).
6. operations: Array of hashline operations using LINE#HASH references from `read` with a line selector (e.g. `read(path="file.ts:50-200")`). When provided, all other parameters except file_path are ignored. Each operation: {operation, content, line/start/end, register, paste_before}. Operations: replace_line, replace_range, prepend, append, cut, paste. See `<hashline_mode>` below.
7. patch: Unified diff text (`--- `/`+++ `/`@@ ` headers) that can create/modify/delete across MULTIPLE files in one call. When provided, old_string/new_string/edits/operations are ignored. See `<patch_mode>` below.
8. file_operations: Array of per-file hashline operation groups for multi-file atomic edits. Each entry: {file_path, operations}. When provided, all other parameters are ignored. Enables cross-file CUT/PASTE (cut lines from one file, paste into another). See `<multi_file_mode>` below.
</parameters>

<special_cases>
- Create file: file_path + new_string, leave old_string empty.
- Delete content: file_path + old_string, leave new_string empty.
- Multiple changes to one file: file_path + edits array.
- Create a file pre-populated with several edits: edits array whose FIRST entry has an empty old_string (its new_string becomes the initial content); later entries are applied on top of that. Errors if the file already exists.
- Multiple files, or multiple disjoint hunks across files, in one call: patch.
- Multi-file hashline edits with cross-file move (cut from file A, paste into file B): file_operations.
</special_cases>

<whitespace_tolerance>
Trailing whitespace and minor indentation variations are handled via fuzzy matching fallback: trailing-whitespace trim, full-line whitespace trim, then indentation-flexible matching. A configurable similarity threshold (Levenshtein-based) can be set in config; when 0, fuzzy similarity matching is disabled.
</whitespace_tolerance>

<hashline_mode>
When exact text matching is brittle (heavy escaping, repeated snippets), read with a line selector to get LINE#HASH references, then pass `operations` instead of old_string/new_string. Operation types:
- replace_line: replace the referenced line with `content`.
- replace_range: replace lines `start`..`end` (inclusive) with `content`. Omit `content` to delete.
- prepend / append: insert `content` before / after the referenced line.
- cut: capture lines `start`..`end` (inclusive) into a clipboard register and delete them. Use `register` to name the register (persists across edit calls); omit for the anonymous register (batch-local).
- paste: insert clipboard register contents near the referenced `line`. Use `paste_before: true` to insert before the line, or `false` (default) to insert after. Use `register` to paste from a named register.

CUT and PASTE enable in-file and cross-file line moves:
- In-file: cut lines, then paste them at a new location within the same operations array.
- Cross-file: cut lines in one file using a named register, then paste from that register in another file's operations (use `file_operations` for this).

On hash mismatch, re-read with a line selector and retry.
</hashline_mode>

<multi_file_mode>
`file_operations` applies hashline operations to multiple files atomically in a single call. Each entry specifies a `file_path` and its `operations` array (same operation types as `operations`). Files are processed in order, so a CUT in an earlier file populates registers that a PASTE in a later file can read.

```
file_operations=[
  {file_path: "/abs/a.go", operations: [
    {operation: "cut", start: "10#XX", end: "15#YY", register: "move1"}
  ]},
  {file_path: "/abs/b.go", operations: [
    {operation: "paste", line: "5#ZZ", register: "move1", paste_before: true}
  ]}
]
```

- Named registers persist across edit calls within the session (for multi-step cross-file moves).
- The anonymous register is cleared after each edit call completes.
- All files are locked for the duration of the operation.
- If any file fails (not found, hash mismatch, permission denied), no files are written.
</multi_file_mode>

<patch_mode>
`patch` accepts standard unified diff text and is the only way to change multiple files in a single call (besides `file_operations`):

```
--- a/src/a.ts
+++ b/src/a.ts
@@ -10,3 +10,3 @@
 context line
-old line
+new line
 context line
--- a/src/b.ts
+++ b/src/b.ts
@@ -1,1 +1,1 @@
-old
+new
```

- Each `--- `/`+++ ` pair starts a file section; each `@@ -oldStart,oldLen +newStart,newLen @@` starts a hunk within it.
- A file's hunks may sit under one `--- `/`+++ ` header or be split across several repeated ones for the same path (both are merged automatically) - either way, keep hunks in ascending line-number order.
- Every context and deletion line must match the file's current content exactly - patch mode does not fall back to fuzzy matching. Context re-anchors automatically if line numbers drifted slightly (up to ~100 lines) from other changes, but the text itself must still match.
- `file_path` is still required even when every hunk carries its own path; it is only used as a fallback (e.g. for a `/dev/null` old-path on a new file).
- Prefer `patch` over several sequential edit calls when a change spans multiple files or many disjoint hunks in one file.
</patch_mode>

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
5. Never guess - always read the file to get exact text.
</recovery>

<best_practices>
- Use absolute paths with forward slashes.
- Multiple edits to one file: use the edits[] array for atomic multi-edit.
- Changes spanning multiple files: use patch instead of separate edit calls per file.
- Cross-file line moves (cut from one file, paste into another): use file_operations with named registers.
- When in doubt, include MORE context rather than less.
- Match the existing code style exactly.
</best_practices>

<windows_notes>
- Forward slashes work throughout (C:/path/file).
- Line endings converted automatically (\n ↔ \r\n).
</windows_notes>
