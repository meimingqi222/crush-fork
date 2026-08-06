package agent

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"

	"charm.land/fantasy"
	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/plugin"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/skills"
)

type registeredTool struct {
	tool     fantasy.AgentTool
	metadata agenttools.ToolMetadata
}

func archiveDirectory(dataDirectory, workingDir string) string {
	if dataDirectory != "" {
		return filepath.Join(dataDirectory, "archive")
	}
	if workingDir != "" {
		return filepath.Join(workingDir, contextWindowArchiveDir)
	}
	return ""
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

func subagentCanSpawn(ctx context.Context) bool {
	runtime, ok := subagentRuntimeFromContext(ctx)
	return ok && runtime.Permissions.CanSpawn
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

	isSubagent := config.NormalizeAgentMode(agent.Mode) == config.AgentModeSubagent

	allowAgentTool := slices.Contains(agent.AllowedTools, AgentToolName)
	if runtime, ok := subagentRuntimeFromContext(ctx); ok && runtime.Permissions.CanSpawn {
		allowAgentTool = true
	}
	if allowAgentTool && (!isSubagent || subagentCanSpawn(ctx)) {
		agentTool, err := c.agentTool(ctx)
		if err != nil {
			return nil, err
		}
		register(agentTool, "builtin", builtinToolMetadataForAgent(AgentToolName, isSubagent, mode))
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

	canonicalID := config.CanonicalSubagentID(agent.ID)
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
	} else if canonicalID == config.AgentExplore || canonicalID == config.AgentLibrarian {
		// Explore and Librarian agents: no git-only restriction — they already cannot edit/write
		// files (those tools are absent from their AllowedTools list). The system
		// prompt guides them to use read-only operations. Mirroring opencode's
		// approach: rely on tool-level constraints (no edit/write) rather than
		// bash-level constraints, so useful git plumbing commands (cat-file,
		// ls-tree, …) and command chains (cd && git diff) work without errors.
		bashOpts = agenttools.BashToolOptions{
			DisableBackground: true,
		}
	} else if isReadOnlyRuntime(agent) {
		bashOpts = agenttools.BashToolOptions{
			RestrictedToGitReadOnly: true,
			DisableBackground:       true,
			DescriptionOverride:     agenttools.RestrictedGitBashDescription(),
		}
	}

	editTool := agenttools.NewEditTool(c.lspManager, c.permissions, c.history, c.filetracker, c.cfg.WorkingDir(), c.cfg.Config().Tools.Edit.GetFuzzyThreshold())

	// Discover skills for URL resolution in read and bash tools.
	// Use coordinator cache to avoid repeated I/O overhead.
	var discoveredSkills []*skills.Skill
	if c.cfg.Config().Options != nil && len(c.cfg.Config().Options.SkillsPaths) > 0 {
		discoveredSkills = c.getDiscoveredSkills(c.cfg.Config().Options.SkillsPaths)
	}

	bashOpts.SkillList = discoveredSkills

	builtin := []fantasy.AgentTool{
		agenttools.NewRequestUserInputTool(c.userInput),
		agenttools.NewBashToolWithSessions(c.sessions, c.permissions, c.cfg.WorkingDir(), c.cfg.Config().Options.Attribution, modelName, c.hookManager, bashOpts),
		agenttools.NewJobTool(),
		agenttools.NewDownloadTool(c.permissions, c.cfg.WorkingDir(), nil),
		editTool,
		agenttools.NewReadToolWithArchiveDir(c.lspManager, c.permissions, c.filetracker, c.cfg.WorkingDir(), c.cfg.Config().Tools.Ls, nil, archiveDirectory(c.cfg.Config().Options.DataDirectory, c.cfg.WorkingDir()), discoveredSkills, c.cfg.Config().Options.SkillsPaths...),
		agenttools.NewGlobTool(c.cfg.WorkingDir()),
		agenttools.NewGrepTool(c.cfg.WorkingDir(), c.cfg.Config().Tools.Grep),
		agenttools.NewSourcegraphTool(nil),
		agenttools.NewCrushTool(c.cfg, c.lspManager, c.memoryEngine(), filepath.Join(c.cfg.Config().Options.DataDirectory, "logs", "crush.log")),
		agenttools.NewGoalTool(c.sessions, c.goalRuntime),
	}

	// The todo tool is conditionally registered based on the todo.enabled
	// config (default true). max_items is passed through to cap task lists.
	todoCfg := c.cfg.Config().EffectiveTodoConfig()
	if todoCfg.Enabled == nil || *todoCfg.Enabled {
		maxItems := 50
		if todoCfg.MaxItems != nil {
			maxItems = *todoCfg.MaxItems
		}
		builtin = append(builtin, agenttools.NewTodoTool(c.sessions, c.goalRuntime, maxItems))
	}

	builtin = append(builtin,
		agenttools.NewIrcTool(c.agentRegistry.AsIrcRegistry()),
		agenttools.NewWriteTool(c.lspManager, c.permissions, c.history, c.filetracker, c.cfg.WorkingDir()),
	)
	// resolve only works in plan mode -- resolve.go rejects the call with an
	// error in every other mode. Registering it unconditionally spent a tool
	// slot and a decision point on a tool the model could never use.
	if mode == session.CollaborationModePlan {
		builtin = append(builtin, agenttools.NewResolveTool(c.sessions))
	}
	builtin = append(builtin, c.memoryTools()...)
	if isSubagent || allowAgentTool {
		builtin = append(builtin,
			agenttools.NewSendMessageTool(c.mailbox),
			agenttools.NewTaskStopTool(c.mailbox),
		)
	}
	if isSubagent {
		var yieldOpts []agenttools.YieldOption
		if agent.OutputSchema != nil {
			yieldOpts = append(yieldOpts, agenttools.WithOutputSchema(agent.OutputSchema))
		}
		builtin = append(builtin, agenttools.NewYieldTool(c.messages, yieldOpts...))
	}
	for _, tool := range builtin {
		register(tool, "builtin", builtinToolMetadataForAgent(tool.Info().Name, isSubagent, mode))
	}

	if len(c.cfg.Config().LSP) > 0 || c.cfg.Config().Options.AutoLSP == nil || *c.cfg.Config().Options.AutoLSP {
		lspTools := []fantasy.AgentTool{
			agenttools.NewLSPTool(c.lspManager, c.permissions, c.cfg.WorkingDir()),
		}
		for _, tool := range lspTools {
			register(tool, "builtin", builtinToolMetadata(tool.Info().Name))
		}
	}

	// Register the describe_image tool only when the primary model does not
	// support images and a vision helper model is configured. When the primary
	// model already has vision, the read tool handles images natively and this
	// tool is unnecessary.
	if c.visionService != nil && c.visionService.IsAvailable() {
		supportsImages, err := c.resolveCoderModelSupportsImages()
		if err != nil {
			slog.Warn("Could not resolve coder model image support; skipping describe_image registration", "error", err)
		} else if !supportsImages {
			describeImageTool := agenttools.NewDescribeImageTool(c.permissions, c.cfg.WorkingDir())
			register(describeImageTool, "builtin", builtinToolMetadata(agenttools.DescribeImageToolName))
		}
	}

	for _, customTool := range c.plugins().GetCustomTools() {
		customAgentTool := plugin.NewCustomToolAgentTool(customTool, c.cfg.WorkingDir())
		register(customAgentTool, "plugin", metadataFromPluginToolDefinition(customTool))
	}

	for _, mcpTool := range agenttools.GetMCPTools(ctx, c.permissions, c.cfg, c.cfg.WorkingDir()) {
		if !allowMCPToolForAgent(agent, mcpTool) {
			continue
		}
		register(mcpTool, fmt.Sprintf("mcp:%s", mcpTool.MCP()), metadataForMCPTool(mcpTool))
	}

	if mode != session.CollaborationModePlan {
		// Only register tool_search when there are deferred tools to discover.
		// This prevents the LLM from calling tool_search when no MCP or
		// external tools are available, reducing unnecessary tool calls.
		hasDeferred := false
		for _, entry := range registry.Search("", agenttools.RegistrySearchOptions{
			Limit:           10_000,
			IncludeDeferred: true,
		}) {
			if entry.Metadata.IsDeferred() {
				hasDeferred = true
				break
			}
		}
		if hasDeferred {
			toolSearch := agenttools.NewToolSearchTool(registry, c.activateDeferredTools)
			register(toolSearch, "builtin", builtinToolMetadata(agenttools.ToolSearchToolName))
		}
	}

	return registered, nil
}

// memoryTools returns the LLM-facing memory tools, gated by the active
// backend's Capabilities rather than registered unconditionally. When memory
// is disabled (nil backend or Enabled() == false) no memory tools are
// returned at all, so the LLM never sees a tool whose only possible outcome
// is "memory engine is not available" -- mirrors oh-my-pi's
// `createIf(session)` tool gating. Capability-specific tools (reflect,
// graph) are only included when the backend actually supports them: e.g.
// hindsight has no local triple store, so graph is omitted for it.
func (c *coordinator) memoryTools() []fantasy.AgentTool {
	if c.memoryBackend == nil || !c.memoryBackend.Enabled() {
		return nil
	}
	caps := c.memoryBackend.Capabilities()

	memTools := []fantasy.AgentTool{
		agenttools.NewRecallTool(c.memoryEngineRetriever(), c.memoryEngineEventStore()),
		agenttools.NewMemoryStatusTool(c.memoryBackend),
	}
	if caps.Retain {
		memTools = append(memTools, agenttools.NewRetainTool(c.memoryEngineEventStore(), c.cfg.WorkingDir(), c.memoryBackend))
	}
	if caps.Reflect {
		memTools = append(memTools, agenttools.NewReflectTool(c.memoryEngineRetriever()))
	}
	if caps.Triples {
		memTools = append(memTools, agenttools.NewGraphTool(c.memoryEngineTripleStore()))
	}
	return memTools
}

func metadataForMCPTool(tool *agenttools.Tool) agenttools.ToolMetadata {
	info := tool.Info()
	searchHint := strings.TrimSpace(info.Description)
	if searchHint == "" {
		searchHint = fmt.Sprintf("invoke external integration tool %s.%s", tool.MCP(), tool.MCPToolName())
	}

	return agenttools.ToolMetadata{
		ReadOnly:        false,
		ConcurrencySafe: false,
		RiskHint:        "network",
		Exposure:        agenttools.ToolExposureDeferred,
		SearchHint:      searchHint,
		SearchTags:      []string{tool.MCP(), tool.MCPToolName()},
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
