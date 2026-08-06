package shell

import (
	"testing"
)

// Benchmark CPU usage during polling.
func BenchmarkShellPolling(b *testing.B) {
	shell := NewShell(&Options{WorkingDir: b.TempDir()})

	b.ReportAllocs()

	for b.Loop() {
		// Use a short sleep to measure polling overhead.
		_, _, err := shell.Exec(b.Context(), "sleep 0.02")
		exitCode := ExitCode(err)
		if err != nil || exitCode != 0 {
			b.Fatalf("Command failed: %v, exit code: %d", err, exitCode)
		}
	}
}
