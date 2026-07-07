package session

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGoalOccupiesSession(t *testing.T) {
	t.Parallel()

	require.False(t, Goal{}.OccupiesSession())
	require.True(t, Goal{Status: GoalStatusActive}.OccupiesSession())
	require.True(t, Goal{Status: GoalStatusPaused}.OccupiesSession())
	require.True(t, Goal{Status: GoalStatusBudgetLimited}.OccupiesSession())
	require.False(t, Goal{Status: GoalStatusComplete}.OccupiesSession())
	require.False(t, Goal{Status: GoalStatusDropped}.OccupiesSession())
}

func TestValidateEnterPlanMode(t *testing.T) {
	t.Parallel()

	sess := Session{
		CollaborationMode: CollaborationModeDefault,
		Goal:              Goal{Status: GoalStatusPaused, Text: "Ship it"},
	}
	require.ErrorIs(t, sess.ValidateEnterPlanMode(), ErrGoalBlocksPlanMode)

	sess.Goal.Status = GoalStatusDropped
	require.NoError(t, sess.ValidateEnterPlanMode())

	sess.CollaborationMode = CollaborationModePlan
	sess.Goal.Status = GoalStatusActive
	require.NoError(t, sess.ValidateEnterPlanMode())
}

func TestValidateGoalWork(t *testing.T) {
	t.Parallel()

	sess := Session{CollaborationMode: CollaborationModePlan}
	require.ErrorIs(t, sess.ValidateGoalWork(), ErrPlanBlocksGoalMode)

	sess.CollaborationMode = CollaborationModePlanPaused
	require.ErrorIs(t, sess.ValidateGoalWork(), ErrPlanBlocksGoalMode)

	sess.CollaborationMode = CollaborationModeDefault
	require.NoError(t, sess.ValidateGoalWork())
}

func TestIsPlanFlow(t *testing.T) {
	t.Parallel()

	require.True(t, (Session{CollaborationMode: CollaborationModePlan}).IsPlanFlow())
	require.True(t, (Session{CollaborationMode: CollaborationModePlanPaused}).IsPlanFlow())
	require.False(t, (Session{CollaborationMode: CollaborationModeDefault}).IsPlanFlow())
}
