package mcp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	crushoauth "github.com/charmbracelet/crush/internal/oauth"
	"golang.org/x/oauth2"
)

// refreshMutexes provides per-server locking for the DB-based refresh
// fallback used when no live authorizer is registered. LoadOrStore ensures
// each server name maps to a single mutex, avoiding the TOCTOU race a
// check-then-set would introduce.
var refreshMutexes sync.Map

// refreshMutexFor returns the mutex used to serialize DB-based refreshes for
// the named server.
func refreshMutexFor(serverName string) *sync.Mutex {
	v, _ := refreshMutexes.LoadOrStore(serverName, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// RefreshToken refreshes the OAuth access token for the named MCP server. It
// prefers the live authorizer registered by the round tripper so the refresh
// is serialized with the round tripper's pre-refresh path (avoiding a race
// when the OAuth server rotates refresh tokens). When no authorizer is
// registered it falls back to refreshing from the token persisted in the
// database. The refreshed token is persisted to both the database and the
// config store so subsequent requests use the new credentials.
//
// The refresh is always forced — a 401 means the server rejected the current
// token even if it has not expired, so the oauth2 library must not short-
// circuit and return the same token.
func RefreshToken(ctx context.Context, serverName string) error {
	if configStore == nil {
		return fmt.Errorf("oauth refresh unavailable: config store not configured")
	}

	// Prefer the registered authorizer so the refresh shares its mutex with
	// the round tripper's pre-refresh path and reuses the single, consistent
	// write logic in refreshTokenLocked.
	if authorizer, ok := authorizers.Get(serverName); ok {
		if _, err := authorizer.forceRefresh(ctx); err != nil {
			return fmt.Errorf("oauth token refresh failed for %s: %w", serverName, err)
		}
		slog.Info("Refreshed MCP OAuth token", "name", serverName)
		return nil
	}

	// Fallback: no live authorizer (e.g. the server is not connected or
	// does not use the round tripper). Refresh directly from the token
	// persisted in the database, guarded by a per-server mutex so concurrent
	// 401 retries do not race.
	return refreshTokenFromDB(ctx, serverName)
}

// refreshTokenFromDB refreshes the OAuth token using the refresh token
// persisted in the database. It is the fallback path used when no live
// authorizer is registered. The refresh is serialized per server and forced
// so a still-valid-but-rejected token is always replaced.
func refreshTokenFromDB(ctx context.Context, serverName string) error {
	if queries == nil {
		return fmt.Errorf("oauth refresh unavailable: database not configured")
	}

	mu := refreshMutexFor(serverName)
	mu.Lock()
	defer mu.Unlock()

	row, err := queries.GetMCPOAuthToken(ctx, serverName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("no oauth token stored for %s", serverName)
		}
		return fmt.Errorf("error reading oauth token for %s: %w", serverName, err)
	}
	if row.RefreshToken == "" {
		return fmt.Errorf("no refresh token stored for %s", serverName)
	}

	m, ok := configStore.Config().MCP[serverName]
	if !ok {
		return fmt.Errorf("mcp %s not found", serverName)
	}
	if m.OAuth == nil || m.OAuth.AuthServer == nil || m.OAuth.AuthServer.TokenEndpoint == "" {
		return fmt.Errorf("oauth refresh is not configured for %s", serverName)
	}

	clientID, clientSecret := mcpOAuthClientCredentials(m.OAuth)
	if clientID == "" {
		return fmt.Errorf("oauth client id is not configured for %s", serverName)
	}

	cfg := oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint: oauth2.Endpoint{
			TokenURL:  m.OAuth.AuthServer.TokenEndpoint,
			AuthStyle: selectAuthStyle(m.OAuth.AuthServer.TokenEndpointAuthMethodsSupported, clientSecret != ""),
		},
	}

	current := &oauth2.Token{
		AccessToken:  row.AccessToken,
		RefreshToken: row.RefreshToken,
	}
	// Force a refresh: a 401 means the server rejected the token even if it
	// has not expired, so the oauth2 library must not short-circuit and
	// return the same token.
	current.Expiry = time.Now()

	tokenSource := cfg.TokenSource(ctx, current)
	newToken, err := tokenSource.Token()
	if err != nil {
		return fmt.Errorf("oauth token refresh failed for %s: %w", serverName, err)
	}

	internalToken := fromOAuth2Token(newToken)

	// Persist to the database first, then the config store, matching the
	// order used by refreshTokenLocked so the two stores stay consistent.
	if err := saveOAuthTokenToDB(ctx, serverName, internalToken); err != nil {
		return fmt.Errorf("error persisting refreshed oauth token for %s: %w", serverName, err)
	}
	updated := cloneMCPOAuthConfig(m.OAuth)
	updated.Token = internalToken
	if err := configStore.SetMCPOAuthConfig(config.ScopeGlobal, serverName, updated); err != nil {
		return fmt.Errorf("error updating oauth config for %s: %w", serverName, err)
	}

	slog.Info("Refreshed MCP OAuth token", "name", serverName)
	return nil
}

// mcpOAuthClientCredentials extracts the client ID and secret from the OAuth
// config, preferring the top-level client credentials over the dynamic
// registration.
func mcpOAuthClientCredentials(oauthCfg *config.MCPOAuthConfig) (string, string) {
	if oauthCfg == nil {
		return "", ""
	}
	if oauthCfg.ClientID != "" {
		return oauthCfg.ClientID, oauthCfg.ClientSecret
	}
	if oauthCfg.Registration != nil {
		return oauthCfg.Registration.ClientID, oauthCfg.Registration.ClientSecret
	}
	return "", ""
}

// saveOAuthTokenToDB persists the token to the mcp_oauth_tokens table. It is a
// no-op when no database handle is available or the token is nil.
func saveOAuthTokenToDB(ctx context.Context, serverName string, token *crushoauth.Token) error {
	if queries == nil || token == nil {
		return nil
	}
	var expiresAt sql.NullInt64
	if token.ExpiresAt > 0 {
		expiresAt = sql.NullInt64{Int64: token.ExpiresAt, Valid: true}
	}
	return queries.UpsertMCPOAuthToken(ctx, db.UpsertMCPOAuthTokenParams{
		ServerName:   serverName,
		AccessToken:  token.AccessToken,
		RefreshToken: token.RefreshToken,
		ExpiresAt:    expiresAt,
	})
}

// loadOAuthTokenFromDB reads the persisted token for the given server. It
// returns nil (without error) when no database handle is available or no row
// exists.
func loadOAuthTokenFromDB(ctx context.Context, serverName string) (*crushoauth.Token, error) {
	if queries == nil {
		return nil, nil
	}
	row, err := queries.GetMCPOAuthToken(ctx, serverName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	token := &crushoauth.Token{
		AccessToken:  row.AccessToken,
		RefreshToken: row.RefreshToken,
	}
	if row.ExpiresAt.Valid {
		token.ExpiresAt = row.ExpiresAt.Int64
		token.SetExpiresIn()
	}
	return token, nil
}

// deleteOAuthTokenFromDB removes the persisted token for the given server. It
// is a no-op when no database handle is available.
func deleteOAuthTokenFromDB(ctx context.Context, serverName string) error {
	if queries == nil {
		return nil
	}
	return queries.DeleteMCPOAuthToken(ctx, serverName)
}
