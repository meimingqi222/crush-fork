package agent

import (
	"context"
	"encoding/json"
	"strings"
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
		"reviewer": {
			Mode:         config.AgentModeSubagent,
			Description:  "Reviews changes before handoff.",
			AllowedTools: []string{"view"},
		},
	}
	cfg.SetupAgents()

	coord := &coordinator{cfg: cfg}

	agentCfg, err := coord.subagentConfig("reviewer")
	require.NoError(t, err)
	assert.Equal(t, "reviewer", agentCfg.ID)
	assert.Equal(t, []string{"view"}, agentCfg.AllowedTools)
}

func TestBuildAgentToolDescriptionDeduplicatesExploreAlias(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)

	coord := &coordinator{cfg: cfg}
	description := coord.buildAgentToolDescription()

	assert.Contains(t, description, "- general:")
	assert.Contains(t, description, "- explore:")
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
	assert.Equal(t, []string{"bash", "glob", "grep", "tool_search", "view"}, exploreNames)
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
		taskGraphTask{ID: "task", SubagentType: config.AgentExplore},
		agentCfg,
		ParentPermissionContext{SessionID: "parent-session", AllowedTools: []string{tools.ViewToolName, tools.ToolSearchToolName}},
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
	assert.Equal(t, []string{tools.SubagentFinishToolName, tools.ToolSearchToolName, tools.ViewToolName}, names)
}

func TestDeriveSubagentPermissionsDenyWins(t *testing.T) {
	profile := SubagentProfile{
		Name:      config.AgentGeneral,
		Kind:      SubagentProfileGeneral,
		ToolNames: []string{"bash", "edit", "view", tools.SubagentFinishToolName},
	}

	derived := DeriveSubagentPermissions(ParentPermissionContext{
		AllowedTools: []string{"bash", "edit", "view", tools.SubagentFinishToolName},
		DeniedTools:  []string{"edit"},
	}, profile, []string{"bash", "edit", "view", tools.SubagentFinishToolName})

	assert.Contains(t, toolNamesFromSet(derived.AllowedTools), "bash")
	assert.Contains(t, toolNamesFromSet(derived.AllowedTools), "view")
	assert.Contains(t, toolNamesFromSet(derived.AllowedTools), tools.SubagentFinishToolName)
	assert.NotContains(t, toolNamesFromSet(derived.AllowedTools), "edit")
	assert.Contains(t, toolNamesFromSet(derived.DeniedTools), "edit")
}

func TestShapeToolsForSubagentFiltersToolList(t *testing.T) {
	shaped := ShapeToolsForSubagent([]fantasy.AgentTool{
		tools.NewGlobTool("/tmp"),
		tools.NewViewTool(nil, nil, nil, "/tmp", config.ToolLs{}),
		tools.NewEditTool(nil, nil, nil, nil, "/tmp"),
	}, SubagentToolProfile{Allowed: map[string]struct{}{"glob": {}, "view": {}}, Denied: map[string]struct{}{"edit": {}}})

	var names []string
	for _, tool := range shaped {
		names = append(names, tool.Info().Name)
	}
	assert.Equal(t, []string{"glob", "view"}, names)
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
		"recall",
		"reflect",
		"request_user_input",
		"view",
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
	cfg.Config().Options.DisabledTools = []string{"bash", "fetch"}

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
	assert.NotContains(t, defaultNames, "fetch")
	assert.Contains(t, defaultNames, "view")
	assert.Contains(t, defaultNames, "write")
}

func TestValidateExploreDelegationsBlocksFileContentRelay(t *testing.T) {
	message := validateExploreDelegations([]taskGraphTask{{
		ID: "relay",
		Prompt: "Read internal/agent/agent.go and internal/agent/coordinator.go " +
			"completely and return their full contents.",
		SubagentType: config.AgentExplore,
	}})

	require.NotEmpty(t, message)
	assert.Contains(t, message, "full-file content relay")
	assert.Contains(t, message, "direct file reads")
}

func TestValidateExploreDelegationsAllowsQuestionableExplorePrompts(t *testing.T) {
	tasks := []taskGraphTask{
		{
			ID:           "review-evidence",
			Prompt:       "Review the current diff and report file:line evidence for the primary agent to verify.",
			SubagentType: config.AgentExplore,
		},
		{
			ID:           "test-command-reference",
			Prompt:       "Find where go test ./internal/agent is documented. Do not run it.",
			SubagentType: config.AgentExplore,
		},
		{
			ID:           "fix-location",
			Prompt:       "Locate the code likely responsible for the fix in coordinator.go and return concise evidence.",
			SubagentType: config.AgentExplore,
		},
	}

	assert.Empty(t, validateExploreDelegations(tasks))
}

func TestValidateExploreDelegationsAllowsCodeLocationSearch(t *testing.T) {
	message := validateExploreDelegations([]taskGraphTask{{
		ID: "locate",
		Prompt: "Locate the files and symbols involved in MCP authentication state " +
			"rendering. Return concise file:line references and observed facts only.",
		SubagentType: config.AgentExplore,
	}})

	assert.Empty(t, message)
}

func TestValidateExploreDelegationsAllowsNegatedBuildTestLintInstruction(t *testing.T) {
	message := validateExploreDelegations([]taskGraphTask{{
		ID: "locate",
		Prompt: "Locate red frame layer implementation. Return key files and line references. " +
			"Do not run any build, test, lint, or non-git shell commands.",
		SubagentType: config.AgentExplore,
	}})

	assert.Empty(t, message)
}

func TestValidateExploreDelegationsAllowsChineseNegatedBuildTestLintInstruction(t *testing.T) {
	message := validateExploreDelegations([]taskGraphTask{{
		ID:           "locate",
		Prompt:       "定位红色图框图层实现。不要运行任何构建、测试、lint 或非 git shell 命令。",
		SubagentType: config.AgentExplore,
	}})

	assert.Empty(t, message)
}

func TestValidateExploreDelegationsAllowsChineseImplementationLookup(t *testing.T) {
	message := validateExploreDelegations([]taskGraphTask{{
		ID:           "locate",
		Prompt:       "定位 MCP 认证状态渲染的实现入口，只返回简洁的 file:line 证据。",
		SubagentType: config.AgentExplore,
	}})

	assert.Empty(t, message)
}

func TestValidateExploreDelegationsOnlyAppliesToExplore(t *testing.T) {
	message := validateExploreDelegations([]taskGraphTask{{
		ID:           "general-review",
		Prompt:       "Review the current diff and decide whether it is safe to merge.",
		SubagentType: config.AgentGeneral,
	}})

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

func TestAgentToolUsesTaskGraphWhenTasksProvided(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)

	coord := &coordinator{cfg: cfg}
	called := false
	coord.taskGraphScheduler = func(_ context.Context, params taskGraphParams) (fantasy.ToolResponse, error) {
		called = true
		require.Equal(t, "session-1", params.SessionID)
		require.Equal(t, "msg-1", params.AgentMessageID)
		require.Equal(t, "call-1", params.ToolCallID)
		require.Len(t, params.Tasks, 2)
		require.Equal(t, "fetch", params.Tasks[0].ID)
		require.Equal(t, []string{"fetch"}, params.Tasks[1].DependsOn)
		return fantasy.NewTextResponse("graph"), nil
	}

	tool, err := coord.agentTool(t.Context())
	require.NoError(t, err)

	resp, err := runAgentToolForTest(t, tool, AgentParams{Tasks: []AgentTaskParams{
		{ID: "fetch", Prompt: "fetch info", SubagentType: "explore", Description: "Fetch"},
		{ID: "summarize", Prompt: "summarize info", SubagentType: "general", DependsOn: []string{"fetch"}},
	}})
	require.NoError(t, err)
	require.True(t, called)
	require.Equal(t, "graph", resp.Content)
}

func TestAgentToolKeepsSinglePromptPathCompatible(t *testing.T) {
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)

	coord := &coordinator{cfg: cfg}
	coord.subAgentFactory = func(_ context.Context, requestedType string) (SessionAgent, config.Agent, error) {
		return &mockSessionAgent{}, config.Agent{ID: config.CanonicalSubagentID(requestedType), Mode: config.AgentModeSubagent}, nil
	}

	called := false
	coord.taskGraphScheduler = func(_ context.Context, params taskGraphParams) (fantasy.ToolResponse, error) {
		called = true
		require.Equal(t, "call-1", params.ToolCallID)
		require.Len(t, params.Tasks, 1)
		require.Equal(t, "task", params.Tasks[0].ID)
		require.Equal(t, "fix issue", params.Tasks[0].Prompt)
		return fantasy.NewTextResponse("single"), nil
	}

	tool, err := coord.agentTool(t.Context())
	require.NoError(t, err)

	resp, err := runAgentToolForTest(t, tool, AgentParams{Prompt: "fix issue", SubagentType: "general"})
	require.NoError(t, err)
	require.True(t, called)
	require.Equal(t, "single", resp.Content)
}
