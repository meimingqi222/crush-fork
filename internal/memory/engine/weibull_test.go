package engine

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWeibullDecayFreshMemoryReturnsOne(t *testing.T) {
	t.Parallel()
	params := weibullParamsForKind(MemoryKindPreference)
	require.InDelta(t, 1.0, params.Decay(0), 1e-9)
	require.InDelta(t, 1.0, params.Decay(-1), 1e-9)
}

func TestWeibullDecayPreferenceRetainsLong(t *testing.T) {
	t.Parallel()
	params := weibullParamsForKind(MemoryKindPreference)
	// After 1 week (168h), preference should still have significant weight.
	week := params.Decay(168)
	require.Greater(t, week, 0.5, "preference should retain > 0.5 after 1 week")
	// After 1 month (720h), preference should still be noticeable.
	month := params.Decay(720)
	require.Greater(t, month, 0.2, "preference should retain > 0.2 after 1 month")
}

func TestWeibullDecayWorkingMemoryFadesFast(t *testing.T) {
	t.Parallel()
	params := weibullParamsForKind(MemoryKindWorkingMemory)
	// After 1 day (24h), working memory should be heavily decayed.
	day := params.Decay(24)
	require.Less(t, day, 0.5, "working memory should decay below 0.5 after 1 day")
	// After 3 days (72h), working memory should be nearly gone.
	threeDays := params.Decay(72)
	require.Less(t, threeDays, 0.1, "working memory should decay below 0.1 after 3 days")
}

func TestWeibullDecayDecisionModerate(t *testing.T) {
	t.Parallel()
	params := weibullParamsForKind(MemoryKindDecision)
	// After 1 day, decision should still be strong.
	day := params.Decay(24)
	require.Greater(t, day, 0.9, "decision should retain > 0.9 after 1 day")
	// After 2 weeks (336h), decision should have noticeable decay.
	twoWeeks := params.Decay(336)
	require.Less(t, twoWeeks, 0.5, "decision should decay below 0.5 after 2 weeks")
}

func TestWeibullDecayPitfallRetainsLonger(t *testing.T) {
	t.Parallel()
	pitfallParams := weibullParamsForKind(MemoryKindPitfall)
	decisionParams := weibullParamsForKind(MemoryKindDecision)
	// Pitfalls should retain more weight than decisions after 2 weeks.
	twoWeeks := 336.0
	require.Greater(t, pitfallParams.Decay(twoWeeks), decisionParams.Decay(twoWeeks),
		"pitfalls should decay slower than decisions")
}

func TestWeibullDecayZeroEtaReturnsZero(t *testing.T) {
	t.Parallel()
	params := WeibullParams{K: 1.0, Eta: 0}
	require.InDelta(t, 0.0, params.Decay(1.0), 1e-9)
}

func TestWeibullDecayMonotonicallyDecreasing(t *testing.T) {
	t.Parallel()
	params := weibullParamsForKind(MemoryKindProcedure)
	prev := 1.0
	for hours := 1.0; hours < 1000; hours += 10 {
		current := params.Decay(hours)
		require.LessOrEqual(t, current, prev+1e-12,
			"Weibull decay should be monotonically decreasing")
		prev = current
	}
}

func TestWeibullDecayConsistentWithFormula(t *testing.T) {
	t.Parallel()
	params := WeibullParams{K: 0.7, Eta: 1440.0}
	age := 720.0 // hours
	expected := math.Exp(-math.Pow(age/params.Eta, params.K))
	require.InDelta(t, expected, params.Decay(age), 1e-12)
}

func TestHeuristicRerankerUsesWeibullDecay(t *testing.T) {
	reranker := NewHeuristicReranker()
	now := time.Now()

	oldPreference := testEvent(MemoryScopeUser, MemoryKindPreference, "Prefer concise output.")
	oldPreference.UpdatedAt = now.Add(-72 * time.Hour) // 3 days ago
	oldPreference.Importance = 0.5

	recentWorking := testEvent(MemoryScopeSession, MemoryKindWorkingMemory, "Currently refactoring auth module.")
	recentWorking.UpdatedAt = now.Add(-1 * time.Hour) // 1 hour ago
	recentWorking.Importance = 0.5

	// Both events match the query equally (no term overlap), so the
	// Weibull decay should differentiate them: a 3-day-old preference
	// should still outrank a 1-hour-old working memory because
	// preferences have a much longer half-life.
	events, err := reranker.Rerank(context.Background(), "output", []MemoryEvent{oldPreference, recentWorking})
	require.NoError(t, err)
	require.Len(t, events, 2)
	// Preference (long-lived kind) should rank higher than working memory
	// (ephemeral kind) even though the working memory is more recent.
	require.Equal(t, oldPreference.ID, events[0].ID)
}
