package memory

import (
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/stretchr/testify/require"
)

// TestResolve_OffReturnsNil verifies that a disabled memory configuration
// (backend "off", or enabled=false) resolves to a nil Backend so downstream
// callers can gate the entire memory subsystem on a single nil check.
func TestResolve_OffReturnsNil(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	deps := Deps{DB: conn, DataDirectory: t.TempDir(), WorkingDir: t.TempDir()}

	t.Run("backend off", func(t *testing.T) {
		t.Parallel()
		b := Resolve(&config.MemoryConfig{Backend: "off"}, deps)
		require.Nil(t, b)
	})

	t.Run("enabled false", func(t *testing.T) {
		t.Parallel()
		disabled := false
		b := Resolve(&config.MemoryConfig{Enabled: &disabled}, deps)
		require.Nil(t, b)
	})
}

// TestResolve_DefaultsToLocal verifies that a nil config, or a config with no
// backend/remote set, resolves to a *LocalBackend (matching MemoryConfig's
// documented default).
func TestResolve_DefaultsToLocal(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	deps := Deps{DB: conn, DataDirectory: t.TempDir(), WorkingDir: t.TempDir()}

	t.Run("nil config", func(t *testing.T) {
		t.Parallel()
		b := Resolve(nil, deps)
		require.NotNil(t, b)
		require.Equal(t, "local", b.ID())
		_, ok := b.(*LocalBackend)
		require.True(t, ok, "expected *LocalBackend, got %T", b)
		require.True(t, b.Capabilities().SessionWorkingMemory)
		t.Cleanup(func() { _ = b.Close() })
	})

	t.Run("empty config", func(t *testing.T) {
		t.Parallel()
		b := Resolve(&config.MemoryConfig{}, deps)
		require.NotNil(t, b)
		require.Equal(t, "local", b.ID())
		t.Cleanup(func() { _ = b.Close() })
	})

	t.Run("explicit local backend", func(t *testing.T) {
		t.Parallel()
		b := Resolve(&config.MemoryConfig{Backend: "local"}, deps)
		require.NotNil(t, b)
		require.Equal(t, "local", b.ID())
		t.Cleanup(func() { _ = b.Close() })
	})
}

// TestResolve_HindsightBackend verifies that an explicit "hindsight" backend
// (or a bare Remote URL with no explicit backend) resolves to a
// *HindsightBackend. A missing memory.remote degrades the backend rather than
// resolving to nil or panicking, so operators see a clear degraded-mode
// reason instead of silent failure.
func TestResolve_HindsightBackend(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	deps := Deps{DB: conn, DataDirectory: t.TempDir(), WorkingDir: t.TempDir()}

	t.Run("explicit backend, no remote configured", func(t *testing.T) {
		t.Parallel()
		b := Resolve(&config.MemoryConfig{Backend: "hindsight"}, deps)
		require.NotNil(t, b)
		require.Equal(t, "hindsight", b.ID())
		_, ok := b.(*HindsightBackend)
		require.True(t, ok, "expected *HindsightBackend, got %T", b)
		require.True(t, b.IsDegraded(), "expected degraded mode without memory.remote")
		require.False(t, b.Capabilities().SessionWorkingMemory)
		require.False(t, b.Capabilities().Triples)
		t.Cleanup(func() { _ = b.Close() })
	})

	t.Run("remote implies hindsight backend", func(t *testing.T) {
		t.Parallel()
		b := Resolve(&config.MemoryConfig{Remote: "http://localhost:8888"}, deps)
		require.NotNil(t, b)
		require.Equal(t, "hindsight", b.ID())
		t.Cleanup(func() { _ = b.Close() })
	})
}
