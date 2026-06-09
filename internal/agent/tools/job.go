package tools

import (
	"context"
	_ "embed"
	"fmt"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/shell"
	"github.com/charmbracelet/crush/internal/toolruntime"
)

const JobToolName = "job"

//go:embed job.md
var jobDescription []byte

// JobParams is the unified parameter struct for the consolidated job tool.
type JobParams struct {
	Action  string `json:"action" description:"The job action to perform. One of: output (get current output), wait (block until completion), kill (terminate the job)."`
	ShellID string `json:"shell_id" description:"The ID of the background shell."`
}

// JobResponseMetadata is the unified metadata for job output/wait responses.
type JobResponseMetadata struct {
	ShellID          string `json:"shell_id"`
	Command          string `json:"command"`
	Description      string `json:"description"`
	Done             bool   `json:"done"`
	ExitCode         int    `json:"exit_code,omitempty"`
	WorkingDirectory string `json:"working_directory"`
}

// NewJobTool creates the consolidated job tool that replaces job_output, job_wait, and job_kill.
func NewJobTool() fantasy.AgentTool {
	return fantasy.NewAgentTool(
		JobToolName,
		string(jobDescription),
		func(ctx context.Context, params JobParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.ShellID == "" {
				return fantasy.NewTextErrorResponse("missing shell_id"), nil
			}

			bgManager := shell.GetBackgroundShellManager()
			bgShell, ok := bgManager.Get(params.ShellID)
			if !ok {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("background shell not found: %s", params.ShellID)), nil
			}

			switch params.Action {
			case "output":
				result, meta := formatJobToolResponse(bgShell, params.ShellID)
				metadata := JobResponseMetadata(meta)
				return fantasy.WithResponseMetadata(fantasy.NewTextResponse(result), metadata), nil

			case "wait":
				if !bgShell.WaitContext(ctx) {
					return fantasy.ToolResponse{}, ctx.Err()
				}
				result, meta := formatJobToolResponse(bgShell, params.ShellID)
				metadata := JobResponseMetadata{
					ShellID:          meta.ShellID,
					Command:          meta.Command,
					Description:      meta.Description,
					Done:             meta.Done,
					ExitCode:         meta.ExitCode,
					WorkingDirectory: meta.WorkingDirectory,
				}
				return fantasy.WithResponseMetadata(fantasy.NewTextResponse(result), metadata), nil

			case "kill":
				killMetadata := JobKillResponseMetadata{
					ShellID:     params.ShellID,
					Command:     bgShell.Command,
					Description: bgShell.Description,
				}
				bgShell.KillByUser()
				err := bgManager.Kill(params.ShellID)
				if err != nil {
					return fantasy.NewTextErrorResponse(err.Error()), nil
				}
				if bgShell.SessionID != "" && bgShell.ToolCallID != "" {
					stdout, stderr, _, execErr := bgShell.GetOutput()
					toolruntime.Report(ctx, toolruntime.State{
						SessionID:    bgShell.SessionID,
						ToolCallID:   bgShell.ToolCallID,
						ToolName:     bgShell.ToolName,
						Status:       toolruntime.StatusCanceled,
						SnapshotText: finalShellOutput(stdout, stderr, execErr),
						ClientMetadata: map[string]any{
							"shell_id":   bgShell.ID,
							"background": true,
						},
					})
				}
				result := fmt.Sprintf("Background shell %s terminated successfully", params.ShellID)
				return fantasy.WithResponseMetadata(fantasy.NewTextResponse(result), killMetadata), nil

			default:
				return fantasy.NewTextErrorResponse(fmt.Sprintf("unknown job action: %q. Valid actions: output, wait, kill", params.Action)), nil
			}
		},
	)
}
