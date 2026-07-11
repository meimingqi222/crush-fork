package agent

import (
	"context"
	"strings"

	"github.com/charmbracelet/crush/internal/plan"
	"github.com/charmbracelet/crush/internal/session"
)

// ensurePlanFileForSession makes sure the session has a plan file path
// assigned, creating one if needed. It persists the updated path to the
// session store.
func (c *coordinator) ensurePlanFileForSession(ctx context.Context, sess session.Session) (session.Session, error) {
	workspaceRoot := strings.TrimSpace(sess.WorkspaceCWD)
	if workspaceRoot == "" {
		workspaceRoot = c.cfg.WorkingDir()
	}
	planPath, err := plan.EnsureSessionPlanPath(workspaceRoot, sess.ID, sess.PlanFilePath)
	if err != nil {
		return session.Session{}, err
	}
	if planPath == sess.PlanFilePath {
		return sess, nil
	}
	sess.PlanFilePath = planPath
	return c.sessions.Save(ctx, sess)
}
