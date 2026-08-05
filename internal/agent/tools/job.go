package tools

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/shell"
	"github.com/charmbracelet/crush/internal/toolruntime"
)

const JobToolName = "job"

// Defaults and bounds for the wait action's bounded poll window. The wait
// never blocks forever: if the job does not finish within the window, the
// tool returns the current snapshot (Done=false, TimedOut=true) so the agent
// can decide whether to wait again, check output, or kill the job. This
// mirrors oh-my-pi's bounded-poll design and avoids the agent deadlocking on
// a stuck job (e.g. tail -f, a server waiting on stdin).
const (
	defaultJobWaitTimeout = 5 * time.Minute
	minJobWaitTimeout     = 1 * time.Second
	maxJobWaitTimeout     = 30 * time.Minute
)

// resolveJobWaitTimeout maps the optional wait_timeout_ms parameter to a
// clamped duration. Non-positive values fall back to the default.
func resolveJobWaitTimeout(ms int) time.Duration {
	if ms <= 0 {
		return defaultJobWaitTimeout
	}
	d := time.Duration(ms) * time.Millisecond
	if d < minJobWaitTimeout {
		return minJobWaitTimeout
	}
	if d > maxJobWaitTimeout {
		return maxJobWaitTimeout
	}
	return d
}

//go:embed job.md
var jobDescription []byte

// JobParams is the unified parameter struct for the consolidated job tool.
type JobParams struct {
	Action        string `json:"action" description:"The job action to perform. One of: output (get current output), wait (block until completion or timeout), kill (terminate the job)."`
	ShellID       string `json:"shell_id" description:"The ID of the background shell."`
	WaitTimeoutMs int    `json:"wait_timeout_ms,omitempty" description:"Optional, action=wait only. Max milliseconds to wait before returning the current snapshot. Default 300000 (5m), clamped to [1000, 1800000]. On timeout the job keeps running, Done=false, TimedOut=true. Use a shorter value to poll; a longer one to block longer."`
}

// JobResponseMetadata is the unified metadata for job output/wait responses.
type JobResponseMetadata struct {
	ToolPathMetadata
	ShellID          string `json:"shell_id"`
	Command          string `json:"command"`
	Description      string `json:"description"`
	Done             bool   `json:"done"`
	ExitCode         int    `json:"exit_code,omitempty"`
	WorkingDirectory string `json:"working_directory"`
	TimedOut         bool   `json:"timed_out,omitempty"`
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
				metadata := JobResponseMetadata{
					ToolPathMetadata: NewCommandToolPathMetadata(EffectiveWorkingDir(ctx, ""), meta.WorkingDirectory, meta.WorkingDirectory),
					ShellID:          meta.ShellID,
					Command:          meta.Command,
					Description:      meta.Description,
					Done:             meta.Done,
					ExitCode:         meta.ExitCode,
					WorkingDirectory: meta.WorkingDirectory,
				}
				return fantasy.WithResponseMetadata(fantasy.NewTextResponse(result), metadata), nil

			case "wait":
				// Bounded wait: never block forever. If the parent ctx is already
				// cancelled (agent run aborted), propagate immediately. Otherwise
				// derive a timeout ctx and return the current snapshot on deadline.
				if err := ctx.Err(); err != nil {
					return fantasy.ToolResponse{}, err
				}
				waitTimeout := resolveJobWaitTimeout(params.WaitTimeoutMs)
				waitCtx, cancel := context.WithTimeout(ctx, waitTimeout)
				done := bgShell.WaitContext(waitCtx)
				cancel()

				// Parent ctx cancelled (agent run aborted, Esc, etc.) — propagate.
				if err := ctx.Err(); err != nil {
					return fantasy.ToolResponse{}, err
				}

				result, meta := formatJobToolResponse(bgShell, params.ShellID)
				timedOut := !done
				// Race guard: if WaitContext returned false because the deadline
				// fired but the job actually completed in the window between the
				// deadline and GetOutput, treat it as completed (Done=true,
				// TimedOut=false) instead of reporting a contradictory
				// "timed out but done" state.
				if timedOut && meta.Done {
					timedOut = false
				}
				if timedOut {
					result = fmt.Sprintf(
						"Timed out waiting for background shell %s after %s. The job is still running — call job again with action=wait to keep waiting, action=output to peek at progress, or action=kill to stop it.\n\n%s",
						params.ShellID, waitTimeout, result,
					)
				}
				metadata := JobResponseMetadata{
					ToolPathMetadata: NewCommandToolPathMetadata(EffectiveWorkingDir(ctx, ""), meta.WorkingDirectory, meta.WorkingDirectory),
					ShellID:          meta.ShellID,
					Command:          meta.Command,
					Description:      meta.Description,
					Done:             meta.Done,
					ExitCode:         meta.ExitCode,
					WorkingDirectory: meta.WorkingDirectory,
					TimedOut:         timedOut,
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
