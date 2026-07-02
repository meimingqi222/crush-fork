package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/stretchr/testify/require"
)

// newTestQueries opens a fresh SQLite database with migrations applied and
// returns a *db.Queries backed by it. The database is cleaned up automatically.
func newTestQueries(t *testing.T) *db.Queries {
	t.Helper()
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	return db.New(conn)
}

func testTools() []*Tool {
	return []*Tool{
		{Name: "tool-a", Description: "first tool"},
		{Name: "tool-b", Description: "second tool"},
	}
}

func TestComputeConfigHashStable(t *testing.T) {
	t.Parallel()

	cfg := config.MCPConfig{Type: config.MCPStdio, Command: "echo"}
	h1 := ComputeConfigHash(cfg)
	h2 := ComputeConfigHash(cfg)
	require.Equal(t, h1, h2)
	require.Len(t, h1, 64) // SHA-256 hex digest length

	// A different config yields a different hash.
	other := config.MCPConfig{Type: config.MCPStdio, Command: "ls"}
	require.NotEqual(t, h1, ComputeConfigHash(other))

	// Unserializable input yields an empty hash without panicking.
	require.Empty(t, ComputeConfigHash(make(chan struct{})))
}

func TestIsCacheValid(t *testing.T) {
	t.Parallel()

	require.True(t, IsCacheValid(time.Now(), DefaultCacheTTL))
	require.False(t, IsCacheValid(time.Now().Add(-31*24*time.Hour), DefaultCacheTTL))
	// A zero TTL means any non-future entry is already expired. Use a
	// timestamp clearly in the past to avoid clock-resolution boundary
	// effects where time.Since(now) could be exactly zero.
	require.False(t, IsCacheValid(time.Now().Add(-time.Second), 0))
}

func TestSaveAndLoadCachedTools_Hit(t *testing.T) {
	t.Parallel()

	q := newTestQueries(t)
	ctx := t.Context()
	const name = "server-hit"
	cfg := config.MCPConfig{Type: config.MCPStdio, Command: "echo"}
	hash := ComputeConfigHash(cfg)
	tools := testTools()

	require.NoError(t, SaveCachedTools(ctx, q, name, hash, tools))

	loaded, err := LoadCachedTools(ctx, q, name, hash, DefaultCacheTTL)
	require.NoError(t, err)
	require.Len(t, loaded, len(tools))
	require.Equal(t, tools[0].Name, loaded[0].Name)
	require.Equal(t, tools[0].Description, loaded[0].Description)
	require.Equal(t, tools[1].Name, loaded[1].Name)
}

func TestLoadCachedTools_ConfigHashMismatch(t *testing.T) {
	t.Parallel()

	q := newTestQueries(t)
	ctx := t.Context()
	const name = "server-mismatch"
	cfg := config.MCPConfig{Type: config.MCPStdio, Command: "echo"}
	hash := ComputeConfigHash(cfg)
	require.NoError(t, SaveCachedTools(ctx, q, name, hash, testTools()))

	// A changed config produces a different hash and must invalidate the cache.
	newHash := ComputeConfigHash(config.MCPConfig{Type: config.MCPStdio, Command: "ls"})
	_, err := LoadCachedTools(ctx, q, name, newHash, DefaultCacheTTL)
	require.ErrorIs(t, err, ErrCacheMiss)
}

func TestLoadCachedTools_TTLExpired(t *testing.T) {
	t.Parallel()

	q := newTestQueries(t)
	ctx := t.Context()
	const name = "server-expired"
	cfg := config.MCPConfig{Type: config.MCPStdio, Command: "echo"}
	hash := ComputeConfigHash(cfg)
	require.NoError(t, SaveCachedTools(ctx, q, name, hash, testTools()))

	// A zero TTL means the entry is already expired.
	_, err := LoadCachedTools(ctx, q, name, hash, 0)
	require.ErrorIs(t, err, ErrCacheMiss)
}

func TestLoadCachedTools_NoEntry(t *testing.T) {
	t.Parallel()

	q := newTestQueries(t)
	ctx := t.Context()

	_, err := LoadCachedTools(ctx, q, "never-cached", "any-hash", DefaultCacheTTL)
	require.ErrorIs(t, err, ErrCacheMiss)
}

func TestSaveCachedTools_UpsertOverwritesOnReconnect(t *testing.T) {
	t.Parallel()

	q := newTestQueries(t)
	ctx := t.Context()
	const name = "server-upsert"
	cfg := config.MCPConfig{Type: config.MCPStdio, Command: "echo"}
	hash := ComputeConfigHash(cfg)

	// Initial successful connection persists the original tool set.
	original := []*Tool{{Name: "old-tool", Description: "v1"}}
	require.NoError(t, SaveCachedTools(ctx, q, name, hash, original))
	loaded, err := LoadCachedTools(ctx, q, name, hash, DefaultCacheTTL)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	require.Equal(t, "old-tool", loaded[0].Name)

	// A subsequent successful reconnect refreshes the cache with the latest
	// tool definitions, replacing the previous entry.
	refreshed := []*Tool{
		{Name: "new-tool-1", Description: "v2"},
		{Name: "new-tool-2", Description: "v2"},
	}
	require.NoError(t, SaveCachedTools(ctx, q, name, hash, refreshed))
	loaded, err = LoadCachedTools(ctx, q, name, hash, DefaultCacheTTL)
	require.NoError(t, err)
	require.Len(t, loaded, 2)
	require.Equal(t, "new-tool-1", loaded[0].Name)
	require.Equal(t, "new-tool-2", loaded[1].Name)
}

func TestDeleteCachedTools(t *testing.T) {
	t.Parallel()

	q := newTestQueries(t)
	ctx := t.Context()
	const name = "server-delete"
	cfg := config.MCPConfig{Type: config.MCPStdio, Command: "echo"}
	hash := ComputeConfigHash(cfg)
	require.NoError(t, SaveCachedTools(ctx, q, name, hash, testTools()))

	require.NoError(t, DeleteCachedTools(ctx, q, name))

	_, err := LoadCachedTools(ctx, q, name, hash, DefaultCacheTTL)
	require.ErrorIs(t, err, ErrCacheMiss)
}

func TestSaveCachedTools_EmptyHashSkipsWrite(t *testing.T) {
	t.Parallel()

	q := newTestQueries(t)
	ctx := t.Context()
	const name = "server-empty-hash"

	// An empty configHash (unserializable config) skips the write entirely.
	require.NoError(t, SaveCachedTools(ctx, q, name, "", testTools()))

	_, err := LoadCachedTools(ctx, q, name, "", DefaultCacheTTL)
	require.ErrorIs(t, err, ErrCacheMiss)
}

// TestLoadCachedToolsRespectsContextCancellation ensures the cache read honors
// a cancelled context rather than blocking.
func TestLoadCachedToolsRespectsContextCancellation(t *testing.T) {
	t.Parallel()

	q := newTestQueries(t)
	const name = "server-cancel"
	cfg := config.MCPConfig{Type: config.MCPStdio, Command: "echo"}
	hash := ComputeConfigHash(cfg)
	require.NoError(t, SaveCachedTools(t.Context(), q, name, hash, testTools()))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := LoadCachedTools(ctx, q, name, hash, DefaultCacheTTL)
	require.ErrorIs(t, err, context.Canceled)
}
