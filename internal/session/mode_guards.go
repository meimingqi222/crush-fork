package session

import "errors"

var (
	// ErrGoalBlocksPlanMode is returned when entering plan mode while a goal is
	// still active, paused, or budget-limited.
	ErrGoalBlocksPlanMode = errors.New("exit goal mode first: drop or complete the current goal before entering plan mode")
	// ErrPlanBlocksGoalMode is returned when starting or resuming goal work
	// while plan mode is active.
	ErrPlanBlocksGoalMode = errors.New("exit plan mode first")
)

// OccupiesSession reports whether the goal still holds the session and blocks
// entering plan mode. Completed and dropped goals do not occupy the session.
func (g Goal) OccupiesSession() bool {
	switch g.Status {
	case GoalStatusActive, GoalStatusPaused, GoalStatusBudgetLimited:
		return true
	default:
		return false
	}
}

// IsPlanMode reports whether the session is in active plan collaboration mode.
func (s Session) IsPlanMode() bool {
	return s.IsActivePlanMode()
}

// IsActivePlanMode reports whether plan tools, prompts, and write guards apply.
func (s Session) IsActivePlanMode() bool {
	return NormalizeCollaborationMode(string(s.CollaborationMode)) == CollaborationModePlan
}

// IsPlanPaused reports whether plan mode was paused without fully exiting.
func (s Session) IsPlanPaused() bool {
	return NormalizeCollaborationMode(string(s.CollaborationMode)) == CollaborationModePlanPaused
}

// IsPlanFlow reports whether the session is still in the plan workflow, including
// the paused state that blocks goal mode until fully exited.
func (s Session) IsPlanFlow() bool {
	switch NormalizeCollaborationMode(string(s.CollaborationMode)) {
	case CollaborationModePlan, CollaborationModePlanPaused:
		return true
	default:
		return false
	}
}

// ValidateEnterPlanMode returns an error when the session cannot enter active
// plan mode because an existing goal still occupies it.
func (s Session) ValidateEnterPlanMode() error {
	if s.IsActivePlanMode() {
		return nil
	}
	if s.Goal.OccupiesSession() {
		return ErrGoalBlocksPlanMode
	}
	return nil
}

// ValidateGoalWork returns an error when goal operations that start or resume
// autonomous work are blocked by active or paused plan mode.
func (s Session) ValidateGoalWork() error {
	if s.IsPlanFlow() {
		return ErrPlanBlocksGoalMode
	}
	return nil
}
