package agent

import (
	"context"
	_ "embed"
	"encoding/json"
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
	ID              string   `json:"id" description:"The unique task identifier used for dependency references"`
	Description     string   `json:"description,omitempty" description:"A short title for the delegated task"`
	Prompt          string   `json:"prompt" description:"The task for the agent to perform"`
	SubagentType    string   `json:"subagent_type,omitempty" description:"The subagent type to use: general, explore, or a configured subagent name"`
	DependsOn       []string `json:"depends_on,omitempty" description:"Task IDs that must complete successfully before this task runs"`
	RunInBackground bool     `json:"run_in_background,omitempty" description:"Run this task in the background and return immediately with a task ID"`
}

type AgentParams struct {
	Description     string            `json:"description,omitempty" description:"A short title for the delegated task"`
	Prompt          string            `json:"prompt,omitempty" description:"The task for the agent to perform"`
	SubagentType    string            `json:"subagent_type,omitempty" description:"The subagent type to use: general, explore, or a configured subagent name"`
	Tasks           []AgentTaskParams `json:"tasks,omitempty" description:"Optional task graph with dependency-aware delegation"`
	RunInBackground bool              `json:"run_in_background,omitempty" description:"Run the agent in the background and return immediately with an agent ID"`
}

// UnmarshalJSON implements custom JSON unmarshalling for AgentParams.
// It handles the case where the LLM incorrectly sends the "tasks" field as a
// JSON-encoded string instead of an array (e.g. `"tasks": "[...]"` instead
// of `"tasks": [...]`).
func (p *AgentParams) UnmarshalJSON(data []byte) error {
	type plain AgentParams
	if err := json.Unmarshal(data, (*plain)(p)); err == nil {
		return nil
	}

	// Fallback: try to recover tasks from a JSON-encoded string.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	tasksRaw, hasTasks := raw["tasks"]
	if !hasTasks {
		// No tasks field at all; re-run the normal unmarshal to get the
		// original error.
		return json.Unmarshal(data, (*plain)(p))
	}

	// Check if tasks is a string containing a JSON array.
	var tasksStr string
	if err := json.Unmarshal(tasksRaw, &tasksStr); err != nil {
		// Not a string – return the original typed unmarshal error.
		return json.Unmarshal(data, (*plain)(p))
	}

	var tasks []AgentTaskParams
	if err := json.Unmarshal([]byte(tasksStr), &tasks); err != nil {
		return fmt.Errorf("failed to parse tasks string as JSON array: %w", err)
	}

	slog.Warn("Recovered AgentParams.Tasks from JSON string (LLM sent string instead of array)")

	// Remove tasks from raw so the rest can be unmarshalled normally.
	delete(raw, "tasks")
	patched, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(patched, (*plain)(p)); err != nil {
		return err
	}
	p.Tasks = tasks
	return nil
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

			// Unified path: construct task graph and delegate to runTaskGraph
			var tasks []taskGraphTask
			if len(params.Tasks) > 0 {
				for _, task := range params.Tasks {
					if strings.TrimSpace(task.ID) == "" {
						return fantasy.NewTextErrorResponse("task id is required"), nil
					}
					if strings.TrimSpace(task.Prompt) == "" {
						slog.Warn("Agent task missing prompt",
							"task_id", task.ID,
							"description", task.Description,
						)
						return fantasy.NewTextErrorResponse(fmt.Sprintf(
							"task %q is missing the required \"prompt\" field. "+
								"The \"prompt\" field must contain the full task instructions. "+
								"The \"description\" field (currently %q) is only a short display label. "+
								"Please retry with a \"prompt\" field containing the detailed task instructions.",
							task.ID, task.Description,
						)), nil
					}
					tasks = append(tasks, taskGraphTask{
						ID:              strings.TrimSpace(task.ID),
						Description:     task.Description,
						Prompt:          task.Prompt,
						SubagentType:    task.SubagentType,
						DependsOn:       task.DependsOn,
						RunInBackground: task.RunInBackground,
					})
				}
			} else {
				// Single-task case: convert to single-node task graph
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
					// Use subagent type to determine default description
					subagentType := config.CanonicalSubagentID(strings.TrimSpace(params.SubagentType))
					description = defaultSubagentDescription(subagentType, params.Prompt)
				}
				tasks = []taskGraphTask{{
					ID:              "task",
					Description:     description,
					Prompt:          params.Prompt,
					SubagentType:    params.SubagentType,
					RunInBackground: params.RunInBackground,
				}}
			}

			if message := validateExploreDelegations(tasks); message != "" {
				return fantasy.NewTextErrorResponse(message), nil
			}

			return c.runTaskGraph(ctx, taskGraphParams{
				SessionID:       sessionID,
				AgentMessageID:  agentMessageID,
				ToolCallID:      call.ID,
				Tasks:           tasks,
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
		canonicalID := config.CanonicalSubagentID(agentCfg.ID)
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

func validateExploreDelegations(tasks []taskGraphTask) string {
	for _, task := range tasks {
		if config.CanonicalSubagentID(strings.TrimSpace(task.SubagentType)) != config.AgentExplore {
			continue
		}
		if reason := exploreDelegationViolation(task.Prompt); reason != "" {
			return fmt.Sprintf(
				"task %q cannot use `explore`: %s\n\n"+
					"`explore` is only for quick codebase discovery: locating relevant files, symbols, call chains, "+
					"and concise file:line evidence. The primary agent must do final review decisions and direct full-file reads. "+
					"Use `general` for implementation, reproduction, build, test, lint, or other execution tasks.",
				task.ID,
				reason,
			)
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
	return ""
}

func containsAnyLower(text string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func (c *coordinator) buildSubAgentForType(ctx context.Context, requestedType string) (SessionAgent, config.Agent, error) {
	if c.subAgentFactory != nil {
		return c.subAgentFactory(ctx, requestedType)
	}

	agentCfg, err := c.subagentConfig(requestedType)
	if err != nil {
		return nil, config.Agent{}, err
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

func (c *coordinator) subagentConfig(requestedType string) (config.Agent, error) {
	subagentType := config.CanonicalSubagentID(strings.TrimSpace(requestedType))
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
