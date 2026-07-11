package model

import (
	"testing"

	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/memory"
	"github.com/charmbracelet/crush/internal/memory/engine"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/util"
	"github.com/stretchr/testify/require"
)

func TestFormatMemoryStatus(t *testing.T) {
	t.Parallel()

	require.Equal(t, "Memory status is unavailable.", formatMemoryStatus(nil))

	s := &memory.Status{Backend: "local", Enabled: true, EventCount: 5}
	got := formatMemoryStatus(s)
	require.Contains(t, got, "backend=local")
	require.Contains(t, got, "enabled=true")
	require.Contains(t, got, "events=5")

	degraded := &memory.Status{Backend: "hindsight", Enabled: true, Degraded: true, DegradedReason: "missing remote"}
	gotDegraded := formatMemoryStatus(degraded)
	require.Contains(t, gotDegraded, "degraded=true")
	require.Contains(t, gotDegraded, "missing remote")
}

func TestFormatMemorySearchResults(t *testing.T) {
	t.Parallel()

	events := []engine.MemoryEvent{
		{Scope: engine.MemoryScopeProject, Kind: engine.MemoryKindDecision, Summary: "Use SQLite", Content: "full content"},
		{Scope: engine.MemoryScopeUser, Kind: engine.MemoryKindPreference, Content: "prefers dark mode"},
	}
	got := formatMemorySearchResults("sqlite", events)
	require.Contains(t, got, `"sqlite"`)
	require.Contains(t, got, "2 result")
	require.Contains(t, got, "[project/decision] Use SQLite")
	require.Contains(t, got, "[user/preference] prefers dark mode")
}

// TestMemoryConsolidateCmd_HindsightReportsRemoteManagement is a regression
// test for review finding A6: the hindsight backend has no local
// consolidator/materializer wired in (SetMemoryBackend only wires those for
// LocalBackend), so calling Backend.TriggerConsolidation/
// TriggerMaterialization on it is a silent no-op. The "Memory: Consolidate
// Now" command used to report success regardless, which is misleading for
// the hindsight backend. It should now short-circuit with an informational
// message instead of claiming a pass ran.
func TestMemoryConsolidateCmd_HindsightReportsRemoteManagement(t *testing.T) {
	t.Parallel()

	eng := engine.New(nil, engine.Config{Enabled: true, Backend: "hindsight"})
	backend := memory.NewHindsightBackend(eng, nil, nil)
	require.True(t, backend.Capabilities().RemoteConsolidation)

	ui := &UI{com: &common.Common{App: &app.App{MemoryBackend: backend}}}
	cmd := ui.memoryConsolidateCmd()
	require.NotNil(t, cmd)

	msg := cmd()
	info, ok := msg.(util.InfoMsg)
	require.True(t, ok, "expected util.InfoMsg, got %T", msg)
	require.Contains(t, info.Msg, "remote memory service")
}
