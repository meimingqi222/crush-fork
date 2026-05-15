Reads file contents or URL content with flexible output formatting.

<usage>
- Provide path (file path or URL starting with http:// or https://)
- For files: optional offset/limit for pagination, hashline for line-addressable editing
- For URLs: optional format (text, markdown, html) and timeout
- When path is a directory, returns a directory tree listing
- Supports image files (PNG, JPEG, GIF, WebP)
</usage>

<features>
- Unified interface for reading files and URLs
- Displays file contents with line numbers
- Handles large files with offset/limit pagination
- Auto-truncates very long lines for display
- Suggests similar filenames when file not found
- Renders image files directly in terminal
- Automatically lists directory contents when given a directory path (up to 1000 files)
- URL content can be returned as text, markdown, or HTML
</features>

<file_reading>
When path is a file:
- Offset: start from specific line (0-based)
- Limit: number of lines to read (default 2000)
- Hashline: include hash anchors for line-addressable editing
- Wait for LSP diagnostics by default for code intelligence
</file_reading>

<url_reading>
When path is a URL (http:// or https://):
- Format: text (plain text), markdown (converted from HTML), or html (raw HTML body)
- Timeout: optional timeout in seconds (max 120)
- Auto-converts HTML to markdown by default
- Max response size: 1MB
</url_reading>

<directory_support>
When path is a directory:
- Returns a tree view of the directory contents
- Optional ignore: list of glob patterns to exclude
- Optional depth: maximum directory depth to traverse
- Truncates at 1000 files with a notice if too large
</directory_support>

<limitations>
- Max file size: 1MB for URLs, 5MB for local files
- Default limit: 2000 lines for files
- Lines >2000 chars truncated
- Binary files (except images) cannot be displayed
- URL reads cannot handle authentication or cookies
</limitations>

<tips>
- Use with Glob to find files first
- For code exploration: Grep to find relevant files, then read to examine
- For large files: use offset parameter for specific sections
- When output says `Use offset=<n> to continue`, pass that exact offset in the next read call
- Set `hashline=true` when preparing line-addressable edits
- Pass a directory path to get a directory tree listing
- For URLs, markdown format is usually best for AI processing
</tips>
