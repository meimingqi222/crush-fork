package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/lsp"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubagentConfigUsesCanonicalExplore(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)

	coord := &coordinator{cfg: cfg}

	agentCfg, err := coord.subagentConfig(config.AgentTask)
	require.NoError(t, err)
	assert.Equal(t, config.AgentExplore, agentCfg.ID)

	agentCfg, err = coord.subagentConfig("")
	require.NoError(t, err)
	assert.Equal(t, config.AgentExplore, agentCfg.ID)
}

func TestSubagentConfigSupportsConfiguredSubagents(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)

	cfg.Config().Agents = map[string]config.Agent{
		"custom-reviewer": {
			Mode:         config.AgentModeSubagent,
			Description:  "Reviews changes before handoff.",
			AllowedTools: []string{"read"},
		},
		"reviewer": {
			Mode:         config.AgentModeSubagent,
			Description:  "Configured reviewer should win over built-in alias.",
			AllowedTools: []string{"grep", "read"},
		},
	}
	cfg.SetupAgents()

	coord := &coordinator{cfg: cfg}

	agentCfg, err := coord.subagentConfig("custom-reviewer")
	require.NoError(t, err)
	assert.Equal(t, "custom-reviewer", agentCfg.ID)
	assert.Equal(t, []string{"read"}, agentCfg.AllowedTools)

	agentCfg, err = coord.subagentConfig("reviewer")
	require.NoError(t, err)
	assert.Equal(t, "reviewer", agentCfg.ID)
	assert.Equal(t, []string{"grep", "read"}, agentCfg.AllowedTools)
}

func TestBuildAgentToolDescriptionDeduplicatesExploreAlias(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)

	coord := &coordinator{cfg: cfg}
	description := coord.buildAgentToolDescription()

	assert.Contains(t, description, "- general:")
	assert.Contains(t, description, "- explore:")
	assert.Contains(t, description, "- plan:")
	assert.Contains(t, description, "- review:")
	assert.Contains(t, description, "- designer:")
	assert.Contains(t, description, "- librarian:")
	assert.Contains(t, description, "- quick_task:")
	assert.Equal(t, 1, strings.Count(description, "- explore:"))
}

func TestBuildAgentToolDescriptionEmphasizesParallelDelegation(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)

	coord := &coordinator{cfg: cfg}
	description := coord.buildAgentToolDescription()

	assert.Contains(t, description, "If 2 or more substantial independent tasks can proceed in parallel")
	assert.Contains(t, description, "use a single Agent call with the `tasks` array")
	assert.Contains(t, description, "Prefer early delegation for bounded work")
	assert.Contains(t, description, "Use `explore` only for evidence gathering, not final judgment")
	assert.Contains(t, description, "Use `review` for final code review")
	assert.Contains(t, description, "Use `plan` for architecture planning")
	assert.Contains(t, description, "Use `librarian` for source-verified")
	assert.Contains(t, description, "Do not delegate final code review, correctness approval, or bug triage decisions to `explore`")
	assert.Contains(t, description, "restricted `bash` tool")
	assert.Contains(t, description, "git diff")
	assert.Contains(t, description, "Do not claim that you are delegating")
	assert.Contains(t, description, "make the tool call first rather than narrating a future intention to delegate")
	assert.Contains(t, description, "Do not use the main thread for broad implementation work just because you already know which files are involved")
	assert.Contains(t, description, "prefer multiple direct tool calls in one response instead of subagents")
}

func TestCoderPromptTemplateRequiresOrchestrationFirstDelegation(t *testing.T) {
	promptText := string(coderPromptTmpl)

	assert.Contains(t, promptText, "The main agent is the orchestrator, not the default worker")
	assert.Contains(t, promptText, "you MUST prefer a single Agent call with the `tasks` array")
	assert.Contains(t, promptText, "After delegating independent work, continue on the critical path locally")
	assert.Contains(t, promptText, "prefer batching direct tool calls in parallel instead of paying subagent overhead")
	assert.Contains(t, promptText, "Use subagents when each independent workstream is substantial enough")
	assert.Contains(t, promptText, "Do not merely say that you will use subagents or parallelize work")
	assert.Contains(t, promptText, "If you describe a plan that depends on subagents but then continue doing the delegated work yourself without calling `agent`, you are behaving incorrectly")
}

func TestCoderPromptTemplateRequiresPathGroundingBeforeRead(t *testing.T) {
	promptText := string(coderPromptTmpl)

	assert.Contains(t, promptText, "Ground file paths before reading")
	assert.Contains(t, promptText, "only read paths explicitly provided by the user")
	assert.Contains(t, promptText, "do not probe guessed paths with read")
	assert.Contains(t, promptText, "do not retry guessed paths until a tool confirms the exact path")
}

func TestBuildToolsForSubagentsUseExpectedCapabilities(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)

	coord := &coordinator{
		cfg:         cfg,
		sessions:    env.sessions,
		messages:    env.messages,
		permissions: env.permissions,
		userInput:   nil,
		history:     env.history,
		filetracker: *env.filetracker,
		lspManager:  lsp.NewManager(cfg),
	}

	coderTools, err := coord.buildTools(t.Context(), cfg.Config().Agents[config.AgentCoder], session.CollaborationModeDefault)
	require.NoError(t, err)

	coderNames := make([]string, 0, len(coderTools))
	for _, tool := range coderTools {
		coderNames = append(coderNames, tool.Info().Name)
	}
	assert.Contains(t, coderNames, "request_user_input")
	assert.NotContains(t, coderNames, "plan_exit")

	generalTools, err := coord.buildTools(t.Context(), cfg.Config().Agents[config.AgentGeneral], session.CollaborationModeDefault)
	require.NoError(t, err)

	generalNames := make([]string, 0, len(generalTools))
	for _, tool := range generalTools {
		generalNames = append(generalNames, tool.Info().Name)
	}
	assert.Contains(t, generalNames, "bash")
	assert.Contains(t, generalNames, "edit")
	assert.Contains(t, generalNames, tools.SendMessageToolName)
	assert.Contains(t, generalNames, tools.TaskStopToolName)
	assert.Contains(t, generalNames, tools.LSPCodeActionToolName)
	assert.Contains(t, generalNames, tools.LSPRenameToolName)
	assert.Contains(t, generalNames, tools.LSPFormatToolName)
	assert.NotContains(t, generalNames, AgentToolName)
	assert.NotContains(t, generalNames, "request_user_input")

	exploreTools, err := coord.buildTools(t.Context(), cfg.Config().Agents[config.AgentExplore], session.CollaborationModeDefault)
	require.NoError(t, err)

	exploreNames := make([]string, 0, len(exploreTools))
	for _, tool := range exploreTools {
		exploreNames = append(exploreNames, tool.Info().Name)
	}
	assert.Equal(t, []string{"bash", "glob", "grep", "read", "tool_search"}, exploreNames)

	reviewTools, err := coord.buildTools(t.Context(), cfg.Config().Agents[config.AgentReview], session.CollaborationModeDefault)
	require.NoError(t, err)

	reviewNames := make([]string, 0, len(reviewTools))
	for _, tool := range reviewTools {
		reviewNames = append(reviewNames, tool.Info().Name)
	}
	assert.Contains(t, reviewNames, "bash")
	assert.Contains(t, reviewNames, "read")
	assert.Contains(t, reviewNames, tools.ReferencesToolName)
	assert.Contains(t, reviewNames, tools.YieldToolName)
	assert.NotContains(t, reviewNames, "edit")
	assert.NotContains(t, reviewNames, "write")
	assert.NotContains(t, reviewNames, AgentToolName)

	librarianTools, err := coord.buildTools(t.Context(), cfg.Config().Agents[config.AgentLibrarian], session.CollaborationModeDefault)
	require.NoError(t, err)

	librarianNames := make([]string, 0, len(librarianTools))
	for _, tool := range librarianTools {
		librarianNames = append(librarianNames, tool.Info().Name)
	}
	assert.Contains(t, librarianNames, tools.AgenticFetchToolName)
	assert.Contains(t, librarianNames, tools.YieldToolName)
	assert.NotContains(t, librarianNames, "edit")
	assert.NotContains(t, librarianNames, "write")

	quickTaskTools, err := coord.buildTools(t.Context(), cfg.Config().Agents[config.AgentQuickTask], session.CollaborationModeDefault)
	require.NoError(t, err)

	quickTaskNames := make([]string, 0, len(quickTaskTools))
	for _, tool := range quickTaskTools {
		quickTaskNames = append(quickTaskNames, tool.Info().Name)
	}
	assert.Contains(t, quickTaskNames, "edit")
	assert.Contains(t, quickTaskNames, "write")
	assert.Contains(t, quickTaskNames, tools.YieldToolName)
	assert.NotContains(t, quickTaskNames, AgentToolName)
}

func TestBuildToolsAllowsRecursiveAgentToolWhenConfigured(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.Config().Subagents = &config.SubagentRuntimeConfig{StructuredCompletionRequired: true, AllowRecursiveAgents: true}
	agentCfg := cfg.Config().Agents[config.AgentGeneral]
	agentCfg.AllowedTools = append(agentCfg.AllowedTools, AgentToolName)

	coord := &coordinator{
		cfg:         cfg,
		sessions:    env.sessions,
		messages:    env.messages,
		permissions: env.permissions,
		userInput:   nil,
		history:     env.history,
		filetracker: *env.filetracker,
		lspManager:  lsp.NewManager(cfg),
	}

	runtime := buildSubagentRuntimeContext(
		"parent-session",
		"child-session",
		"msg-1",
		"call-1",
		subagentTask{Name: "task", SubagentType: config.AgentGeneral},
		agentCfg,
		ParentPermissionContext{SessionID: "parent-session", AllowedTools: []string{AgentToolName, tools.ReadToolName}},
		agentCfg.AllowedTools,
		"session",
		env.workingDir,
		nil,
	)
	applySubagentRuntimeConfig(&runtime, cfg.Config().EffectiveSubagentRuntime())

	toolSet, err := coord.buildTools(withSubagentRuntimeContext(t.Context(), runtime), agentCfg, session.CollaborationModeDefault)
	require.NoError(t, err)

	var names []string
	for _, tool := range toolSet {
		names = append(names, tool.Info().Name)
	}
	assert.Contains(t, names, AgentToolName)
}

func TestBuildToolsDoesNotBypassDisabledAgentToolForRecursiveSubagents(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.Config().Options.DisabledTools = []string{AgentToolName}
	cfg.Config().Subagents = &config.SubagentRuntimeConfig{StructuredCompletionRequired: true, AllowRecursiveAgents: true}
	agentCfg := cfg.Config().Agents[config.AgentGeneral]
	agentCfg.AllowedTools = append(agentCfg.AllowedTools, AgentToolName)

	coord := &coordinator{
		cfg:         cfg,
		sessions:    env.sessions,
		messages:    env.messages,
		permissions: env.permissions,
		userInput:   nil,
		history:     env.history,
		filetracker: *env.filetracker,
		lspManager:  lsp.NewManager(cfg),
	}

	runtime := buildSubagentRuntimeContext(
		"parent-session",
		"child-session",
		"msg-1",
		"call-1",
		subagentTask{Name: "task", SubagentType: config.AgentGeneral},
		agentCfg,
		ParentPermissionContext{SessionID: "parent-session", AllowedTools: []string{AgentToolName, tools.ReadToolName}, ExternalDeny: []string{AgentToolName}},
		agentCfg.AllowedTools,
		"session",
		env.workingDir,
		nil,
	)
	applySubagentRuntimeConfig(&runtime, cfg.Config().EffectiveSubagentRuntime())

	toolSet, err := coord.buildTools(withSubagentRuntimeContext(t.Context(), runtime), agentCfg, session.CollaborationModeDefault)
	require.NoError(t, err)

	var names []string
	for _, tool := range toolSet {
		names = append(names, tool.Info().Name)
	}
	assert.NotContains(t, names, AgentToolName)
}

func TestBuildToolsWithSubagentRuntimeShapesFromParentPolicy(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)

	coord := &coordinator{
		cfg:         cfg,
		sessions:    env.sessions,
		messages:    env.messages,
		permissions: env.permissions,
		userInput:   nil,
		history:     env.history,
		filetracker: *env.filetracker,
		lspManager:  lsp.NewManager(cfg),
	}

	agentCfg := cfg.Config().Agents[config.AgentExplore]
	runtime := buildSubagentRuntimeContext(
		"parent-session",
		"child-session",
		"msg-1",
		"call-1",
		subagentTask{Name: "task", SubagentType: config.AgentExplore},
		agentCfg,
		ParentPermissionContext{SessionID: "parent-session", AllowedTools: []string{tools.ReadToolName, tools.ToolSearchToolName}},
		agentCfg.AllowedTools,
		"session",
		env.workingDir,
		nil,
	)

	toolSet, err := coord.buildTools(withSubagentRuntimeContext(t.Context(), runtime), agentCfg, session.CollaborationModeDefault)
	require.NoError(t, err)

	var names []string
	for _, tool := range toolSet {
		names = append(names, tool.Info().Name)
	}
	assert.Equal(t, []string{tools.ReadToolName, tools.ToolSearchToolName, tools.YieldToolName}, names)
}

func TestDeriveSubagentPermissionsDenyWins(t *testing.T) {
	profile := SubagentProfile{
		Name:      config.AgentGeneral,
		Kind:      SubagentProfileGeneral,
		ToolNames: []string{"bash", "edit", "read", tools.YieldToolName},
	}

	derived := DeriveSubagentPermissions(ParentPermissionContext{
		AllowedTools: []string{"bash", "edit", "read", tools.YieldToolName},
		DeniedTools:  []string{"edit"},
	}, profile, []string{"bash", "edit", "read", tools.YieldToolName})

	assert.Contains(t, toolNamesFromSet(derived.AllowedTools), "bash")
	assert.Contains(t, toolNamesFromSet(derived.AllowedTools), "read")
	assert.Contains(t, toolNamesFromSet(derived.AllowedTools), tools.YieldToolName)
	assert.NotContains(t, toolNamesFromSet(derived.AllowedTools), "edit")
	assert.Contains(t, toolNamesFromSet(derived.DeniedTools), "edit")
}

func TestDeriveSubagentPermissionsPreservesMCPAndPlugins(t *testing.T) {
	profile := SubagentProfile{
		Name:      config.AgentGeneral,
		Kind:      SubagentProfileGeneral,
		ToolNames: []string{"bash", "edit", "read", tools.YieldToolName},
	}

	// mcp:acemcp-go/search_context and custom_compact are not in static allToolNames(),
	// but are listed in parent's AllowedTools and availableTools.
	mcpTool := "mcp:acemcp-go/search_context"
	customTool := "custom_compact"

	derived := DeriveSubagentPermissions(ParentPermissionContext{
		AllowedTools: []string{"bash", "edit", "read", tools.YieldToolName, mcpTool, customTool},
	}, profile, []string{"bash", "edit", "read", tools.YieldToolName, mcpTool, customTool})

	allowed := toolNamesFromSet(derived.AllowedTools)
	assert.Contains(t, allowed, "bash")
	assert.Contains(t, allowed, mcpTool)
	assert.Contains(t, allowed, customTool)
}

func TestDeriveSubagentPermissionsFiltersUnallowedMCPAndPlugins(t *testing.T) {
	profile := SubagentProfile{
		Name:      config.AgentGeneral,
		Kind:      SubagentProfileGeneral,
		ToolNames: []string{"bash", "edit", "read", tools.YieldToolName},
	}

	mcpTool := "mcp:acemcp-go/search_context"
	customTool := "custom_compact"

	// parent's AllowedTools only allows basic tools, while availableTools has non-builtins.
	// The subagent should NOT be granted mcpTool or customTool.
	derived := DeriveSubagentPermissions(ParentPermissionContext{
		AllowedTools: []string{"bash", "edit", "read", tools.YieldToolName},
	}, profile, []string{"bash", "edit", "read", tools.YieldToolName, mcpTool, customTool})

	allowed := toolNamesFromSet(derived.AllowedTools)
	assert.Contains(t, allowed, "bash")
	assert.NotContains(t, allowed, mcpTool)
	assert.NotContains(t, allowed, customTool)
}

func TestShapeToolsForSubagentFiltersToolList(t *testing.T) {
	shaped := ShapeToolsForSubagent([]fantasy.AgentTool{
		tools.NewGlobTool("/tmp"),
		tools.NewReadTool(nil, nil, nil, "/tmp", config.ToolLs{}, nil),
		tools.NewEditTool(nil, nil, nil, nil, "/tmp"),
	}, SubagentToolProfile{Allowed: map[string]struct{}{"glob": {}, "read": {}}, Denied: map[string]struct{}{"edit": {}}})

	var names []string
	for _, tool := range shaped {
		names = append(names, tool.Info().Name)
	}
	assert.Equal(t, []string{"glob", "read"}, names)
}

func TestBuildToolsForPlanModeUsesReadOnlyCapabilities(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)

	coord := &coordinator{
		cfg:         cfg,
		sessions:    env.sessions,
		messages:    env.messages,
		permissions: env.permissions,
		userInput:   nil,
		history:     env.history,
		filetracker: *env.filetracker,
		lspManager:  lsp.NewManager(cfg),
	}

	planTools, err := coord.buildTools(t.Context(), cfg.Config().Agents[config.AgentCoder], session.CollaborationModePlan)
	require.NoError(t, err)

	planNames := make([]string, 0, len(planTools))
	for _, tool := range planTools {
		planNames = append(planNames, tool.Info().Name)
	}

	assert.Equal(t, []string{
		"glob",
		"grep",
		"lsp_declaration",
		"lsp_definition",
		"lsp_diagnostics",
		"lsp_document_symbols",
		"lsp_hover",
		"lsp_implementation",
		"lsp_references",
		"lsp_type_definition",
		"lsp_workspace_symbols",
		"memory_status",
		"plan_exit",
		"read",
		"recall",
		"reflect",
		"request_user_input",
	}, planNames)
	assert.NotContains(t, planNames, AgentToolName)
	assert.NotContains(t, planNames, "agentic_fetch")
	assert.NotContains(t, planNames, "bash")
	assert.NotContains(t, planNames, "fetch")
	assert.NotContains(t, planNames, "sourcegraph")
	assert.NotContains(t, planNames, "tool_search")
	assert.NotContains(t, planNames, "edit")
	assert.NotContains(t, planNames, tools.RetainToolName)
	assert.NotContains(t, planNames, "write")
	assert.NotContains(t, planNames, "todos")
}

func TestBuildToolsHonorsDisabledToolsInDefaultMode(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.Config().Options.DisabledTools = []string{"bash", "read"}

	coord := &coordinator{
		cfg:         cfg,
		sessions:    env.sessions,
		messages:    env.messages,
		permissions: env.permissions,
		userInput:   nil,
		history:     env.history,
		filetracker: *env.filetracker,
		lspManager:  lsp.NewManager(cfg),
	}

	defaultTools, err := coord.buildTools(t.Context(), cfg.Config().Agents[config.AgentCoder], session.CollaborationModeDefault)
	require.NoError(t, err)

	defaultNames := make([]string, 0, len(defaultTools))
	for _, tool := range defaultTools {
		defaultNames = append(defaultNames, tool.Info().Name)
	}

	assert.NotContains(t, defaultNames, "bash")
	assert.NotContains(t, defaultNames, "read")
	assert.Contains(t, defaultNames, "write")
}

func TestValidateExploreDelegationsBlocksFileContentRelay(t *testing.T) {
	message := validateExploreDelegations([]subagentTask{{
		Name: "relay",
		Assignment: "Read internal/agent/agent.go and internal/agent/coordinator.go " +
			"completely and return their full contents.",
		SubagentType: config.AgentExplore,
	}})

	require.NotEmpty(t, message)
	assert.Contains(t, message, "full-file content relay")
	assert.Contains(t, message, "direct file reads")
}

func TestValidateExploreDelegationsAllowsQuestionableExplorePrompts(t *testing.T) {
	tasks := []subagentTask{
		{
			Name:         "review-evidence",
			Assignment:   "Find changed files in the current diff and report file:line evidence for the primary agent to verify.",
			SubagentType: config.AgentExplore,
		},
		{
			Name:         "test-command-reference",
			Assignment:   "Find where go test ./internal/agent is documented. Do not run it.",
			SubagentType: config.AgentExplore,
		},
		{
			Name:         "fix-location",
			Assignment:   "Locate the code likely responsible for the fix in coordinator.go and return concise evidence.",
			SubagentType: config.AgentExplore,
		},
	}

	assert.Empty(t, validateExploreDelegations(tasks))
}

func TestValidateExploreDelegationsAllowsCodeLocationSearch(t *testing.T) {
	message := validateExploreDelegations([]subagentTask{{
		Name: "locate",
		Assignment: "Locate the files and symbols involved in MCP authentication state " +
			"rendering. Return concise file:line references and observed facts only.",
		SubagentType: config.AgentExplore,
	}})

	assert.Empty(t, message)
}

func TestValidateExploreDelegationsAllowsNegatedBuildTestLintInstruction(t *testing.T) {
	message := validateExploreDelegations([]subagentTask{{
		Name: "locate",
		Assignment: "Locate red frame layer implementation. Return key files and line references. " +
			"Do not run any build, test, lint, or non-git shell commands.",
		SubagentType: config.AgentExplore,
	}})

	assert.Empty(t, message)
}

func TestValidateExploreDelegationsAllowsChineseNegatedBuildTestLintInstruction(t *testing.T) {
	message := validateExploreDelegations([]subagentTask{{
		Name:         "locate",
		Assignment:   "定位红色图框图层实现。不要运行任何构建、测试、lint 或非 git shell 命令。",
		SubagentType: config.AgentExplore,
	}})

	assert.Empty(t, message)
}

func TestValidateExploreDelegationsAllowsChineseImplementationLookup(t *testing.T) {
	message := validateExploreDelegations([]subagentTask{{
		Name:         "locate",
		Assignment:   "定位 MCP 认证状态渲染的实现入口，只返回简洁的 file:line 证据。",
		SubagentType: config.AgentExplore,
	}})

	assert.Empty(t, message)
}

func TestValidateExploreDelegationsBlocksFinalReviewInExplore(t *testing.T) {
	message := validateExploreDelegations([]subagentTask{{
		Name:         "explore-review",
		Assignment:   "Review the current diff and decide whether it is safe to merge.",
		SubagentType: config.AgentExplore,
	}})

	require.NotEmpty(t, message)
	assert.Contains(t, message, "review` subagent")
}

func TestValidateReviewDelegationsBlocksImplementation(t *testing.T) {
	message := validateExploreDelegations([]subagentTask{{
		Name:         "review-fix",
		Assignment:   "Review the current diff and fix any bugs you find.",
		SubagentType: config.AgentReview,
	}})

	require.NotEmpty(t, message)
	assert.Contains(t, message, "read-only")
}

func TestValidateReviewDelegationsAllowsNegatedMutation(t *testing.T) {
	message := validateExploreDelegations([]subagentTask{{
		Name:         "review-only",
		Assignment:   "Review the current diff. Do not modify files or run tests.",
		SubagentType: config.AgentReview,
	}})

	assert.Empty(t, message)
}

func TestValidatePlanAndLibrarianDelegationsBlockMutation(t *testing.T) {
	planMessage := validateExploreDelegations([]subagentTask{{
		Name:         "plan-fix",
		Assignment:   "Plan and implement the fix.",
		SubagentType: config.AgentPlan,
	}})
	require.NotEmpty(t, planMessage)
	assert.Contains(t, planMessage, "architecture planning")

	librarianMessage := validateExploreDelegations([]subagentTask{{
		Name:         "library-fix",
		Assignment:   "Check the library API and edit the integration.",
		SubagentType: config.AgentLibrarian,
	}})
	require.NotEmpty(t, librarianMessage)
	assert.Contains(t, librarianMessage, "dependency/API research")
}

func TestValidateExploreDelegationsOnlyAppliesToExplore(t *testing.T) {
	message := validateExploreDelegations([]subagentTask{{
		Name:         "general-review",
		Assignment:   "Review the current diff and decide whether it is safe to merge.",
		SubagentType: config.AgentGeneral,
	}})

	assert.Empty(t, message)
}

func TestValidateSubagentDelegationsPreservesConfiguredAliasNames(t *testing.T) {
	agents := map[string]config.Agent{
		"reviewer": {Name: "reviewer", Mode: config.AgentModeSubagent},
	}

	message := validateSubagentDelegations([]subagentTask{{
		Name:         "custom-reviewer",
		Assignment:   "Review the current diff and fix any bugs you find.",
		SubagentType: "reviewer",
	}}, agents)

	assert.Empty(t, message)
}

func runAgentToolForTest(t *testing.T, tool fantasy.AgentTool, params AgentParams) (fantasy.ToolResponse, error) {
	t.Helper()
	input, err := json.Marshal(params)
	require.NoError(t, err)

	ctx := context.WithValue(context.Background(), tools.SessionIDContextKey, "session-1")
	ctx = context.WithValue(ctx, tools.MessageIDContextKey, "msg-1")
	return tool.Run(ctx, fantasy.ToolCall{ID: "call-1", Name: AgentToolName, Input: string(input)})
}

func TestAgentToolParsesTasksCorrectly(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)

	coord := &coordinator{cfg: cfg}
	coord.subAgentFactory = func(_ context.Context, requestedType string) (SessionAgent, config.Agent, error) {
		return &mockSessionAgent{}, config.Agent{Name: config.RequestedSubagentID(requestedType), Mode: config.AgentModeSubagent}, nil
	}

	tool, err := coord.agentTool(t.Context())
	require.NoError(t, err)

	input, err := json.Marshal(AgentParams{Tasks: []AgentTaskParams{
		{Name: "collect", Assignment: "collect info", SubagentType: "explore", Description: "Collect"},
		{Name: "summarize", Assignment: "summarize info", SubagentType: "general"},
	}})
	require.NoError(t, err)

	ctx := context.WithValue(context.Background(), tools.SessionIDContextKey, "session-1")
	ctx = context.WithValue(ctx, tools.MessageIDContextKey, "msg-1")
	_, _ = tool.Run(ctx, fantasy.ToolCall{ID: "call-1", Name: AgentToolName, Input: string(input)})
}

func TestAgentToolParsesSinglePromptCorrectly(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)

	coord := &coordinator{cfg: cfg}
	coord.subAgentFactory = func(_ context.Context, requestedType string) (SessionAgent, config.Agent, error) {
		return &mockSessionAgent{}, config.Agent{Name: config.RequestedSubagentID(requestedType), Mode: config.AgentModeSubagent}, nil
	}

	tool, err := coord.agentTool(t.Context())
	require.NoError(t, err)

	input, err := json.Marshal(AgentParams{Prompt: "fix issue", SubagentType: "general"})
	require.NoError(t, err)

	ctx := context.WithValue(context.Background(), tools.SessionIDContextKey, "session-1")
	ctx = context.WithValue(ctx, tools.MessageIDContextKey, "msg-1")
	_, _ = tool.Run(ctx, fantasy.ToolCall{ID: "call-1", Name: AgentToolName, Input: string(input)})
}

func TestTaskGraphSemaphoreUsesGlobalRuntimeLimit(t *testing.T) {
	semaphores := make(map[string]chan struct{})
	var semMu sync.Mutex

	semaphore := subagentSemaphoreForAgent(
		config.Agent{Name: config.AgentGeneral, Mode: config.AgentModeSubagent},
		config.SubagentRuntimeConfig{MaxConcurrency: 2},
		semaphores,
		&semMu,
	)

	require.NotNil(t, semaphore)
	require.Equal(t, 2, cap(semaphore))
	assert.True(t, semaphore == subagentSemaphoreForAgent(
		config.Agent{Name: config.AgentExplore, Mode: config.AgentModeSubagent},
		config.SubagentRuntimeConfig{MaxConcurrency: 2},
		semaphores,
		&semMu,
	))
}

func TestTaskGraphSemaphoreAgentLimitOverridesRuntimeLimit(t *testing.T) {
	semaphores := make(map[string]chan struct{})
	var semMu sync.Mutex
	limit := 1

	semaphore := subagentSemaphoreForAgent(
		config.Agent{
			Name: config.AgentGeneral,
			Mode: config.AgentModeSubagent,
			TaskGovernance: &config.TaskGovernance{
				MaxConcurrent: &limit,
			},
		},
		config.SubagentRuntimeConfig{MaxConcurrency: 4},
		semaphores,
		&semMu,
	)

	require.NotNil(t, semaphore)
	require.Equal(t, 1, cap(semaphore))
}

func TestTaskGraphSemaphoreKeepsCustomAliasAgentBucket(t *testing.T) {
	semaphores := make(map[string]chan struct{})
	var semMu sync.Mutex
	customLimit := 1
	builtInLimit := 2

	custom := subagentSemaphoreForAgent(
		config.Agent{ID: "reviewer", Mode: config.AgentModeSubagent, TaskGovernance: &config.TaskGovernance{MaxConcurrent: &customLimit}},
		config.SubagentRuntimeConfig{MaxConcurrency: 4},
		semaphores,
		&semMu,
	)
	builtIn := subagentSemaphoreForAgent(
		config.Agent{ID: config.AgentReview, Mode: config.AgentModeSubagent, TaskGovernance: &config.TaskGovernance{MaxConcurrent: &builtInLimit}},
		config.SubagentRuntimeConfig{MaxConcurrency: 4},
		semaphores,
		&semMu,
	)

	require.NotNil(t, custom)
	require.NotNil(t, builtIn)
	require.Equal(t, 1, cap(custom))
	require.Equal(t, 2, cap(builtIn))
}
