Unified Language Server Protocol tool for code intelligence operations.

## Operations

| op | Description |
|---|---|
| `diagnostics` | Get LSP diagnostics (errors, warnings) for a file or the entire project |
| `references` | Find all references to a symbol by name |
| `definition` | Find the definition of a symbol **by name** (language-aware; prefer over grep) |
| `document_symbols` | List all symbols (functions, types, variables) in a file |
| `workspace_symbols` | Search symbols across the entire workspace |
| `code_action` | List or apply code actions (quickfixes, refactors) at a position |
| `rename` | Rename a symbol across the workspace |
| `replace_symbol` | Replace, insert before/after, or delete an entire symbol by name (semantic edit) |
| `call_hierarchy` | Show callers and callees of a symbol (incoming/outgoing call hierarchy) |
| `restart` | Restart one or all LSP clients |

## Parameters by Operation

- **diagnostics**: `file_path` (optional, empty = project-wide)
- **references**: `symbol` (required), `path` (optional search directory)
- **definition**: `symbol` (required), `path` (optional search directory)
- **document_symbols**: `file_path` (required)
- **workspace_symbols**: `query` (optional filter)
- **code_action**: `file_path`, `line`, `character` (required), `action_kind` (optional filter), `apply` (optional bool), `action_index` (optional, 1-based)
- **rename**: `file_path`, `line`, `character`, `new_name` (all required)
- **replace_symbol**: `file_path` (required), `symbol` (required), `replacement` (required for replace/add_before/add_after), `action` (replace | add_before | add_after | delete)
- **call_hierarchy**: `symbol` (by name) OR `file_path`, `line`, `character`
- **restart**: `name` (optional, empty = restart all)

## Notes

- Line/character positions are 1-based.
- Write operations (code_action with apply, rename, replace_symbol) require permission approval.
- Prefer name-based ops (`definition` by `symbol`, `replace_symbol`) over position-based ones: they are language-aware and skip matches in comments, strings, and partial identifiers.
- Diagnostics are automatically included in edit/write tool output.
