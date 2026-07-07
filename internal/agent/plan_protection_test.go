package agent

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/plan"
	"github.com/stretchr/testify/require"
)

func TestPlanCompactionProtectorMatchesPlanRead(t *testing.T) {
	t.Parallel()

	workspaceRoot := t.TempDir()
	sessionID := "sess-plan-protect"
	planPath := plan.PlanFilePath(workspaceRoot, sessionID, "plan")

	meta, err := json.Marshal(tools.ReadResponseMetadata{Path: planPath})
	require.NoError(t, err)

	protect := planCompactionProtector(workspaceRoot, sessionID, planPath)
	require.True(t, protect(message.ToolResult{
		Name:     tools.ReadToolName,
		Metadata: string(meta),
	}))
	require.False(t, protect(message.ToolResult{
		Name:     tools.ReadToolName,
		Metadata: string(mustJSON(t, tools.ReadResponseMetadata{Path: filepath.Join(workspaceRoot, "main.go")})),
	}))
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	return string(data)
}
