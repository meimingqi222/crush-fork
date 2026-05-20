package agent

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"unicode"

	"charm.land/fantasy"

	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/agent/tools"

	"github.com/charmbracelet/crush/internal/config"
)

//go:embed templates/agent_tool.md
var agentToolDescription []byte

type AgentTaskParams struct {
	Name         string `json:"name,omitempty" description:"A short identifier for the task (e.g. AuthLoader, FixTests)"`
	Description  string `json:"description,omitempty" description:"A short title for the delegated task"`
	Assignment   string `json:"assignment" description:"The full task instructions for the subagent to perform"`
	SubagentType string `json:"subagent_type,omitempty" description:"The subagent type to use: general, quick_task, explore, plan, review, designer, librarian, or a configured subagent name"`
	OutputSchema any    `json:"output_schema,omitempty" description:"Expected structured output schema for this task"`
}

type AgentParams struct {
	Description     string            `json:"description,omitempty" description:"A short title for the delegated task"`
	Prompt          string            `json:"prompt,omitempty" description:"The task for the agent to perform"`
	SubagentType    string            `json:"subagent_type,omitempty" description:"The subagent type to use: general, quick_task, explore, plan, review, designer, librarian, or a configured subagent name"`
	Tasks           []AgentTaskParams `json:"tasks,omitempty" description:"Optional list of independent tasks to execute in parallel"`
	Context         string            `json:"context,omitempty" description:"Shared background information for all subagents"`
	RunInBackground bool              `json:"run_in_background,omitempty" description:"Run the agent in the background and return immediately with an agent ID"`
	ModelPriority   []string          `json:"model_priority,omitempty" description:"Override model priority for this invocation"`
}

const (
	AgentToolName = "agent"
)

func (c *coordinator) agentTool(_ context.Context) (fantasy.AgentTool, error) {
	return fantasy.NewParallelAgentTool(
		AgentToolName,
		c.buildAgentToolDescription(),
		func(ctx context.Context, params AgentParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			sessionID := tools.GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, errors.New("session id missing from context")
			}

			agentMessageID := tools.GetMessageFromContext(ctx)
			if agentMessageID == "" {
				return fantasy.ToolResponse{}, errors.New("agent message id missing from context")
			}

			slog.Info("Agent tool invoked",
				"has_prompt", params.Prompt != "",
				"has_description", params.Description != "",
				"task_count", len(params.Tasks),
				"subagent_type", params.SubagentType,
				"run_in_background", params.RunInBackground,
			)

			var tasks []subagentTask
			if len(params.Tasks) > 0 {
				for i, task := range params.Tasks {
					assignment := strings.TrimSpace(task.Assignment)
					if assignment == "" {
						slog.Warn("Agent task missing assignment",
							"task_index", i,
							"task_name", task.Name,
							"description", task.Description,
						)
						return fantasy.NewTextErrorResponse(fmt.Sprintf(
							"task %d is missing the required \"assignment\" field. "+
								"The \"assignment\" field must contain the full task instructions. "+
								"The \"description\" field (currently %q) is only a short display label. "+
								"Please retry with an \"assignment\" field containing the detailed task instructions.",
							i, task.Description,
						)), nil
					}
					name := strings.TrimSpace(task.Name)
					if name == "" {
						name = fmt.Sprintf("task_%d", i)
					}
					description := strings.TrimSpace(task.Description)
					if description == "" {
						subagentType := config.RequestedSubagentID(strings.TrimSpace(task.SubagentType))
						description = defaultSubagentDescription(subagentType, assignment)
					}
					tasks = append(tasks, subagentTask{
						Name:         name,
						Description:  description,
						Assignment:   assignment,
						SubagentType: task.SubagentType,
						OutputSchema: task.OutputSchema,
					})
				}
			} else {
				if params.Prompt == "" {
					slog.Warn("Agent tool missing prompt",
						"description", params.Description,
					)
					return fantasy.NewTextErrorResponse(
						"The \"prompt\" field is required but was not provided. " +
							"The \"prompt\" field must contain the full task instructions for the subagent. " +
							"The \"description\" field is only a short display label shown in the UI. " +
							"Please retry with a \"prompt\" field containing the detailed task instructions.",
					), nil
				}
				description := strings.TrimSpace(params.Description)
				if description == "" {
					subagentType := config.RequestedSubagentID(strings.TrimSpace(params.SubagentType))
					description = defaultSubagentDescription(subagentType, params.Prompt)
				}
				tasks = []subagentTask{{
					Name:         "task",
					Description:  description,
					Assignment:   params.Prompt,
					SubagentType: params.SubagentType,
				}}
			}

			if message := c.validateSubagentDelegations(tasks); message != "" {
				return fantasy.NewTextErrorResponse(message), nil
			}

			return c.runSubagents(ctx, subagentBatchParams{
				SessionID:       sessionID,
				AgentMessageID:  agentMessageID,
				ToolCallID:      call.ID,
				Tasks:           tasks,
				Context:         params.Context,
				RunInBackground: params.RunInBackground,
				ModelPriority:   params.ModelPriority,
			})
		}), nil
}

func (c *coordinator) buildAgentToolDescription() string {
	subagents := make([]config.Agent, 0)
	seen := make(map[string]struct{})
	for _, agentCfg := range c.cfg.Config().Agents {
		if config.NormalizeAgentMode(agentCfg.Mode) == config.AgentModePrimary {
			continue
		}
		canonicalID := config.ResolveSubagentID(c.cfg.Config().Agents, agentCfg.ID)
		if _, ok := seen[canonicalID]; ok {
			continue
		}
		seen[canonicalID] = struct{}{}
		subagents = append(subagents, agentCfg)
	}
	slices.SortFunc(subagents, func(a, b config.Agent) int {
		return strings.Compare(a.ID, b.ID)
	})

	entries := make([]string, 0, len(subagents))
	for _, agentCfg := range subagents {
		entries = append(entries, fmt.Sprintf("- %s: %s", config.CanonicalSubagentID(agentCfg.ID), agentCfg.Description))
	}

	return strings.ReplaceAll(string(agentToolDescription), "{agents}", strings.Join(entries, "\n"))
}

func (c *coordinator) validateSubagentDelegations(tasks []subagentTask) string {
	return validateSubagentDelegations(tasks, c.cfg.Config().Agents)
}

func validateExploreDelegations(tasks []subagentTask) string {
	return validateSubagentDelegations(tasks, nil)
}

func validateSubagentDelegations(tasks []subagentTask, agents map[string]config.Agent) string {
	for _, task := range tasks {
		subagentType := config.ResolveSubagentID(agents, strings.TrimSpace(task.SubagentType))
		if subagentType == config.AgentExplore {
			if reason := exploreDelegationViolation(task.Assignment); reason != "" {
				return fmt.Sprintf(
					"task %q cannot use `explore`: %s\n\n"+
						"`explore` is only for quick codebase discovery: locating relevant files, symbols, call chains, "+
						"and concise file:line evidence. Use direct primary reads for full-file reads, `review` for final code review, "+
						"and `general` for implementation, reproduction, build, test, lint, or other execution tasks.",
					task.Name,
					reason,
				)
			}
		}
		if subagentType == config.AgentReview {
			if reason := reviewDelegationViolation(task.Assignment); reason != "" {
				return fmt.Sprintf("task %q cannot use `review`: %s", task.Name, reason)
			}
		}
		if subagentType == config.AgentPlan {
			if reason := planDelegationViolation(task.Assignment); reason != "" {
				return fmt.Sprintf("task %q cannot use `plan`: %s", task.Name, reason)
			}
		}
		if subagentType == config.AgentLibrarian {
			if reason := librarianDelegationViolation(task.Assignment); reason != "" {
				return fmt.Sprintf("task %q cannot use `librarian`: %s", task.Name, reason)
			}
		}
	}
	return ""
}

func exploreDelegationViolation(prompt string) string {
	text := strings.ToLower(prompt)
	if containsAnyLower(text, []string{
		"return their full contents",
		"return the full contents",
		"return full contents",
		"full file contents",
		"read full contents",
		"read the full",
		"read these files completely",
		"read this file completely",
		"完整读取",
		"读取完整",
		"返回完整内容",
		"完整内容",
		"全文返回",
	}) {
		return "full-file content relay belongs in the primary agent with direct file reads"
	}
	if looksLikeFinalReview(text) {
		return "final code review belongs in the `review` subagent, not `explore`"
	}
	return ""
}

func reviewDelegationViolation(prompt string) string {
	text := strings.ToLower(prompt)
	if containsAnyUnnegatedLower(text, mutatingOrTestMarkers()) {
		return "`review` is read-only and for code-review findings; use `general` for fixes or test execution"
	}
	return ""
}

func planDelegationViolation(prompt string) string {
	text := strings.ToLower(prompt)
	if containsAnyUnnegatedLower(text, mutatingOrTestMarkers()) {
		return "`plan` is read-only and for architecture planning; use `general` for fixes or test execution"
	}
	return ""
}

func librarianDelegationViolation(prompt string) string {
	text := strings.ToLower(prompt)
	if containsAnyUnnegatedLower(text, mutatingOrTestMarkers()) {
		return "`librarian` is read-only and for source-verified dependency/API research; use `general` for fixes"
	}
	return ""
}

func mutatingOrTestMarkers() []string {
	return []string{
		"implement the",
		"implement a",
		"implement this",
		"modify code",
		"modify file",
		"modify the",
		"edit file",
		"edit the",
		"write code",
		"write file",
		"write the",
		"fix bug",
		"fix the",
		"fix any",
		"run tests",
		"go test",
		"npm test",
		"pytest",
		"cargo test",
		"执行测试",
		"运行测试",
		"修改代码",
		"修改文件",
		"实现修复",
		"修复问题",
		"修复 bug",
	}
}

func looksLikeFinalReview(text string) bool {
	return containsAnyLower(text, []string{
		"final code review",
		"review the current diff",
		"review this diff",
		"review these changes",
		"decide whether",
		"safe to merge",
		"approve",
		"bug triage",
		"审阅",
		"代码审查",
		"代码评审",
		"是否安全",
	})
}

func containsAnyLower(text string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func containsAnyUnnegatedLower(text string, markers []string) bool {
	for _, marker := range markers {
		index := strings.Index(text, marker)
		if index < 0 {
			continue
		}
		prefix := text[:index]
		window := prefix
		if len(window) > 32 {
			window = window[len(window)-32:]
		}
		if containsAnyLower(window, []string{"do not ", "don't ", "without ", "禁止", "不要", "不得"}) {
			continue
		}
		return true
	}
	return false
}

func (c *coordinator) buildSubAgentForType(ctx context.Context, requestedType string) (SessionAgent, config.Agent, error) {
	return c.buildSubAgentForTypeWithPriority(ctx, requestedType, nil)
}

func (c *coordinator) buildSubAgentForTypeWithPriority(ctx context.Context, requestedType string, modelPriority []string) (SessionAgent, config.Agent, error) {
	if c.subAgentFactory != nil {
		return c.subAgentFactory(ctx, requestedType)
	}

	agentCfg, err := c.subagentConfig(requestedType)
	if err != nil {
		return nil, config.Agent{}, err
	}

	// Apply per-invocation model priority override. Each entry is treated as
	// a SelectedModelType; the first one with a configured selection wins.
	if overridden, ok := c.firstAvailableModelType(modelPriority); ok {
		agentCfg.Model = overridden
	}

	promptBuilder, err := promptForAgent(agentCfg, true, prompt.WithWorkingDir(c.cfg.WorkingDir()))
	if err != nil {
		return nil, config.Agent{}, err
	}

	subAgent, err := c.buildAgent(ctx, promptBuilder, agentCfg, true)
	if err != nil {
		return nil, config.Agent{}, err
	}

	return subAgent, agentCfg, nil
}

// firstAvailableModelType returns the first entry in priority that resolves
// to a configured selected model type, if any.
func (c *coordinator) firstAvailableModelType(priority []string) (config.SelectedModelType, bool) {
	for _, entry := range priority {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" {
			continue
		}
		candidate := config.SelectedModelType(trimmed)
		if _, ok := c.cfg.Config().SelectedModelForType(candidate); ok {
			return candidate, true
		}
	}
	return "", false
}

func (c *coordinator) subagentConfig(requestedType string) (config.Agent, error) {
	subagentType := config.ResolveSubagentID(c.cfg.Config().Agents, strings.TrimSpace(requestedType))
	agentCfg, ok := c.cfg.Config().Agents[subagentType]
	if !ok {
		return config.Agent{}, fmt.Errorf("unknown subagent type: %s", subagentType)
	}
	if config.NormalizeAgentMode(agentCfg.Mode) == config.AgentModePrimary {
		return config.Agent{}, fmt.Errorf("agent %s is not available as a subagent", agentCfg.ID)
	}
	return agentCfg, nil
}

func defaultSubagentDescription(subagentType, prompt string) string {
	title := strings.TrimSpace(prompt)
	if title == "" {
		return titleCase(subagentType) + " task"
	}
	words := strings.Fields(title)
	if len(words) > 6 {
		words = words[:6]
	}
	return strings.Join(words, " ")
}

func formatSubagentSessionTitle(description, subagentType string) string {
	if description == "" {
		description = titleCase(subagentType) + " task"
	}
	return fmt.Sprintf("%s (@%s subagent)", description, subagentType)
}

func titleCase(value string) string {
	if value == "" {
		return ""
	}
	runes := []rune(value)
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}
