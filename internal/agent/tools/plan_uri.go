package tools

import (
	"cmp"
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/crush/internal/plan"
	"github.com/charmbracelet/crush/internal/session"
)

// resolveLocalPlanURI converts a local plan URI (local://<slug>-plan.md) into
// an absolute filesystem path. Non-URI paths are returned unchanged.
func resolveLocalPlanURI(ctx context.Context, uri string, fallbackWorkingDir string) (string, error) {
	if !plan.IsLocalPlanURI(uri) {
		return uri, nil
	}

	sessionID := GetSessionFromContext(ctx)
	if sessionID == "" {
		return "", fmt.Errorf("session ID is required for local plan URI")
	}

	workspaceRoot := cmp.Or(GetWorkingDirFromContext(ctx), fallbackWorkingDir)
	if workspaceRoot == "" {
		return "", fmt.Errorf("working directory is required for local plan URI")
	}

	resolved, err := plan.ResolveLocalPlanURI(workspaceRoot, sessionID, uri)
	if err != nil {
		return "", err
	}

	if err := plan.EnsureDir(workspaceRoot); err != nil {
		return "", fmt.Errorf("failed to create plans directory: %w", err)
	}

	return resolved, nil
}

// adoptPlanFilePathIfNeeded updates the session's active plan file path when
// the agent writes to a recognized plan file for that session.
func adoptPlanFilePathIfNeeded(ctx context.Context, filePath string) error {
	sessionID := GetSessionFromContext(ctx)
	if sessionID == "" {
		return nil
	}
	sessions := GetSessionServiceFromContext(ctx)
	if sessions == nil {
		return nil
	}
	sess, err := sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to load session for plan path adoption: %w", err)
	}

	workspaceRoot := strings.TrimSpace(sess.WorkspaceCWD)
	if workspaceRoot == "" {
		workspaceRoot = cmp.Or(GetWorkingDirFromContext(ctx), "")
	}
	if workspaceRoot == "" {
		return nil
	}

	if _, ok := plan.SlugFromPlanPath(workspaceRoot, sessionID, filePath); !ok {
		return nil
	}

	if samePlanPath(sess.PlanFilePath, filePath) {
		return nil
	}

	sess.PlanFilePath = filePath
	if _, err := sessions.Save(ctx, sess); err != nil {
		return fmt.Errorf("failed to adopt plan file path: %w", err)
	}
	return nil
}

// activePlanFilePath returns the active plan file path for the session, or an
// empty string if none is set.
func activePlanFilePath(ctx context.Context) string {
	sessionID := GetSessionFromContext(ctx)
	if sessionID == "" {
		return ""
	}
	sessions := GetSessionServiceFromContext(ctx)
	if sessions == nil {
		return ""
	}
	sess, err := sessions.Get(ctx, sessionID)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(sess.PlanFilePath)
}

// ensureSessionPlanFilePath makes sure the session has an active plan file
// path. If it does not, it creates a default plan file and persists the path.
func ensureSessionPlanFilePath(ctx context.Context, sessions session.Service, workspaceRoot, sessionID string) (string, error) {
	sess, err := sessions.Get(ctx, sessionID)
	if err != nil {
		return "", fmt.Errorf("failed to load session: %w", err)
	}

	if existing := strings.TrimSpace(sess.PlanFilePath); existing != "" {
		return existing, nil
	}

	path, err := plan.EnsureSessionFile(workspaceRoot, sessionID)
	if err != nil {
		return "", err
	}

	sess.PlanFilePath = path
	if _, err := sessions.Save(ctx, sess); err != nil {
		return "", fmt.Errorf("failed to save plan file path: %w", err)
	}
	return path, nil
}
