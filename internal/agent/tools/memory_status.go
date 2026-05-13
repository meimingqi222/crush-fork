package tools

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/memory/engine"
)

//go:embed memory_status.md
var memoryStatusDescription []byte

const MemoryStatusToolName = "memory_status"

type MemoryStatusParams struct {
	ViewName string `json:"view_name,omitempty" description:"Optional specific view name to inspect (e.g. memory_summary, MEMORY, skills)"`
}

// EngineStatusProvider is satisfied by *engine.Engine.
type EngineStatusProvider interface {
	Status(ctx context.Context) (*engine.EngineStatus, error)
}

func NewMemoryStatusTool(memoryEngine EngineStatusProvider) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		MemoryStatusToolName,
		string(memoryStatusDescription),
		func(ctx context.Context, params MemoryStatusParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if memoryEngine == nil {
				return fantasy.NewTextErrorResponse("Memory engine is not configured."), nil
			}

			status, err := memoryEngine.Status(ctx)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to get memory status: %s", err.Error())), nil
			}

			formatted := formatEngineStatus(status, params.ViewName)
			return fantasy.NewTextResponse(formatted), nil
		},
	)
}

func formatEngineStatus(status *engine.EngineStatus, viewFilter string) string {
	var b strings.Builder

	b.WriteString("Memory Engine Status\n")
	b.WriteString(strings.Repeat("=", 50))
	b.WriteString("\n\n")

	b.WriteString(fmt.Sprintf("Event Store: %s\n", status.EventStoreStatus))

	if status.DegradedMode != nil && status.DegradedMode.Active {
		b.WriteString(fmt.Sprintf("Degraded Mode: YES (%s)\n", status.DegradedMode.Reason))
	}
	b.WriteString("\n")

	b.WriteString("Pipeline\n")
	b.WriteString("--------\n")
	b.WriteString(formatPipelineStage("Extraction", status.ExtractionStatus))
	b.WriteString(formatPipelineStage("Consolidation", status.ConsolidationStatus))
	b.WriteString("\n")

	b.WriteString("Materialized Views\n")
	b.WriteString("------------------\n")
	if len(status.MaterializationViews) == 0 {
		b.WriteString("(none)\n")
	} else {
		for _, v := range status.MaterializationViews {
			if viewFilter != "" && v.ViewName != viewFilter {
				continue
			}
			b.WriteString(formatViewStatus(v))
			b.WriteString("\n")
		}
	}

	if len(status.Jobs) > 0 {
		b.WriteString("\nBackground Jobs\n")
		b.WriteString("---------------\n")
		for _, j := range status.Jobs {
			b.WriteString(formatJobStatus(j))
		}
	}

	return strings.TrimSpace(b.String())
}

func formatPipelineStage(name string, s engine.MemoryPipelineStatus) string {
	line := fmt.Sprintf("  %s: %s", name, s.State)
	if s.LastRunAt != nil {
		line += fmt.Sprintf(" (last run: %s)", s.LastRunAt.Format(time.RFC3339))
	}
	if s.LastWatermark > 0 {
		line += fmt.Sprintf(" (watermark: %d)", s.LastWatermark)
	}
	if s.Error != "" {
		line += fmt.Sprintf(" [error: %s]", s.Error)
	}
	return line + "\n"
}

func formatViewStatus(v engine.MaterializedViewStatus) string {
	line := fmt.Sprintf("  %s: %s (watermark: %d, schema v%d)", v.ViewName, v.State, v.Watermark, v.SchemaVersion)
	if v.LastUpdatedAt != nil {
		line += fmt.Sprintf(", updated: %s", v.LastUpdatedAt.Format(time.RFC3339))
	}
	return line
}

func formatJobStatus(j engine.MemoryJobStatus) string {
	line := fmt.Sprintf("  %s [%s]: %s", j.ID, j.JobType, j.Status)
	if j.ErrorMessage != "" {
		line += fmt.Sprintf(" - error: %s", j.ErrorMessage)
	}
	if j.RetryCount > 0 {
		line += fmt.Sprintf(" (retry %d/%d)", j.RetryCount, j.MaxRetries)
	}
	return line + "\n"
}
