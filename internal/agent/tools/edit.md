Edits files by replacing text, creating new files, or deleting content. For moving/renaming use Bash 'mv'. For large edits use Write tool.

<prerequisites>
1. Use the read tool to understand file contents and context
2. For new files: Use bash to check directory exists
3. Note whitespace, indentation, and formatting from read output
</prerequisites>

<parameters>
1. file_path: Absolute path to file (required)
2. old_string: Text to replace (must match exactly including whitespace/indentation)
3. new_string: Replacement text
4. replace_all: Replace all occurrences (default false)
5. edits: Array of edit operations for multiple changes (when provided, old_string/new_string/replace_all are ignored)
6. operations: Array of hashline operations using LINE#HASH line references from read with a line selector (e.g. path="file.ts:50-200"). When provided, all other parameters except file_path are ignored. Each operation: {operation, content, line/start/end}. Operations: replace_line, replace_range, prepend, append.
</parameters>

<special_cases>

- Create file: provide file_path + new_string, leave old_string empty
- Delete content: provide file_path + old_string, leave new_string empty
- Multiple changes: provide file_path + edits array of {old_string, new_string, replace_all}
  </special_cases>

<whitespace_tolerance>
Trailing whitespace differences and minor indentation variations are handled automatically via fuzzy matching fallback. When exact matching fails, the tool tries:
1. Trimming trailing whitespace per line
2. Trimming all surrounding whitespace per line
3. Indentation-flexible matching (common indent prefix stripped)
</whitespace_tolerance>

<hashline_mode>
When exact text matching is brittle (heavy escaping, repeated snippets, special characters), use hashline mode:

1. Read the file with a line selector (e.g. `read(path="file.ts:50-200")`) to get LINE#HASH references
2. Call `edit` with `operations` array instead of `old_string`/`new_string`
3. Each operation uses `LINE#HASH` anchors (e.g. `"5#aa"`) from the read output

Operation types:
- `replace_line`: replace the referenced line with `content`
- `replace_range`: replace lines from `start` to `end` (inclusive) with `content`
- `prepend`: insert `content` before the referenced line
- `append`: insert `content` after the referenced line

If a hash mismatch error occurs, re-read the file with a line selector and retry.
</hashline_mode>

<critical_requirements>
UNIQUENESS (when replace_all=false): old_string MUST uniquely identify target instance

- Include 3-5 lines context BEFORE and AFTER change point
- Include exact whitespace, indentation, surrounding code
- If text appears multiple times, add more context to make it unique

SINGLE INSTANCE: Tool changes ONE instance when replace_all=false

- For multiple instances: set replace_all=true OR make separate calls with unique context
- Plan calls carefully to avoid conflicts

VERIFICATION BEFORE USING: Before every edit

1. Read the file and locate exact target location
2. Check how many instances of target text exist
3. Copy the text including all whitespace
4. Verify you have enough context for unique identification
5. Plan separate calls or use edits[] array or replace_all for multiple changes
   </critical_requirements>

<warnings>
Tool fails if:
- old_string matches multiple locations and replace_all=false
- Insufficient context causes wrong instance change
- Missing or extra blank lines
</warnings>

<recovery_steps>
If you get "old_string not found in file":

1. **Read the file again** at the specific location
2. **Copy more context** - include entire function if needed
3. **Check whitespace**:
   - Count indentation spaces/tabs
   - Look for blank lines
   - Check for trailing spaces
4. **Verify character-by-character** that your old_string matches
5. If the target contains heavy escaping, repeated text, or special characters, read the file with a line selector (e.g. `path="file.ts:1-100"`) to get LINE#HASH references, then pass `operations` array to this tool instead of `old_string`
6. **Never guess** - always read the file to get exact text
   </recovery_steps>

<best_practices>

- Ensure edits result in correct, idiomatic code
- Don't leave code in broken state
- Use absolute file paths (starting with /)
- Use forward slashes (/) for cross-platform compatibility
- Multiple edits to same file: use the edits[] array parameter for atomic multi-edit
- **When in doubt, include MORE context rather than less**
- Match the existing code style exactly (spaces, tabs, blank lines)
  </best_practices>

<examples>
✅ Correct: Exact match with context

```
old_string: "func ProcessData(input string) error {\n    if input == \"\" {\n        return errors.New(\"empty input\")\n    }\n    return nil\n}"

new_string: "func ProcessData(input string) error {\n    if input == \"\" {\n        return errors.New(\"empty input\")\n    }\n    // New validation\n    if len(input) > 1000 {\n        return errors.New(\"input too long\")\n    }\n    return nil\n}"
```

❌ Incorrect: Not enough context

```
old_string: "return nil"  // Appears many times!
```

✅ Correct: Multiple edits using edits[] array

```
file_path: "/path/to/file.go"
edits: [
  {"old_string": "foo", "new_string": "bar"},
  {"old_string": "baz", "new_string": "qux"}
]
```

✅ Correct: Including context to make unique

```
old_string: "func ProcessData(input string) error {\n    if input == \"\" {\n        return errors.New(\"empty input\")\n    }\n    return nil"
```

✅ Correct: Hashline mode for brittle matches

```
file_path: "/path/to/file.go"
operations: [
  {"operation": "replace_line", "line": "12#PV", "content": "func main() {"},
  {"operation": "append", "line": "12#PV", "content": "\tlog.Println(\"started\")"},
  {"operation": "replace_range", "start": "40#MW", "end": "44#QH", "content": "\treturn nil"}
]
```

</examples>

<windows_notes>

- Forward slashes work throughout (C:/path/file)
- File permissions handled automatically
- Line endings converted automatically (\n ↔ \r\n)
  </windows_notes>
