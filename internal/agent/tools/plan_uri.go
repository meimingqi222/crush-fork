package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/crush/internal/plan"
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

	workspaceRoot := EffectiveWorkingDir(ctx, fallbackWorkingDir)
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

	workspaceRoot := EffectiveWorkingDir(ctx, "")
	if workspaceRoot == "" {
		workspaceRoot = strings.TrimSpace(sess.WorkspaceCWD)
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

	// Use the narrow updater so we don't clobber concurrent writes to other
	// session fields (e.g. usage counters, goal state) that may land between
	// the Get above and this Save.
	if _, err := sessions.UpdatePlanFilePath(ctx, sessionID, filePath); err != nil {
		return fmt.Errorf("failed to adopt plan file path: %w", err)
	}
	return nil
}
