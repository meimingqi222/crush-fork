package cmd

import (
	"bytes"
	"context"
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/memory"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func newSessionDeleteCommand(t *testing.T, dataDir string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.Flags().String("data-dir", dataDir, "")
	cmd.Flags().String("cwd", "", "")
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	return cmd
}

func TestRunSessionDeleteRemovesSessionMemory(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })

	q := db.New(conn)
	sessionSvc := session.NewService(q, conn)
	sess, err := sessionSvc.Create(t.Context(), "delete session memory")
	require.NoError(t, err)

	memorySvc, err := memory.NewService(dataDir)
	require.NoError(t, err)
	require.NoError(t, memorySvc.Store(t.Context(), memory.StoreParams{
		Key:   "session/" + sess.ID + "/current",
		Value: "# Current session state\n\nresume later",
		Scope: "session",
	}))

	cmd := newSessionDeleteCommand(t, dataDir)
	sessionDeleteJSON = false
	err = runSessionDelete(cmd, []string{session.HashID(sess.ID)[:12]})
	require.NoError(t, err)

	_, err = memorySvc.Get(t.Context(), "session/"+sess.ID+"/current")
	require.ErrorIs(t, err, memory.ErrNotFound)
}
