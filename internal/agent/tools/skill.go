package tools

import (
	"context"
	"fmt"
	"os"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/skills"
)

const SkillToolName = "Skill"

// SkillTool allows the agent to explicitly invoke a skill with arguments.
// This is useful when the skill needs to be run in a specific context or with
// specific parameters that the agent determines programmatically.
type SkillTool struct {
	skillsPaths []string
}

// SkillParams defines the parameters for the Skill tool.
type SkillParams struct {
	Name string `json:"name" jsonschema:"required,description=The name of the skill to invoke"`
	Args string `json:"args" jsonschema:"description=Arguments to pass to the skill"`
}

// NewSkillTool creates a new Skill tool.
func NewSkillTool(skillsPaths []string) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		SkillToolName,
		"Invoke a skill by name with optional arguments. Use this tool when you need to explicitly invoke a skill rather than just reading its SKILL.md file. The skill's instructions will be loaded and arguments substituted according to the skill's defined placeholders ($ARGUMENTS, $0, $1, named args, etc.).",
		func(ctx context.Context, params SkillParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Name == "" {
				return fantasy.NewTextErrorResponse("skill name is required"), nil
			}

			// Discover skills
			if len(skillsPaths) == 0 {
				return fantasy.NewTextErrorResponse("no skills paths configured"), nil
			}

			discoveredSkills := skills.Discover(skillsPaths)
			if len(discoveredSkills) == 0 {
				return fantasy.NewTextErrorResponse("no skills found"), nil
			}

			// Find the requested skill
			var targetSkill *skills.Skill
			for _, s := range discoveredSkills {
				if s.Name == params.Name {
					targetSkill = s
					break
				}
			}

			if targetSkill == nil {
				availableNames := make([]string, 0, len(discoveredSkills))
				for _, s := range discoveredSkills {
					availableNames = append(availableNames, s.Name)
				}
				return fantasy.NewTextErrorResponse(fmt.Sprintf("skill %q not found. Available skills: %s", params.Name, strings.Join(availableNames, ", "))), nil
			}

			// Read the full skill content
			content, err := os.ReadFile(targetSkill.SkillFilePath)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to read skill file: %s", err)), nil
			}

			// Get the instruction part (after frontmatter)
			instructions := targetSkill.Instructions
			if instructions == "" {
				// Fallback to full content if instructions not parsed
				instructions = string(content)
			}

			// Substitute arguments
			if params.Args != "" {
				instructions = skills.SubstituteArguments(instructions, params.Args, targetSkill.Arguments)
			}

			// Build the response with skill metadata
			var response strings.Builder
			response.WriteString(fmt.Sprintf("Skill: %s\n", targetSkill.Name))
			response.WriteString(fmt.Sprintf("Description: %s\n", targetSkill.Description))
			if targetSkill.WhenToUse != "" {
				response.WriteString(fmt.Sprintf("When to use: %s\n", targetSkill.WhenToUse))
			}
			if len(targetSkill.AllowedTools) > 0 {
				response.WriteString(fmt.Sprintf("Allowed tools: %s\n", strings.Join(targetSkill.AllowedTools, ", ")))
			}
			if targetSkill.Model != "" {
				response.WriteString(fmt.Sprintf("Model: %s\n", targetSkill.Model))
			}
			if targetSkill.Context != "" {
				response.WriteString(fmt.Sprintf("Context: %s\n", targetSkill.Context))
			}
			response.WriteString("\n---\n\n")
			response.WriteString(instructions)

			return fantasy.NewTextResponse(response.String()), nil
		})
}
