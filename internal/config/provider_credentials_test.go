package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/oauth"
	"github.com/stretchr/testify/require"
)

func TestClearProviderCredentialsPersistsOnceAndPreservesProvider(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "crush.json")
	require.NoError(t, os.WriteFile(path, []byte(`{
  "providers": {
    "copilot": {
      "api_key": "encrypted-secret",
      "oauth": {"access_token":"access-secret","refresh_token":"refresh-secret"},
      "base_url": "https://example.test/v1",
      "models": [{"id":"model"}]
    }
  }
}`), 0o600))
	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set("copilot", ProviderConfig{
		ID: "copilot", Name: "Copilot", BaseURL: "https://example.test/v1",
		APIKey: "access-secret", APIKeyTemplate: "$COPILOT_TOKEN",
		OAuthToken: &oauth.Token{AccessToken: "access-secret", RefreshToken: "refresh-secret"},
		Models:     []ProviderModel{ProviderModelID("model")},
	})
	store := &ConfigStore{config: &Config{Providers: providers}, globalDataPath: path}

	require.NoError(t, store.ClearProviderCredentials(ScopeGlobal, "copilot"))
	provider, ok := store.config.Providers.Get("copilot")
	require.True(t, ok)
	require.Empty(t, provider.APIKey)
	require.Empty(t, provider.APIKeyTemplate)
	require.Nil(t, provider.OAuthToken)
	require.Equal(t, "https://example.test/v1", provider.BaseURL)
	require.Equal(t, "model", provider.Models[0].ID)

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var persisted map[string]any
	require.NoError(t, json.Unmarshal(raw, &persisted))
	providerJSON := persisted["providers"].(map[string]any)["copilot"].(map[string]any)
	require.NotContains(t, providerJSON, "api_key")
	require.NotContains(t, providerJSON, "oauth")
	require.Equal(t, "https://example.test/v1", providerJSON["base_url"])
	require.Contains(t, providerJSON, "models")
}

func TestClearProviderCredentialsDoesNotMutateMemoryOnPersistenceFailure(t *testing.T) {
	t.Parallel()

	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set("provider", ProviderConfig{
		APIKey: "secret", APIKeyTemplate: "$TOKEN",
		OAuthToken: &oauth.Token{AccessToken: "secret", RefreshToken: "refresh"},
	})
	store := &ConfigStore{
		config: &Config{Providers: providers}, globalDataPath: t.TempDir(),
	}

	require.Error(t, store.ClearProviderCredentials(ScopeGlobal, "provider"))
	provider, ok := store.config.Providers.Get("provider")
	require.True(t, ok)
	require.Equal(t, "secret", provider.APIKey)
	require.Equal(t, "$TOKEN", provider.APIKeyTemplate)
	require.Equal(t, "refresh", provider.OAuthToken.RefreshToken)
}

func TestProviderCatalogSnapshotIsDetached(t *testing.T) {
	t.Parallel()

	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set("custom", ProviderConfig{
		ID: "custom", APIKey: "secret", ExtraHeaders: map[string]string{"Authorization": "secret"},
		OAuthToken: &oauth.Token{AccessToken: "secret"},
		Models:     []ProviderModel{{Model: catwalk.Model{ID: "configured", ReasoningLevels: []string{"low"}}}},
	})
	store := &ConfigStore{
		config: &Config{Providers: providers},
		knownProviders: []catwalk.Provider{{
			ID: "known", DefaultHeaders: map[string]string{"Authorization": "secret"},
			Models: []catwalk.Model{{ID: "known-model", ReasoningLevels: []string{"high"}}},
		}},
	}

	known, configured := store.ProviderCatalogSnapshot()
	known[0].DefaultHeaders["Authorization"] = "changed"
	known[0].Models[0].ReasoningLevels[0] = "changed"
	configuredProvider := configured["custom"]
	configuredProvider.ExtraHeaders["Authorization"] = "changed"
	configuredProvider.OAuthToken.AccessToken = "changed"
	configuredProvider.Models[0].ReasoningLevels[0] = "changed"

	knownAgain, configuredAgain := store.ProviderCatalogSnapshot()
	require.Equal(t, "secret", knownAgain[0].DefaultHeaders["Authorization"])
	require.Equal(t, "high", knownAgain[0].Models[0].ReasoningLevels[0])
	require.Equal(t, "secret", configuredAgain["custom"].ExtraHeaders["Authorization"])
	require.Equal(t, "secret", configuredAgain["custom"].OAuthToken.AccessToken)
	require.Equal(t, "low", configuredAgain["custom"].Models[0].ReasoningLevels[0])

	knownOnly := store.KnownProviders()
	knownOnly[0].Models[0].ID = "changed"
	require.Equal(t, "known-model", store.KnownProviders()[0].Models[0].ID)
}

func TestSetProviderAPIKeyValidatesBeforePersistence(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "crush.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"providers":{}}`), 0o600))
	store := &ConfigStore{
		config:         &Config{Providers: csync.NewMap[string, ProviderConfig]()},
		globalDataPath: path,
	}

	require.Error(t, store.SetProviderAPIKey(ScopeGlobal, "unknown", "must-not-persist"))
	require.Error(t, store.SetProviderAPIKey(ScopeGlobal, "unknown", struct{}{}))
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "must-not-persist")
	require.JSONEq(t, `{"providers":{}}`, string(raw))
}

func TestSetProviderAPIKeyReplacesPersistedOAuthAtomically(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "crush.json")
	require.NoError(t, os.WriteFile(path, []byte(`{
  "providers":{"custom":{"oauth":{"access_token":"old-secret"}}}
}`), 0o600))
	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set("custom", ProviderConfig{
		ID: "custom", OAuthToken: &oauth.Token{AccessToken: "old-secret"},
	})
	store := &ConfigStore{config: &Config{Providers: providers}, globalDataPath: path}

	require.NoError(t, store.SetProviderAPIKey(ScopeGlobal, "custom", "new-secret"))
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var persisted map[string]any
	require.NoError(t, json.Unmarshal(raw, &persisted))
	providerJSON := persisted["providers"].(map[string]any)["custom"].(map[string]any)
	require.NotContains(t, providerJSON, "oauth")
	storedKey := providerJSON["api_key"].(string)
	decrypted, err := DecryptAPIKey(storedKey)
	require.NoError(t, err)
	require.Equal(t, "new-secret", decrypted)
	provider, ok := store.config.Providers.Get("custom")
	require.True(t, ok)
	require.Nil(t, provider.OAuthToken)
	require.Equal(t, "new-secret", provider.APIKey)
}

func TestMCPSnapshotIsDetached(t *testing.T) {
	t.Parallel()

	store := &ConfigStore{config: &Config{MCP: MCPs{
		"server": {
			Args: []string{"arg"}, Env: map[string]string{"TOKEN": "secret"},
			Headers: map[string]string{"Authorization": "secret"},
			OAuth: &MCPOAuthConfig{
				ClientSecret: "client-secret", Token: &oauth.Token{AccessToken: "access-secret"},
				Registration: &MCPOAuthRegistration{ClientID: "client-id"},
				AuthServer: &MCPOAuthAuthServer{
					TokenEndpointAuthMethodsSupported: []string{"client_secret_post"},
				},
				Scopes: []string{"read"},
			},
		},
	}}}
	snapshot := store.MCPSnapshot()
	value := snapshot["server"]
	value.Args[0] = "changed"
	value.Env["TOKEN"] = "changed"
	value.Headers["Authorization"] = "changed"
	value.OAuth.ClientSecret = "changed"
	value.OAuth.Token.AccessToken = "changed"
	value.OAuth.Registration.ClientID = "changed"
	value.OAuth.AuthServer.TokenEndpointAuthMethodsSupported[0] = "changed"
	value.OAuth.Scopes[0] = "changed"

	again := store.MCPSnapshot()["server"]
	require.Equal(t, "arg", again.Args[0])
	require.Equal(t, "secret", again.Env["TOKEN"])
	require.Equal(t, "secret", again.Headers["Authorization"])
	require.Equal(t, "client-secret", again.OAuth.ClientSecret)
	require.Equal(t, "access-secret", again.OAuth.Token.AccessToken)
	require.Equal(t, "client-id", again.OAuth.Registration.ClientID)
	require.Equal(t, "client_secret_post", again.OAuth.AuthServer.TokenEndpointAuthMethodsSupported[0])
	require.Equal(t, "read", again.OAuth.Scopes[0])
}
