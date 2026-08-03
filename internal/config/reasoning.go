package config

import (
	"strconv"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
)

// ResolveReasoningLevels fills in missing reasoning tiers on a catwalk.Model.
//
// Explicit levels from the user config or the Catwalk catalog are preserved. If
// the model can reason but has no levels, they are inferred from the model ID
// so the agent gating and GUI/TUI selectors always see a concrete set.
func ResolveReasoningLevels(model *catwalk.Model) {
	if len(model.ReasoningLevels) > 0 {
		return
	}
	if !model.CanReason {
		return
	}

	model.ReasoningLevels = inferReasoningLevels(model.ID)
}

// inferReasoningLevels returns the supported reasoning effort levels for a
// model based on its ID. Models that use binary thinking (no selectable effort)
// return an empty slice.
func inferReasoningLevels(modelID string) []string {
	id := strings.ToLower(modelID)

	switch {
	case isClaudeAdaptiveThinkingModel(id):
		// Claude 4.6+ supports effort-based adaptive thinking.
		return []string{"low", "medium", "high"}
	case strings.Contains(id, "claude"):
		// Older Claude thinking models only support a binary thinking toggle.
		return nil
	case strings.Contains(id, "gpt-5"):
		return []string{"low", "medium", "high"}
	case strings.Contains(id, "o3") || strings.Contains(id, "o4"):
		return []string{"low", "medium", "high"}
	case strings.Contains(id, "gemini"):
		return []string{"low", "medium", "high"}
	case isGLM52Model(id):
		// GLM-5.2 exposes native thinking levels high/max rather than the
		// conventional low/medium/high scale. See opencode's provider transform.
		return []string{"high", "max"}
	default:
		// Unknown reasoning model: expose generic effort levels. This matches
		// the agent behavior which treats an empty level list as "any effort
		// allowed" and keeps custom-provider reasoning configurable.
		return []string{"low", "medium", "high"}
	}
}

// isGLM52Model reports whether the model ID corresponds to GLM-5.2, whose
// native thinking levels are high/max instead of the usual low/medium/high.
func isGLM52Model(modelID string) bool {
	id := strings.ToLower(modelID)
	for _, pattern := range []string{"glm-5.2", "glm-5-2", "glm-5p2"} {
		if strings.Contains(id, pattern) {
			return true
		}
	}
	return false
}

// isClaudeAdaptiveThinkingModel reports whether a Claude model uses the
// effort-based "adaptive thinking" API introduced in Claude 4.6.
func isClaudeAdaptiveThinkingModel(modelID string) bool {
	// Provider-prefixed IDs such as "anthropic/claude-sonnet-4.6" are reduced
	// to the base model identifier before matching.
	id := modelID
	if idx := strings.LastIndex(id, "claude-"); idx > 0 {
		id = id[idx:]
	}
	id = strings.ToLower(id)

	for _, variant := range []string{"sonnet", "opus", "haiku"} {
		prefix := "claude-" + variant + "-4"
		for _, sep := range []string{".", "-"} {
			if !strings.HasPrefix(id, prefix+sep) {
				continue
			}
			minor := id[len(prefix)+1:]
			if minor == "" {
				continue
			}
			if n, err := strconv.Atoi(minor[:1]); err == nil && n >= 6 {
				return true
			}
		}
	}

	return false
}
