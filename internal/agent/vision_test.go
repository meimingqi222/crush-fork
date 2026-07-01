package agent

import (
	"context"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestVisionServiceAvailableForConfiguredModelMissingProviderMetadata(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	coord := newTestCoordinator(t, env, "test-provider", config.ProviderConfig{
		ID:      "test-provider",
		Type:    openaicompat.Name,
		BaseURL: "https://example.test/v1",
		APIKey:  "test-key",
		Models: []config.ProviderModel{
			{Model: catwalk.Model{ID: "text-model", SupportsImages: false}},
		},
	})
	coord.cfg.Config().Models[config.SelectedModelTypeVision] = config.SelectedModel{
		Provider:  "test-provider",
		Model:     "private-vision-model",
		MaxTokens: 4096,
	}

	vision := NewVisionService(coord)
	require.True(t, vision.IsAvailable())
}

func TestVisionServiceResolveSynthesizesMetadataForUnlistedConfiguredModel(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	coord := newTestCoordinator(t, env, "test-provider", config.ProviderConfig{
		ID:      "test-provider",
		Type:    openaicompat.Name,
		BaseURL: "https://example.test/v1",
		APIKey:  "test-key",
		Models: []config.ProviderModel{
			{Model: catwalk.Model{ID: "text-model", SupportsImages: false}},
		},
	})
	coord.cfg.Config().Models[config.SelectedModelTypeVision] = config.SelectedModel{
		Provider:      "test-provider",
		Model:         "private-vision-model",
		MaxTokens:     4096,
		ContextWindow: 128000,
	}

	vision := NewVisionService(coord)
	model, providerCfg, err := vision.resolveVisionModel(context.Background())
	require.NoError(t, err)
	require.Equal(t, "test-provider", providerCfg.ID)
	require.Equal(t, "private-vision-model", model.ModelCfg.Model)
	require.Equal(t, "private-vision-model", model.CatwalkCfg.ID)
	require.True(t, model.CatwalkCfg.SupportsImages)
	require.Equal(t, int64(4096), model.CatwalkCfg.DefaultMaxTokens)
	require.Equal(t, int64(128000), model.CatwalkCfg.ContextWindow)
	require.NotNil(t, model.Model)
}

func TestBuildAgentReceivesCoordinatorVisionService(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	coord := newTestCoordinator(t, env, "test-provider", config.ProviderConfig{
		ID:      "test-provider",
		Type:    openaicompat.Name,
		BaseURL: "https://example.test/v1",
		APIKey:  "test-key",
		Models: []config.ProviderModel{
			{Model: catwalk.Model{ID: "test-model"}},
		},
	})
	coord.cfg.Config().Models[config.SelectedModelTypeLarge] = config.SelectedModel{
		Provider: "test-provider",
		Model:    "test-model",
	}
	coord.cfg.Config().Models[config.SelectedModelTypeSmall] = config.SelectedModel{
		Provider: "test-provider",
		Model:    "test-model",
	}
	coord.visionService = NewVisionService(coord)

	agent, err := coord.buildAgent(context.Background(), nil, config.Agent{}, true)
	require.NoError(t, err)
	sessionAgent, ok := agent.(*sessionAgent)
	require.True(t, ok)
	require.Same(t, coord.visionService, sessionAgent.visionService)
}
