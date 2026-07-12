package acp

import (
	"context"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/timeline"
	"github.com/charmbracelet/crush/internal/toolruntime"
)

// MCPManagerFuncs is an MCPManager implementation backed by the package-level
// functions in internal/agent/tools/mcp. It is defined here to avoid an import
// cycle between the acp package and the mcp tools package.
type MCPManagerFuncs struct {
	ReconnectFn     func(ctx context.Context, cfg *config.ConfigStore, name string) error
	DisableSingleFn func(cfg *config.ConfigStore, name string) error
}

// Reconnect connects (or reconnects) the named MCP server.
func (m MCPManagerFuncs) Reconnect(ctx context.Context, cfg *config.ConfigStore, name string) error {
	if m.ReconnectFn == nil {
		return nil
	}
	return m.ReconnectFn(ctx, cfg, name)
}

// DisableSingle disconnects and removes the named MCP server.
func (m MCPManagerFuncs) DisableSingle(cfg *config.ConfigStore, name string) error {
	if m.DisableSingleFn == nil {
		return nil
	}
	return m.DisableSingleFn(cfg, name)
}

// AppAdapter wraps app.App (or a compatible struct) to satisfy the acp.App
// interface without importing the app package (which would create a cycle).
type AppAdapter struct {
	sessions    session.Service
	messages    message.Service
	coordinator agent.Coordinator
	permissions permission.Service
	runtime     toolruntime.Service
	timeline    timeline.Service
	cfg         *config.ConfigStore
	mcpMgr      MCPManager
}

// NewAppAdapter wraps the necessary services to satisfy the acp.App interface.
func NewAppAdapter(
	sessions session.Service,
	messages message.Service,
	coordinator agent.Coordinator,
	permissions permission.Service,
	runtime toolruntime.Service,
	timeline timeline.Service,
	cfg *config.ConfigStore,
	mcpMgr MCPManager,
) *AppAdapter {
	return &AppAdapter{
		sessions:    sessions,
		messages:    messages,
		coordinator: coordinator,
		permissions: permissions,
		runtime:     runtime,
		timeline:    timeline,
		cfg:         cfg,
		mcpMgr:      mcpMgr,
	}
}

func (a *AppAdapter) GetSessions() session.Service        { return a.sessions }
func (a *AppAdapter) GetMessages() message.Service        { return a.messages }
func (a *AppAdapter) GetCoordinator() agent.Coordinator   { return a.coordinator }
func (a *AppAdapter) GetPermissions() permission.Service  { return a.permissions }
func (a *AppAdapter) GetToolRuntime() toolruntime.Service { return a.runtime }
func (a *AppAdapter) GetTimeline() timeline.Service       { return a.timeline }
func (a *AppAdapter) GetConfig() *config.ConfigStore      { return a.cfg }
func (a *AppAdapter) GetMCPManager() MCPManager           { return a.mcpMgr }
