package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/plan"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestResolveLocalPlanURIUsesSessionWorkingDir(t *testing.T) {
	t.Parallel()

	fallbackDir := t.TempDir()
	sessionDir := t.TempDir()
	ctx := context.WithValue(t.Context(), WorkingDirContextKey, sessionDir)
	ctx = context.WithValue(ctx, SessionIDContextKey, "session-1")

	resolved, err := resolveLocalPlanURI(ctx, "local://auth-refactor-plan.md", fallbackDir)
	require.NoError(t, err)
	require.Equal(t, plan.PlanFilePath(sessionDir, "session-1", "auth-refactor"), resolved)
	require.DirExists(t, filepath.Dir(resolved))
}

func TestResolveLocalPlanURIReturnsNonURIInputUnchanged(t *testing.T) {
	t.Parallel()

	input := "src/plan.md"
	resolved, err := resolveLocalPlanURI(t.Context(), input, t.TempDir())
	require.NoError(t, err)
	require.Equal(t, input, resolved)
}

func TestResolveToolUsesDisplayPathForPlanFile(t *testing.T) {
	t.Parallel()

	workspaceRoot := t.TempDir()
	planPath := plan.PlanFilePath(workspaceRoot, "session-1", "auth-refactor")
	require.NoError(t, plan.EnsureDir(workspaceRoot))
	require.NoError(t, os.WriteFile(planPath, []byte("# Auth refactor\n\n- [ ] Update auth\n"), 0o644))

	sessions := &planGuardSessionService{sess: session.Session{
		ID:                "session-1",
		WorkspaceCWD:      workspaceRoot,
		CollaborationMode: session.CollaborationModePlan,
		PlanFilePath:      planPath,
	}}
	tool := NewResolveTool(sessions)
	input, err := json.Marshal(ResolveParams{Action: "apply", Extra: ResolveExtra{Title: "auth-refactor"}})
	require.NoError(t, err)

	response, err := tool.Run(context.WithValue(t.Context(), SessionIDContextKey, "session-1"), fantasy.ToolCall{
		ID:    "resolve-1",
		Name:  ResolveToolName,
		Input: string(input),
	})
	require.NoError(t, err)
	require.False(t, response.IsError)
	require.Contains(t, response.Content, ".crush/plans/session-1-auth-refactor.md")

	var metadata ResolveMetadata
	require.NoError(t, json.Unmarshal([]byte(response.Metadata), &metadata))
	require.Equal(t, workspaceRoot, metadata.WorkingDirectory)
	require.Equal(t, planPath, metadata.ResolvedPath)
	require.Equal(t, ".crush/plans/session-1-auth-refactor.md", metadata.DisplayPath)
	require.Equal(t, metadata.DisplayPath, metadata.PlanFilePath)
}
