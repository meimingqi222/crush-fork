package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/plan"
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

	workspaceRoot := strings.TrimSpace(sess.WorkspaceCWD)
	if workspaceRoot == "" {
		workspaceRoot = activeWorkingDir(ctx)
	}
	if workspaceRoot == "" {
		return fantasy.NewTextErrorResponse("Plan Mode allows writing only to the session plan file, but the workspace root is unknown."), true, nil
	}

	if _, ok := plan.SlugFromPlanPath(workspaceRoot, sessionID, filePath); ok {
		return fantasy.ToolResponse{}, false, nil
	}

	activePath := strings.TrimSpace(sess.PlanFilePath)
	if activePath != "" && samePlanPath(filePath, activePath) {
		return fantasy.ToolResponse{}, false, nil
	}

	if activePath == "" {
		return fantasy.NewTextErrorResponse("Plan Mode allows writing only to the session plan file. Use a local plan URI such as local://<slug>-plan.md."), true, nil
	}
	return fantasy.NewTextErrorResponse(fmt.Sprintf("Plan Mode is read-only except for the session plan file. Write blocked: %s. Active plan file: %s", filePath, activePath)), true, nil
}

func activeWorkingDir(ctx context.Context) string {
	if dir := GetWorkingDirFromContext(ctx); dir != "" {
		return dir
	}
	return ""
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
