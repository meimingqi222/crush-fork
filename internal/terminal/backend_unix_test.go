//go:build !windows

package terminal

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUnixPTYLifecycle(t *testing.T) {
	manager := New(Config{})
	t.Cleanup(manager.Close)
	metadata, err := manager.Open(t.Context(), OpenRequest{
		ClientID: "client", SessionID: "session", Command: "/bin/sh",
		Args: []string{"-c", "printf native-unix-pty-marker"}, Cols: 80, Rows: 24,
	})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		value, snapshotErr := manager.Snapshot("client", "session", metadata.ID, 0)
		return snapshotErr == nil && value.State == StateExited && strings.Contains(string(value.Data), "native-unix-pty-marker")
	}, 10*time.Second, 10*time.Millisecond)
	require.Zero(t, manager.ActiveCount())
}
