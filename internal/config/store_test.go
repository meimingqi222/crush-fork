package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestMCPStartupGracePeriod_Defaults verifies the default grace period is
// returned when no server explicitly configures a startup grace period.
func TestMCPStartupGracePeriod_Defaults(t *testing.T) {
	t.Parallel()

	t.Run("nil store returns default", func(t *testing.T) {
		t.Parallel()
		var s *ConfigStore
		require.Equal(t, 2*time.Second, s.MCPStartupGracePeriod())
	})

	t.Run("nil config returns default", func(t *testing.T) {
		t.Parallel()
		s := &ConfigStore{}
		require.Equal(t, 2*time.Second, s.MCPStartupGracePeriod())
	})

	t.Run("nil MCP map returns default", func(t *testing.T) {
		t.Parallel()
		s := &ConfigStore{config: &Config{}}
		require.Equal(t, 2*time.Second, s.MCPStartupGracePeriod())
	})

	t.Run("empty MCP map returns default", func(t *testing.T) {
		t.Parallel()
		s := &ConfigStore{config: &Config{MCP: map[string]MCPConfig{}}}
		require.Equal(t, 2*time.Second, s.MCPStartupGracePeriod())
	})
}

// TestMCPStartupGracePeriod_AllDisabledReturnsDefault verifies that disabled
// servers are skipped entirely; their configured grace periods do not apply.
func TestMCPStartupGracePeriod_AllDisabledReturnsDefault(t *testing.T) {
	t.Parallel()

	s := &ConfigStore{config: &Config{MCP: map[string]MCPConfig{
		"slow":  {Disabled: true, StartupGracePeriodMs: 10000},
		"slow2": {Disabled: true, StartupGracePeriodMs: 20000},
	}}}
	require.Equal(t, 2*time.Second, s.MCPStartupGracePeriod())
}

// TestMCPStartupGracePeriod_UsesMaxConfigured verifies that when at least one
// non-disabled server configures a grace period, the effective value is the
// maximum across all non-disabled servers with an explicit value.
func TestMCPStartupGracePeriod_UsesMaxConfigured(t *testing.T) {
	t.Parallel()

	s := &ConfigStore{config: &Config{MCP: map[string]MCPConfig{
		"fast":  {StartupGracePeriodMs: 100},
		"slow":  {StartupGracePeriodMs: 5000},
		"unset": {},
	}}}
	require.Equal(t, 5*time.Second, s.MCPStartupGracePeriod())
}

// TestMCPStartupGracePeriod_SkipsDisabledServers verifies that a disabled
// server's large grace period does not inflate the effective value.
func TestMCPStartupGracePeriod_SkipsDisabledServers(t *testing.T) {
	t.Parallel()

	s := &ConfigStore{config: &Config{MCP: map[string]MCPConfig{
		"active": {StartupGracePeriodMs: 800},
		"down":   {Disabled: true, StartupGracePeriodMs: 60000},
	}}}
	require.Equal(t, 800*time.Millisecond, s.MCPStartupGracePeriod())
}

// TestMCPStartupGracePeriod_ZeroOrNegativeIgnored verifies that non-positive
// values are treated as unset, falling back to the default when no server has
// a positive explicit value.
func TestMCPStartupGracePeriod_ZeroOrNegativeIgnored(t *testing.T) {
	t.Parallel()

	s := &ConfigStore{config: &Config{MCP: map[string]MCPConfig{
		"zero":     {StartupGracePeriodMs: 0},
		"negative": {StartupGracePeriodMs: -5},
	}}}
	require.Equal(t, 2*time.Second, s.MCPStartupGracePeriod())
}

// TestMCPStartupGracePeriod_MixedDisabledAndExplicit verifies that disabled
// servers are ignored while non-disabled explicit values are considered.
func TestMCPStartupGracePeriod_MixedDisabledAndExplicit(t *testing.T) {
	t.Parallel()

	s := &ConfigStore{config: &Config{MCP: map[string]MCPConfig{
		"disabled-explicit": {Disabled: true, StartupGracePeriodMs: 99999},
		"explicit-a":        {StartupGracePeriodMs: 300},
		"explicit-b":        {StartupGracePeriodMs: 1500},
		"unset":             {},
	}}}
	require.Equal(t, 1500*time.Millisecond, s.MCPStartupGracePeriod())
}
