package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/oauth"
	"github.com/stretchr/testify/require"
)

func TestEncryptDecryptAPIKey(t *testing.T) {
	t.Parallel()

	plaintext := "sk-test-abc123"
	encrypted, err := EncryptAPIKey(plaintext)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(encrypted, encryptedKeyPrefix))
	require.NotEqual(t, plaintext, encrypted)

	decrypted, err := DecryptAPIKey(encrypted)
	require.NoError(t, err)
	require.Equal(t, plaintext, decrypted)
}

func TestEncryptAPIKeyLeavesEnvVarRefsUntouched(t *testing.T) {
	t.Parallel()

	ref := "$OPENAI_API_KEY"
	out, err := EncryptAPIKey(ref)
	require.NoError(t, err)
	require.Equal(t, ref, out)
}

func TestDecryptAPIKeyLeavesPlaintextUntouched(t *testing.T) {
	t.Parallel()

	plain := "already-plain"
	out, err := DecryptAPIKey(plain)
	require.NoError(t, err)
	require.Equal(t, plain, out)
}

func TestSetProviderAPIKeyEncryptsOAuthAccessToken(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "crush.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"providers":{}}`), 0o600))

	store := &ConfigStore{
		config: &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"copilot": {ExtraHeaders: map[string]string{}},
			}),
		},
		globalDataPath: path,
	}
	token := &oauth.Token{AccessToken: "oauth-access-token", RefreshToken: "oauth-refresh-token"}

	require.NoError(t, store.SetProviderAPIKey(ScopeGlobal, "copilot", token))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(data, &raw))
	providers, _ := raw["providers"].(map[string]any)
	provider, _ := providers["copilot"].(map[string]any)
	storedKey, _ := provider["api_key"].(string)
	require.True(t, strings.HasPrefix(storedKey, encryptedKeyPrefix), "expected enc: prefix, got %q", storedKey)
	decrypted, err := DecryptAPIKey(storedKey)
	require.NoError(t, err)
	require.Equal(t, "oauth-access-token", decrypted)
	providerConfig, ok := store.config.Providers.Get("copilot")
	require.True(t, ok)
	require.Equal(t, token.AccessToken, providerConfig.APIKey)
}

func TestMigrateAPIKeyEncryptionInFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "crush.json")

	alreadyEncrypted, err := EncryptAPIKey("sk-existing-encrypted")
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(path, []byte(fmt.Sprintf(`{
		"providers": {
			"openai":    {"api_key": "sk-test-plaintext"},
			"anthropic": {"api_key": "$ANTHROPIC_KEY"},
			"custom":    {"api_key": %q}
		}
	}`, alreadyEncrypted)), 0o600))

	migrateAPIKeyEncryptionInFile(path)

	readKey := func(providerID string) string {
		t.Helper()
		data, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		var raw map[string]any
		require.NoError(t, json.Unmarshal(data, &raw))
		providers, _ := raw["providers"].(map[string]any)
		provider, _ := providers[providerID].(map[string]any)
		key, _ := provider["api_key"].(string)
		return key
	}

	// Plaintext key must be encrypted.
	got := readKey("openai")
	require.True(t, strings.HasPrefix(got, encryptedKeyPrefix), "expected enc: prefix, got %q", got)
	decrypted, err := DecryptAPIKey(got)
	require.NoError(t, err)
	require.Equal(t, "sk-test-plaintext", decrypted)

	// Env-var reference must be left unchanged.
	require.Equal(t, "$ANTHROPIC_KEY", readKey("anthropic"))

	// Already-encrypted value must be left unchanged.
	require.Equal(t, alreadyEncrypted, readKey("custom"))
}

func TestMigrateAPIKeyEncryptionCoversAllPaths(t *testing.T) {
	// Cannot use t.Parallel() with t.Setenv.
	dir := t.TempDir()
	globalCfgPath := filepath.Join(dir, "global_cfg.json")   // GlobalConfig() equivalent
	globalDataPath := filepath.Join(dir, "global_data.json") // GlobalConfigData() equivalent
	workspacePath := filepath.Join(dir, "workspace.json")

	write := func(path, content string) {
		t.Helper()
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	readKey := func(path, providerID string) string {
		t.Helper()
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		var raw map[string]any
		require.NoError(t, json.Unmarshal(data, &raw))
		providers, _ := raw["providers"].(map[string]any)
		provider, _ := providers[providerID].(map[string]any)
		key, _ := provider["api_key"].(string)
		return key
	}

	write(globalCfgPath, `{"providers":{"openai":{"api_key":"sk-cfg-plain"}}}`)
	write(globalDataPath, `{"providers":{"anthropic":{"api_key":"sk-data-plain"}}}`)
	write(workspacePath, `{"providers":{"custom":{"api_key":"sk-ws-plain"}}}`)

	// Temporarily override GlobalConfig() via environment variable.
	t.Setenv("CRUSH_GLOBAL_CONFIG", dir)
	// Rename so the filenames match what GlobalConfig() expects.
	require.NoError(t, os.Rename(globalCfgPath, filepath.Join(dir, "crush.json")))
	globalCfgPath = filepath.Join(dir, "crush.json")

	store := &ConfigStore{
		globalDataPath: globalDataPath,
		workspacePath:  workspacePath,
	}
	migrateAPIKeyEncryption(store)

	for _, tc := range []struct {
		path       string
		providerID string
		plaintext  string
	}{
		{globalCfgPath, "openai", "sk-cfg-plain"},
		{globalDataPath, "anthropic", "sk-data-plain"},
		{workspacePath, "custom", "sk-ws-plain"},
	} {
		got := readKey(tc.path, tc.providerID)
		require.True(t, strings.HasPrefix(got, encryptedKeyPrefix),
			"file=%s provider=%s: expected enc: prefix, got %q", tc.path, tc.providerID, got)
		decrypted, err := DecryptAPIKey(got)
		require.NoError(t, err)
		require.Equal(t, tc.plaintext, decrypted)
	}
}
