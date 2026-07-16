package providerauth

import (
	"encoding/json"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/oauth"
	"github.com/stretchr/testify/require"
)

func TestConfigBackendProjectionMergesCatalogWithoutSecrets(t *testing.T) {
	t.Parallel()

	known := []catwalk.Provider{
		{
			ID: "openai", Name: "Known OpenAI", Type: catwalk.TypeOpenAI,
			APIKey: "$OPENAI_API_KEY", APIEndpoint: "https://user:password@example.test/v1",
			DefaultHeaders: map[string]string{"Authorization": "Bearer known-secret"},
			Models: []catwalk.Model{{
				ID: "known-model", Name: "Known", ContextWindow: 100,
				DefaultMaxTokens: 20, CanReason: true, ReasoningLevels: []string{"low"},
				Options: catwalk.ModelOptions{ProviderOptions: map[string]any{"token": "model-secret"}},
			}},
		},
		{ID: "copilot", Name: "Copilot", APIKey: "$COPILOT_TOKEN"},
	}
	configured := map[string]config.ProviderConfig{
		"openai": {
			ID: "openai", Name: "Configured OpenAI", Type: catwalk.TypeOpenAI,
			APIKey: "configured-secret", BaseURL: "https://secret-endpoint.test/v1",
			ExtraHeaders:    map[string]string{"Authorization": "Bearer configured-secret"},
			ProviderOptions: map[string]any{"token": "option-secret"},
			Models:          []config.ProviderModel{{Model: catwalk.Model{ID: "configured-model", Name: "Configured", ContextWindow: 200, SupportsImages: true}}},
		},
		"custom": {
			ID: "custom", Name: "Custom", Type: catwalk.TypeOpenAICompat,
			OAuthToken: &oauth.Token{AccessToken: "oauth-secret"},
			Models:     []config.ProviderModel{{Model: catwalk.Model{ID: "custom-model"}}},
		},
	}

	providers := projectProviders(known, configured)
	require.Equal(t, []string{"copilot", "custom", "openai"}, []string{providers[0].ID, providers[1].ID, providers[2].ID})
	require.Equal(t, "Configured OpenAI", providers[2].Name)
	require.True(t, providers[2].Configured)
	require.True(t, providers[2].Authenticated)
	require.Equal(t, 1, providers[2].ModelCount)
	require.Equal(t, []AuthMethod{AuthMethodAPIKey}, providers[2].AuthMethods)
	require.Equal(t, []AuthMethod{AuthMethodDeviceCode}, providers[0].AuthMethods)
	require.Equal(t, []AuthMethod{AuthMethodAPIKey}, providers[1].AuthMethods)

	models, err := projectModels("openai", known, configured)
	require.NoError(t, err)
	require.Equal(t, "configured-model", models[0].ID)
	require.Equal(t, int64(200), models[0].ContextWindow)
	require.True(t, models[0].SupportsImages)

	wire, err := json.Marshal(struct {
		Providers []Provider `json:"providers"`
		Models    []Model    `json:"models"`
	}{providers, models})
	require.NoError(t, err)
	for _, secret := range []string{
		"known-secret", "configured-secret", "oauth-secret", "model-secret",
		"option-secret", "secret-endpoint", "password",
	} {
		require.NotContains(t, string(wire), secret)
	}
}

func TestConfigBackendProjectionSupportsEmptyModelCatalog(t *testing.T) {
	t.Parallel()

	models, err := projectModels("empty", []catwalk.Provider{{ID: "empty"}}, nil)
	require.NoError(t, err)
	require.Empty(t, models)
	_, err = projectModels("missing", []catwalk.Provider{{ID: "empty"}}, nil)
	require.ErrorIs(t, err, ErrProviderNotFound)
}
