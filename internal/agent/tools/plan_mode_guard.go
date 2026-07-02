package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/session"
)

func enforcePlanModeWriteTarget(ctx context.Context, filePath string) (fantasy.ToolResponse, bool, error) {
	sessionID := GetSessionFromContext(ctx)
	if sessionID == "" {
		return fantasy.ToolResponse{}, false, nil
	}
	sessions := GetSessionServiceFromContext(ctx)
	if sessions == nil {
		return fantasy.ToolResponse{}, false, nil
	}
	sess, err := sessions.Get(ctx, sessionID)
	if err != nil {
		return fantasy.ToolResponse{}, false, fmt.Errorf("failed to load session for plan mode write guard: %w", err)
	}
	if sess.CollaborationMode != session.CollaborationModePlan {
		return fantasy.ToolResponse{}, false, nil
	}
	planPath := strings.TrimSpace(sess.PlanFilePath)
	if planPath == "" {
		return fantasy.NewTextErrorResponse("Plan Mode allows writing only to the active plan file, but this session has no plan file. Exit and re-enter Plan Mode to create one."), true, nil
	}
	if samePlanPath(filePath, planPath) {
		return fantasy.ToolResponse{}, false, nil
	}
	return fantasy.NewTextErrorResponse(fmt.Sprintf("Plan Mode is read-only except for the active plan file. Write blocked: %s. Active plan file: %s", filePath, planPath)), true, nil
}

func samePlanPath(path, planPath string) bool {
	left, err := filepath.Abs(path)
	if err != nil {
		left = path
	}
	right, err := filepath.Abs(planPath)
	if err != nil {
		right = planPath
	}
	return filepath.Clean(left) == filepath.Clean(right)
}
