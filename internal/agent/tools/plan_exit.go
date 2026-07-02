package tools

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/session"
)

//go:embed plan_exit.md
var planExitDescription []byte

const PlanExitToolName = "plan_exit"

type PlanExitMetadata struct {
	PlanFilePath string `json:"plan_file_path"`
}

func NewPlanExitTool(sessions session.Service) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		PlanExitToolName,
		string(planExitDescription),
		func(ctx context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for plan_exit")
			}

			sess, err := sessions.Get(ctx, sessionID)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to get session: %w", err)
			}
			if sess.CollaborationMode != session.CollaborationModePlan {
				return fantasy.NewTextErrorResponse("plan_exit can only be used in Plan Mode"), nil
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
				return fantasy.NewTextErrorResponse("The active plan file is empty. Write the final plan before calling plan_exit."), nil
			}
			metadata, err := json.Marshal(PlanExitMetadata{PlanFilePath: planPath})
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to encode plan exit metadata: %w", err)
			}

			return fantasy.ToolResponse{
				Content:  fmt.Sprintf("Plan marked ready for review. Plan file: %s", planPath),
				Metadata: string(metadata),
			}, nil
		},
	)
}
