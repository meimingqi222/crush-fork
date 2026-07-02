package tools

import (
	"context"
	"testing"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

type planGuardSessionService struct {
	session.Service
	sess session.Session
}

func (s *planGuardSessionService) Get(context.Context, string) (session.Session, error) {
	return s.sess, nil
}

func TestEnforcePlanModeWriteTargetAllowsPlanFile(t *testing.T) {
	t.Parallel()

	planPath := t.TempDir() + "/plan.md"
	sessions := &planGuardSessionService{sess: session.Session{
		CollaborationMode: session.CollaborationModePlan,
		PlanFilePath:      planPath,
	}}
	ctx := context.Background()
	ctx = context.WithValue(ctx, SessionIDContextKey, "sess-1")
	ctx = context.WithValue(ctx, SessionServiceContextKey, sessions)

	resp, blocked, err := enforcePlanModeWriteTarget(ctx, planPath)
	require.NoError(t, err)
	require.False(t, blocked)
	require.Empty(t, resp.Content)
}

func TestEnforcePlanModeWriteTargetBlocksOtherFiles(t *testing.T) {
	t.Parallel()

	planPath := t.TempDir() + "/plan.md"
	sessions := &planGuardSessionService{sess: session.Session{
		CollaborationMode: session.CollaborationModePlan,
		PlanFilePath:      planPath,
	}}
	ctx := context.Background()
	ctx = context.WithValue(ctx, SessionIDContextKey, "sess-1")
	ctx = context.WithValue(ctx, SessionServiceContextKey, sessions)

	resp, blocked, err := enforcePlanModeWriteTarget(ctx, t.TempDir()+"/main.go")
	require.NoError(t, err)
	require.True(t, blocked)
	require.Contains(t, resp.Content, "Plan Mode is read-only")
}
