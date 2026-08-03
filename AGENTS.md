# Crush Development Guide

## Project Overview

Crush is a terminal-based AI coding assistant built in Go by
[Charm](https://charm.land). It connects to LLMs and gives them tools to read,
write, and execute code. It supports multiple providers (Anthropic, OpenAI,
Gemini, Bedrock, Copilot, Hyper, MiniMax, Vercel, and more), integrates with
LSPs for code intelligence, and supports extensibility via MCP servers and
agent skills.

The module path is `github.com/charmbracelet/crush`.

## Architecture

```
main.go                            CLI entry point (cobra via internal/cmd)
fantasy/                           Local fork of charm.land/fantasy (go.mod replace);
                                   the LLM framework code is edited HERE, in-repo
internal/
  app/app.go                       Top-level wiring: DB, config, agents, LSP, MCP, events
  cmd/                             CLI commands (root, run, login, models, stats, sessions)
  config/
    config.go                      Config struct, context file paths, agent definitions
    load.go                        crush.json loading and validation
    provider.go                    Provider configuration and model resolution
  agent/
    agent.go                       SessionAgent: runs LLM conversations per session
    coordinator.go                 Coordinator: manages named agents ("coder", "task")
    prompts.go                     Loads Go-template system prompts
    templates/                     System prompt templates (coder.md.tpl, explore.md.tpl, etc.)
    tools/                         All built-in tools (bash, edit, view, grep, glob, etc.)
      mcp/                         MCP client integration
  session/session.go               Session CRUD backed by SQLite
  message/                         Message model and content types
  db/                              SQLite via sqlc, with migrations
    sql/                           Raw SQL queries (consumed by sqlc)
    migrations/                    Schema migrations
  lsp/                             LSP client manager, auto-discovery, on-demand startup
  memory/                          Persistent memory engine (recall, retention)
  plugin/                          Plugin system (chat transform hooks, etc.)
  ui/                              Bubble Tea v2 TUI (see internal/ui/AGENTS.md)
  permission/                      Tool permission checking and allow-lists
  skills/                          Skill file discovery and loading
  shell/                           Bash command execution with background job support
  event/                           Telemetry (PostHog)
  pubsub/                          Internal pub/sub for cross-component messaging
  filetracker/                     Tracks files touched per session
  history/                         Prompt history
```

### Key Dependency Roles

- **`charm.land/fantasy`**: LLM provider abstraction layer AND the agent step
  loop (tool execution, message accumulation). Handles protocol differences
  between Anthropic, OpenAI, Gemini, etc. **Replaced to the local `./fantasy`
  directory via go.mod `replace`** — changes to the framework are made in this
  repo. See `docs/pitfalls/fantasy-dual-message-state.md` before touching
  message flow.
- **`charm.land/bubbletea/v2`**: TUI framework powering the interactive UI.
- **`charm.land/lipgloss/v2`**: Terminal styling.
- **`charm.land/glamour/v2`**: Markdown rendering in the terminal.
- **`charm.land/catwalk`**: Provider/model catalog (embedded data + fetched at
  runtime); consumed by `internal/config/catwalk.go`.
- **`sqlc`**: Generates Go code from SQL queries in `internal/db/sql/`.

### Key Patterns

- **Config is injected, not global**: `config.Init(workingDir, dataDir, debug)`
  (called in `internal/cmd/root.go`) returns a `*config.ConfigStore` that is
  passed explicitly to components.
- **Tools are self-documenting**: each tool has a `.go` implementation and a
  `.md` description file in `internal/agent/tools/`.
- **System prompts are Go templates**: `internal/agent/templates/*.md.tpl`
  with runtime data injected.
- **Context files**: Crush reads AGENTS.md, CRUSH.md, CLAUDE.md, GEMINI.md
  (and `.local` variants) from the working directory for project-specific
  instructions.
- **Persistence**: SQLite + sqlc. All queries live in `internal/db/sql/`,
  generated code in `internal/db/`. Migrations in `internal/db/migrations/`.
- **Pub/sub**: `internal/pubsub` for decoupled communication between agent,
  UI, and services.
- **CGO disabled**: builds with `CGO_ENABLED=0` and
  `GOEXPERIMENT=greenteagc`.

## Build/Test/Lint Commands

- **Build**: `go build .` or `go run .`
- **Test**: `task test` or `go test ./...` (run single test:
  `go test ./internal/agent -run TestApplyTruncatedToolResults`)
- **Update Golden Files**: `go test ./... -update` (regenerates `.golden`
  files when test output changes; e.g.
  `go test ./internal/ui/diffview -update`)
- **Lint**: `task lint:fix`
- **Format**: `task fmt` (`gofumpt -w .`)
- **Modernize**: `task modernize` (runs `modernize` which makes code
  simplifications)
- **Dev**: `task dev` (runs with profiling enabled)

## Code Style Guidelines

Standard Go conventions apply and are not restated here. Project-specific
rules:

- **Formatting**: ALWAYS format Go code you write. Use gofumpt (stricter than
  gofmt; `task fmt` runs `gofumpt -w .`); fall back to `goimports`, then
  `gofmt`, if gofumpt is unavailable.
- **Testing**: Use testify's `require` package, parallel tests with
  `t.Parallel()`, `t.Setenv()` for environment variables, and `t.TempDir()`
  for temporary directories (no cleanup needed).
- **JSON tags**: Use snake_case for JSON field names.
- **Log messages**: Must start with a capital letter (e.g., "Failed to save
  session", not "failed to save session"). Enforced by `task lint:log`
  (part of `task lint`).
- **Comments**: Own-line comments start with a capital letter and end with a
  period; wrap at 78 columns. End-of-line comments need no period.

## Testing with Mock Providers

Tests that need provider configurations inject mock clients rather than
hitting the network — see `mockCatwalkClient` / `mockHyperClient` and
`TestProviders_Integration_WithMockClients` in
`internal/config/provider_test.go` for the pattern.

## Committing

- ALWAYS use semantic commits (`fix:`, `feat:`, `chore:`, `refactor:`,
  `docs:`, `sec:`, etc).
- Try to keep commits to one line, not including your attribution. Only use
  multi-line commits when additional context is truly necessary.

## Creating Pull Requests

- **Default target**: When asked to create a PR, always create it in the fork
  repository (`meimingqi222/crush-fork`), NOT the upstream repository
  (`charmbracelet/crush`).
- PRs should be from a feature branch to `main` within the fork.
- Use `gh pr create --repo meimingqi222/crush-fork` to ensure the PR is created in
  the correct repository.

## Working on the TUI (UI)

Anytime you need to work on the TUI, read `internal/ui/AGENTS.md` before
starting work.

## Code Review Checklist

When reviewing code changes, check for pitfalls documented in `docs/pitfalls/`:

- Scan the changed files for patterns matching known issues
- Verify API contracts match expected data formats (raw vs encoded)
- Cross-reference with symptoms described in each pitfall document
- For internal prompt display, read `docs/pitfalls/internal-prompt-display-leakage.md`
- For TUI dialog cursor/layout changes, read `docs/pitfalls/tui-dialog-cursor-coordinates.md`
