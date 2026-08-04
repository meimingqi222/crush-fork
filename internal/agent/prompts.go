package agent

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/charmbracelet/crush/internal/agent/prompt"
	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
)

//go:embed templates/coder.md.tpl
var coderPromptTmpl []byte

//go:embed templates/explore.md.tpl
var explorePromptTmpl []byte

//go:embed templates/initialize.md.tpl
var initializePromptTmpl []byte

const subagentPromptSuffix = `

<subagent_mode>
You are running as a delegated subagent, not as the primary orchestrator.

Subagent rules:
- Stay inside the delegated scope and complete the bounded task directly with the tools available to you.
- Do not behave like the primary orchestrator or claim that you will spin up other subagents.
- Follow the configured role instructions below when they are present.
- When you are finished, call the yield tool exactly once with the appropriate status, data (the complete, unabridged result text), error, or payload. Calling yield will automatically submit your results and terminate your execution.

Context handoff:
- You may receive a <parent_context> block in your prompt. This contains information the orchestrating agent gathered before delegating to you.
- Use it as background context to understand what has already been discovered and avoid redundant exploration.
- The <parent_context> may include the original user request, recent reasoning, and key actions (tool calls) from the parent session.
- Do not blindly trust parent context for exact values; verify details with your own tool calls when precision matters.
- If a <parent_history_file> tag is present, it points to a condensed index of the full parent conversation. Use your read tool to inspect it when you need context beyond what <parent_context> provides.
</subagent_mode>`

func coderPrompt(opts ...prompt.Option) (*prompt.Prompt, error) {
	systemPrompt, err := prompt.NewPrompt("coder", string(coderPromptTmpl), opts...)
	if err != nil {
		return nil, err
	}
	return systemPrompt, nil
}

func coderPromptForAgent(agentCfg config.Agent, opts ...prompt.Option) (*prompt.Prompt, error) {
	systemPrompt, err := prompt.NewPrompt("coder", buildPrimaryAgentPromptTemplate(string(coderPromptTmpl), agentCfg), promptOptionsForAgent(agentCfg, opts...)...)
	if err != nil {
		return nil, err
	}
	return systemPrompt, nil
}

func promptOptionsForAgent(agentCfg config.Agent, opts ...prompt.Option) []prompt.Option {
	merged := make([]prompt.Option, 0, len(opts)+3)
	merged = append(merged, opts...)
	if len(agentCfg.ContextPaths) > 0 {
		merged = append(merged, prompt.WithContextPathsOverride(agentCfg.ContextPaths))
	}
	if agentCfg.OmitContextFiles {
		merged = append(merged, prompt.WithOmitProjectContextFiles(true), prompt.WithDisableGlobalContextFile(true))
	}
	merged = append(merged, prompt.WithHasBashTool(slices.Contains(agentCfg.AllowedTools, agenttools.BashToolName)))
	return merged
}

func generalPrompt(agentCfg config.Agent, opts ...prompt.Option) (*prompt.Prompt, error) {
	promptOptions := promptOptionsForAgent(agentCfg, opts...)
	systemPrompt, err := prompt.NewPrompt("general", buildSubagentPromptTemplate(string(coderPromptTmpl), agentCfg), promptOptions...)
	if err != nil {
		return nil, err
	}
	return systemPrompt, nil
}

func explorePrompt(agentCfg config.Agent, opts ...prompt.Option) (*prompt.Prompt, error) {
	promptOptions := promptOptionsForAgent(agentCfg, opts...)
	systemPrompt, err := prompt.NewPrompt("explore", buildSubagentPromptTemplate(string(explorePromptTmpl), agentCfg), promptOptions...)
	if err != nil {
		return nil, err
	}
	return systemPrompt, nil
}

func reviewPrompt(agentCfg config.Agent, opts ...prompt.Option) (*prompt.Prompt, error) {
	promptOptions := promptOptionsForAgent(agentCfg, opts...)
	systemPrompt, err := prompt.NewPrompt("review", buildSubagentPromptTemplate(reviewPromptTemplate, agentCfg), promptOptions...)
	if err != nil {
		return nil, err
	}
	return systemPrompt, nil
}

const reviewPromptTemplate = `You are a read-only code review subagent.

<workspace_context>
The current working directory is {{.WorkingDir}}.
Resolve relative paths and commands from this directory; do not prefix a path with the current directory name again. Ground uncertain paths with Glob, directory listing, or tool output before reading them.
</workspace_context>

<role>
Act as the final code-review specialist for the delegated scope: inspect diffs,
nearby implementation, tests, and relevant contracts, then report only concrete
bugs, regressions, correctness issues, or security concerns that are supported
by evidence.
</role>

<limits>
- Do not edit files or suggest that you changed files.
- Do not run build, test, lint, package-manager, server, reproduction, or other
  non-git shell commands.
- Do not approve changes blindly; if no concrete issues are found, say so.
- Prefer patch-anchored findings with file:line references, impact, and a clear
  fix direction.
</limits>`

func buildSubagentPromptTemplate(baseTemplate string, agentCfg config.Agent) string {
	sections := []string{strings.TrimSpace(baseTemplate), strings.TrimSpace(subagentPromptSuffix)}
	if readOnlyPrompt := buildReadOnlyPrompt(agentCfg); readOnlyPrompt != "" {
		sections = append(sections, readOnlyPrompt)
	}
	if lifecyclePrompt := buildAgentLifecyclePrompt(agentCfg); lifecyclePrompt != "" {
		sections = append(sections, lifecyclePrompt)
	}
	if initialPrompt := strings.TrimSpace(agentCfg.InitialPrompt); initialPrompt != "" {
		sections = append(sections, fmt.Sprintf("<initial_prompt>\n%s\n</initial_prompt>", initialPrompt))
	}
	if rolePrompt := buildSubagentRolePrompt(agentCfg); rolePrompt != "" {
		sections = append(sections, rolePrompt)
	}
	if extraPrompt := strings.TrimSpace(agentCfg.AdditionalPrompt); extraPrompt != "" {
		sections = append(sections, fmt.Sprintf("<additional_prompt>\n%s\n</additional_prompt>", extraPrompt))
	}
	if spawnsPrompt := buildSpawnsPrompt(agentCfg); spawnsPrompt != "" {
		sections = append(sections, spawnsPrompt)
	}
	if schemaPrompt := buildOutputSchemaPrompt(agentCfg); schemaPrompt != "" {
		sections = append(sections, schemaPrompt)
	}
	return strings.Join(sections, "\n\n")
}

func buildPrimaryAgentPromptTemplate(baseTemplate string, agentCfg config.Agent) string {
	sections := []string{strings.TrimSpace(baseTemplate)}
	if lifecyclePrompt := buildAgentLifecyclePrompt(agentCfg); lifecyclePrompt != "" {
		sections = append(sections, lifecyclePrompt)
	}
	if initialPrompt := strings.TrimSpace(agentCfg.InitialPrompt); initialPrompt != "" {
		sections = append(sections, fmt.Sprintf("<initial_prompt>\n%s\n</initial_prompt>", initialPrompt))
	}
	return strings.Join(sections, "\n\n")
}

func buildAgentLifecyclePrompt(agentCfg config.Agent) string {
	memoryPolicy := strings.TrimSpace(agentCfg.Memory)
	isolationPolicy := strings.TrimSpace(agentCfg.Isolation)
	backgroundConfigured := agentCfg.Background != nil
	if memoryPolicy == "" && isolationPolicy == "" && !backgroundConfigured {
		return ""
	}

	lines := []string{"<agent_lifecycle>"}
	if backgroundConfigured {
		lines = append(lines, fmt.Sprintf("background: %t", *agentCfg.Background))
	}
	if memoryPolicy != "" {
		lines = append(lines, fmt.Sprintf("memory: %s", memoryPolicy))
	}
	if isolationPolicy != "" {
		lines = append(lines, fmt.Sprintf("isolation: %s", isolationPolicy))
	}
	lines = append(lines, "</agent_lifecycle>")
	return strings.Join(lines, "\n")
}

func buildReadOnlyPrompt(agentCfg config.Agent) string {
	if !isReadOnlyAgent(agentCfg) {
		return ""
	}
	return `<read_only_boundary>
You are running as a read-only subagent. You may only use read and search tools to gather evidence. You MUST NOT modify files, edit code, write files, download assets, update todos, retain memory, send messages, stop tasks, restart LSPs, spawn subagents, or invoke any other state-mutating tool. If a requested action would require a state change, report that limitation instead of attempting it.
</read_only_boundary>`
}

func buildSubagentRolePrompt(agentCfg config.Agent) string {
	switch strings.ToLower(strings.TrimSpace(agentCfg.Role)) {
	case "planner":
		return `<subagent_role>
Role: planner
- Act as the planner: inspect the delegated problem, identify the relevant code or evidence, and return an execution-ready plan or decision support.
- Prefer sequencing, risk analysis, and clear next actions over speculative implementation.
</subagent_role>`
	case "researcher":
		return `<subagent_role>
Role: researcher
- Act as the researcher: gather source-backed context, locate relevant files and symbols, and return concise evidence for the primary agent to analyze.
- Prefer file:line references, observed facts, and open questions over final review judgments or implementation.
</subagent_role>`
	case "reviewer":
		return `<subagent_role>
Role: reviewer
- Act as the reviewer: inspect code, diffs, or outputs, validate assumptions, and call out issues or approvals clearly.
- Prefer verification and concise findings; only make code changes when the delegated task explicitly asks for a fix.
</subagent_role>`
	case "executor":
		return `<subagent_role>
Role: executor
- Act as the executor: implement the delegated task directly, run the most relevant verification you can, and report concrete results.
- Prefer finishing the scoped change over broad exploration or replanning.
</subagent_role>`
	case "":
		return ""
	default:
		return fmt.Sprintf("<subagent_role>\nRole: %s\n- Follow this role while staying within the delegated task and the tools available to you.\n</subagent_role>", strings.TrimSpace(agentCfg.Role))
	}
}

func buildSpawnsPrompt(agentCfg config.Agent) string {
	if len(agentCfg.Spawns) == 0 {
		return ""
	}
	spawnList := strings.Join(agentCfg.Spawns, ", ")
	return fmt.Sprintf("<spawns>\nYou can spawn these subagent types for parallel evidence gathering: %s\nUse the `agent` tool with subagent_type set to one of these values.\n</spawns>", spawnList)
}

// buildOutputSchemaPrompt injects the agent's OutputSchema into the system
// prompt so the subagent knows what structure its yield payload must conform
// to.  Without this, the subagent only sees the generic yield tool description
// and has no idea what fields the schema requires.
func buildOutputSchemaPrompt(agentCfg config.Agent) string {
	if agentCfg.OutputSchema == nil {
		return ""
	}
	schemaBytes, err := json.MarshalIndent(agentCfg.OutputSchema, "", "  ")
	if err != nil || len(schemaBytes) < 3 {
		return ""
	}
	return fmt.Sprintf(`<output_schema>
Your yield payload MUST conform to this JSON schema.
Use the yield tool's "payload" field (not "data") for structured results.

%s
</output_schema>`, string(schemaBytes))
}

func promptForAgent(agentCfg config.Agent, isSubAgent bool, opts ...prompt.Option) (*prompt.Prompt, error) {
	if !isSubAgent {
		return coderPromptForAgent(agentCfg, opts...)
	}

	switch agentCfg.ID {
	case config.AgentReview:
		return reviewPrompt(agentCfg, opts...)
	case config.AgentExplore, config.AgentPlan, config.AgentLibrarian:
		return explorePrompt(agentCfg, opts...)
	case config.AgentCoder, config.AgentGeneral:
		return generalPrompt(agentCfg, opts...)
	default:
		if isReadOnlyAgent(agentCfg) {
			return explorePrompt(agentCfg, opts...)
		}
		return generalPrompt(agentCfg, opts...)
	}
}

func isReadOnlyAgent(agentCfg config.Agent) bool {
	if len(agentCfg.AllowedTools) == 0 {
		if strings.TrimSpace(agentCfg.ID) == "" {
			return false
		}
		return isReadOnlyAgentID(config.RequestedSubagentID(agentCfg.ID))
	}
	readOnlyTools := map[string]struct{}{
		agenttools.GlobToolName:        {},
		agenttools.GrepToolName:        {},
		agenttools.ReadToolName:        {},
		agenttools.SourcegraphToolName: {},
		agenttools.ToolSearchToolName:  {},
		agenttools.LSPToolName:         {},
		agenttools.YieldToolName:       {},
	}
	for _, tool := range agentCfg.AllowedTools {
		if _, ok := readOnlyTools[tool]; !ok {
			return false
		}
	}
	return true
}

func InitializePrompt(cfg *config.ConfigStore) (string, error) {
	systemPrompt, err := prompt.NewPrompt("initialize", string(initializePromptTmpl))
	if err != nil {
		return "", err
	}
	return systemPrompt.Build(context.Background(), "", "", cfg)
}
