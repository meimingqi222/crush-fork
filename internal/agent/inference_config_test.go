package agent

import (
	"context"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestEffectiveInferencePrecedenceAndGlobalImmutability(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	coord := newTestCoordinator(t, env, "provider", config.ProviderConfig{
		ID: "provider", Type: catwalk.TypeOpenAICompat,
		Models: []config.ProviderModel{
			{Model: catwalk.Model{ID: "global", DefaultMaxTokens: 8192}},
			{Model: catwalk.Model{ID: "profile", DefaultMaxTokens: 8192}},
			{Model: catwalk.Model{ID: "plan", DefaultMaxTokens: 8192}},
			{Model: catwalk.Model{ID: "session", DefaultMaxTokens: 16384}},
		},
	})
	globalTemperature := 0.1
	coord.cfg.Config().Agents[config.AgentCoder] = config.Agent{Model: config.SelectedModelTypeReview}
	coord.cfg.Config().Models[config.SelectedModelTypeLarge] = config.SelectedModel{
		Provider: "provider", Model: "global", Temperature: &globalTemperature,
	}
	coord.cfg.Config().Models[config.SelectedModelTypeReview] = config.SelectedModel{Provider: "provider", Model: "profile"}
	coord.cfg.Config().Models[config.SelectedModelTypePlan] = config.SelectedModel{Provider: "provider", Model: "plan"}
	sess, err := env.sessions.Create(t.Context(), "precedence")
	require.NoError(t, err)
	mutations := env.sessions.(session.MutationService)
	effective, err := coord.EffectiveInference(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, "profile", effective.Model)

	sess.CollaborationMode = session.CollaborationModePlan
	sess, err = env.sessions.Save(t.Context(), sess)
	require.NoError(t, err)
	effective, err = coord.EffectiveInference(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, "plan", effective.Model)

	sessionTemperature := 0.4
	maxTokens := int64(4096)
	sess, err = mutations.UpdateInference(t.Context(), sess.ID, 0, session.InferenceOverrides{
		Provider: "provider", Model: "session", Temperature: &sessionTemperature, MaxOutputTokens: &maxTokens,
	})
	require.NoError(t, err)

	effective, err = coord.EffectiveInference(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, "session", effective.Model)
	require.Equal(t, "provider", effective.Provider)
	require.Equal(t, 0.4, *effective.Temperature)
	require.Equal(t, int64(4096), *effective.MaxOutputTokens)
	require.Equal(t, uint64(1), effective.Revision)
	require.Equal(t, "global", coord.cfg.Config().Models[config.SelectedModelTypeLarge].Model)
	require.Equal(t, 0.1, *coord.cfg.Config().Models[config.SelectedModelTypeLarge].Temperature)
}

func TestTurnInferenceIsFrozenAndDoesNotLeakToSubagents(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	coord := newTestCoordinator(t, env, "provider", config.ProviderConfig{ID: "provider"})
	parent, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	child, err := env.sessions.CreateTaskSession(t.Context(), "child", parent.ID, "child")
	require.NoError(t, err)
	mutations := env.sessions.(session.MutationService)
	sessionTemperature := 0.2
	parent, err = mutations.UpdateInference(t.Context(), parent.ID, 0, session.InferenceOverrides{Temperature: &sessionTemperature})
	require.NoError(t, err)
	turnTemperature := 0.9
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, parent.ID)
	ctx = WithTurnInferenceOverrides(ctx, session.InferenceOverrides{Temperature: &turnTemperature})
	ctx = freezeInferenceScope(ctx, parent)

	newTemperature := 0.5
	_, err = mutations.UpdateInference(t.Context(), parent.ID, 1, session.InferenceOverrides{Temperature: &newTemperature})
	require.NoError(t, err)
	frozen, revision, err := coord.inferenceOverridesForContext(ctx)
	require.NoError(t, err)
	require.Equal(t, 0.9, *frozen.Temperature)
	require.Equal(t, uint64(1), revision)

	childCtx := context.WithValue(ctx, tools.SessionIDContextKey, child.ID)
	childOverrides, childRevision, err := coord.inferenceOverridesForContext(childCtx)
	require.NoError(t, err)
	require.Equal(t, session.InferenceOverrides{}, childOverrides)
	require.Zero(t, childRevision)

	freshCtx := context.WithValue(t.Context(), tools.SessionIDContextKey, parent.ID)
	fresh, revision, err := coord.inferenceOverridesForContext(freshCtx)
	require.NoError(t, err)
	require.Equal(t, 0.5, *fresh.Temperature)
	require.Equal(t, uint64(2), revision)
}

func TestRuntimeModelIsPerRunDespiteSharedAgentModelChanges(t *testing.T) {
	t.Parallel()

	shared := Model{ModelCfg: config.SelectedModel{Provider: "provider", Model: "shared"}}
	first := Model{ModelCfg: config.SelectedModel{Provider: "provider", Model: "session-a"}}
	second := Model{ModelCfg: config.SelectedModel{Provider: "provider", Model: "session-b"}}
	require.Equal(t, "session-a", effectiveRuntimeModel(shared, &sessionAgentRuntimeConfig{Model: &first}).ModelCfg.Model)
	require.Equal(t, "session-b", effectiveRuntimeModel(shared, &sessionAgentRuntimeConfig{Model: &second}).ModelCfg.Model)
	require.Equal(t, "shared", effectiveRuntimeModel(shared, nil).ModelCfg.Model)
}
