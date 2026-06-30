package common

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/ui/util"
)

// OpenAttachment writes the attachment data to a temporary file with the
// original extension and opens it with the system's default application.
// The temporary file is intentionally left in place so external viewers can
// read it after this process returns.
func OpenAttachment(data []byte, filename string) tea.Cmd {
	return func() tea.Msg {
		pattern := "crush-attachment-*"
		ext := filepath.Ext(filename)
		if ext != "" {
			pattern += ext
		}

		tmpFile, err := os.CreateTemp("", pattern)
		if err != nil {
			return util.NewErrorMsg(fmt.Errorf("creating temp file: %w", err))
		}
		path := tmpFile.Name()

		if _, writeErr := tmpFile.Write(data); writeErr != nil {
			_ = tmpFile.Close()
			_ = os.Remove(path)
			return util.NewErrorMsg(fmt.Errorf("writing temp file: %w", writeErr))
		}
		if closeErr := tmpFile.Close(); closeErr != nil {
			_ = os.Remove(path)
			return util.NewErrorMsg(fmt.Errorf("closing temp file: %w", closeErr))
		}

		if openErr := openWithDefault(path); openErr != nil {
			_ = os.Remove(path)
			return util.NewErrorMsg(fmt.Errorf("opening attachment: %w", openErr))
		}

		return util.NewInfoMsg(fmt.Sprintf("Opened %s", filename))
	}
}

func openWithDefault(path string) error {
	var cmd string
	var args []string

	switch runtime.GOOS {
	case "windows":
		// The empty first argument is required by "start" to correctly open
		// paths that contain spaces.
		cmd = "cmd"
		args = []string{"/c", "start", "", path}
	case "darwin":
		cmd = "open"
		args = []string{path}
	default:
		cmd = "xdg-open"
		args = []string{path}
	}

	return exec.Command(cmd, args...).Start()
}
