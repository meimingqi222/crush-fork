package mcp

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/charmbracelet/crush/internal/db"
)

// DefaultCacheTTL is the default time-to-live for cached MCP tool
// definitions. Entries older than this are treated as stale.
const DefaultCacheTTL = 30 * 24 * time.Hour

// ErrCacheMiss is returned when no usable cache entry exists for a server,
// either because there is no row, the config hash does not match, or the
// entry has expired.
var ErrCacheMiss = errors.New("mcp tool cache miss")

// ComputeConfigHash serializes the server config to JSON and returns the
// SHA-256 hex digest. The same config always yields the same hash. An empty
// string is returned if the config cannot be serialized.
func ComputeConfigHash(serverConfig interface{}) string {
	data, err := json.Marshal(serverConfig)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// IsCacheValid reports whether a cache entry written at cachedAt is still
// within the given TTL.
func IsCacheValid(cachedAt time.Time, ttl time.Duration) bool {
	return time.Since(cachedAt) <= ttl
}

// LoadCachedTools reads the cached tool definitions for the given server. It
// returns ErrCacheMiss when no entry exists, when the config hash does not
// match the current config, or when the entry has exceeded its TTL.
func LoadCachedTools(ctx context.Context, q *db.Queries, serverName, configHash string, ttl time.Duration) ([]*Tool, error) {
	row, err := q.GetMCPToolCache(ctx, serverName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: no cache entry for %s", ErrCacheMiss, serverName)
		}
		return nil, fmt.Errorf("error reading mcp tool cache for %s: %w", serverName, err)
	}
	if row.ConfigHash != configHash {
		return nil, fmt.Errorf("%w: config hash mismatch for %s", ErrCacheMiss, serverName)
	}
	if !IsCacheValid(time.Unix(row.CachedAt, 0), ttl) {
		return nil, fmt.Errorf("%w: cache expired for %s", ErrCacheMiss, serverName)
	}
	var tools []*Tool
	if err := json.Unmarshal([]byte(row.ToolsJson), &tools); err != nil {
		return nil, fmt.Errorf("error decoding cached tools for %s: %w", serverName, err)
	}
	return tools, nil
}

// SaveCachedTools serializes the tools and writes them to the cache, keyed by
// serverName. An empty configHash skips the write since the entry could never
// be matched on a subsequent load.
func SaveCachedTools(ctx context.Context, q *db.Queries, serverName, configHash string, tools []*Tool) error {
	if configHash == "" {
		return nil
	}
	data, err := json.Marshal(tools)
	if err != nil {
		return fmt.Errorf("error encoding tools for %s: %w", serverName, err)
	}
	if err := q.UpsertMCPToolCache(ctx, db.UpsertMCPToolCacheParams{
		ServerName: serverName,
		ConfigHash: configHash,
		ToolsJson:  string(data),
	}); err != nil {
		return fmt.Errorf("error writing mcp tool cache for %s: %w", serverName, err)
	}
	return nil
}

// DeleteCachedTools removes any cached tool definitions for the given server.
func DeleteCachedTools(ctx context.Context, q *db.Queries, serverName string) error {
	if err := q.DeleteMCPToolCache(ctx, serverName); err != nil {
		return fmt.Errorf("error deleting mcp tool cache for %s: %w", serverName, err)
	}
	return nil
}
