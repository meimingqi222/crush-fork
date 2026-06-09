Unified Language Server Protocol tool for code intelligence operations.

## Operations

| op | Description |
|---|---|
| `diagnostics` | Get LSP diagnostics (errors, warnings) for a file or the entire project |
| `references` | Find all references to a symbol using grep + LSP |
| `declaration` | Jump to the declaration of a symbol at a given position |
| `definition` | Jump to the definition of a symbol at a given position |
| `implementation` | Find implementations of a symbol (interfaces, abstract methods) |
| `type_definition` | Jump to the type definition of a symbol |
| `hover` | Get hover/type information for a symbol at a given position |
| `document_symbols` | List all symbols (functions, types, variables) in a file |
| `workspace_symbols` | Search symbols across the entire workspace |
| `code_action` | List or apply code actions (quickfixes, refactors) at a position |
| `rename` | Rename a symbol across the workspace |
| `format` | Format a file using the LSP formatter |
| `restart` | Restart one or all LSP clients |

## Parameters by Operation

- **diagnostics**: `file_path` (optional, empty = project-wide)
- **references**: `symbol` (required), `path` (optional search directory)
- **declaration/definition/implementation/type_definition**: `file_path`, `line`, `character` (all required)
- **hover**: `file_path`, `line`, `character` (all required)
- **document_symbols**: `file_path` (required)
- **workspace_symbols**: `query` (optional filter)
- **code_action**: `file_path`, `line`, `character` (required), `action_kind` (optional filter), `apply` (optional bool), `action_index` (optional, 1-based)
- **rename**: `file_path`, `line`, `character`, `new_name` (all required)
- **format**: `file_path` (required), `tab_size`, `insert_spaces`, etc. (optional)
- **restart**: `name` (optional, empty = restart all)

## Notes

- All line/character positions are 1-based.
- Write operations (code_action with apply, rename, format) require permission approval.
- Prefer running `references` before `rename` for high-impact symbols.
- Combine `declaration` and `definition` for complete navigation.
- Diagnostics are automatically included in edit/write tool output.
