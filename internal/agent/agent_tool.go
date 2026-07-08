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
	Role         string `json:"role,omitempty" description:"Optional specialist identity for this subagent (e.g. planner, researcher, reviewer, executor)"`
	Isolation    string `json:"isolation,omitempty" description:"Optional isolation override for this task: 'worktree' (isolated git worktree, changes merge back on success), 'session' (shared workspace), or 'none' (use defaults). Use 'worktree' when tasks may touch overlapping files"`
}

type AgentParams struct {
	Description     string            `json:"description,omitempty" description:"A short title for the delegated task"`
	Prompt          string            `json:"prompt,omitempty" description:"The task for the agent to perform"`
	SubagentType    string            `json:"subagent_type,omitempty" description:"The subagent type to use: general, quick_task, explore, plan, review, designer, librarian, or a configured subagent name"`
	Tasks           []AgentTaskParams `json:"tasks,omitempty" description:"Optional list of independent tasks to execute in parallel"`
	Context         string            `json:"context,omitempty" description:"Shared background information for all subagents"`
	RunInBackground bool              `json:"run_in_background,omitempty" description:"Run the agent in the background and return immediately with an agent ID"`
	Role            string            `json:"role,omitempty" description:"Optional specialist identity for the subagent when using a single prompt (e.g. planner, researcher, reviewer, executor)"`
	Isolation       string            `json:"isolation,omitempty" description:"Optional isolation override when using a single prompt: 'worktree', 'session', or 'none' (use defaults)"`
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
						Role:         strings.TrimSpace(task.Role),
						Isolation:    strings.TrimSpace(task.Isolation),
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
					Role:         strings.TrimSpace(params.Role),
					Isolation:    strings.TrimSpace(params.Isolation),
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
	return ""
}

func validateExploreDelegations(tasks []subagentTask) string {
	return ""
}

func validateSubagentDelegations(tasks []subagentTask, agents map[string]config.Agent) string {
	return ""
}

func (c *coordinator) buildSubAgentForType(ctx context.Context, requestedType, role string) (SessionAgent, config.Agent, error) {
	if c.subAgentFactory != nil {
		return c.subAgentFactory(ctx, requestedType)
	}

	agentCfg, err := c.subagentConfig(requestedType)
	if err != nil {
		return nil, config.Agent{}, err
	}

	promptBuilder, err := promptForAgent(agentCfg, true, prompt.WithWorkingDir(c.cfg.WorkingDir()), prompt.WithRole(role))
	if err != nil {
		return nil, config.Agent{}, err
	}

	subAgent, err := c.buildAgent(ctx, promptBuilder, agentCfg, true)
	if err != nil {
		return nil, config.Agent{}, err
	}

	return subAgent, agentCfg, nil
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
