package goal

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/session"
)

// EvaluatorConfig controls the optional external goal evaluator.
type EvaluatorConfig struct {
	Enabled   bool
	TimeoutMs int
}

// EvaluatorVerdict is the structured result returned by the external evaluator.
type EvaluatorVerdict struct {
	Met        bool   `json:"met"`
	Impossible bool   `json:"impossible"`
	Progress   bool   `json:"progress"`
	Waiting    bool   `json:"waiting"`
	Reason     string `json:"reason"`
}

// Evaluator is the interface for an external goal evaluator. Implementations
// may call an LLM, run tests, or inspect repo state. The evaluator is read-only
// and must not mutate session or goal state.
type Evaluator interface {
	Evaluate(ctx context.Context, sess session.Session, goal session.Goal) (EvaluatorVerdict, error)
}

// NoopEvaluator is the default evaluator; it always returns a zero verdict
// with no error, effectively disabling evaluator checks.
type NoopEvaluator struct{}

func (NoopEvaluator) Evaluate(context.Context, session.Session, session.Goal) (EvaluatorVerdict, error) {
	return EvaluatorVerdict{}, nil
}

// ApplyEvaluatorVerdict updates the goal with the evaluator's result and
// adjusts no_progress according to the design:
//   - met == true: no_progress reset to 0.
//   - met == false, progress == false, not waiting: no_progress++.
//   - impossible == true: goal is marked dropped with the evaluator's reason.
//
// The caller is responsible for persisting the updated session.
func ApplyEvaluatorVerdict(g *session.Goal, v EvaluatorVerdict) {
	now := time.Now().Unix()
	g.LastEvaluatorAt = now
	met := v.Met
	g.LastEvaluatorMet = &met

	if v.Impossible {
		TerminateGoal(g, session.GoalStatusDropped, fmt.Sprintf("Evaluator deemed goal impossible: %s", v.Reason))
		return
	}

	if v.Met {
		g.NoProgress = 0
		g.LastReason = ""
		return
	}

	if !v.Progress && !v.Waiting {
		g.NoProgress++
		if g.LastReason == "" {
			g.LastReason = v.Reason
		}
	}
}

// ParseEvaluatorVerdict parses the JSON output of an evaluator call.
// It tolerates trailing whitespace and partial JSON via a simple trim.
func ParseEvaluatorVerdict(raw string) (EvaluatorVerdict, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return EvaluatorVerdict{}, fmt.Errorf("empty evaluator response")
	}
	var v EvaluatorVerdict
	if err := json.Unmarshal([]byte(trimmed), &v); err != nil {
		return EvaluatorVerdict{}, fmt.Errorf("failed to parse evaluator verdict: %w", err)
	}
	return v, nil
}
