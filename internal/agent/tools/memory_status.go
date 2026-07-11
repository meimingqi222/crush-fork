package tools

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/memory"
)

//go:embed memory_status.md
var memoryStatusDescription []byte

const MemoryStatusToolName = "memory_status"

type MemoryStatusParams struct{}

// BackendStatusProvider is satisfied by memory.Backend. The LLM-facing
// memory_status tool only needs the one-line backend summary; deeper
// pipeline diagnostics (per-view watermarks, job leases) are a human-facing
// concern exposed through the Commands panel instead (see
// internal/ui/dialog for the Memory: Status command).
type BackendStatusProvider interface {
	Status(ctx context.Context) (*memory.Status, error)
}

func NewMemoryStatusTool(backend BackendStatusProvider) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		MemoryStatusToolName,
		string(memoryStatusDescription),
		func(ctx context.Context, params MemoryStatusParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if backend == nil {
				return fantasy.NewTextErrorResponse("Memory backend is not configured."), nil
			}

			status, err := backend.Status(ctx)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to get memory status: %s", err.Error())), nil
			}

			return fantasy.NewTextResponse(formatBackendStatus(status)), nil
		},
	)
}

// formatBackendStatus renders a one-line human-readable summary of the
// backend status: which backend is active, whether it is enabled/degraded,
// and rough activity counters.
func formatBackendStatus(s *memory.Status) string {
	if s == nil {
		return "Memory backend status is unavailable."
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
