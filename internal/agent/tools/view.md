Reads and displays file contents with line numbers for examining code, logs, or text data.

<usage>
- Provide file path to read
- Optional offset: start reading from specific line (0-based)
- Optional limit: control lines read (default 2000)
- Optional hashline: include hashline anchors for line-addressable editing
- Optional wait_for_diagnostics: wait for LSP diagnostics before returning (default true; set false to prefer lower latency)
- When file_path is a directory, returns a directory tree listing instead of file content
- Supports image files (PNG, JPEG, GIF, BMP, SVG, WebP)
</usage>

<features>
- Displays contents with line numbers
- Can read from any file position using offset
- Handles large files by limiting lines read
- Auto-truncates very long lines for display
- Suggests similar filenames when file not found
- Renders image files directly in terminal
- Automatically lists directory contents when given a directory path (up to 1000 files)
</features>

<directory_support>
When file_path is a directory:
- Returns a tree view of the directory contents
- Optional ignore: list of glob patterns to exclude from the listing
- Optional depth: maximum directory depth to traverse (default from config)
- Truncates at 1000 files with a notice if the directory is too large
</directory_support>

<limitations>
- Max file size: 5MB
- Default limit: 2000 lines
- Lines >2000 chars truncated
- Binary files (except images) cannot be displayed
</limitations>

<cross_platform>
- Handles Windows (CRLF) and Unix (LF) line endings
- Works with forward slashes (/) and backslashes (\)
- Auto-detects text encoding for common formats
</cross_platform>

<tips>
- Use with Glob to find files first
- For code exploration: Grep to find relevant files, then read to examine
- For large files: use offset parameter for specific sections
- Set `hashline=true` when preparing line-addressable edits or when exact text matching looks brittle
- Tool automatically detects and renders image files
- Pass a directory path to get a directory tree listing
</tips>
