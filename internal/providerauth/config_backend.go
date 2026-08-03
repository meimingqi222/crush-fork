package providerauth

import (
	"slices"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
	hyperprovider "github.com/charmbracelet/crush/internal/agent/hyper"
	"github.com/charmbracelet/crush/internal/config"
)

type ConfigBackend struct {
	store *config.ConfigStore
}

func NewConfigBackend(store *config.ConfigStore) *ConfigBackend {
	return &ConfigBackend{store: store}
}

func (b *ConfigBackend) Providers() []Provider {
	known, configured := b.store.ProviderCatalogSnapshot()
	return projectProviders(known, configured)
}

func projectProviders(known []catwalk.Provider, configured map[string]config.ProviderConfig) []Provider {
	knownByID := make(map[string]catwalk.Provider, len(known))
	ids := make(map[string]struct{}, len(known)+len(configured))
	for _, provider := range known {
		id := string(provider.ID)
		knownByID[id] = provider
		ids[id] = struct{}{}
	}
	for id := range configured {
		ids[id] = struct{}{}
	}

	providers := make([]Provider, 0, len(ids))
	for id := range ids {
		knownProvider, isKnown := knownByID[id]
		configuredProvider, isConfigured := configured[id]
		name := knownProvider.Name
		providerType := string(knownProvider.Type)
		models := len(knownProvider.Models)
		disabled := false
		authenticated := false
		if isConfigured {
			name = configuredProvider.Name
			providerType = string(configuredProvider.Type)
			models = len(configuredProvider.Models)
			disabled = configuredProvider.Disable
			authenticated = configuredProvider.APIKey != "" || configuredProvider.OAuthToken != nil
		}
		if name == "" {
			name = id
		}
		providers = append(providers, Provider{
			ID: id, Name: name, Type: providerType, AuthMethods: authMethods(id, isKnown, isConfigured, knownProvider),
			Configured: isConfigured, Authenticated: authenticated, Disabled: disabled, ModelCount: models,
		})
	}
	slices.SortFunc(providers, func(a, b Provider) int { return strings.Compare(a.ID, b.ID) })
	return providers
}

func (b *ConfigBackend) Models(providerID string) ([]Model, error) {
	known, configured := b.store.ProviderCatalogSnapshot()
	return projectModels(providerID, known, configured)
}

func projectModels(providerID string, known []catwalk.Provider, configured map[string]config.ProviderConfig) ([]Model, error) {
	var models []catwalk.Model
	found := false
	if provider, ok := configured[providerID]; ok {
		found = true
		models = make([]catwalk.Model, len(provider.Models))
		for i := range provider.Models {
			models[i] = provider.Models[i].Model
		}
	} else {
		for _, provider := range known {
			if string(provider.ID) == providerID {
				found = true
				models = provider.Models
				break
			}
		}
	}
	if !found {
		return nil, ErrProviderNotFound
	}
	result := make([]Model, 0, len(models))
	for _, model := range models {
		if model.ID == "" {
			continue
		}
		// Idempotent: config-backed models were already resolved at load time;
		// this covers the known-catalog branch (providers absent from user
		// config) so the GUI always receives concrete reasoning tiers.
		config.ResolveReasoningLevels(&model)
		result = append(result, Model{
			ProviderID: providerID, ID: model.ID, Name: model.Name,
			ContextWindow: model.ContextWindow, MaxOutputTokens: model.DefaultMaxTokens,
			CanReason: model.CanReason, ReasoningLevels: slices.Clone(model.ReasoningLevels),
			DefaultReasoningEffort: model.DefaultReasoningEffort, SupportsImages: model.SupportsImages,
		})
	}
	slices.SortFunc(result, func(a, b Model) int { return strings.Compare(a.ID, b.ID) })
	return result, nil
}

func (b *ConfigBackend) AuthStatus(providerID string) (AuthStatus, error) {
	for _, provider := range b.Providers() {
		if provider.ID == providerID {
			return AuthStatus{ProviderID: providerID, Authenticated: provider.Authenticated}, nil
		}
	}
	return AuthStatus{}, ErrProviderNotFound
}

func (b *ConfigBackend) SaveCredential(providerID string, credential Credential) error {
	if credential.Token != nil {
		return b.store.SetProviderAPIKey(config.ScopeGlobal, providerID, credential.Token)
	}
	if credential.APIKey == "" {
		return ErrAuthMethodUnsupported
	}
	return b.store.SetProviderAPIKey(config.ScopeGlobal, providerID, credential.APIKey)
}

func (b *ConfigBackend) ClearCredential(providerID string) error {
	if _, err := b.AuthStatus(providerID); err != nil {
		return err
	}
	return b.store.ClearProviderCredentials(config.ScopeGlobal, providerID)
}

func authMethods(id string, known, configured bool, provider catwalk.Provider) []AuthMethod {
	switch id {
	case hyperprovider.Name, string(catwalk.InferenceProviderCopilot):
		return []AuthMethod{AuthMethodDeviceCode}
	case string(catwalk.InferenceProviderBedrock), string(catwalk.InferenceProviderVertexAI):
		return nil
	}
	if configured || known && provider.APIKey != "" {
		return []AuthMethod{AuthMethodAPIKey}
	}
	return nil
}
