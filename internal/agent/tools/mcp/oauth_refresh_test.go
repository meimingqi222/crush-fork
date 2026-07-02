package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	crushoauth "github.com/charmbracelet/crush/internal/oauth"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

// TestRefreshToken_Success verifies that RefreshToken reads a stored refresh
// token, calls the OAuth token endpoint, and persists the refreshed token to
// both the database and the config store.
func TestRefreshToken_Success(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		require.Equal(t, "refresh_token", r.Form.Get("grant_type"))
		require.Equal(t, "old-refresh-token", r.Form.Get("refresh_token"))
		require.Equal(t, "test-client-id", r.Form.Get("client_id"))
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-access-token",
			"refresh_token": "new-refresh-token",
			"token_type":    "Bearer",
			"expires_in":    3600,
		}))
	}))
	t.Cleanup(tokenServer.Close)

	q := newTestQueries(t)
	ctx := context.Background()
	const name = "refresh-success-mcp"
	// Seed an expired token so the oauth2 library treats it as stale and
	// performs a refresh request against the mock token endpoint.
	require.NoError(t, q.UpsertMCPOAuthToken(ctx, db.UpsertMCPOAuthTokenParams{
		ServerName:   name,
		AccessToken:  "old-access-token",
		RefreshToken: "old-refresh-token",
		ExpiresAt:    sql.NullInt64{Int64: time.Now().Add(-time.Hour).Unix(), Valid: true},
	}))

	store := loadTestStore(t)
	store.Config().MCP = map[string]config.MCPConfig{
		name: {
			Type: config.MCPHttp,
			OAuth: &config.MCPOAuthConfig{
				Enabled:  true,
				ClientID: "test-client-id",
				AuthServer: &config.MCPOAuthAuthServer{
					TokenEndpoint: tokenServer.URL,
				},
			},
		},
	}

	originalQueries := queries
	originalConfigStore := configStore
	queries = q
	configStore = store
	t.Cleanup(func() {
		queries = originalQueries
		configStore = originalConfigStore
	})

	require.NoError(t, RefreshToken(ctx, name))

	// The database should now hold the refreshed token.
	row, err := q.GetMCPOAuthToken(ctx, name)
	require.NoError(t, err)
	require.Equal(t, "new-access-token", row.AccessToken)
	require.Equal(t, "new-refresh-token", row.RefreshToken)
	require.True(t, row.ExpiresAt.Valid)

	// The config store should also reflect the refreshed token.
	mcpCfg := store.Config().MCP[name]
	require.NotNil(t, mcpCfg.OAuth)
	require.NotNil(t, mcpCfg.OAuth.Token)
	require.Equal(t, "new-access-token", mcpCfg.OAuth.Token.AccessToken)
	require.Equal(t, "new-refresh-token", mcpCfg.OAuth.Token.RefreshToken)
}

// TestRefreshToken_Failure verifies that RefreshToken returns an error when
// the token endpoint rejects the refresh request, and leaves the stored token
// untouched.
func TestRefreshToken_Failure(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"error": "invalid_grant",
		}))
	}))
	t.Cleanup(tokenServer.Close)

	q := newTestQueries(t)
	ctx := context.Background()
	const name = "refresh-fail-mcp"
	// Seed an expired token so the oauth2 library attempts a refresh against
	// the mock token endpoint that returns an error.
	require.NoError(t, q.UpsertMCPOAuthToken(ctx, db.UpsertMCPOAuthTokenParams{
		ServerName:   name,
		AccessToken:  "old-access-token",
		RefreshToken: "old-refresh-token",
		ExpiresAt:    sql.NullInt64{Int64: time.Now().Add(-time.Hour).Unix(), Valid: true},
	}))

	store := loadTestStore(t)
	store.Config().MCP = map[string]config.MCPConfig{
		name: {
			Type: config.MCPHttp,
			OAuth: &config.MCPOAuthConfig{
				Enabled:  true,
				ClientID: "test-client-id",
				AuthServer: &config.MCPOAuthAuthServer{
					TokenEndpoint: tokenServer.URL,
				},
			},
		},
	}

	originalQueries := queries
	originalConfigStore := configStore
	queries = q
	configStore = store
	t.Cleanup(func() {
		queries = originalQueries
		configStore = originalConfigStore
	})

	err := RefreshToken(ctx, name)
	require.Error(t, err)
	require.ErrorContains(t, err, "oauth token refresh failed")

	// The stored token should remain unchanged on failure.
	row, err := q.GetMCPOAuthToken(ctx, name)
	require.NoError(t, err)
	require.Equal(t, "old-access-token", row.AccessToken)
	require.Equal(t, "old-refresh-token", row.RefreshToken)
}

// TestRefreshToken_ForcesRefreshOnNonExpiredToken verifies that RefreshToken
// forces a refresh even when the stored token has not expired. A 401 means the
// server rejected the token, so the oauth2 library must not short-circuit and
// return the same token. This exercises the DB fallback path.
func TestRefreshToken_ForcesRefreshOnNonExpiredToken(t *testing.T) {
	refreshCalled := 0
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalled++
		require.NoError(t, r.ParseForm())
		require.Equal(t, "refresh_token", r.Form.Get("grant_type"))
		require.Equal(t, "old-refresh-token", r.Form.Get("refresh_token"))
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-access-token",
			"refresh_token": "new-refresh-token",
			"token_type":    "Bearer",
			"expires_in":    3600,
		}))
	}))
	t.Cleanup(tokenServer.Close)

	q := newTestQueries(t)
	ctx := context.Background()
	const name = "refresh-force-mcp"
	// Seed a token that is NOT expired. Without the force-refresh fix the
	// oauth2 library would return it as-is and the token endpoint would
	// never be hit.
	require.NoError(t, q.UpsertMCPOAuthToken(ctx, db.UpsertMCPOAuthTokenParams{
		ServerName:   name,
		AccessToken:  "old-access-token",
		RefreshToken: "old-refresh-token",
		ExpiresAt:    sql.NullInt64{Int64: time.Now().Add(time.Hour).Unix(), Valid: true},
	}))

	store := loadTestStore(t)
	store.Config().MCP = map[string]config.MCPConfig{
		name: {
			Type: config.MCPHttp,
			OAuth: &config.MCPOAuthConfig{
				Enabled:  true,
				ClientID: "test-client-id",
				AuthServer: &config.MCPOAuthAuthServer{
					TokenEndpoint: tokenServer.URL,
				},
			},
		},
	}

	originalQueries := queries
	originalConfigStore := configStore
	queries = q
	configStore = store
	t.Cleanup(func() {
		queries = originalQueries
		configStore = originalConfigStore
	})

	require.NoError(t, RefreshToken(ctx, name))
	require.Equal(t, 1, refreshCalled, "token endpoint must be hit even for non-expired tokens")

	// Both stores should hold the refreshed token.
	row, err := q.GetMCPOAuthToken(ctx, name)
	require.NoError(t, err)
	require.Equal(t, "new-access-token", row.AccessToken)
	require.Equal(t, "new-refresh-token", row.RefreshToken)

	mcpCfg := store.Config().MCP[name]
	require.NotNil(t, mcpCfg.OAuth.Token)
	require.Equal(t, "new-access-token", mcpCfg.OAuth.Token.AccessToken)
}

// TestRefreshToken_UsesAuthorizerPath verifies that RefreshToken delegates to
// the registered authorizer (sharing its mutex with the round tripper) instead
// of refreshing from the database independently. The seeded token is not
// expired, so this also confirms the authorizer path forces a refresh.
func TestRefreshToken_UsesAuthorizerPath(t *testing.T) {
	refreshCalled := 0
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalled++
		require.NoError(t, r.ParseForm())
		require.Equal(t, "refresh_token", r.Form.Get("grant_type"))
		require.Equal(t, "old-refresh-token", r.Form.Get("refresh_token"))
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-access-token",
			"refresh_token": "new-refresh-token",
			"token_type":    "Bearer",
			"expires_in":    3600,
		}))
	}))
	t.Cleanup(tokenServer.Close)

	q := newTestQueries(t)
	ctx := context.Background()
	const name = "refresh-authorizer-mcp"

	store := loadTestStore(t)
	// The config store holds a non-expired token; the authorizer reads it
	// from here via currentConfig().
	store.Config().MCP = map[string]config.MCPConfig{
		name: {
			Type: config.MCPHttp,
			OAuth: &config.MCPOAuthConfig{
				Enabled:  true,
				ClientID: "test-client-id",
				AuthServer: &config.MCPOAuthAuthServer{
					TokenEndpoint: tokenServer.URL,
				},
				Token: &crushoauth.Token{
					AccessToken:  "old-access-token",
					RefreshToken: "old-refresh-token",
					ExpiresAt:    time.Now().Add(time.Hour).Unix(),
				},
			},
		},
	}

	originalQueries := queries
	originalConfigStore := configStore
	queries = q
	configStore = store
	t.Cleanup(func() {
		queries = originalQueries
		configStore = originalConfigStore
	})

	// Register an authorizer so RefreshToken takes the authorizer path.
	authorizer := newMCPOAuthAuthorizer(name, store, nil)
	authorizers.Set(name, authorizer)
	t.Cleanup(func() { authorizers.Del(name) })

	require.NoError(t, RefreshToken(ctx, name))
	require.Equal(t, 1, refreshCalled, "authorizer path must force a refresh")

	// The authorizer path writes to both the database and the config store.
	row, err := q.GetMCPOAuthToken(ctx, name)
	require.NoError(t, err)
	require.Equal(t, "new-access-token", row.AccessToken)
	require.Equal(t, "new-refresh-token", row.RefreshToken)

	mcpCfg := store.Config().MCP[name]
	require.NotNil(t, mcpCfg.OAuth.Token)
	require.Equal(t, "new-access-token", mcpCfg.OAuth.Token.AccessToken)
	require.Equal(t, "new-refresh-token", mcpCfg.OAuth.Token.RefreshToken)
}

// TestCallToolWithRetry_401_TransparentRetry verifies that callToolWithRetry
// transparently refreshes the OAuth token and retries the tool call when the
// first attempt returns a 401 auth error.
func TestCallToolWithRetry_401_TransparentRetry(t *testing.T) {
	store := loadTestStore(t)
	const name = "auth-retry-mcp"

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

	callCount := 0
	callToolOnSession = func(ctx context.Context, session *ClientSession, params *sdkmcp.CallToolParams) (*sdkmcp.CallToolResult, error) {
		callCount++
		if callCount == 1 {
			return nil, errors.New("HTTP 401 Unauthorized")
		}
		return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "ok"}}}, nil
	}

	refreshCalled := false
	refreshToken = func(ctx context.Context, serverName string) error {
		refreshCalled = true
		require.Equal(t, name, serverName)
		return nil
	}

	reconnectClient = func(ctx context.Context, cfg *config.ConfigStore, gotName string) error {
		t.Fatal("reconnect should not be called on 401 auth error")
		return nil
	}

	result, err := callToolWithRetry(context.Background(), store, name, &ClientSession{}, &sdkmcp.CallToolParams{Name: "test-tool"})
	require.NoError(t, err)
	require.Equal(t, "ok", result.Content[0].(*sdkmcp.TextContent).Text)
	require.Equal(t, 2, callCount)
	require.True(t, refreshCalled)
}

// TestCallToolWithRetry_401_RefreshFails verifies that callToolWithRetry marks
// the server as StateNeedsAuth when token refresh fails after a 401.
func TestCallToolWithRetry_401_RefreshFails(t *testing.T) {
	store := loadTestStore(t)
	const name = "auth-fail-mcp"

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
		return fmt.Errorf("refresh failed")
	}

	reconnectClient = func(ctx context.Context, cfg *config.ConfigStore, gotName string) error {
		t.Fatal("reconnect should not be called on 401 auth error")
		return nil
	}

	_, err := callToolWithRetry(context.Background(), store, name, &ClientSession{}, &sdkmcp.CallToolParams{Name: "test-tool"})
	require.Error(t, err)
	require.ErrorContains(t, err, "requires reauthentication")

	info, ok := states.Get(name)
	require.True(t, ok)
	require.Equal(t, StateNeedsAuth, info.State)
}

// TestMCPOAuthTokenRoundtrip verifies that tokens written via
// UpsertMCPOAuthToken can be read back consistently via GetMCPOAuthToken, and
// exercises the saveOAuthTokenToDB/loadOAuthTokenFromDB helper functions used
// by RefreshToken.
func TestMCPOAuthTokenRoundtrip(t *testing.T) {
	q := newTestQueries(t)
	ctx := context.Background()

	// Insert an initial token row.
	expiry := time.Now().Add(time.Hour).Truncate(time.Second)
	params := db.UpsertMCPOAuthTokenParams{
		ServerName:   "roundtrip-mcp",
		AccessToken:  "access-123",
		RefreshToken: "refresh-456",
		ExpiresAt:    sql.NullInt64{Int64: expiry.Unix(), Valid: true},
	}
	require.NoError(t, q.UpsertMCPOAuthToken(ctx, params))

	row, err := q.GetMCPOAuthToken(ctx, "roundtrip-mcp")
	require.NoError(t, err)
	require.Equal(t, params.ServerName, row.ServerName)
	require.Equal(t, params.AccessToken, row.AccessToken)
	require.Equal(t, params.RefreshToken, row.RefreshToken)
	require.True(t, row.ExpiresAt.Valid)
	require.WithinDuration(t, expiry, time.Unix(row.ExpiresAt.Int64, 0), time.Second)

	// Upsert overwrites the existing row.
	params.AccessToken = "access-789"
	params.RefreshToken = "refresh-012"
	require.NoError(t, q.UpsertMCPOAuthToken(ctx, params))
	row, err = q.GetMCPOAuthToken(ctx, "roundtrip-mcp")
	require.NoError(t, err)
	require.Equal(t, "access-789", row.AccessToken)
	require.Equal(t, "refresh-012", row.RefreshToken)

	// Delete removes the row.
	require.NoError(t, q.DeleteMCPOAuthToken(ctx, "roundtrip-mcp"))
	_, err = q.GetMCPOAuthToken(ctx, "roundtrip-mcp")
	require.ErrorIs(t, err, sql.ErrNoRows)

	// Exercise the package-level helper functions used by RefreshToken. These
	// operate on the package-level queries variable, so we inject our test
	// database and restore the original on cleanup.
	originalQueries := queries
	queries = q
	t.Cleanup(func() { queries = originalQueries })

	helperToken := &crushoauth.Token{
		AccessToken:  "helper-access",
		RefreshToken: "helper-refresh",
		ExpiresAt:    time.Now().Add(2 * time.Hour).Unix(),
	}
	helperToken.SetExpiresIn()

	require.NoError(t, saveOAuthTokenToDB(ctx, "helper-mcp", helperToken))

	loaded, err := loadOAuthTokenFromDB(ctx, "helper-mcp")
	require.NoError(t, err)
	require.NotNil(t, loaded)
	require.Equal(t, helperToken.AccessToken, loaded.AccessToken)
	require.Equal(t, helperToken.RefreshToken, loaded.RefreshToken)
	require.Equal(t, helperToken.ExpiresAt, loaded.ExpiresAt)

	require.NoError(t, deleteOAuthTokenFromDB(ctx, "helper-mcp"))
	loaded, err = loadOAuthTokenFromDB(ctx, "helper-mcp")
	require.NoError(t, err)
	require.Nil(t, loaded)
}
