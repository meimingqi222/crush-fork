package agent

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/config"
)

func TestRequestStepBudgetFor(t *testing.T) {
	t.Parallel()

	rc := config.SubagentRuntimeConfig{SoftRequestBudget: 90}

	require.Equal(t, 0, requestStepBudgetFor(false, rc), "main agent should never have a budget")
	require.Equal(t, 90, requestStepBudgetFor(true, rc), "subagent should inherit the soft budget")

	rc.SoftRequestBudget = 0
	require.Equal(t, 0, requestStepBudgetFor(true, rc), "zero budget should be respected")
}

func TestHardRequestBudgetFor(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		isSub  bool
		soft   int
		mult   float64
		wantHd int
	}{
		{name: "default", isSub: true, soft: 90, mult: 1.5, wantHd: 135},
		{name: "multiplier clamped to 1.0", isSub: true, soft: 100, mult: 0.7, wantHd: 101},
		{name: "multiplier exactly 1.0", isSub: true, soft: 50, mult: 1.0, wantHd: 51},
		{name: "main agent disabled", isSub: false, soft: 90, mult: 1.5, wantHd: 0},
		{name: "zero soft budget disables hard", isSub: true, soft: 0, mult: 1.5, wantHd: 0},
		{name: "fractional ceiling rounds up", isSub: true, soft: 10, mult: 1.05, wantHd: 11},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rc := config.SubagentRuntimeConfig{
				SoftRequestBudget:           tc.soft,
				HardRequestBudgetMultiplier: tc.mult,
			}
			got := hardRequestBudgetFor(tc.isSub, rc)
			require.Equal(t, tc.wantHd, got)
			if tc.isSub && tc.soft > 0 {
				require.True(t, got > tc.soft, "hard cap (%d) must be strictly greater than soft cap (%d)", got, tc.soft)
			}
		})
	}
}
