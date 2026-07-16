//go:build windows

package terminal

import (
	"strings"
	"testing"
	"time"

	"github.com/UserExistsError/conpty"
	"github.com/stretchr/testify/require"
)

func TestWindowsConPTYInteractiveLifecycle(t *testing.T) {
	if !conpty.IsConPtyAvailable() {
		t.Skip("Windows ConPTY is unavailable")
	}
	manager := New(Config{})
	t.Cleanup(manager.Close)
	metadata, err := manager.Open(t.Context(), OpenRequest{
		ClientID: "client", SessionID: "session", Command: "cmd.exe", Args: []string{"/d"}, Cols: 80, Rows: 24,
	})
	require.NoError(t, err)
	_, err = manager.Resize("client", "session", metadata.ID, 100, 30)
	require.NoError(t, err)
	_, err = manager.Input(t.Context(), "client", "session", metadata.ID, []byte("echo native-conpty-marker\r\nexit\r\n"))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		value, snapshotErr := manager.Snapshot("client", "session", metadata.ID, 0)
		return snapshotErr == nil && value.State == StateExited && strings.Contains(string(value.Data), "native-conpty-marker")
	}, 10*time.Second, 10*time.Millisecond)
	require.Zero(t, manager.ActiveCount())
}

func TestWindowsConPTYKillReleasesProcess(t *testing.T) {
	if !conpty.IsConPtyAvailable() {
		t.Skip("Windows ConPTY is unavailable")
	}
	manager := New(Config{})
	t.Cleanup(manager.Close)
	metadata, err := manager.Open(t.Context(), OpenRequest{
		ClientID: "client", SessionID: "session", Command: "cmd.exe",
		Args: []string{"/d", "/c", "ping", "-n", "30", "127.0.0.1"},
	})
	require.NoError(t, err)
	require.NoError(t, manager.Kill("client", "session", metadata.ID, "kill"))
	require.Eventually(t, func() bool {
		value, snapshotErr := manager.Snapshot("client", "session", metadata.ID, 0)
		return snapshotErr == nil && value.State == StateKilled
	}, 10*time.Second, 10*time.Millisecond)
	require.Zero(t, manager.ActiveCount())
}
