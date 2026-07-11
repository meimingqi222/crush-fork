package model

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/memory"
	"github.com/charmbracelet/crush/internal/memory/engine"
	"github.com/charmbracelet/crush/internal/ui/util"
)

// memoryOpTimeout bounds the background/network-adjacent memory operations
// (consolidation, clear) triggered from the Commands panel so a stuck
// backend cannot hang the UI indefinitely.
const memoryOpTimeout = 60 * time.Second

// memoryStatusCmd renders a one-line summary of the memory backend status
// for the "Memory: Status" command (docs/refactor-memory.md Phase 4).
func (m *UI) memoryStatusCmd() tea.Cmd {
	backend := m.memoryBackend()
	if backend == nil {
		return util.ReportWarn("Memory backend is not configured.")
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), memoryOpTimeout)
		defer cancel()
		status, err := backend.Status(ctx)
		if err != nil {
			return util.NewErrorMsg(err)
		}
		return util.InfoMsg{
			Type:       util.InfoTypeInfo,
			Msg:        formatMemoryStatus(status),
			Persistent: true,
		}
	}
}

// memorySearchCmd runs a read-only Retriever.Retrieve query for the
// "Memory: Search" command.
func (m *UI) memorySearchCmd(query string) tea.Cmd {
	backend := m.memoryBackend()
	if backend == nil || backend.Retriever() == nil {
		return util.ReportWarn("Memory retriever is not available.")
	}
	retriever := backend.Retriever()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), memoryOpTimeout)
		defer cancel()
		events, err := retriever.Retrieve(ctx, query, map[string]any{"limit": 8})
		if err != nil {
			return util.NewErrorMsg(err)
		}
		if len(events) == 0 {
			return util.NewInfoMsg(fmt.Sprintf("No matching memories found for %q.", query))
		}
		return util.InfoMsg{
			Type:       util.InfoTypeInfo,
			Msg:        formatMemorySearchResults(query, events),
			Persistent: true,
		}
	}
}

// memoryConsolidateCmd triggers an on-demand consolidation + materialization
// pass for the "Memory: Consolidate Now" command.
func (m *UI) memoryConsolidateCmd() tea.Cmd {
	backend := m.memoryBackend()
	if backend == nil {
		return util.ReportWarn("Memory backend is not configured.")
	}
	if backend.Capabilities().RemoteConsolidation {
		// Consolidation/materialization are managed remotely (hindsight);
		// calling TriggerConsolidation/TriggerMaterialization locally is a
		// no-op, so reporting "triggered" would be misleading.
		return util.ReportInfo("Consolidation is managed by the remote memory service; there is no local pass to trigger.")
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), memoryOpTimeout)
		defer cancel()
		if err := backend.TriggerConsolidation(ctx); err != nil {
			return util.NewErrorMsg(err)
		}
		if err := backend.TriggerMaterialization(ctx); err != nil {
			return util.NewErrorMsg(err)
		}
		return util.NewInfoMsg("Memory consolidation triggered.")
	}
}

// memoryClearCmd clears all memory state for the "Memory: Clear" command,
// after the user has confirmed via the MemoryClear dialog. For the hindsight
// backend this only clears the local cache -- the remote bank is untouched
// and must be managed separately.
func (m *UI) memoryClearCmd() tea.Cmd {
	backend := m.memoryBackend()
	if backend == nil {
		return util.ReportWarn("Memory backend is not configured.")
	}
	remoteNote := backend.ID() == "hindsight"
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), memoryOpTimeout)
		defer cancel()
		if err := backend.Clear(ctx); err != nil {
			return util.NewErrorMsg(err)
		}
		if remoteNote {
			return util.NewInfoMsg("Local memory cache cleared. The remote hindsight bank is untouched and must be managed separately.")
		}
		return util.NewInfoMsg("Memory cleared.")
	}
}

// memoryBackend returns the active memory backend, or nil if none is
// configured.
func (m *UI) memoryBackend() memory.Backend {
	if m.com == nil || m.com.App == nil {
		return nil
	}
	return m.com.App.MemoryBackend
}

// formatMemoryStatus renders a one-line summary of the backend status.
func formatMemoryStatus(s *memory.Status) string {
	if s == nil {
		return "Memory status is unavailable."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "backend=%s enabled=%t", s.Backend, s.Enabled)
	if s.Degraded {
		fmt.Fprintf(&b, " degraded=true reason=%q", s.DegradedReason)
	}
	if s.EventCount > 0 {
		fmt.Fprintf(&b, " events=%d", s.EventCount)
	}
	if s.LastConsolidation > 0 {
		fmt.Fprintf(&b, " last_consolidation=%s", time.Unix(s.LastConsolidation, 0).Format(time.RFC3339))
	}
	return b.String()
}

// formatMemorySearchResults renders retrieved events as a read-only,
// human-scannable list.
func formatMemorySearchResults(query string, events []engine.MemoryEvent) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Memory search for %q (%d result(s)):\n", query, len(events))
	for _, e := range events {
		fmt.Fprintf(&b, "- [%s/%s] %s\n", e.Scope, e.Kind, firstNonEmpty(e.Summary, e.Content))
	}
	return strings.TrimRight(b.String(), "\n")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
