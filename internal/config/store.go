package config

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	hyperp "github.com/charmbracelet/crush/internal/agent/hyper"
	"github.com/charmbracelet/crush/internal/oauth"
	"github.com/charmbracelet/crush/internal/oauth/copilot"
	"github.com/charmbracelet/crush/internal/oauth/hyper"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConfigStore is the single entry point for all config access. It owns the
// pure-data Config, runtime state (working directory, resolver, known
// providers), and persistence to both global and workspace config files.
type ConfigStore struct {
	config         *Config
	configMu       sync.RWMutex // protects config and its mutable maps/fields
	workingDir     string
	workingDirMu   sync.RWMutex // protects workingDir
	resolver       VariableResolver
	globalDataPath string // ~/.local/share/crush/crush.json
	workspacePath  string // <workspace>/.crush/crush.json
	projectDataDir string // ~/.local/share/crush/projects/<slug>/
	knownProviders []catwalk.Provider
}

// Config returns the pure-data config struct.
//
// The returned pointer aliases the store's live config. Callers must not
// mutate the MCP, Models, or RecentModels maps in place; use the ConfigStore
// setter methods instead, which apply copy-on-write under configMu. Reader
// access to these maps is safe because writers never mutate an installed map.
func (s *ConfigStore) Config() *Config {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.config
}

// WorkingDir returns the current working directory.
func (s *ConfigStore) WorkingDir() string {
	s.workingDirMu.RLock()
	defer s.workingDirMu.RUnlock()
	return s.workingDir
}

// SetWorkingDir updates the current working directory.
func (s *ConfigStore) SetWorkingDir(dir string) {
	s.workingDirMu.Lock()
	s.workingDir = dir
	s.workingDirMu.Unlock()
}

// Resolver returns the variable resolver.
func (s *ConfigStore) Resolver() VariableResolver {
	return s.resolver
}

// Resolve resolves a variable reference using the configured resolver.
func (s *ConfigStore) Resolve(key string) (string, error) {
	if s.resolver == nil {
		return "", fmt.Errorf("no variable resolver configured")
	}
	return s.resolver.ResolveValue(key)
}

// KnownProviders returns the list of known providers.
func (s *ConfigStore) KnownProviders() []catwalk.Provider {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return cloneCatwalkProviders(s.knownProviders)
}

// ProviderCatalogSnapshot returns detached known and configured provider
// values for read-only discovery surfaces. The snapshot may contain secrets
// and therefore must be projected into a safe DTO before leaving the process.
func (s *ConfigStore) ProviderCatalogSnapshot() ([]catwalk.Provider, map[string]ProviderConfig) {
	s.configMu.RLock()
	defer s.configMu.RUnlock()

	configured := make(map[string]ProviderConfig)
	if s.config != nil && s.config.Providers != nil {
		for id, provider := range s.config.Providers.Seq2() {
			configured[id] = cloneProviderConfig(provider)
		}
	}
	return cloneCatwalkProviders(s.knownProviders), configured
}

// SetupAgents configures the built-in agents and merges configured overrides.
func (s *ConfigStore) SetupAgents() {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	s.config.SetupAgents()
}

// ProjectDataDir returns the centralized project data directory path.
// This is where sessions, memory, and logs are stored.
func (s *ConfigStore) ProjectDataDir() string {
	return s.projectDataDir
}

// configPath returns the file path for the given scope.
func (s *ConfigStore) configPath(scope Scope) string {
	switch scope {
	case ScopeWorkspace:
		return s.workspacePath
	default:
		return s.globalDataPath
	}
}

// HasConfigField checks whether a key exists in the config file for the given
// scope.
func (s *ConfigStore) HasConfigField(scope Scope, key string) bool {
	data, err := os.ReadFile(s.configPath(scope))
	if err != nil {
		return false
	}
	return gjson.Get(string(data), key).Exists()
}

// SetConfigField sets a key/value pair in the config file for the given scope.
func (s *ConfigStore) SetConfigField(scope Scope, key string, value any) error {
	path := s.configPath(scope)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			data = []byte("{}")
		} else {
			return fmt.Errorf("failed to read config file: %w", err)
		}
	}

	newValue, err := sjson.Set(string(data), key, value)
	if err != nil {
		return fmt.Errorf("failed to set config field %s: %w", key, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("failed to create config directory %q: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(newValue), 0o600); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}
	return nil
}

// RemoveConfigField removes a key from the config file for the given scope.
func (s *ConfigStore) RemoveConfigField(scope Scope, key string) error {
	path := s.configPath(scope)
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read config file: %w", err)
	}

	newValue, err := sjson.Delete(string(data), key)
	if err != nil {
		return fmt.Errorf("failed to delete config field %s: %w", key, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("failed to create config directory %q: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(newValue), 0o600); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}
	return nil
}

// UpdatePreferredModel updates the preferred model for the given type and
// persists it to the config file at the given scope.
func (s *ConfigStore) UpdatePreferredModel(scope Scope, modelType SelectedModelType, model SelectedModel) error {
	s.ApplyPreferredModel(modelType, model)
	return s.PersistPreferredModel(scope, modelType, model)
}

// ApplyPreferredModel updates the in-memory preferred model and recent models.
func (s *ConfigStore) ApplyPreferredModel(modelType SelectedModelType, model SelectedModel) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	// Copy-on-write the Models map so concurrent readers of the previous
	// map are safe.
	newModels := make(map[SelectedModelType]SelectedModel, len(s.config.Models)+1)
	for k, v := range s.config.Models {
		newModels[k] = v
	}
	newModels[modelType] = model
	s.config.Models = newModels

	recent, changed, ok := updatedRecentModels(s.config.RecentModels[modelType], model)
	if !ok || !changed {
		return
	}
	// Copy-on-write the RecentModels map.
	newRecent := make(map[SelectedModelType][]SelectedModel, len(s.config.RecentModels)+1)
	for k, v := range s.config.RecentModels {
		newRecent[k] = v
	}
	newRecent[modelType] = recent
	s.config.RecentModels = newRecent
}

// PersistPreferredModel persists the preferred model and current recent list
// without mutating in-memory config.
func (s *ConfigStore) PersistPreferredModel(scope Scope, modelType SelectedModelType, model SelectedModel) error {
	if err := s.SetConfigField(scope, fmt.Sprintf("models.%s", modelType), model); err != nil {
		return fmt.Errorf("failed to update preferred model: %w", err)
	}
	s.configMu.RLock()
	recent := s.config.RecentModels[modelType]
	s.configMu.RUnlock()
	if recent == nil {
		return nil
	}
	if err := s.SetConfigField(scope, fmt.Sprintf("recent_models.%s", modelType), recent); err != nil {
		return fmt.Errorf("failed to persist recent models: %w", err)
	}
	return nil
}

// SetCompactMode sets the compact mode setting and persists it.
func (s *ConfigStore) SetCompactMode(scope Scope, enabled bool) error {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if s.config.Options == nil {
		s.config.Options = &Options{}
	}
	if s.config.Options.TUI == nil {
		s.config.Options.TUI = &TUIOptions{}
	}
	s.config.Options.TUI.CompactMode = enabled
	return s.SetConfigField(scope, "options.tui.compact_mode", enabled)
}

// SetSkipRequests sets the skip requests (YOLO mode) setting and persists it.
func (s *ConfigStore) SetSkipRequests(scope Scope, enabled bool) error {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if s.config.Permissions == nil {
		s.config.Permissions = &Permissions{}
	}
	s.config.Permissions.SkipRequests = enabled
	return s.SetConfigField(scope, "permissions.skip_requests", enabled)
}

// SetPreferredPermissionMode sets the preferred interactive permission mode
// and persists it.
func (s *ConfigStore) SetPreferredPermissionMode(scope Scope, mode string) error {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if s.config.Options == nil {
		s.config.Options = &Options{}
	}
	s.config.Options.PreferredPermissionMode = mode
	return s.SetConfigField(scope, "options.preferred_permission_mode", mode)
}

// SetPreferredCollaborationMode keeps backward compatibility by mapping the
// deprecated setting to preferred_permission_mode.
func (s *ConfigStore) SetPreferredCollaborationMode(scope Scope, mode string) error {
	return s.SetPreferredPermissionMode(scope, mode)
}

// MCPStartupGracePeriod returns the effective startup grace period for MCP
// servers. When at least one non-disabled server explicitly configures
// startup_grace_period_ms, the maximum configured value is used. When no
// server configures it, the default of 2 seconds is returned. This value
// controls how long the application waits for MCP servers to connect before
// unblocking the main flow; servers still connecting after the grace period
// continue in the background.
func (s *ConfigStore) MCPStartupGracePeriod() time.Duration {
	const defaultGrace = 2 * time.Second
	if s == nil || s.config == nil {
		return defaultGrace
	}
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	var max time.Duration
	found := false
	for _, m := range s.config.MCP {
		if m.Disabled {
			continue
		}
		if m.StartupGracePeriodMs <= 0 {
			continue
		}
		found = true
		d := time.Duration(m.StartupGracePeriodMs) * time.Millisecond
		if d > max {
			max = d
		}
	}
	if !found {
		return defaultGrace
	}
	return max
}

// MCPSnapshot returns detached MCP configurations for discovery and lifecycle
// projection. Callers must still redact credentials before external exposure.
func (s *ConfigStore) MCPSnapshot() map[string]MCPConfig {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	result := make(map[string]MCPConfig, len(s.config.MCP))
	for name, value := range s.config.MCP {
		result[name] = CloneMCPConfig(value)
	}
	return result
}

// CloneMCPConfig returns a detached copy suitable for asynchronous lifecycle
// work. MCP OAuth metadata contains nested mutable pointers and slices, so a
// shallow struct copy is not safe while token refresh is active.
func CloneMCPConfig(value MCPConfig) MCPConfig {
	value.Args = slices.Clone(value.Args)
	value.Env = maps.Clone(value.Env)
	value.Headers = maps.Clone(value.Headers)
	value.DisabledTools = slices.Clone(value.DisabledTools)
	value.EnabledTools = slices.Clone(value.EnabledTools)
	value.OAuth = cloneMCPOAuthConfig(value.OAuth)
	return value
}

// SetMCPDisabled sets the disabled state for an MCP server and persists it.
func (s *ConfigStore) SetMCPDisabled(scope Scope, name string, disabled bool) error {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	mcpConfig, ok := s.config.MCP[name]
	if !ok {
		return fmt.Errorf("mcp %s not found", name)
	}
	mcpConfig.Disabled = disabled
	s.config.MCP = copyMCPsWith(s.config.MCP, name, mcpConfig)
	return s.SetConfigField(scope, fmt.Sprintf("mcp.%s.disabled", name), disabled)
}

// RemoveMCP removes an MCP server from the in-memory config. It does not
// persist the change; callers that need persistence should use SetConfigField
// or RemoveConfigField with the appropriate scope.
func (s *ConfigStore) RemoveMCP(name string) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if _, ok := s.config.MCP[name]; !ok {
		return
	}
	newMCP := make(MCPs, len(s.config.MCP))
	for k, v := range s.config.MCP {
		if k != name {
			newMCP[k] = v
		}
	}
	s.config.MCP = newMCP
}

// AddMCP adds an ephemeral MCP transport configuration without persisting it.
// Visibility and authorization are owned by the caller. If a server with the
// same name already exists, it is replaced.
func (s *ConfigStore) AddMCP(name string, cfg MCPConfig) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	s.config.MCP = copyMCPsWith(s.config.MCP, name, cfg)
}

// copyMCPsWith returns a copy of mcp with the entry at name replaced by cfg.
// The original map is not mutated, so concurrent readers of the old map are
// safe.
func copyMCPsWith(mcp MCPs, name string, cfg MCPConfig) MCPs {
	newMCP := make(MCPs, len(mcp))
	for k, v := range mcp {
		newMCP[k] = v
	}
	newMCP[name] = cfg
	return newMCP
}

// SetTransparentBackground sets the transparent background setting and persists it.
func (s *ConfigStore) SetTransparentBackground(scope Scope, enabled bool) error {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if s.config.Options == nil {
		s.config.Options = &Options{}
	}
	s.config.Options.TUI.Transparent = &enabled
	return s.SetConfigField(scope, "options.tui.transparent", enabled)
}

// SetProviderAPIKey sets the API key for a provider and persists it.
func (s *ConfigStore) SetProviderAPIKey(scope Scope, providerID string, apiKey any) error {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	providerConfig, exists := s.config.Providers.Get(providerID)
	if !exists {
		var foundProvider *catwalk.Provider
		for i := range s.knownProviders {
			if string(s.knownProviders[i].ID) == providerID {
				foundProvider = &s.knownProviders[i]
				break
			}
		}
		if foundProvider == nil {
			return fmt.Errorf("provider with ID %s not found in known providers", providerID)
		}
		providerConfig = ProviderConfig{
			ID:           providerID,
			Name:         foundProvider.Name,
			BaseURL:      foundProvider.APIEndpoint,
			Type:         foundProvider.Type,
			Disable:      false,
			ExtraHeaders: make(map[string]string),
			ExtraParams:  make(map[string]string),
			Models:       ProviderModelsFromCatwalk(foundProvider.Models),
		}
	}

	switch value := apiKey.(type) {
	case string:
		if value == "" {
			return errors.New("provider api key is empty")
		}
		if err := s.persistProviderAPIKey(scope, providerID, value); err != nil {
			return fmt.Errorf("failed to save api key to config file: %w", err)
		}
		providerConfig.APIKey = value
		providerConfig.APIKeyTemplate = ""
		providerConfig.OAuthToken = nil
	case *oauth.Token:
		if value == nil || value.AccessToken == "" {
			return errors.New("provider oauth token is empty")
		}
		if err := s.persistProviderOAuthCredential(scope, providerID, value); err != nil {
			return fmt.Errorf("failed to save provider oauth credential: %w", err)
		}
		token := *value
		providerConfig.APIKey = token.AccessToken
		providerConfig.APIKeyTemplate = ""
		providerConfig.OAuthToken = &token
		if providerID == string(catwalk.InferenceProviderCopilot) {
			providerConfig.SetupGitHubCopilot()
		}
	default:
		return fmt.Errorf("unsupported provider credential type %T", apiKey)
	}
	s.config.Providers.Set(providerID, providerConfig)
	return nil
}

// ClearProviderCredentials atomically removes a provider's persisted API key
// and OAuth token before clearing the corresponding in-memory credentials.
func (s *ConfigStore) ClearProviderCredentials(scope Scope, providerID string) error {
	s.configMu.Lock()
	defer s.configMu.Unlock()

	path := s.configPath(scope)
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed to read config file: %w", err)
		}
		data = []byte("{}")
	}
	updated, err := sjson.DeleteBytes(data, fmt.Sprintf("providers.%s.api_key", providerID))
	if err != nil {
		return fmt.Errorf("failed to delete provider api key: %w", err)
	}
	updated, err = sjson.DeleteBytes(updated, fmt.Sprintf("providers.%s.oauth", providerID))
	if err != nil {
		return fmt.Errorf("failed to delete provider oauth token: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("failed to create config directory %q: %w", path, err)
	}
	if err := os.WriteFile(path, updated, 0o600); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	if s.config == nil || s.config.Providers == nil {
		return nil
	}
	provider, ok := s.config.Providers.Get(providerID)
	if !ok {
		return nil
	}
	provider.APIKey = ""
	provider.APIKeyTemplate = ""
	provider.OAuthToken = nil
	s.config.Providers.Set(providerID, provider)
	return nil
}

func cloneCatwalkProviders(in []catwalk.Provider) []catwalk.Provider {
	out := make([]catwalk.Provider, len(in))
	for i, provider := range in {
		out[i] = provider
		out[i].Models = slices.Clone(provider.Models)
		for j := range out[i].Models {
			out[i].Models[j].ReasoningLevels = slices.Clone(provider.Models[j].ReasoningLevels)
		}
		out[i].DefaultHeaders = maps.Clone(provider.DefaultHeaders)
	}
	return out
}

func cloneProviderConfig(provider ProviderConfig) ProviderConfig {
	provider.Models = slices.Clone(provider.Models)
	for i := range provider.Models {
		provider.Models[i].ReasoningLevels = slices.Clone(provider.Models[i].ReasoningLevels)
	}
	provider.ExtraHeaders = maps.Clone(provider.ExtraHeaders)
	provider.ExtraBody = maps.Clone(provider.ExtraBody)
	provider.ProviderOptions = maps.Clone(provider.ProviderOptions)
	provider.ExtraParams = maps.Clone(provider.ExtraParams)
	if provider.OAuthToken != nil {
		token := *provider.OAuthToken
		provider.OAuthToken = &token
	}
	return provider
}

func (s *ConfigStore) persistProviderAPIKey(scope Scope, providerID string, apiKey string) error {
	path := s.configPath(scope)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			data = []byte("{}")
		} else {
			return fmt.Errorf("failed to read config file: %w", err)
		}
	}
	updated, err := sjson.SetBytes(data, fmt.Sprintf("providers.%s.api_key", providerID), encodedProviderAPIKey(apiKey))
	if err != nil {
		return fmt.Errorf("failed to set provider api key: %w", err)
	}
	updated, err = sjson.DeleteBytes(updated, fmt.Sprintf("providers.%s.oauth", providerID))
	if err != nil {
		return fmt.Errorf("failed to delete provider oauth token: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("failed to create config directory %q: %w", path, err)
	}
	if err := os.WriteFile(path, updated, 0o600); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}
	return nil
}

func (s *ConfigStore) persistProviderOAuthCredential(scope Scope, providerID string, token *oauth.Token) error {
	path := s.configPath(scope)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			data = []byte("{}")
		} else {
			return fmt.Errorf("failed to read config file: %w", err)
		}
	}
	updated, err := sjson.SetBytes(data, fmt.Sprintf("providers.%s.api_key", providerID), encodedProviderAPIKey(token.AccessToken))
	if err != nil {
		return fmt.Errorf("failed to set provider api key: %w", err)
	}
	updated, err = sjson.SetBytes(updated, fmt.Sprintf("providers.%s.oauth", providerID), token)
	if err != nil {
		return fmt.Errorf("failed to set provider oauth token: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("failed to create config directory %q: %w", path, err)
	}
	if err := os.WriteFile(path, updated, 0o600); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}
	return nil
}

func encodedProviderAPIKey(apiKey string) string {
	if strings.HasPrefix(apiKey, "$") {
		return apiKey
	}
	encrypted, err := EncryptAPIKey(apiKey)
	if err == nil {
		return encrypted
	}
	slog.Warn("Failed to encrypt API key, storing in plaintext", "error", err)
	return apiKey
}

func (s *ConfigStore) SetMCPOAuthConfig(scope Scope, mcpName string, oauthCfg *MCPOAuthConfig) error {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	return s.setMCPOAuthConfigLocked(scope, mcpName, oauthCfg)
}

// setMCPOAuthConfigLocked updates the OAuth config for an MCP server. The
// caller must hold configMu. The MCP map is replaced via copy-on-write so
// concurrent readers of the previous map are safe.
func (s *ConfigStore) setMCPOAuthConfigLocked(scope Scope, mcpName string, oauthCfg *MCPOAuthConfig) error {
	mcpConfig, ok := s.config.MCP[mcpName]
	if !ok {
		return fmt.Errorf("mcp %s not found", mcpName)
	}

	if oauthCfg == nil {
		mcpConfig.OAuth = nil
		s.config.MCP = copyMCPsWith(s.config.MCP, mcpName, mcpConfig)
		if !s.HasConfigField(scope, fmt.Sprintf("mcp.%s.oauth", mcpName)) {
			return nil
		}
		return s.RemoveConfigField(scope, fmt.Sprintf("mcp.%s.oauth", mcpName))
	}

	mcpConfig.OAuth = cloneMCPOAuthConfig(oauthCfg)
	s.config.MCP = copyMCPsWith(s.config.MCP, mcpName, mcpConfig)
	if err := s.SetConfigField(scope, fmt.Sprintf("mcp.%s.oauth", mcpName), mcpConfig.OAuth); err != nil {
		return fmt.Errorf("failed to save mcp oauth config: %w", err)
	}
	return nil
}

func (s *ConfigStore) SetMCPOAuthToken(scope Scope, mcpName string, token *oauth.Token) error {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	oauthCfg, err := s.cloneCurrentMCPOAuthConfigLocked(mcpName)
	if err != nil {
		return err
	}
	if token == nil && oauthCfg == nil {
		return nil
	}
	if oauthCfg == nil {
		oauthCfg = &MCPOAuthConfig{}
	}
	oauthCfg.Token = token
	return s.setMCPOAuthConfigLocked(scope, mcpName, oauthCfg)
}

// cloneCurrentMCPOAuthConfigLocked returns a deep copy of the current OAuth
// config for the given MCP server. The caller must hold configMu.
func (s *ConfigStore) cloneCurrentMCPOAuthConfigLocked(mcpName string) (*MCPOAuthConfig, error) {
	mcpConfig, ok := s.config.MCP[mcpName]
	if !ok {
		return nil, fmt.Errorf("mcp %s not found", mcpName)
	}
	if mcpConfig.OAuth == nil {
		return nil, nil
	}
	return cloneMCPOAuthConfig(mcpConfig.OAuth), nil
}

func cloneMCPOAuthConfig(in *MCPOAuthConfig) *MCPOAuthConfig {
	if in == nil {
		return nil
	}
	out := *in
	if in.Token != nil {
		token := *in.Token
		out.Token = &token
	}
	if in.Registration != nil {
		registration := *in.Registration
		out.Registration = &registration
	}
	if in.AuthServer != nil {
		authServer := *in.AuthServer
		authServer.TokenEndpointAuthMethodsSupported = slices.Clone(in.AuthServer.TokenEndpointAuthMethodsSupported)
		out.AuthServer = &authServer
	}
	if in.Scopes != nil {
		out.Scopes = slices.Clone(in.Scopes)
	}
	return &out
}

// RefreshOAuthToken refreshes the OAuth token for the given provider.
func (s *ConfigStore) RefreshOAuthToken(ctx context.Context, scope Scope, providerID string) error {
	s.configMu.RLock()
	providerConfig, exists := s.config.Providers.Get(providerID)
	s.configMu.RUnlock()
	if !exists {
		return fmt.Errorf("provider %s not found", providerID)
	}

	if providerConfig.OAuthToken == nil {
		return fmt.Errorf("provider %s does not have an OAuth token", providerID)
	}
	originalToken := *providerConfig.OAuthToken

	var newToken *oauth.Token
	var refreshErr error
	switch providerID {
	case string(catwalk.InferenceProviderCopilot):
		newToken, refreshErr = copilot.RefreshToken(ctx, providerConfig.OAuthToken.RefreshToken)
	case hyperp.Name:
		newToken, refreshErr = hyper.ExchangeToken(ctx, providerConfig.OAuthToken.RefreshToken)
	default:
		return fmt.Errorf("OAuth refresh not supported for provider %s", providerID)
	}
	if refreshErr != nil {
		return fmt.Errorf("failed to refresh OAuth token for provider %s: %w", providerID, refreshErr)
	}
	if newToken == nil || newToken.AccessToken == "" {
		return fmt.Errorf("provider %s returned an empty OAuth token", providerID)
	}

	s.configMu.Lock()
	defer s.configMu.Unlock()
	current, exists := s.config.Providers.Get(providerID)
	if !exists || current.OAuthToken == nil || *current.OAuthToken != originalToken {
		return fmt.Errorf("provider %s credentials changed during OAuth refresh", providerID)
	}
	if err := s.persistProviderOAuthCredential(scope, providerID, newToken); err != nil {
		return fmt.Errorf("failed to persist refreshed token: %w", err)
	}
	token := *newToken
	current.OAuthToken = &token
	current.APIKey = token.AccessToken
	current.APIKeyTemplate = ""
	if providerID == string(catwalk.InferenceProviderCopilot) {
		current.SetupGitHubCopilot()
	}
	s.config.Providers.Set(providerID, current)
	slog.Info("Successfully refreshed OAuth token", "provider", providerID)

	return nil
}

// recordRecentModel records a model in the recent models list.
func (s *ConfigStore) recordRecentModel(scope Scope, modelType SelectedModelType, model SelectedModel) error {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	updated, changed, ok := updatedRecentModels(s.config.RecentModels[modelType], model)
	if !ok || !changed {
		return nil
	}

	// Copy-on-write the RecentModels map so concurrent readers of the
	// previous map are safe.
	newRecent := make(map[SelectedModelType][]SelectedModel, len(s.config.RecentModels)+1)
	for k, v := range s.config.RecentModels {
		newRecent[k] = v
	}
	newRecent[modelType] = updated
	s.config.RecentModels = newRecent

	if err := s.SetConfigField(scope, fmt.Sprintf("recent_models.%s", modelType), updated); err != nil {
		return fmt.Errorf("failed to persist recent models: %w", err)
	}

	return nil
}

func updatedRecentModels(current []SelectedModel, model SelectedModel) ([]SelectedModel, bool, bool) {
	if model.Provider == "" || model.Model == "" {
		return nil, false, false
	}

	eq := func(a, b SelectedModel) bool {
		return a.Provider == b.Provider && a.Model == b.Model
	}

	entry := SelectedModel{
		Provider: model.Provider,
		Model:    model.Model,
	}

	withoutCurrent := slices.DeleteFunc(slices.Clone(current), func(existing SelectedModel) bool {
		return eq(existing, entry)
	})

	updated := append([]SelectedModel{entry}, withoutCurrent...)
	if len(updated) > maxRecentModelsPerType {
		updated = updated[:maxRecentModelsPerType]
	}

	if slices.EqualFunc(current, updated, eq) {
		return current, false, true
	}
	return updated, true, true
}

// ImportCopilot attempts to import a GitHub Copilot token from disk.
func (s *ConfigStore) ImportCopilot() (*oauth.Token, bool) {
	if s.HasConfigField(ScopeGlobal, "providers.copilot.api_key") || s.HasConfigField(ScopeGlobal, "providers.copilot.oauth") {
		return nil, false
	}

	diskToken, hasDiskToken := copilot.RefreshTokenFromDisk()
	if !hasDiskToken {
		return nil, false
	}

	slog.Info("Found existing GitHub Copilot token on disk. Authenticating...")
	token, err := copilot.RefreshToken(context.TODO(), diskToken)
	if err != nil {
		slog.Error("Unable to import GitHub Copilot token", "error", err)
		return nil, false
	}

	if err := s.SetProviderAPIKey(ScopeGlobal, string(catwalk.InferenceProviderCopilot), token); err != nil {
		return token, false
	}

	slog.Info("GitHub Copilot successfully imported")
	return token, true
}
