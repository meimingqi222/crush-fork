package tools

import (
	"bytes"
	"cmp"
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/filepathext"
	"github.com/charmbracelet/crush/internal/hooks"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/shell"
	"github.com/charmbracelet/crush/internal/toolruntime"
)

type BashParams struct {
	Description         string `json:"description,omitempty" description:"A brief description of what the command does, try to keep it under 30 characters or so"`
	Command             string `json:"command" description:"The command to execute"`
	WorkingDir          string `json:"working_dir,omitempty" description:"The working directory to execute the command in. Relative paths resolve from the current session working directory; defaults to the current session working directory."`
	RunInBackground     bool   `json:"run_in_background,omitempty" description:"Set to true to run this command in the background. Use the job tool with action=output for snapshots, action=wait to block until it finishes, and action=kill to terminate it."`
	TimeoutSeconds      *int   `json:"timeout_seconds,omitempty" description:"Maximum time to allow the command to run in the foreground before it is terminated. Defaults to 120 seconds. Set to 0 to disable timeout. Maximum allowed value is 600 seconds."`
	AutoBackgroundAfter int    `json:"auto_background_after,omitempty" description:"Deprecated compatibility field. If provided and timeout_seconds is omitted, its value is interpreted as timeout_seconds."`
}

type BashPermissionsParams struct {
	Description         string `json:"description"`
	Command             string `json:"command"`
	WorkingDir          string `json:"working_dir"`
	RunInBackground     bool   `json:"run_in_background"`
	TimeoutSeconds      *int   `json:"timeout_seconds"`
	AutoBackgroundAfter int    `json:"auto_background_after"`
}

type BashResponseMetadata struct {
	StartTime        int64    `json:"start_time"`
	EndTime          int64    `json:"end_time"`
	Output           string   `json:"output"`
	Description      string   `json:"description"`
	WorkingDirectory string   `json:"working_directory"`
	SafeReadOnly     bool     `json:"safe_read_only,omitempty"`
	Background       bool     `json:"background,omitempty"`
	ShellID          string   `json:"shell_id,omitempty"`
	TimedOut         bool     `json:"timed_out,omitempty"`
	TimeoutSeconds   int      `json:"timeout_seconds,omitempty"`
	DeprecationNotes []string `json:"deprecation_notes,omitempty"`
}

const (
	BashToolName    = "bash"
	MaxOutputLength = 30000
	BashNoOutput    = "no output"
)

// bashTimeoutUnit is overridden in tests to avoid waiting real seconds.
var bashTimeoutUnit = time.Second

//go:embed bash.tpl
var bashDescriptionTmpl []byte

var bashDescriptionTpl = template.Must(
	template.New("bashDescription").
		Parse(string(bashDescriptionTmpl)),
)

type bashDescriptionData struct {
	BannedCommands  string
	MaxOutputLength int
	Attribution     config.Attribution
	ModelName       string
}

func bashDescription(attribution *config.Attribution, modelName string) string {
	var out bytes.Buffer
	if err := bashDescriptionTpl.Execute(&out, bashDescriptionData{
		BannedCommands:  "",
		MaxOutputLength: MaxOutputLength,
		Attribution:     *attribution,
		ModelName:       modelName,
	}); err != nil {
		panic("failed to execute bash description template: " + err.Error())
	}
	return out.String()
}

// Pattern-based security check function similar to claude-code's approach.
// In claude-code, security checks return allow/ask/deny. Since shell.BlockFunc
// only supports block/allow, we only block patterns that should ALWAYS be denied
// (control characters, Zsh dangerous commands). Patterns like $() and ${} that
// claude-code maps to "ask" are handled by the permission system instead.
func patternBlockFunc(args []string) bool {
	if len(args) == 0 {
		return false
	}

	command := strings.Join(args, " ")

	// Check for Zsh-specific dangerous commands (from claude-code).
	// These provide capabilities like loading kernel modules, raw file I/O,
	// network access, and pseudo-terminal execution that circumvent normal
	// permission checks.
	zshDangerousCommands := []string{
		"zmodload", "emulate", "sysopen", "sysread", "syswrite",
		"sysseek", "zpty", "ztcp", "zsocket", "zf_rm", "zf_mv",
		"zf_ln", "zf_chmod", "zf_chown", "zf_mkdir", "zf_rmdir", "zf_chgrp",
	}

	baseCommand := strings.ToLower(filepath.Base(args[0]))
	for _, cmd := range zshDangerousCommands {
		if baseCommand == cmd {
			return true
		}
	}

	// Check for control characters (non-printable).
	// Null bytes and other non-printable chars are silently dropped by bash
	// but confuse our validators, allowing metacharacters to slip through.
	for _, r := range command {
		if r < 32 && r != '\t' && r != '\n' && r != '\r' {
			return true
		}
		if r == 127 {
			return true
		}
	}

	return false
}

func blockFuncs() []shell.BlockFunc {
	return []shell.BlockFunc{
		patternBlockFunc,
	}
}

func NewBashTool(permissions permission.Service, workingDir string, attribution *config.Attribution, modelName string, hookMgr *hooks.Manager, opts ...BashToolOptions) fantasy.AgentTool {
	return NewBashToolWithSessions(nil, permissions, workingDir, attribution, modelName, hookMgr, opts...)
}

func NewBashToolWithSessions(sessions session.Service, permissions permission.Service, workingDir string, attribution *config.Attribution, modelName string, hookMgr *hooks.Manager, opts ...BashToolOptions) fantasy.AgentTool {
	var toolOpts BashToolOptions
	if len(opts) > 0 {
		toolOpts = opts[0]
	}

	description := bashDescription(attribution, modelName)
	if toolOpts.DescriptionOverride != "" {
		description = toolOpts.DescriptionOverride
	}

	execBlockFuncs := blockFuncs()
	if toolOpts.RestrictedToGitReadOnly {
		execBlockFuncs = append(execBlockFuncs, restrictedGitBlockFunc())
	}

	return fantasy.NewAgentTool(
		BashToolName,
		string(description),
		func(ctx context.Context, params BashParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Command == "" {
				return fantasy.NewTextErrorResponse("missing command"), nil
			}

			if params.Description == "" {
				runes := []rune(params.Command)
				if len(runes) > 30 {
					runes = runes[:30]
				}
				params.Description = string(runes)
			}

			// Expand skill:// URLs in the command to filesystem paths.
			if toolOpts.SkillList != nil {
				params.Command = ExpandSkillURLs(params.Command, toolOpts.SkillList)
				if IsSkillURL(params.WorkingDir) {
					resolvedWd, err := ResolveSkillURL(params.WorkingDir, toolOpts.SkillList)
					if err != nil {
						return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to resolve skill:// URL in working_dir: %s", err)), nil
					}
					params.WorkingDir = resolvedWd
				}
			}

			sessionWorkingDir := cmp.Or(GetWorkingDirFromContext(ctx), workingDir)
			execWorkingDir := resolveBashWorkingDir(sessionWorkingDir, params.WorkingDir)
			fallbackCommand := ""
			sessionID := GetSessionFromContext(ctx)

			// Determine block funcs based on permission mode
			runningBlockFuncs := execBlockFuncs
			if sessions != nil && sessionID != "" {
				if sess, err := sessions.Get(ctx, sessionID); err == nil && sess.PermissionMode == session.PermissionModeYolo {
					// In yolo mode, don't block any commands
					runningBlockFuncs = nil
				}
			}

			if hookMgr != nil {
				hookResult, hookErr := hookMgr.RunPreToolUse(ctx, BashToolName, map[string]any{
					"command":     params.Command,
					"working_dir": execWorkingDir,
				}, sessionID)
				if hookErr == nil && hookResult != nil {
					switch hookResult.Decision {
					case hooks.DecisionDeny:
						reason := hookResult.Reason
						if reason == "" {
							reason = "command blocked by hook"
						}
						return fantasy.NewTextErrorResponse(reason), nil
					case hooks.DecisionModify:
						if cmd, ok := hookResult.ModifiedInput["command"].(string); ok && cmd != "" {
							params.Command = cmd
						}
						if wd, ok := hookResult.ModifiedInput["working_dir"].(string); ok && wd != "" {
							execWorkingDir = resolveBashWorkingDir(sessionWorkingDir, wd)
						}
						if hookResult.FallbackOnError {
							if cmd, ok := hookResult.FallbackInput["command"].(string); ok && cmd != "" && cmd != params.Command {
								fallbackCommand = cmd
							}
						}
					}
				}
			}

			if toolOpts.RestrictedToGitReadOnly {
				if err := validateRestrictedGitCommand(params.Command); err != nil {
					return fantasy.NewTextErrorResponse(err.Error()), nil
				}
				if fallbackCommand != "" {
					if err := validateRestrictedGitCommand(fallbackCommand); err != nil {
						return fantasy.NewTextErrorResponse(err.Error()), nil
					}
				}
			}
			if err := validateNoWrapperShellCommand(params.Command); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if fallbackCommand != "" {
				if err := validateNoWrapperShellCommand(fallbackCommand); err != nil {
					return fantasy.NewTextErrorResponse(err.Error()), nil
				}
			}

			if toolOpts.DisableBackground && params.RunInBackground {
				return fantasy.NewTextErrorResponse("background execution is disabled for this bash tool"), nil
			}

			isSafeReadOnly := toolOpts.RestrictedToGitReadOnly
			if isSafeReadOnly && containsCommandChaining(params.Command) {
				isSafeReadOnly = false
			}

			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for executing shell command")
			}

			timeoutSeconds, deprecationNotes := effectiveBashTimeout(params)
			if !isSafeReadOnly {
				if err := requestBashPermission(ctx, permissions, sessionID, call.ID, execWorkingDir, params); err != nil {
					return fantasy.ToolResponse{}, err
				}
			}

			if params.RunInBackground {
				return runBackgroundBash(ctx, call, params, execWorkingDir, runningBlockFuncs, timeoutSeconds, deprecationNotes, isSafeReadOnly)
			}

			startTime := time.Now()
			commandToRun := params.Command
			attemptedFallback := false
			for {
				output, execErr, timedOut, promotedShellID, runErr := runForegroundBashCommand(ctx, call.ID, execWorkingDir, runningBlockFuncs, commandToRun, timeoutSeconds, sessionID, params.Description)
				if runErr != nil {
					return fantasy.ToolResponse{}, runErr
				}

				if shouldRetryOriginalCommand(execErr, commandToRun, fallbackCommand, attemptedFallback) {
					if !toolOpts.RestrictedToGitReadOnly && !attemptedFallback {
						fallbackParams := BashParams{Command: fallbackCommand}
						if err := requestBashPermission(ctx, permissions, sessionID, call.ID, execWorkingDir, fallbackParams); err != nil {
							return fantasy.ToolResponse{}, err
						}
					}
					commandToRun = fallbackCommand
					attemptedFallback = true
					continue
				}

				metadata := BashResponseMetadata{
					StartTime:        startTime.UnixMilli(),
					EndTime:          time.Now().UnixMilli(),
					Output:           output,
					Description:      params.Description,
					WorkingDirectory: execWorkingDir,
					SafeReadOnly:     isSafeReadOnly,
					TimeoutSeconds:   timeoutSeconds,
					TimedOut:         timedOut,
				}
				appendDeprecationNotes(&metadata, deprecationNotes)

				responseText := buildBashResponseText(output, execWorkingDir)
				if timedOut && promotedShellID != "" {
					metadata.Background = true
					metadata.ShellID = promotedShellID
					msg := fmt.Sprintf("Command timed out after %d seconds. The process is still running in the background (shell ID: %s).", timeoutSeconds, promotedShellID)
					if output != "" {
						msg += "\n\nOutput so far:\n" + output
					}
					msg += "\n\nUse the job tool with action=output to check current output, action=wait to wait for completion, or action=kill to terminate it."
					metadata.Output = msg
					responseText = msg
				} else if timedOut {
					timeoutNote := fmt.Sprintf("Command timed out after %d seconds", timeoutSeconds)
					if output == "" {
						metadata.Output = timeoutNote
						responseText = buildBashResponseText(timeoutNote, execWorkingDir)
					} else {
						metadata.Output = output + "\n" + timeoutNote
						responseText = buildBashResponseText(metadata.Output, execWorkingDir)
					}
				}
				if hookMgr != nil {
					hookMgr.RunPostToolUse(ctx, BashToolName, map[string]any{
						"command":     commandToRun,
						"working_dir": execWorkingDir,
					}, metadata.Output, sessionID)
				}
				return fantasy.WithResponseMetadata(fantasy.NewTextResponse(responseText), metadata), nil
			}
		})
}

func resolveBashWorkingDir(sessionWorkingDir, requestedWorkingDir string) string {
	if strings.TrimSpace(requestedWorkingDir) == "" {
		return sessionWorkingDir
	}
	return filepathext.SmartJoin(sessionWorkingDir, requestedWorkingDir)
}

func requestBashPermission(ctx context.Context, permissions permission.Service, sessionID, toolCallID, workingDir string, params BashParams) error {
	p, err := permissions.Request(ctx,
		permission.CreatePermissionRequest{
			SessionID:          sessionID,
			AuthoritySessionID: ResolveAuthoritySessionID(ctx, sessionID),
			Path:               workingDir,
			ToolCallID:         toolCallID,
			ToolName:           BashToolName,
			Action:             "execute",
			Description:        fmt.Sprintf("Execute command: %s", params.Command),
			Params:             BashPermissionsParams(params),
		},
	)
	if err != nil {
		return err
	}
	if !p {
		return permission.ErrorPermissionDenied
	}
	return nil
}

func runBackgroundBash(ctx context.Context, call fantasy.ToolCall, params BashParams, execWorkingDir string, execBlockFuncs []shell.BlockFunc, timeoutSeconds int, deprecationNotes []string, safeReadOnly bool) (fantasy.ToolResponse, error) {
	startTime := time.Now()
	bgManager := shell.GetBackgroundShellManager()
	bgManager.Cleanup()
	bgShell, err := bgManager.StartWithMetadata(context.Background(), execWorkingDir, execBlockFuncs, params.Command, params.Description, GetSessionFromContext(ctx), call.ID, BashToolName)
	if err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("error starting background shell: %w", err)
	}

	publishBashRuntime(ctx, call.ID, toolruntime.StatusBackgroundRunning, "", map[string]any{
		"shell_id":   bgShell.ID,
		"background": true,
	})
	go watchBackgroundShellRuntime(detachedToolRuntimeContext(ctx), bgShell)

	metadata := BashResponseMetadata{
		StartTime:        startTime.UnixMilli(),
		EndTime:          time.Now().UnixMilli(),
		Description:      params.Description,
		WorkingDirectory: bgShell.WorkingDir,
		SafeReadOnly:     safeReadOnly,
		Background:       true,
		ShellID:          bgShell.ID,
		TimeoutSeconds:   timeoutSeconds,
	}
	appendDeprecationNotes(&metadata, deprecationNotes)

	response := fmt.Sprintf("Background shell started with ID: %s\n\nUse the job tool with action=output to view a snapshot, action=wait to wait for completion, or action=kill to terminate it.", bgShell.ID)
	return fantasy.WithResponseMetadata(fantasy.NewTextResponse(response), metadata), nil
}

func runForegroundBashCommand(ctx context.Context, toolCallID string, execWorkingDir string, execBlockFuncs []shell.BlockFunc, command string, timeoutSeconds int, sessionID string, description string) (output string, execErr error, timedOut bool, promotedShellID string, runErr error) {
	bgManager := shell.GetBackgroundShellManager()
	bgManager.Cleanup()
	bgShell, err := bgManager.StartWithMetadata(context.Background(), execWorkingDir, execBlockFuncs, command, description, sessionID, toolCallID, BashToolName)
	if err != nil {
		return "", nil, false, "", fmt.Errorf("error starting foreground shell: %w", err)
	}

	publishBashRuntime(ctx, toolCallID, toolruntime.StatusRunning, "", nil)

	var timer <-chan time.Time
	if timeoutSeconds > 0 {
		t := time.NewTimer(time.Duration(timeoutSeconds) * bashTimeoutUnit)
		defer t.Stop()
		timer = t.C
	}

	ticker := time.NewTicker(bashStreamThrottle)
	defer ticker.Stop()

	lastSnapshot := ""
	for {
		select {
		case <-bgShell.Done():
			stdout, stderr, _, exitErr := bgShell.GetOutput()
			if ctx.Err() != nil {
				_ = bgManager.Remove(bgShell.ID)
				publishBashRuntime(ctx, toolCallID, toolruntime.StatusCanceled, truncateOutput(combinedOutputSnapshot(stdout, stderr)), nil)
				return "", nil, false, "", ctx.Err()
			}
			_ = bgManager.Remove(bgShell.ID)
			out := finalShellOutput(stdout, stderr, exitErr)
			status := toolruntime.StatusCompleted
			switch {
			case shell.IsInterrupt(exitErr):
				status = toolruntime.StatusCanceled
			case exitErr != nil && shell.ExitCode(exitErr) != 0:
				status = toolruntime.StatusFailed
			}
			publishBashRuntime(ctx, toolCallID, status, out, nil)
			return out, exitErr, false, "", nil

		case <-timer:
			stdout, stderr, _, _ := bgShell.GetOutput()
			partialOut := truncateOutput(combinedOutputSnapshot(stdout, stderr))
			publishBashRuntime(ctx, toolCallID, toolruntime.StatusBackgroundRunning, partialOut, map[string]any{
				"shell_id":   bgShell.ID,
				"background": true,
			})
			go watchBackgroundShellRuntime(detachedToolRuntimeContext(ctx), bgShell)
			return partialOut, nil, true, bgShell.ID, nil

		case <-ctx.Done():
			_ = bgManager.Kill(bgShell.ID)
			return "", nil, false, "", ctx.Err()

		case <-ticker.C:
			stdout, stderr, _, _ := bgShell.GetOutput()
			snapshot := truncateOutput(combinedOutputSnapshot(stdout, stderr))
			if snapshot == lastSnapshot {
				continue
			}
			lastSnapshot = snapshot
			publishBashRuntime(ctx, toolCallID, toolruntime.StatusRunning, snapshot, nil)
		}
	}
}

func watchBackgroundShellRuntime(ctx context.Context, bgShell *shell.BackgroundShell) {
	if bgShell == nil {
		return
	}

	ticker := time.NewTicker(bashStreamThrottle)
	defer ticker.Stop()
	lastSnapshot := ""

	for {
		stdout, stderr, done, execErr := bgShell.GetOutput()
		snapshot := truncateOutput(combinedOutputSnapshot(stdout, stderr))
		if snapshot != lastSnapshot {
			lastSnapshot = snapshot
			publishShellRuntime(ctx, bgShell, toolruntime.StatusBackgroundRunning, snapshot)
		}
		if done {
			if bgShell.WasKilled() {
				return
			}
			finalSnapshot := finalShellOutput(stdout, stderr, execErr)
			status := toolruntime.StatusCompleted
			switch {
			case shell.IsInterrupt(execErr):
				status = toolruntime.StatusCanceled
			case execErr != nil && shell.ExitCode(execErr) != 0:
				status = toolruntime.StatusFailed
			}
			publishShellRuntime(ctx, bgShell, status, finalSnapshot)
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

type liveOutputBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *liveOutputBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *liveOutputBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func shouldRetryOriginalCommand(execErr error, attemptedCommand, fallbackCommand string, attemptedFallback bool) bool {
	if attemptedFallback || fallbackCommand == "" || attemptedCommand == "" || attemptedCommand == fallbackCommand {
		return false
	}
	if execErr == nil || shell.IsInterrupt(execErr) {
		return false
	}
	return shell.ExitCode(execErr) != 0
}

func formatOutput(stdout, stderr string, execErr error) string {
	interrupted := shell.IsInterrupt(execErr)
	exitCode := shell.ExitCode(execErr)

	stdout = truncateOutput(stdout)
	stderr = truncateOutput(stderr)

	errorMessage := stderr
	if errorMessage == "" && execErr != nil {
		errorMessage = execErr.Error()
	}

	if interrupted {
		if errorMessage != "" {
			errorMessage += "\n"
		}
		errorMessage += "Command was aborted before completion"
	} else if exitCode != 0 {
		if errorMessage != "" {
			errorMessage += "\n"
		}
		errorMessage += fmt.Sprintf("Exit code %d", exitCode)
	}

	hasBothOutputs := stdout != "" && stderr != ""
	if hasBothOutputs {
		stdout += "\n"
	}
	if errorMessage != "" {
		stdout += "\n" + errorMessage
	}
	return strings.TrimPrefix(stdout, "\n")
}

func truncateOutput(content string) string {
	if len(content) <= MaxOutputLength {
		return content
	}

	halfLength := MaxOutputLength / 2
	start := content[:halfLength]
	end := content[len(content)-halfLength:]

	truncatedLinesCount := countLines(content[halfLength : len(content)-halfLength])
	return fmt.Sprintf("%s\n\n... [%d lines truncated] ...\n\n%s", start, truncatedLinesCount, end)
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	count := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		count++
	}
	return count
}
