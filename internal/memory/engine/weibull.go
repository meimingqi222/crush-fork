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

// VoiceWeights defines the RRF fusion weights for each retrieval "voice".
// These weights are tuned to match Mnemopi's polyphonic recall profile.
type VoiceWeights struct {
	Vector    float64 // Semantic vector similarity
	FTS       float64 // Lexical full-text search
	Temporal  float64 // Recency-weighted temporal scoring
	Triple    float64 // Knowledge graph / triple matching
}

// DefaultVoiceWeights returns the recommended voice balance:
// vector semantics lead, with graph/fact and temporal signals as complements.
func DefaultVoiceWeights() VoiceWeights {
	return VoiceWeights{
		Vector:   0.35,
		FTS:      0.25,
		Temporal: 0.15,
		Triple:   0.25,
	}
}

// weibullParamsForKind returns the Weibull decay parameters for a given
// memory kind. The parameters are tuned so that long-lived knowledge
// (profile, preferences, corrections) decays slowly while transient state
// (working memory, task state) decays quickly.
//
// Values are adapted from Mnemopi's per-type Weibull table of 22 types,
// with k (shape) and eta (scale in hours). Lower k = heavier tail (slower
// long-term decay); higher eta = longer overall timescale.
func weibullParamsForKind(kind MemoryKind) WeibullParams {
	switch kind {
	// --- Long-lived (months to years) ---
	case MemoryKindProfile:
		return WeibullParams{K: 0.3, Eta: 8760.0} // ~1 year half-life
	case MemoryKindIdentity:
		return WeibullParams{K: 0.35, Eta: 8760.0} // ~1 year
	case MemoryKindPreference:
		return WeibullParams{K: 0.4, Eta: 4380.0} // ~6 months
	case MemoryKindCorrection:
		return WeibullParams{K: 0.5, Eta: 4380.0} // ~6 months
	case MemoryKindConstraint:
		return WeibullParams{K: 0.5, Eta: 4380.0} // ~6 months
	case MemoryKindTeam:
		return WeibullParams{K: 0.5, Eta: 4380.0} // ~6 months

	// --- Medium-term (weeks to months) ---
	case MemoryKindSkill:
		return WeibullParams{K: 0.6, Eta: 2160.0} // ~3 months
	case MemoryKindProject:
		return WeibullParams{K: 0.6, Eta: 2160.0} // ~3 months
	case MemoryKindProcedure:
		return WeibullParams{K: 0.6, Eta: 1680.0} // ~10 weeks
	case MemoryKindPitfall:
		return WeibullParams{K: 0.7, Eta: 1440.0} // ~8 weeks
	case MemoryKindLesson:
		return WeibullParams{K: 0.8, Eta: 720.0} // ~30 days
	case MemoryKindPattern:
		return WeibullParams{K: 0.85, Eta: 720.0} // ~30 days

	// --- Shorter-term (days to weeks) ---
	case MemoryKindFact:
		return WeibullParams{K: 0.8, Eta: 720.0} // ~30 days
	case MemoryKindReference:
		return WeibullParams{K: 0.8, Eta: 720.0} // ~30 days
	case MemoryKindDecision:
		return WeibullParams{K: 1.0, Eta: 336.0} // ~2 weeks
	case MemoryKindApproach:
		return WeibullParams{K: 0.9, Eta: 504.0} // ~3 weeks
	case MemoryKindAttempt:
		return WeibullParams{K: 1.0, Eta: 336.0} // ~2 weeks
	case MemoryKindOutcome:
		return WeibullParams{K: 1.1, Eta: 336.0} // ~2 weeks
	case MemoryKindContext:
		return WeibullParams{K: 1.1, Eta: 336.0} // ~2 weeks
	case MemoryKindEvent:
		return WeibullParams{K: 1.2, Eta: 168.0} // ~1 week
	case MemoryKindConversation:
		return WeibullParams{K: 1.2, Eta: 168.0} // ~1 week

	// --- Transient (hours to days) ---
	case MemoryKindRequest:
		return WeibullParams{K: 1.5, Eta: 72.0} // ~3 days
	case MemoryKindWorkingMemory:
		return WeibullParams{K: 1.5, Eta: 24.0} // ~1 day
	case MemoryKindTaskState:
		return WeibullParams{K: 1.8, Eta: 12.0} // ~12 hours

	default:
		// Default: ~1 week half-life for general/unknown kinds.
		return WeibullParams{K: 1.0, Eta: 168.0}
	}
}
