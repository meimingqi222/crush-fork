package mcp

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	crushlog "github.com/charmbracelet/crush/internal/log"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestEnsureBase64(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    []byte
		wantData []byte
	}{
		{
			name:     "already base64 encoded",
			input:    []byte("SGVsbG8gV29ybGQh"),
			wantData: []byte("SGVsbG8gV29ybGQh"),
		},
		{
			name:     "raw binary data",
			input:    []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A},
			wantData: []byte(base64.StdEncoding.EncodeToString([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A})),
		},
		{
			name:     "raw binary with high bytes",
			input:    []byte{0xFF, 0xD8, 0xFF, 0xE0},
			wantData: []byte(base64.StdEncoding.EncodeToString([]byte{0xFF, 0xD8, 0xFF, 0xE0})),
		},
		{
			name:     "empty data",
			input:    []byte{},
			wantData: []byte{},
		},
		{
			name:     "base64 with padding",
			input:    []byte("YQ=="),
			wantData: []byte("YQ=="),
		},
		{
			name:     "base64 without padding (short, treated as raw)",
			input:    []byte("YQ"),
			wantData: []byte(base64.StdEncoding.EncodeToString([]byte("YQ"))), // "YQ" is too short, encoded as raw
		},
		{
			name:     "base64 with whitespace",
			input:    []byte("U0dWc2JHOGdWMjl5YkdRaA==\n"),
			wantData: []byte("U0dWc2JHOGdWMjl5YkdRaA=="),
		},
		{
			// RawStdEncoding fallback requires len >= 8 and len % 4 == 0
			name:     "base64 without padding (8 chars, valid alignment)",
			input:    []byte("SGVsbG8h"), // "Hello!" in base64 without padding
			wantData: []byte("SGVsbG8h"),
		},
		{
			// "ABCD" is valid StdEncoding base64 (4 chars = 3 bytes decoded)
			name:     "4-char valid base64 (StdEncoding)",
			input:    []byte("ABCD"),
			wantData: []byte("ABCD"), // Already valid base64, returned as-is after normalization
		},
		{
			// 6 chars but not aligned to 4, RawStdEncoding fallback won't trigger
			name:     "6-char ASCII treated as raw (not 4-aligned for raw fallback)",
			input:    []byte("ABCDEF"), // 6 chars, not multiple of 4 for raw fallback
			wantData: []byte(base64.StdEncoding.EncodeToString([]byte("ABCDEF"))),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := ensureBase64(tt.input)
			require.Equal(t, tt.wantData, result)

			if len(result) > 0 {
				_, err := base64.StdEncoding.DecodeString(string(result))
				if err != nil {
					_, err = base64.RawStdEncoding.DecodeString(string(result))
				}
				require.NoError(t, err)
			}
		})
	}
}

func TestIsValidBase64(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input []byte
		want  bool
	}{
		{
			name:  "valid base64",
			input: []byte("SGVsbG8gV29ybGQh"),
			want:  true,
		},
		{
			name:  "valid base64 with padding",
			input: []byte("YQ=="),
			want:  true,
		},
		{
			name:  "raw binary with high bytes",
			input: []byte{0xFF, 0xD8, 0xFF},
			want:  false,
		},
		{
			name:  "empty",
			input: []byte{},
			want:  true,
		},
		{
			name:  "valid raw base64 without padding",
			input: []byte("YQ"),
			want:  true,
		},
		{
			name:  "valid base64 with whitespace",
			input: normalizeBase64Input([]byte("U0dWc2JHOGdWMjl5YkdRaA==\n")),
			want:  true,
		},
		{
			name:  "invalid base64 characters",
			input: []byte("SGVsbG8!@#$"),
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, isValidBase64(tt.input))
		})
	}
}

func TestResultFromMCPContent(t *testing.T) {
	t.Parallel()

	t.Run("text only", func(t *testing.T) {
		t.Parallel()

		result := resultFromMCPContent([]sdkmcp.Content{
			&sdkmcp.TextContent{Text: "first"},
			&sdkmcp.TextContent{Text: "second"},
		})

		require.Equal(t, "text", result.Type)
		require.Equal(t, "first\nsecond", result.Content)
		require.Empty(t, result.Data)
		require.Empty(t, result.MediaType)
		require.Empty(t, result.AdditionalMedia)
	})

	t.Run("multiple image/audio payloads", func(t *testing.T) {
		t.Parallel()

		result := resultFromMCPContent([]sdkmcp.Content{
			&sdkmcp.TextContent{Text: "captured"},
			&sdkmcp.ImageContent{MIMEType: "image/png", Data: []byte("AQID")},
			&sdkmcp.ImageContent{MIMEType: "image/jpeg", Data: []byte{0xFF, 0xD8, 0xFF}},
			&sdkmcp.AudioContent{MIMEType: "audio/wav", Data: []byte("BAUG")},
		})

		require.Equal(t, "image", result.Type)
		require.Equal(t, "captured", result.Content)
		require.Equal(t, "image/png", result.MediaType)
		require.Equal(t, []byte("AQID"), result.Data)
		require.Len(t, result.AdditionalMedia, 2)
		require.Equal(t, ToolMedia{Type: "image", MediaType: "image/jpeg", Data: []byte(base64.StdEncoding.EncodeToString([]byte{0xFF, 0xD8, 0xFF}))}, result.AdditionalMedia[0])
		require.Equal(t, ToolMedia{Type: "media", MediaType: "audio/wav", Data: []byte("BAUG")}, result.AdditionalMedia[1])
	})
}

func TestCallToolWithRetryReconnectsOnce(t *testing.T) {
	store := loadTestStore(t)
	const name = "retry-mcp"

	originalCallToolOnSession := callToolOnSession
	originalReconnectClient := reconnectClient
	t.Cleanup(func() {
		callToolOnSession = originalCallToolOnSession
		reconnectClient = originalReconnectClient
		states.Del(name)
		sessions.Del(name)
		allTools.Del(name)
	})

	updateState(name, StateConnected, nil, &ClientSession{}, Counts{Tools: 1})

	callCount := 0
	callToolOnSession = func(ctx context.Context, session *ClientSession, params *sdkmcp.CallToolParams) (*sdkmcp.CallToolResult, error) {
		callCount++
		require.Equal(t, "test-tool", params.Name)
		if callCount == 1 {
			return nil, fmt.Errorf("transport failed: %w", sdkmcp.ErrConnectionClosed)
		}
		return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "ok"}}}, nil
	}

	reconnectCount := 0
	reconnectClient = func(ctx context.Context, cfg *config.ConfigStore, gotName string) error {
		reconnectCount++
		require.Same(t, store, cfg)
		require.Equal(t, name, gotName)
		session := &ClientSession{}
		sessions.Set(name, session)
		updateState(name, StateConnected, nil, session, Counts{Tools: 1})
		return nil
	}

	result, err := callToolWithRetry(context.Background(), store, name, &ClientSession{}, &sdkmcp.CallToolParams{Name: "test-tool"})
	require.NoError(t, err)
	require.Equal(t, "ok", result.Content[0].(*sdkmcp.TextContent).Text)
	require.Equal(t, 2, callCount)
	require.Equal(t, 1, reconnectCount)
}

func TestCallToolWithRetryDoesNotRetryCanceledContext(t *testing.T) {
	store := loadTestStore(t)
	const name = "canceled-mcp"

	originalCallToolOnSession := callToolOnSession
	originalReconnectClient := reconnectClient
	t.Cleanup(func() {
		callToolOnSession = originalCallToolOnSession
		reconnectClient = originalReconnectClient
		states.Del(name)
		sessions.Del(name)
		allTools.Del(name)
	})

	updateState(name, StateConnected, nil, &ClientSession{}, Counts{Tools: 1})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	callCount := 0
	callToolOnSession = func(ctx context.Context, session *ClientSession, params *sdkmcp.CallToolParams) (*sdkmcp.CallToolResult, error) {
		callCount++
		return nil, errors.Join(ctx.Err(), sdkmcp.ErrConnectionClosed)
	}

	reconnectClient = func(ctx context.Context, cfg *config.ConfigStore, gotName string) error {
		t.Fatal("reconnect should not be called")
		return nil
	}

	_, err := callToolWithRetry(ctx, store, name, &ClientSession{}, &sdkmcp.CallToolParams{Name: "test-tool"})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, callCount)
}

// TestShouldRetryToolCall_SyscallErrors verifies that low-level connection
// failures surfaced as syscall errors are treated as retryable on every
// platform (these constants exist in the syscall package on both Unix and
// Windows).
func TestShouldRetryToolCall_SyscallErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
	}{
		{"ECONNRESET", fmt.Errorf("connection reset by peer: %w", syscall.ECONNRESET)},
		{"ECONNREFUSED", fmt.Errorf("connection refused: %w", syscall.ECONNREFUSED)},
		{"EPIPE", fmt.Errorf("broken pipe: %w", syscall.EPIPE)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.True(t, shouldRetryToolCall(context.Background(), tt.err),
				"expected shouldRetryToolCall to return true for %s", tt.name)
			// The wrapped error must still match the underlying syscall
			// value via errors.Is so the retry decision is reliable.
			require.True(t, errors.Is(tt.err, syscall.ECONNRESET) ||
				errors.Is(tt.err, syscall.ECONNREFUSED) ||
				errors.Is(tt.err, syscall.EPIPE))
		})
	}
}

// TestShouldRetryToolCall_NonRetryableErrors verifies that benign and
// context-driven errors are not treated as retryable, guarding against the
// expanded retry set masking cancellations.
func TestShouldRetryToolCall_NonRetryableErrors(t *testing.T) {
	t.Parallel()

	require.False(t, shouldRetryToolCall(context.Background(), nil))
	require.False(t, shouldRetryToolCall(context.Background(), errors.New("some benign error")))

	// A cancelled context short-circuits the retry decision even when the
	// error would otherwise be retryable.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.False(t, shouldRetryToolCall(ctx, fmt.Errorf("connection reset: %w", syscall.ECONNRESET)))
}

// TestCallToolWithRetry_NetworkRetryDelay verifies that the network-error
// retry path waits reconnectRetryDelay (~100ms) before triggering a
// reconnect, while still ultimately invoking the reconnect.
func TestCallToolWithRetry_NetworkRetryDelay(t *testing.T) {
	store := loadTestStore(t)
	const name = "retry-delay-mcp"

	originalCallToolOnSession := callToolOnSession
	originalReconnectClient := reconnectClient
	t.Cleanup(func() {
		callToolOnSession = originalCallToolOnSession
		reconnectClient = originalReconnectClient
		states.Del(name)
		sessions.Del(name)
		allTools.Del(name)
	})

	updateState(name, StateConnected, nil, &ClientSession{}, Counts{Tools: 1})

	callToolOnSession = func(ctx context.Context, session *ClientSession, params *sdkmcp.CallToolParams) (*sdkmcp.CallToolResult, error) {
		return nil, fmt.Errorf("transport failed: %w", syscall.ECONNRESET)
	}

	var reconnectTime time.Time
	reconnectClient = func(ctx context.Context, cfg *config.ConfigStore, gotName string) error {
		reconnectTime = time.Now()
		require.Equal(t, name, gotName)
		return nil
	}

	start := time.Now()
	_, err := callToolWithRetry(context.Background(), store, name, &ClientSession{}, &sdkmcp.CallToolParams{Name: "test-tool"})
	elapsed := time.Since(start)
	require.Error(t, err)

	// reconnectClient must have been invoked after the backoff.
	require.False(t, reconnectTime.IsZero(), "reconnect must be attempted after the delay")
	delay := reconnectTime.Sub(start)
	// The reconnect must happen after ~reconnectRetryDelay (100ms). Allow a
	// ±50ms tolerance per the task spec, with a generous upper bound to
	// absorb scheduler jitter on busy CI hosts.
	require.GreaterOrEqual(t, delay, reconnectRetryDelay-50*time.Millisecond,
		"expected at least ~100ms delay before reconnect, got %v", delay)
	require.LessOrEqual(t, delay, reconnectRetryDelay+150*time.Millisecond,
		"reconnect delayed too long: %v", delay)
	// Sanity: total elapsed should also reflect the sleep.
	require.GreaterOrEqual(t, elapsed, reconnectRetryDelay-50*time.Millisecond)
}

// TestCallToolWithRetry_OAuthRetryHasNoDelay verifies that the OAuth 401
// refresh path retries immediately, without the 100ms backoff that gates the
// network-error path. This locks in the distinction between the two retry
// paths required by the spec.
func TestCallToolWithRetry_OAuthRetryHasNoDelay(t *testing.T) {
	store := loadTestStore(t)
	const name = "oauth-no-delay-mcp"

	originalCallToolOnSession := callToolOnSession
	originalRefreshToken := refreshToken
	originalReconnectClient := reconnectClient
	t.Cleanup(func() {
		callToolOnSession = originalCallToolOnSession
		refreshToken = originalRefreshToken
		reconnectClient = originalReconnectClient
		states.Del(name)
		sessions.Del(name)
		allTools.Del(name)
	})

	updateState(name, StateConnected, nil, &ClientSession{}, Counts{Tools: 1})

	callToolOnSession = func(ctx context.Context, session *ClientSession, params *sdkmcp.CallToolParams) (*sdkmcp.CallToolResult, error) {
		return nil, errors.New("HTTP 401 Unauthorized")
	}
	refreshToken = func(ctx context.Context, serverName string) error {
		require.Equal(t, name, serverName)
		return nil
	}
	reconnectClient = func(ctx context.Context, cfg *config.ConfigStore, gotName string) error {
		t.Fatal("reconnect should not be called on 401 auth error")
		return nil
	}

	start := time.Now()
	_, err := callToolWithRetry(context.Background(), store, name, &ClientSession{}, &sdkmcp.CallToolParams{Name: "test-tool"})
	elapsed := time.Since(start)

	// Refresh succeeds but the retry call still returns 401, so an error is
	// expected. The important assertion is that the OAuth path did not wait
	// for reconnectRetryDelay before retrying.
	require.Error(t, err)
	require.Less(t, elapsed, reconnectRetryDelay-50*time.Millisecond,
		"OAuth retry path must not apply the network backoff, took %v", elapsed)
}

// TestCallToolWithRetry_ReconnectLimitWhileReconnecting verifies the single
// reconnect limit: when a background reconnectLoop is already running for the
// server, callToolWithRetry must not trigger a second synchronous reconnect.
// It surfaces the original failure instead.
func TestCallToolWithRetry_ReconnectLimitWhileReconnecting(t *testing.T) {
	store := loadTestStore(t)
	const name = "reconnect-limit-mcp"

	originalCallToolOnSession := callToolOnSession
	originalReconnectClient := reconnectClient
	t.Cleanup(func() {
		callToolOnSession = originalCallToolOnSession
		reconnectClient = originalReconnectClient
		states.Del(name)
		sessions.Del(name)
		allTools.Del(name)
		reconnecting.Delete(name)
	})

	updateState(name, StateConnected, nil, &ClientSession{}, Counts{Tools: 1})

	callToolOnSession = func(ctx context.Context, session *ClientSession, params *sdkmcp.CallToolParams) (*sdkmcp.CallToolResult, error) {
		return nil, fmt.Errorf("transport failed: %w", syscall.ECONNRESET)
	}

	reconnectCalled := false
	reconnectClient = func(ctx context.Context, cfg *config.ConfigStore, gotName string) error {
		reconnectCalled = true
		return nil
	}

	// Simulate an in-flight background reconnectLoop for this server.
	reconnecting.Store(name, struct{}{})
	require.True(t, isReconnecting(name))

	start := time.Now()
	_, err := callToolWithRetry(context.Background(), store, name, &ClientSession{}, &sdkmcp.CallToolParams{Name: "test-tool"})
	elapsed := time.Since(start)

	require.Error(t, err, "original failure should be surfaced")
	require.False(t, reconnectCalled, "reconnect must not be triggered while a background loop is running")
	// Because we short-circuit before the backoff, this path must return
	// promptly.
	require.Less(t, elapsed, reconnectRetryDelay-50*time.Millisecond,
		"reconnect-limit path must skip the backoff, took %v", elapsed)
}

func loadTestStore(t *testing.T) *config.ConfigStore {
	t.Helper()
	workingDir := t.TempDir()
	globalConfigRoot := filepath.Join(t.TempDir(), "mcp-global")
	globalDataRoot := filepath.Join(t.TempDir(), "mcp-data")
	require.NoError(t, os.MkdirAll(globalConfigRoot, 0o755))
	require.NoError(t, os.MkdirAll(globalDataRoot, 0o755))

	t.Setenv("CRUSH_GLOBAL_CONFIG", globalConfigRoot)
	t.Setenv("CRUSH_GLOBAL_DATA", globalDataRoot)
	t.Setenv("CRUSH_DISABLE_PROVIDER_AUTO_UPDATE", "1")
	t.Cleanup(func() {
		require.NoError(t, crushlog.ResetForTesting())
	})

	payload := []byte("{}")
	require.NoError(t, os.WriteFile(filepath.Join(workingDir, "crush.json"), payload, 0o600))

	store, err := config.Load(workingDir, "", false)
	require.NoError(t, err)
	return store
}

func TestFilterTools(t *testing.T) {
	rawTools := []*Tool{
		{Name: "tool1"},
		{Name: "tool2"},
		{Name: "tool3"},
	}

	tests := []struct {
		name     string
		mcpName  string
		mcpCfg   config.MCPConfig
		expected []string
	}{
		{
			name:     "no filters specified",
			mcpName:  "test-mcp",
			mcpCfg:   config.MCPConfig{},
			expected: []string{"tool1", "tool2", "tool3"},
		},
		{
			name:    "only disabled tools specified",
			mcpName: "test-mcp",
			mcpCfg: config.MCPConfig{
				DisabledTools: []string{"tool2"},
			},
			expected: []string{"tool1", "tool3"},
		},
		{
			name:    "only enabled tools specified",
			mcpName: "test-mcp",
			mcpCfg: config.MCPConfig{
				EnabledTools: []string{"tool1", "tool3"},
			},
			expected: []string{"tool1", "tool3"},
		},
		{
			name:    "both enabled and disabled tools specified",
			mcpName: "test-mcp",
			mcpCfg: config.MCPConfig{
				EnabledTools:  []string{"tool1", "tool2"},
				DisabledTools: []string{"tool2"},
			},
			expected: []string{"tool1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := loadTestStore(t)
			cfg := store.Config()
			cfg.MCP = map[string]config.MCPConfig{
				tt.mcpName: tt.mcpCfg,
			}

			filtered := filterTools(store, tt.mcpName, rawTools)
			var names []string
			for _, tool := range filtered {
				names = append(names, tool.Name)
			}
			require.Equal(t, tt.expected, names)
		})
	}
}
