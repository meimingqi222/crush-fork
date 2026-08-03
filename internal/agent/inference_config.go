package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/session"
)

// InferenceResolver exposes side-effect-free session inference validation and
// projection without allowing callers to mutate workspace/global model maps.
type InferenceResolver interface {
	ValidateInferenceOverrides(context.Context, string, session.InferenceOverrides) error
	EffectiveInference(context.Context, string) (session.EffectiveInference, error)
	// DefaultInference resolves the inference config a brand-new session would
	// get without any per-session overrides, so callers can preview the workspace
	// default model before a session exists.
	DefaultInference(context.Context) (session.EffectiveInference, error)
}

type (
	turnInferenceOverridesKey struct{}
	frozenInferenceScopeKey   struct{}
)

type frozenInferenceScope struct {
	sessionID string
	overrides session.InferenceOverrides
	revision  uint64
}

// WithTurnInferenceOverrides attaches ephemeral overrides to one root turn.
// Coordinator.Run freezes the merged session/turn value before any model call.
func WithTurnInferenceOverrides(ctx context.Context, overrides session.InferenceOverrides) context.Context {
	return context.WithValue(ctx, turnInferenceOverridesKey{}, overrides)
}

func TurnInferenceOverridesFromContext(ctx context.Context) (session.InferenceOverrides, bool) {
	value, ok := ctx.Value(turnInferenceOverridesKey{}).(session.InferenceOverrides)
	return value, ok
}

func freezeInferenceScope(ctx context.Context, sess session.Session) context.Context {
	overrides := sess.Inference
	if turn, ok := ctx.Value(turnInferenceOverridesKey{}).(session.InferenceOverrides); ok {
		overrides = mergeInferenceOverrides(overrides, turn)
	}
	return context.WithValue(ctx, frozenInferenceScopeKey{}, frozenInferenceScope{
		sessionID: sess.ID, overrides: overrides, revision: sess.InferenceRevision,
	})
}

func mergeInferenceOverrides(base, override session.InferenceOverrides) session.InferenceOverrides {
	result := base
	if override.Model != "" || override.Provider != "" {
		result.Model, result.Provider = override.Model, override.Provider
	}
	if override.MaxOutputTokens != nil {
		result.MaxOutputTokens = override.MaxOutputTokens
	}
	if override.Temperature != nil {
		result.Temperature = override.Temperature
	}
	if override.TopP != nil {
		result.TopP = override.TopP
	}
	if override.TopK != nil {
		result.TopK = override.TopK
	}
	if override.FrequencyPenalty != nil {
		result.FrequencyPenalty = override.FrequencyPenalty
	}
	if override.PresencePenalty != nil {
		result.PresencePenalty = override.PresencePenalty
	}
	if override.Think != nil {
		result.Think = override.Think
	}
	if override.ReasoningEffort != nil {
		result.ReasoningEffort = override.ReasoningEffort
	}
	return result
}

func inferenceOverridesEmpty(value session.InferenceOverrides) bool {
	return value.Model == "" && value.Provider == "" && value.MaxOutputTokens == nil &&
		value.Temperature == nil && value.TopP == nil && value.TopK == nil &&
		value.FrequencyPenalty == nil && value.PresencePenalty == nil && value.Think == nil &&
		value.ReasoningEffort == nil
}

func (c *coordinator) inferenceOverridesForContext(ctx context.Context) (session.InferenceOverrides, uint64, error) {
	sessionID := tools.GetSessionFromContext(ctx)
	if frozen, ok := ctx.Value(frozenInferenceScopeKey{}).(frozenInferenceScope); ok && frozen.sessionID == sessionID {
		return frozen.overrides, frozen.revision, nil
	}
	if sessionID == "" {
		return session.InferenceOverrides{}, 0, nil
	}
	sess, err := c.sessions.Get(ctx, sessionID)
	if err != nil {
		return session.InferenceOverrides{}, 0, err
	}
	return sess.Inference, sess.InferenceRevision, nil
}

func (c *coordinator) applyInferenceOverrides(
	ctx context.Context,
	base Model,
	providerCfg config.ProviderConfig,
	isSubAgent bool,
) (Model, config.ProviderConfig, error) {
	overrides, _, err := c.inferenceOverridesForContext(ctx)
	if err != nil || inferenceOverridesEmpty(overrides) {
		return base, providerCfg, err
	}
	selected := base.ModelCfg
	applyInferenceToSelectedModel(&selected, overrides)
	return c.modelFromSelected(ctx, selected, isSubAgent)
}

func applyInferenceToSelectedModel(selected *config.SelectedModel, overrides session.InferenceOverrides) {
	if overrides.Model != "" || overrides.Provider != "" {
		selected.Model, selected.Provider = overrides.Model, overrides.Provider
	}
	if overrides.MaxOutputTokens != nil {
		selected.MaxTokens = *overrides.MaxOutputTokens
	}
	if overrides.Temperature != nil {
		selected.Temperature = overrides.Temperature
	}
	if overrides.TopP != nil {
		selected.TopP = overrides.TopP
	}
	if overrides.TopK != nil {
		selected.TopK = overrides.TopK
	}
	if overrides.FrequencyPenalty != nil {
		selected.FrequencyPenalty = overrides.FrequencyPenalty
	}
	if overrides.PresencePenalty != nil {
		selected.PresencePenalty = overrides.PresencePenalty
	}
	if overrides.Think != nil {
		selected.Think = overrides.Think
	}
	if overrides.ReasoningEffort != nil {
		selected.ReasoningEffort = *overrides.ReasoningEffort
	}
}

func validateInferenceValues(value session.InferenceOverrides) error {
	if (value.Model == "") != (value.Provider == "") {
		return errors.New("model and provider must be supplied together")
	}
	if value.MaxOutputTokens != nil && (*value.MaxOutputTokens <= 0 || *value.MaxOutputTokens > 200_000) {
		return errors.New("maxOutputTokens must be between 1 and 200000")
	}
	if value.Temperature != nil && (*value.Temperature < 0 || *value.Temperature > 2) {
		return errors.New("temperature must be between 0 and 2")
	}
	if value.TopP != nil && (*value.TopP < 0 || *value.TopP > 1) {
		return errors.New("topP must be between 0 and 1")
	}
	if value.TopK != nil && *value.TopK < 0 {
		return errors.New("topK must be non-negative")
	}
	for name, penalty := range map[string]*float64{
		"frequencyPenalty": value.FrequencyPenalty,
		"presencePenalty":  value.PresencePenalty,
	} {
		if penalty != nil && (*penalty < -2 || *penalty > 2) {
			return fmt.Errorf("%s must be between -2 and 2", name)
		}
	}
	if value.ReasoningEffort != nil {
		switch *value.ReasoningEffort {
		case "", "none", "minimal", "low", "medium", "high", "xhigh", "max":
		default:
			return errors.New("reasoningEffort must be one of none, minimal, low, medium, high, xhigh, max")
		}
	}
	return nil
}

func (c *coordinator) ValidateInferenceOverrides(ctx context.Context, sessionID string, overrides session.InferenceOverrides) error {
	if err := validateInferenceValues(overrides); err != nil {
		return err
	}
	if _, err := c.sessions.Get(ctx, sessionID); err != nil {
		return err
	}
	if overrides.Model == "" {
		return nil
	}
	_, err := c.lookupCatwalkModel(config.SelectedModel{Provider: overrides.Provider, Model: overrides.Model})
	return err
}

func (c *coordinator) EffectiveInference(ctx context.Context, sessionID string) (session.EffectiveInference, error) {
	sess, err := c.sessions.Get(ctx, sessionID)
	if err != nil {
		return session.EffectiveInference{}, err
	}
	selected, err := c.selectedModelForCoder(sess.CollaborationMode == session.CollaborationModePlan)
	if err != nil {
		return session.EffectiveInference{}, err
	}
	applyInferenceToSelectedModel(&selected, sess.Inference)
	if err := validateInferenceValues(sess.Inference); err != nil {
		return session.EffectiveInference{}, err
	}
	return c.buildEffectiveInference(selected, sess.InferenceRevision)
}

// DefaultInference resolves the model a brand-new coder session would use,
// without any per-session overrides or plan-mode substitution. It lets the GUI
// show the workspace default model in the composer before a session exists.
func (c *coordinator) DefaultInference(ctx context.Context) (session.EffectiveInference, error) {
	selected, err := c.selectedModelForCoder(false)
	if err != nil {
		return session.EffectiveInference{}, err
	}
	return c.buildEffectiveInference(selected, 0)
}

// selectedModelForCoder resolves the coder agent's configured SelectedModel,
// substituting the plan model type when planMode is set.
func (c *coordinator) selectedModelForCoder(planMode bool) (config.SelectedModel, error) {
	agentCfg, ok := c.cfg.Config().Agents[config.AgentCoder]
	if !ok {
		return config.SelectedModel{}, errCoderAgentNotConfigured
	}
	modelType := agentCfg.Model
	if modelType == "" {
		modelType = config.SelectedModelTypeLarge
	}
	if planMode {
		modelType = config.SelectedModelTypePlan
	}
	selected, ok := c.cfg.Config().SelectedModelForType(modelType)
	if !ok {
		return config.SelectedModel{}, fmt.Errorf("model type %q not configured", modelType)
	}
	return selected, nil
}

// buildEffectiveInference projects a resolved SelectedModel into the wire
// EffectiveInference, filling max output tokens from the model catalog default
// when the selection does not pin one.
func (c *coordinator) buildEffectiveInference(selected config.SelectedModel, revision uint64) (session.EffectiveInference, error) {
	if strings.TrimSpace(selected.Provider) == "" || strings.TrimSpace(selected.Model) == "" {
		return session.EffectiveInference{}, errors.New("effective model is incomplete")
	}
	maxTokens := selected.MaxTokens
	if maxTokens <= 0 {
		if model := c.cfg.Config().GetModel(selected.Provider, selected.Model); model != nil {
			maxTokens = model.DefaultMaxTokens
		}
	}
	// Reasoning effort: use the explicit selection, else fall back to the
	// catalog model's default so the GUI can preselect the real effective tier
	// before the user overrides it.
	reasoningEffort := selected.ReasoningEffort
	if reasoningEffort == "" {
		if model := c.cfg.Config().GetModel(selected.Provider, selected.Model); model != nil && model.CanReason {
			reasoningEffort = model.DefaultReasoningEffort
		}
	}
	result := session.EffectiveInference{
		InferenceOverrides: session.InferenceOverrides{
			Model: selected.Model, Provider: selected.Provider,
			Temperature: selected.Temperature, TopP: selected.TopP, TopK: selected.TopK,
			FrequencyPenalty: selected.FrequencyPenalty, PresencePenalty: selected.PresencePenalty,
			Think: selected.Think,
		},
		Revision: revision,
	}
	if reasoningEffort != "" {
		result.ReasoningEffort = &reasoningEffort
	}
	if maxTokens > 0 {
		result.MaxOutputTokens = &maxTokens
	}
	return result, nil
}
