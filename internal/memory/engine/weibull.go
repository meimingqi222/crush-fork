package engine

import "math"

// WeibullParams holds the shape (k) and scale (eta) parameters for a Weibull
// decay curve. The decay factor for a memory of ageHours is:
//
//	decay = exp(-(ageHours / eta) ^ k)
//
// Lower k produces a heavier tail (slower long-term decay). Higher eta
// stretches the timescale (slower decay overall).
type WeibullParams struct {
	K   float64 // Shape parameter.
	Eta float64 // Scale parameter in hours.
}

// Decay returns the Weibull decay factor for the given age in hours.
func (p WeibullParams) Decay(ageHours float64) float64 {
	if ageHours <= 0 {
		return 1.0
	}
	if p.Eta <= 0 {
		return 0.0
	}
	return math.Exp(-math.Pow(ageHours/p.Eta, p.K))
}

// weibullParamsForKind returns the Weibull decay parameters for a given
// memory kind. The parameters are tuned so that long-lived knowledge
// (preferences, decisions) decays slowly while transient state (working
// memory, task state) decays quickly.
//
// Reference values from Mnemopi's per-type Weibull table, adapted to
// Crush's MemoryKind taxonomy. Eta values are in hours.
func weibullParamsForKind(kind MemoryKind) WeibullParams {
	switch kind {
	case MemoryKindPreference:
		// User preferences are very stable: ~6 month half-life.
		return WeibullParams{K: 0.4, Eta: 4380.0}
	case MemoryKindDecision:
		// Architectural decisions are moderately stable: ~2 week half-life.
		return WeibullParams{K: 1.0, Eta: 336.0}
	case MemoryKindProcedure:
		// Procedures and workflows: ~10 week half-life.
		return WeibullParams{K: 0.6, Eta: 1680.0}
	case MemoryKindPitfall:
		// Pitfalls and gotchas: ~8 week half-life.
		return WeibullParams{K: 0.7, Eta: 1440.0}
	case MemoryKindReference:
		// Reference material: ~4 week half-life.
		return WeibullParams{K: 0.8, Eta: 720.0}
	case MemoryKindWorkingMemory:
		// Working memory is transient: ~1 day half-life.
		return WeibullParams{K: 1.5, Eta: 24.0}
	case MemoryKindTaskState:
		// Task state is the most ephemeral: ~3 hour half-life.
		return WeibullParams{K: 1.8, Eta: 12.0}
	default:
		// Default: ~1 week half-life (general knowledge).
		return WeibullParams{K: 1.0, Eta: 168.0}
	}
}
