package tools

import (
	"fmt"

	"github.com/charmbracelet/crush/internal/shell"
)

type jobToolResponseMetadata struct {
	ShellID          string
	Command          string
	Description      string
	Done             bool
	ExitCode         int
	WorkingDirectory string
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
