package tools

import (
	"cmp"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/plan"
	"github.com/charmbracelet/crush/internal/session"
)

//go:embed resolve.md
var resolveDescription []byte

// ResolveToolName is the name of the resolve tool used to finish planning.
const ResolveToolName = "resolve"

// ResolveExtra holds additional metadata for a resolve call.
type ResolveExtra struct {
	Title string `json:"title" description:"Plan slug that matches the local plan file name (e.g. 'auth-refactor')."`
}

// ResolveParams defines the parameters for the resolve tool.
type ResolveParams struct {
	Action string       `json:"action" description:"Resolution action. Only 'apply' is currently supported."`
	Reason string       `json:"reason,omitempty" description:"Short explanation for the resolution."`
	Extra  ResolveExtra `json:"extra" description:"Additional resolution metadata."`
}

// ResolveMetadata is attached to tool responses for UI rendering.
type ResolveMetadata struct {
	Action       string `json:"action"`
	Reason       string `json:"reason,omitempty"`
	Title        string `json:"title,omitempty"`
	PlanFilePath string `json:"plan_file_path"`
}

// NewResolveTool creates a new resolve tool instance.
func NewResolveTool(sessions session.Service) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		ResolveToolName,
		string(resolveDescription),
		func(ctx context.Context, params ResolveParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for resolve")
			}

			sess, err := sessions.Get(ctx, sessionID)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to get session: %w", err)
			}
			if sess.CollaborationMode != session.CollaborationModePlan {
				return fantasy.NewTextErrorResponse("resolve can only be used in Plan Mode"), nil
			}
			if params.Action != "apply" {
				return fantasy.NewTextErrorResponse("resolve currently only supports action='apply'"), nil
			}

			planPath := strings.TrimSpace(sess.PlanFilePath)
			if planPath == "" {
				return fantasy.NewTextErrorResponse("No active plan file is set for this session."), nil
			}
			data, err := os.ReadFile(planPath)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to read plan file: %w", err)
			}
			if strings.TrimSpace(string(data)) == "" {
				return fantasy.NewTextErrorResponse("The active plan file is empty. Write the final plan before calling resolve."), nil
			}

			workspaceRoot := strings.TrimSpace(sess.WorkspaceCWD)
			if workspaceRoot == "" {
				workspaceRoot = cmp.Or(GetWorkingDirFromContext(ctx), "")
			}
			title := strings.TrimSpace(params.Extra.Title)
			if title == "" {
				return fantasy.NewTextErrorResponse("resolve requires extra.title to be a non-empty plan slug (e.g. 'auth-refactor')."), nil
			}
			if workspaceRoot != "" {
				if slug, ok := plan.SlugFromPlanPath(workspaceRoot, sessionID, planPath); ok && slug != "" && slug != "plan" {
					if slug != title {
						return fantasy.NewTextErrorResponse(fmt.Sprintf("extra.title '%s' does not match the active plan file slug '%s'. They must match.", title, slug)), nil
					}
				}
			}

			metadata, err := json.Marshal(ResolveMetadata{
				Action:       params.Action,
				Reason:       params.Reason,
				Title:        params.Extra.Title,
				PlanFilePath: planPath,
			})
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to encode resolve metadata: %w", err)
			}

			content := fmt.Sprintf("Plan submitted for review. Plan file: %s", planPath)
			if params.Extra.Title != "" {
				content = fmt.Sprintf("Plan '%s' submitted for review. Plan file: %s", params.Extra.Title, planPath)
			}

			return fantasy.ToolResponse{
				Content:  content,
				Metadata: string(metadata),
			}, nil
		},
	)
}
