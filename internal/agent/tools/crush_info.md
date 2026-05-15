Get information about Crush's current runtime configuration and service
state.

<usage>
- Shows active model and provider, LSP/MCP server status, discovered skills,
  permissions mode, disabled tools, key options, and attribution settings
- Use when diagnosing why something isn't working (missing diagnostics,
  provider errors, MCP disconnections, or tool access restrictions)
- No parameters needed — always returns the full current state
</usage>

<tips>
- Check [paths] first when diagnosing config/database location issues
- Check [context_files] to verify which AGENTS.md or CRUSH.md files are active
- Check [memory] for memory engine backend, event count, and materialized view state
- Check [lsp] and [mcp] sections for service health
- Check [providers] and [model] to confirm model/provider wiring
- Check [permissions] and [tools] when tool calls are blocked
- Check [attribution] before generating commit/PR text
</tips>
