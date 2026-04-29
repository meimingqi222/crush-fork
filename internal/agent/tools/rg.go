package tools

import (
	"context"
	"log/slog"
	"os/exec"
	"runtime"
	"sync"

	"github.com/charmbracelet/crush/internal/log"
)

var getRg = sync.OnceValue(func() string {
	path, err := exec.LookPath("rg")
	if err != nil {
		if log.Initialized() {
			slog.Warn("Ripgrep (rg) not found in $PATH. Some grep features might be limited or slower.")
		}
		return ""
	}
	return path
})

func getRgCmd(ctx context.Context, globPattern string) *exec.Cmd {
	name := getRg()
	if name == "" {
		return nil
	}
	// Note: -L (--follow) is not used on Windows due to historical
	// bugs in ripgrep < 0.8.1. On Windows, symlinks have different
	// semantics and the -L flag can cause issues.
	var args []string
	if runtime.GOOS == "windows" {
		args = []string{"--files", "--hidden", "--null"}
	} else {
		args = []string{"--files", "-L", "--hidden", "--null"}
	}
	if globPattern != "" {
		args = append(args, "--glob", globPattern)
	}
	return exec.CommandContext(ctx, name, args...)
}

func getRgSearchCmd(ctx context.Context, pattern, path, include string) *exec.Cmd {
	name := getRg()
	if name == "" {
		return nil
	}
	// Use -n to show line numbers. Note: -0 is not needed with --json mode.
	args := []string{"--json", "-H", "-n", pattern}
	if include != "" {
		args = append(args, "--glob", include)
	}
	args = append(args, path)

	return exec.CommandContext(ctx, name, args...)
}
