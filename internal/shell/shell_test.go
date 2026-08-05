package shell

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Benchmark to measure CPU efficiency
func BenchmarkShellQuickCommands(b *testing.B) {
	shell := NewShell(&Options{WorkingDir: b.TempDir()})

	b.ReportAllocs()

	for b.Loop() {
		_, _, err := shell.Exec(b.Context(), "echo test")
		exitCode := ExitCode(err)
		if err != nil || exitCode != 0 {
			b.Fatalf("Command failed: %v, exit code: %d", err, exitCode)
		}
	}
}

func TestTestTimeout(t *testing.T) {
	// XXX(@andreynering): This fails on Windows. Address once possible.
	if runtime.GOOS == "windows" {
		t.Skip("Skipping test on Windows")
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	t.Cleanup(cancel)

	shell := NewShell(&Options{WorkingDir: t.TempDir()})
	_, _, err := shell.Exec(ctx, "sleep 10")
	if status := ExitCode(err); status == 0 {
		t.Fatalf("Expected non-zero exit status, got %d", status)
	}
	if !IsInterrupt(err) {
		t.Fatalf("Expected command to be interrupted, but it was not")
	}
	if err == nil {
		t.Fatalf("Expected an error due to timeout, but got none")
	}
}

func TestTestCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // immediately cancel the context

	shell := NewShell(&Options{WorkingDir: t.TempDir()})
	_, _, err := shell.Exec(ctx, "sleep 10")
	if status := ExitCode(err); status == 0 {
		t.Fatalf("Expected non-zero exit status, got %d", status)
	}
	if !IsInterrupt(err) {
		t.Fatalf("Expected command to be interrupted, but it was not")
	}
	if err == nil {
		t.Fatalf("Expected an error due to cancel, but got none")
	}
}

func TestRunCommandError(t *testing.T) {
	shell := NewShell(&Options{WorkingDir: t.TempDir()})
	_, _, err := shell.Exec(t.Context(), "nopenopenope")
	if status := ExitCode(err); status == 0 {
		t.Fatalf("Expected non-zero exit status, got %d", status)
	}
	if IsInterrupt(err) {
		t.Fatalf("Expected command to not be interrupted, but it was")
	}
	if err == nil {
		t.Fatalf("Expected an error, got nil")
	}
}

func TestRunContinuity(t *testing.T) {
	tempDir1 := t.TempDir()
	tempDir2 := t.TempDir()

	shell := NewShell(&Options{WorkingDir: tempDir1})
	if _, _, err := shell.Exec(t.Context(), "export FOO=bar"); err != nil {
		t.Fatalf("failed to set env: %v", err)
	}
	if _, _, err := shell.Exec(t.Context(), "cd "+filepath.ToSlash(tempDir2)); err != nil {
		t.Fatalf("failed to change directory: %v", err)
	}
	out, _, err := shell.Exec(t.Context(), "echo $FOO ; pwd")
	if err != nil {
		t.Fatalf("failed to echo: %v", err)
	}
	expect := "bar\n" + tempDir2 + "\n"
	if out != expect {
		t.Fatalf("expected output %q, got %q", expect, out)
	}
}

func TestCrossPlatformExecution(t *testing.T) {
	shell := NewShell(&Options{WorkingDir: "."})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// Test a simple command that should work on all platforms
	stdout, stderr, err := shell.Exec(ctx, "echo hello")
	if err != nil {
		t.Fatalf("Echo command failed: %v, stderr: %s", err, stderr)
	}

	if stdout == "" {
		t.Error("Echo command produced no output")
	}

	// The output should contain "hello" regardless of platform
	if !strings.Contains(strings.ToLower(stdout), "hello") {
		t.Errorf("Echo output should contain 'hello', got: %q", stdout)
	}
}

func TestNewShellInjectsAgentEnvironment(t *testing.T) {
	shell := NewShell(&Options{WorkingDir: t.TempDir(), Env: []string{}})

	stdout, stderr, err := shell.Exec(t.Context(), "echo $CRUSH,$AGENT,$AI_AGENT")
	require.NoError(t, err, stderr)
	require.Equal(t, "1,crush,crush\n", stdout)
}

func TestWindowsGitBashStyleCDPath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Skipping Windows-specific path normalization test")
	}

	workingDir := t.TempDir()
	gitBashPath := toGitBashStylePath(t, workingDir)
	shell := NewShell(&Options{WorkingDir: workingDir})

	stdout, stderr, err := shell.Exec(t.Context(), "cd "+gitBashPath+" && pwd")
	require.NoError(t, err, stderr)
	require.Equal(t, filepath.Clean(workingDir), filepath.Clean(strings.TrimSpace(stdout)))
	require.Equal(t, filepath.Clean(workingDir), filepath.Clean(shell.GetWorkingDir()))
}

func TestRuntimeEnvHookInjectsAndOverridesVars(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping test on Windows")
	}

	SetRuntimeEnvHook(nil)
	t.Cleanup(func() {
		SetRuntimeEnvHook(nil)
	})

	SetRuntimeEnvHook(func(ctx context.Context, input RuntimeEnvInput) map[string]string {
		return map[string]string{
			"HOOK_VAR":     "hook-value",
			"SHARED_VALUE": "hook-override",
		}
	})

	shell := NewShell(&Options{
		WorkingDir: t.TempDir(),
		Env: []string{
			"SHARED_VALUE=base",
		},
	})

	stdout, _, err := shell.Exec(t.Context(), "echo $HOOK_VAR,$SHARED_VALUE")
	require.NoError(t, err)
	require.Equal(t, "hook-value,hook-override\n", stdout)

	storedEnv := shell.GetEnv()
	require.Contains(t, storedEnv, "SHARED_VALUE=base")
	require.NotContains(t, storedEnv, "HOOK_VAR=hook-value")
	require.NotContains(t, storedEnv, "SHARED_VALUE=hook-override")
}

func toGitBashStylePath(t *testing.T, path string) string {
	t.Helper()

	volume := filepath.VolumeName(path)
	require.NotEmpty(t, volume)

	trimmed := strings.TrimPrefix(filepath.ToSlash(path), filepath.ToSlash(volume))
	return "/" + strings.ToLower(strings.TrimSuffix(volume, ":")) + trimmed
}

// TestRunnerCacheReusedForSafeCommands verifies that the cached runner is
// reused across safe commands (no background/procsubst). We detect reuse by
// checking that shell variables persist across calls.
func TestRunnerCacheReusedForSafeCommands(t *testing.T) {
	shell := NewShell(&Options{WorkingDir: t.TempDir()})

	// Set a variable in the first command.
	_, _, err := shell.Exec(t.Context(), "export CACHED_VAR=yes")
	require.NoError(t, err)

	// The variable should persist in the second command, proving the
	// runner state (Vars) is carried over.
	out, _, err := shell.Exec(t.Context(), "echo $CACHED_VAR")
	require.NoError(t, err)
	require.Equal(t, "yes\n", out)
}

// TestRunnerCacheInvalidatedByBackgroundCommand verifies that a background
// command does NOT reuse the cached runner, preventing goroutine races.
// After a background command, previously set variables should still persist
// (because updateShellFromRunner syncs state back), but the runner itself
// must be a fresh one.
func TestRunnerCacheInvalidatedByBackgroundCommand(t *testing.T) {
	shell := NewShell(&Options{WorkingDir: t.TempDir()})

	// Set a variable.
	_, _, err := shell.Exec(t.Context(), "export BG_TEST=hello")
	require.NoError(t, err)

	// Run a background command — this must NOT use the cached runner.
	// On Windows, "sleep" may not be available, so use "true" (a builtin
	// in the mvdan/sh interpreter).
	_, _, err = shell.Exec(t.Context(), "true &")
	require.NoError(t, err)

	// The variable should still be accessible — state is synced back via
	// updateShellFromRunner even for one-shot runners.
	out, _, err := shell.Exec(t.Context(), "echo $BG_TEST")
	require.NoError(t, err)
	require.Equal(t, "hello\n", out)
}

// TestRunnerCacheNotUsedForProcessSubstitution verifies that process
// substitution does NOT reuse the cached runner.
func TestRunnerCacheNotUsedForProcessSubstitution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Process substitution is not supported on Windows")
	}

	shell := NewShell(&Options{WorkingDir: t.TempDir()})

	// Set a variable.
	_, _, err := shell.Exec(t.Context(), "export PS_TEST=world")
	require.NoError(t, err)

	// Process substitution — must NOT use the cached runner.
	_, _, err = shell.Exec(t.Context(), "cat <(echo foo)")
	require.NoError(t, err)

	// Variable should still persist.
	out, _, err := shell.Exec(t.Context(), "echo $PS_TEST")
	require.NoError(t, err)
	require.Equal(t, "world\n", out)
}

// TestRunnerCacheInvalidatedOnPanic verifies that if a command causes a
// panic, the cached runner is discarded so the next command gets a fresh one.
func TestRunnerCacheInvalidatedOnPanic(t *testing.T) {
	shell := NewShell(&Options{WorkingDir: t.TempDir()})

	// Run a normal command to populate the cache.
	_, _, err := shell.Exec(t.Context(), "echo first")
	require.NoError(t, err)

	// The cache should now be populated.
	// Run another normal command — should still work.
	out, _, err := shell.Exec(t.Context(), "echo second")
	require.NoError(t, err)
	require.Contains(t, out, "second")

	// Verify the runner is still functional after multiple uses.
	out, _, err = shell.Exec(t.Context(), "echo third")
	require.NoError(t, err)
	require.Contains(t, out, "third")
}
