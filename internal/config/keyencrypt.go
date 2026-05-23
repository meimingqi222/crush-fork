package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/denisbrodbeck/machineid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/crypto/pbkdf2"
)

const encryptedKeyPrefix = "enc:"

var (
	encryptSalt         = []byte("crush-key-encrypt-v1")
	encryptSaltFallback = []byte("crush-key-encrypt-v1-fallback")
)

// deriveEncryptionKey derives a 32-byte AES-256 key using PBKDF2 with the
// machine ID as the password. Falls back to a persistent random key file if machineid fails.
func deriveEncryptionKey() ([]byte, error) {
	password, err := machineid.ID()
	salt := encryptSalt
	if err != nil || password == "" {
		configDir := filepath.Dir(GlobalConfig())
		keyFilePath := filepath.Join(configDir, ".key")
		var keyData []byte
		keyData, err = os.ReadFile(keyFilePath)
		if err != nil {
			_ = os.MkdirAll(configDir, 0o700)
			randomBytes := make([]byte, 32)
			if _, randErr := rand.Read(randomBytes); randErr == nil {
				keyData = []byte(base64.URLEncoding.EncodeToString(randomBytes))
				_ = os.WriteFile(keyFilePath, keyData, 0o600)
			}
		}
		if len(keyData) > 0 {
			password = string(keyData)
		} else {
			slog.Warn("Encryption key fallback to hostname+USER is insecure; machine ID and random key file both unavailable")
			hostname, _ := os.Hostname()
			password = hostname + os.Getenv("USER")
		}
		salt = encryptSaltFallback
	}
	key := pbkdf2.Key([]byte(password), salt, 100000, 32, sha256.New)
	return key, nil
}

// EncryptAPIKey encrypts a plaintext API key using AES-256-GCM and returns an
// "enc:"-prefixed base64-encoded ciphertext. Env-var references (starting with
// "$") are returned unchanged.
func EncryptAPIKey(plaintext string) (string, error) {
	if strings.HasPrefix(plaintext, "$") {
		return plaintext, nil
	}
	key, err := deriveEncryptionKey()
	if err != nil {
		return "", fmt.Errorf("failed to derive encryption key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("failed to create AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("failed to create GCM: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("failed to generate nonce: %w", err)
	}
	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	encoded := base64.URLEncoding.EncodeToString(ciphertext)
	return encryptedKeyPrefix + encoded, nil
}

// DecryptAPIKey decrypts an "enc:"-prefixed value. Values not carrying the
// prefix are returned unchanged so that legacy plaintext or env-var references
// continue to work.
func DecryptAPIKey(value string) (string, error) {
	if !strings.HasPrefix(value, encryptedKeyPrefix) {
		return value, nil
	}
	encoded := strings.TrimPrefix(value, encryptedKeyPrefix)
	data, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("failed to decode encrypted key: %w", err)
	}
	key, err := deriveEncryptionKey()
	if err != nil {
		return "", fmt.Errorf("failed to derive encryption key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("failed to create AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("failed to create GCM: %w", err)
	}
	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return "", fmt.Errorf("encrypted key data too short")
	}
	nonce, ciphertext := data[:nonceSize], data[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt API key: %w", err)
	}
	return string(plaintext), nil
}

// DecryptAPIKeyIfNeeded decrypts an "enc:"-prefixed value, returning the
// original value on any error (logging a warning).
func DecryptAPIKeyIfNeeded(value string) string {
	result, err := DecryptAPIKey(value)
	if err != nil {
		slog.Warn("Failed to decrypt API key, using original value", "error", err)
		return value
	}
	return result
}

// migrateAPIKeyEncryption re-writes any plaintext api_key values in every
// config file that crush reads. The set of paths to scan is:
//   - GlobalConfig()     — ~/.config/crush/crush.json  (user-edited file)
//   - store.globalDataPath — GlobalConfigData()         (writable data dir)
//   - store.workspacePath  — <workspace>/.crush/crush.json
//
// Values already encrypted ("enc:" prefix) or env-var references ("$") are
// left unchanged. Errors are logged and never block startup.
func migrateAPIKeyEncryption(store *ConfigStore) {
	seen := make(map[string]bool)
	for _, path := range []string{
		GlobalConfig(),
		store.globalDataPath,
		store.workspacePath,
	} {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		migrateAPIKeyEncryptionInFile(path)
	}
}

func migrateAPIKeyEncryptionInFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}

	providers := gjson.GetBytes(data, "providers")
	if !providers.Exists() {
		return
	}

	current := string(data)
	dirty := false
	providers.ForEach(func(providerID, providerVal gjson.Result) bool {
		apiKey := providerVal.Get("api_key").String()
		if apiKey == "" || strings.HasPrefix(apiKey, "$") || strings.HasPrefix(apiKey, encryptedKeyPrefix) {
			return true
		}
		encrypted, encErr := EncryptAPIKey(apiKey)
		if encErr != nil {
			slog.Warn("Failed to encrypt API key during migration, leaving as plaintext",
				"provider", providerID.String(), "error", encErr)
			return true
		}
		updated, setErr := sjson.Set(current, fmt.Sprintf("providers.%s.api_key", providerID.String()), encrypted)
		if setErr != nil {
			slog.Warn("Failed to update API key in config during migration",
				"provider", providerID.String(), "error", setErr)
			return true
		}
		current = updated
		dirty = true
		slog.Info("Migrated plaintext API key to encrypted storage.",
			"provider", providerID.String(), "file", path)
		return true
	})

	if !dirty {
		return
	}
	if err := os.WriteFile(path, []byte(current), 0o600); err != nil {
		slog.Warn("Failed to write migrated config file", "path", path, "error", err)
	}
}
