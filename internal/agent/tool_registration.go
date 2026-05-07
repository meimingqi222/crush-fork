package agent

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"

	"charm.land/fantasy"
	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/memory"
	"github.com/charmbracelet/crush/internal/plugin"
	"github.com/charmbracelet/crush/internal/session"
)

type registeredTool struct {
	tool     fantasy.AgentTool
	metadata agenttools.ToolMetadata
}

func enableNativeToolParallelism(tool fantasy.AgentTool, metadata agenttools.ToolMetadata) {
	if tool == nil || tool.Info().Parallel {
		return
	}
	if !metadata.ReadOnly || !metadata.ConcurrencySafe {
		return
	}
	if setter, ok := tool.(interface{ SetParallel(bool) }); ok {
		setter.SetParallel(true)
	}
}

func (c *coordinator) registerAgentTools(ctx context.Context, agent config.Agent, mode session.CollaborationMode, registry *toolRegistry) ([]registeredTool, error) {
	registered := make([]registeredTool, 0, 48)

	register := func(tool fantasy.AgentTool, source string, metadata agenttools.ToolMetadata) {
		if tool == nil {
			return
		}
		enableNativeToolParallelism(tool, metadata)
		entry := buildRegistryEntryFromTool(tool, source, metadata, false)
		registry.register(entry, invokeFantasyTool(tool))
		registered = append(registered, registeredTool{tool: tool, metadata: entry.Metadata})
	}

	if config.NormalizeAgentMode(agent.Mode) != config.AgentModeSubagent && slices.Contains(agent.AllowedTools, AgentToolName) {
		agentTool, err := c.agentTool(ctx)
		if err != nil {
			return nil, err
		}
		register(agentTool, "builtin", builtinToolMetadata(AgentToolName))
	}

	if slices.Contains(agent.AllowedTools, agenttools.AgenticFetchToolName) {
		agenticFetchTool, err := c.agenticFetchTool(ctx, nil)
		if err != nil {
			return nil, err
		}
		register(agenticFetchTool, "builtin", builtinToolMetadata(agenttools.AgenticFetchToolName))
	}

	modelName := ""
	if modelCfg, ok := c.cfg.Config().Models[agent.Model]; ok {
		if model := c.cfg.Config().GetModel(modelCfg.Provider, modelCfg.Model); model != nil {
			modelName = model.Name
		}
	}

	bashOpts := agenttools.BashToolOptions{}
	if mode == session.CollaborationModePlan {
		// Plan mode: restrict bash to read-only git commands only.
		// Users are collaborating in real-time and arbitrary shell commands
		// would be unexpected.
		bashOpts = agenttools.BashToolOptions{
			RestrictedToGitReadOnly: true,
			DisableBackground:       true,
			DescriptionOverride:     agenttools.RestrictedGitBashDescription(),
		}
	} else if agent.ID == config.AgentExplore {
		// Explore agent: no git-only restriction — it already cannot edit/write
		// files (those tools are absent from its AllowedTools list). The system
		// prompt guides it to use read-only operations. Mirroring opencode's
		// approach: rely on tool-level constraints (no edit/write) rather than
		// bash-level constraints, so useful git plumbing commands (cat-file,
		// ls-tree, …) and command chains (cd && git diff) work without errors.
		bashOpts = agenttools.BashToolOptions{
			DisableBackground: true,
		}
	}

	editTool := agenttools.NewEditTool(c.lspManager, c.permissions, c.history, c.filetracker, c.cfg.WorkingDir())
	hashlineEditTool := agenttools.NewHashlineEditTool(c.lspManager, c.permissions, c.history, c.filetracker, c.cfg.WorkingDir())

	builtin := []fantasy.AgentTool{
		agenttools.NewRequestUserInputTool(c.userInput),
		agenttools.NewPlanExitTool(c.sessions),
		agenttools.NewBashToolWithSessions(c.sessions, c.permissions, c.cfg.WorkingDir(), c.cfg.Config().Options.Attribution, modelName, c.hookManager, bashOpts),
		agenttools.NewJobOutputTool(),
		agenttools.NewJobWaitTool(),
		agenttools.NewJobKillTool(),
		agenttools.NewDownloadTool(c.permissions, c.cfg.WorkingDir(), nil),
		editTool,
		hashlineEditTool,
		agenttools.NewFetchTool(c.permissions, c.cfg.WorkingDir(), nil),
		agenttools.NewGlobTool(c.cfg.WorkingDir()),
		agenttools.NewGrepTool(c.cfg.WorkingDir(), c.cfg.Config().Tools.Grep),
		agenttools.NewLsTool(c.permissions, c.cfg.WorkingDir(), c.cfg.Config().Tools.Ls),
		agenttools.NewSourcegraphTool(nil),
		agenttools.NewCrushInfoTool(c.cfg, c.lspManager),
		agenttools.NewCrushLogsTool(filepath.Join(c.cfg.Config().Options.DataDirectory, "logs", "crush.log")),
		agenttools.NewLongTermMemoryTool(c.longTermMemory, c.permissions, c.cfg.WorkingDir(), func(callCtx context.Context, params memory.SearchParams) ([]memory.Entry, error) {
			bgModel := c.resolveBackgroundModel(callCtx)
			if bgModel == nil {
				return c.longTermMemory.Search(callCtx, params)
			}
			return bgModel.semanticSearch(callCtx, c.longTermMemory, params)
		}),
		agenttools.NewTodosTool(c.sessions),
		agenttools.NewSendMessageTool(c.mailbox),
		agenttools.NewTaskStopTool(c.mailbox),
		agenttools.NewSubtaskResultTool(c.messages),
		agenttools.NewViewTool(c.lspManager, c.permissions, c.filetracker, c.cfg.WorkingDir(), c.cfg.Config().Options.SkillsPaths...),
		agenttools.NewWriteTool(c.lspManager, c.permissions, c.history, c.filetracker, c.cfg.WorkingDir()),
	}
	for _, tool := range builtin {
		register(tool, "builtin", builtinToolMetadata(tool.Info().Name))
	}

	if len(c.cfg.Config().LSP) > 0 || c.cfg.Config().Options.AutoLSP == nil || *c.cfg.Config().Options.AutoLSP {
		lspTools := []fantasy.AgentTool{
			agenttools.NewDiagnosticsTool(c.lspManager),
			agenttools.NewReferencesTool(c.lspManager),
			agenttools.NewLSPDeclarationTool(c.lspManager),
			agenttools.NewLSPDefinitionTool(c.lspManager),
			agenttools.NewLSPImplementationTool(c.lspManager),
			agenttools.NewLSPTypeDefinitionTool(c.lspManager),
			agenttools.NewLSPHoverTool(c.lspManager),
			agenttools.NewLSPDocumentSymbolsTool(c.lspManager),
			agenttools.NewLSPWorkspaceSymbolsTool(c.lspManager),
			agenttools.NewLSPCodeActionTool(c.lspManager, c.permissions, c.cfg.WorkingDir()),
			agenttools.NewLSPRenameTool(c.lspManager, c.permissions, c.cfg.WorkingDir()),
			agenttools.NewLSPFormatTool(c.lspManager, c.permissions, c.cfg.WorkingDir()),
			agenttools.NewLSPRestartTool(c.lspManager),
		}
		for _, tool := range lspTools {
			register(tool, "builtin", builtinToolMetadata(tool.Info().Name))
		}
	}

	for _, customTool := range c.plugins().GetCustomTools() {
		customAgentTool := plugin.NewCustomToolAgentTool(customTool, c.cfg.WorkingDir())
		register(customAgentTool, "plugin", metadataFromPluginToolDefinition(customTool))
	}

	for _, mcpTool := range agenttools.GetMCPTools(c.permissions, c.cfg, c.cfg.WorkingDir()) {
		if !allowMCPToolForAgent(agent, mcpTool) {
			continue
		}
		register(mcpTool, fmt.Sprintf("mcp:%s", mcpTool.MCP()), metadataForMCPTool(mcpTool))
	}

	if mode != session.CollaborationModePlan {
		toolSearch := agenttools.NewToolSearchTool(registry, c.activateDeferredTools)
		register(toolSearch, "builtin", builtinToolMetadata(agenttools.ToolSearchToolName))
	}

	return registered, nil
}

func metadataForMCPTool(tool *agenttools.Tool) agenttools.ToolMetadata {
	return agenttools.ToolMetadata{
		ReadOnly:        false,
		ConcurrencySafe: false,
		RiskHint:        "network",
		Exposure:        agenttools.ToolExposureDeferred,
		SearchHint:      fmt.Sprintf("invoke MCP tool %s", tool.MCPToolName()),
		SearchTags:      []string{"mcp", tool.MCP(), tool.MCPToolName()},
	}
}

func allowMCPToolForAgent(agent config.Agent, tool *agenttools.Tool) bool {
	if agent.AllowedMCP == nil {
		return true
	}
	if len(agent.AllowedMCP) == 0 {
		slog.Debug("No MCPs allowed", "tool", tool.Name(), "agent", agent.Name)
		return false
	}
	for mcpName, allowedTools := range agent.AllowedMCP {
		if mcpName != tool.MCP() {
			continue
		}
		if len(allowedTools) == 0 || slices.Contains(allowedTools, tool.MCPToolName()) {
			return true
		}
		slog.Debug("MCP not allowed", "tool", tool.Name(), "agent", agent.Name)
		return false
	}
	return false
}
