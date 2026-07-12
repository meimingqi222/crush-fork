package hooks_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/hooks"
	"github.com/stretchr/testify/require"
)

// helperSrc is a single dispatcher binary shared by every test in this file.
// It picks its behavior from os.Args[1] so tests only need to differ in the
// hook Args they configure, instead of each compiling their own throwaway
// binary (compiling one Go binary per test made this package's tests
// dominated by `go build` process overhead).
const helperSrc = `package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(1)
	}
	switch os.Args[1] {
	case "wrap":
		if len(os.Args) < 3 {
			os.Exit(1)
		}
		fmt.Print("wrapped:" + os.Args[2])
	case "fail":
		os.Exit(1)
	case "deny":
		fmt.Fprint(os.Stdout, ` + "`{\"decision\":\"deny\",\"reason\":\"not allowed\"}`" + `)
	case "sleep":
		time.Sleep(10 * time.Second)
	case "chain":
		suffix := ""
		if len(os.Args) > 2 {
			suffix = os.Args[2]
		}
		var in struct {
			ToolInput map[string]any ` + "`json:\"tool_input\"`" + `
		}
		_ = json.NewDecoder(os.Stdin).Decode(&in)
		cmd, _ := in.ToolInput["command"].(string)
		fmt.Fprintf(os.Stdout, "{\"decision\":\"modify\",\"modified_input\":{\"command\":%q}}", cmd+suffix)
	default:
		os.Exit(1)
	}
}
`

var helperBin string

// TestMain compiles the shared helper binary once for the whole test binary
// run, instead of once per test.
func TestMain(m *testing.M) {
	if _, err := exec.LookPath("go"); err == nil {
		dir, err := os.MkdirTemp("", "crush-hooks-helper")
		if err != nil {
			panic(err)
		}
		defer os.RemoveAll(dir)

		srcFile := filepath.Join(dir, "main.go")
		if err := os.WriteFile(srcFile, []byte(helperSrc), 0o644); err != nil {
			panic(err)
		}

		binName := "helper"
		if runtime.GOOS == "windows" {
			binName += ".exe"
		}
		bin := filepath.Join(dir, binName)
		if out, err := exec.Command("go", "build", "-o", bin, srcFile).CombinedOutput(); err != nil {
			panic("build helper binary: " + err.Error() + ": " + string(out))
		}
		helperBin = bin
	}

	os.Exit(m.Run())
}

func requireHelperBin(t *testing.T) string {
	t.Helper()
	if helperBin == "" {
		t.Skip("go toolchain not found, skipping")
	}
	return helperBin
}

func TestCommandHandler_Passthrough_Supported(t *testing.T) {
	t.Parallel()

	bin := requireHelperBin(t)

	enabled := true
	mgr, err := hooks.NewManager([]hooks.HookConfig{
		{
			Name:      "test-passthrough",
			Enabled:   &enabled,
			Events:    []hooks.Event{hooks.EventPreToolUse},
			Type:      hooks.HandlerTypeCommand,
			TimeoutMs: 15000,
			Command: &hooks.CommandConfig{
				Command:     bin,
				Args:        []string{"wrap"},
				Passthrough: true,
			},
		},
	})
	require.NoError(t, err)

	out, err := mgr.RunPreToolUse(context.Background(), "bash", map[string]any{
		"command": "git status",
	}, "sess-1")
	require.NoError(t, err)
	require.Equal(t, hooks.DecisionModify, out.Decision)
	require.Equal(t, "wrapped:git status", out.ModifiedInput["command"])
}

func TestCommandHandler_Passthrough_Unsupported(t *testing.T) {
	t.Parallel()

	bin := requireHelperBin(t)

	enabled := true
	mgr, err := hooks.NewManager([]hooks.HookConfig{
		{
			Name:      "test-fail",
			Enabled:   &enabled,
			Events:    []hooks.Event{hooks.EventPreToolUse},
			Type:      hooks.HandlerTypeCommand,
			TimeoutMs: 15000,
			Command: &hooks.CommandConfig{
				Command:     bin,
				Args:        []string{"fail"},
				Passthrough: true,
			},
		},
	})
	require.NoError(t, err)

	out, err := mgr.RunPreToolUse(context.Background(), "bash", map[string]any{
		"command": "htop",
	}, "sess-2")
	require.NoError(t, err)
	require.Equal(t, hooks.DecisionAllow, out.Decision)
}

func TestCommandHandler_JSON_Deny(t *testing.T) {
	t.Parallel()

	bin := requireHelperBin(t)

	enabled := true
	mgr, err := hooks.NewManager([]hooks.HookConfig{
		{
			Name:      "deny-hook",
			Enabled:   &enabled,
			Events:    []hooks.Event{hooks.EventPreToolUse},
			Type:      hooks.HandlerTypeCommand,
			TimeoutMs: 15000,
			Command: &hooks.CommandConfig{
				Command: bin,
				Args:    []string{"deny"},
			},
		},
	})
	require.NoError(t, err)

	result, err := mgr.RunPreToolUse(context.Background(), "bash", map[string]any{
		"command": "rm -rf /",
	}, "sess-3")
	require.NoError(t, err)
	require.Equal(t, hooks.DecisionDeny, result.Decision)
	require.Equal(t, "not allowed", result.Reason)
}

func TestManager_Timeout(t *testing.T) {
	t.Parallel()

	bin := requireHelperBin(t)

	enabled := true
	mgr, err := hooks.NewManager([]hooks.HookConfig{
		{
			Name:      "sleep-hook",
			Enabled:   &enabled,
			Events:    []hooks.Event{hooks.EventPreToolUse},
			Type:      hooks.HandlerTypeCommand,
			TimeoutMs: 200,
			Command: &hooks.CommandConfig{
				Command:     bin,
				Args:        []string{"sleep"},
				Passthrough: true,
			},
		},
	})
	require.NoError(t, err)

	start := time.Now()
	out, err := mgr.RunPreToolUse(context.Background(), "bash", map[string]any{
		"command": "something",
	}, "sess-4")
	elapsed := time.Since(start)

	require.NoError(t, err)
	require.Equal(t, hooks.DecisionAllow, out.Decision)
	require.Less(t, elapsed, 2*time.Second)
}

func TestManager_ChainedHooks(t *testing.T) {
	t.Parallel()

	bin := requireHelperBin(t)

	enabled := true
	mgr, err := hooks.NewManager([]hooks.HookConfig{
		{
			Name:      "hook1",
			Enabled:   &enabled,
			Events:    []hooks.Event{hooks.EventPreToolUse},
			Type:      hooks.HandlerTypeCommand,
			TimeoutMs: 15000,
			Command:   &hooks.CommandConfig{Command: bin, Args: []string{"chain", "-A"}},
		},
		{
			Name:      "hook2",
			Enabled:   &enabled,
			Events:    []hooks.Event{hooks.EventPreToolUse},
			Type:      hooks.HandlerTypeCommand,
			TimeoutMs: 15000,
			Command:   &hooks.CommandConfig{Command: bin, Args: []string{"chain", "-B"}},
		},
	})
	require.NoError(t, err)

	out, err := mgr.RunPreToolUse(context.Background(), "bash", map[string]any{
		"command": "git status",
	}, "sess-5")
	require.NoError(t, err)
	require.Equal(t, hooks.DecisionModify, out.Decision)
	require.Equal(t, "git status-A-B", out.ModifiedInput["command"])
}

func TestHTTPHandler_Basic(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in hooks.HookInput
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		require.Equal(t, "bash", in.ToolName)

		cmd, _ := in.ToolInput["command"].(string)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(hooks.HookOutput{
			Decision: hooks.DecisionModify,
			ModifiedInput: map[string]any{
				"command": "rtk " + cmd,
			},
		})
	}))
	defer srv.Close()

	enabled := true
	mgr, err := hooks.NewManager([]hooks.HookConfig{
		{
			Name:    "http-hook",
			Enabled: &enabled,
			Events:  []hooks.Event{hooks.EventPreToolUse},
			Type:    hooks.HandlerTypeHTTP,
			HTTP: &hooks.HTTPConfig{
				URL: srv.URL + "/hook",
			},
		},
	})
	require.NoError(t, err)

	out, err := mgr.RunPreToolUse(context.Background(), "bash", map[string]any{
		"command": "git log",
	}, "sess-6")
	require.NoError(t, err)
	require.Equal(t, hooks.DecisionModify, out.Decision)
	require.Equal(t, "rtk git log", out.ModifiedInput["command"])
}

func TestManager_DisabledHook(t *testing.T) {
	t.Parallel()

	disabled := false
	mgr, err := hooks.NewManager([]hooks.HookConfig{
		{
			Name:    "disabled",
			Enabled: &disabled,
			Events:  []hooks.Event{hooks.EventPreToolUse},
			Type:    hooks.HandlerTypeCommand,
			Command: &hooks.CommandConfig{
				Command: "this-binary-does-not-exist",
			},
		},
	})
	require.NoError(t, err)

	out, err := mgr.RunPreToolUse(context.Background(), "bash", map[string]any{
		"command": "ls",
	}, "sess-7")
	require.NoError(t, err)
	require.Equal(t, hooks.DecisionAllow, out.Decision)
}
