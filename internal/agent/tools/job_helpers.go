package tools

import (
	"fmt"

	"github.com/charmbracelet/crush/internal/shell"
)

// jobToolResponseMetadata is the internal shared metadata shape produced by
// formatJobToolResponse. The public JobResponseMetadata / JobOutputResponseMetadata
// / JobWaitResponseMetadata types below embed the same fields (with JSON tags)
// so the UI layer can unmarshal tool results per action.
type jobToolResponseMetadata struct {
	ShellID          string
	Command          string
	Description      string
	Done             bool
	ExitCode         int
	WorkingDirectory string
}

// JobOutputParams is the legacy parameter shape for the retired standalone
// job_output tool. Kept because the UI router still unmarshals unified `job`
// tool calls with action=output into this type. The unified JobParams is the
// live shape consumed by the model.
type JobOutputParams struct {
	ShellID string `json:"shell_id" description:"The ID of the background shell to retrieve output from"`
}

// JobOutputResponseMetadata mirrors JobResponseMetadata for action=output
// results; the UI unmarshals tool-result metadata into this shape.
type JobOutputResponseMetadata struct {
	ShellID          string `json:"shell_id"`
	Command          string `json:"command"`
	Description      string `json:"description"`
	Done             bool   `json:"done"`
	ExitCode         int    `json:"exit_code,omitempty"`
	WorkingDirectory string `json:"working_directory"`
}

// JobWaitParams is the legacy parameter shape for the retired standalone
// job_wait tool. Kept for UI unmarshalling of unified `job` action=wait calls.
type JobWaitParams struct {
	ShellID string `json:"shell_id" description:"The ID of the background shell to wait for"`
}

// JobWaitResponseMetadata mirrors JobResponseMetadata for action=wait results.
type JobWaitResponseMetadata struct {
	ShellID          string `json:"shell_id"`
	Command          string `json:"command"`
	Description      string `json:"description"`
	Done             bool   `json:"done"`
	ExitCode         int    `json:"exit_code,omitempty"`
	WorkingDirectory string `json:"working_directory"`
	TimedOut         bool   `json:"timed_out,omitempty"`
}

// JobKillParams is the legacy parameter shape for the retired standalone
// job_kill tool. Kept for UI unmarshalling of unified `job` action=kill calls.
type JobKillParams struct {
	ShellID string `json:"shell_id" description:"The ID of the background shell to terminate"`
}

// JobKillResponseMetadata is the metadata shape for action=kill results.
type JobKillResponseMetadata struct {
	ShellID     string `json:"shell_id"`
	Command     string `json:"command"`
	Description string `json:"description"`
}

func formatJobToolResponse(bgShell *shell.BackgroundShell, shellID string) (string, jobToolResponseMetadata) {
	stdout, stderr, done, execErr := bgShell.GetOutput()
	output := formatJobOutput(stdout, stderr, execErr, done)
	if output == "" {
		output = BashNoOutput
	}

	exitCode := 0
	if done && execErr != nil {
		exitCode = shell.ExitCode(execErr)
	}

	metadata := jobToolResponseMetadata{
		ShellID:          shellID,
		Command:          bgShell.Command,
		Description:      bgShell.Description,
		Done:             done,
		ExitCode:         exitCode,
		WorkingDirectory: bgShell.WorkingDir,
	}
	result := fmt.Sprintf("Status: %s\n\n%s", jobStatusText(done), output)
	return result, metadata
}
