package app

import (
	"testing"

	"github.com/charmbracelet/crush/internal/memory"
	"github.com/stretchr/testify/require"
)

func TestNewDeletesSessionMemoryOnSessionDelete(t *testing.T) {
	t.Parallel()

	conn, store := setupMessageSubscriberDependencies(t)
	defer func() {
		require.NoError(t, conn.Close())
	}()

	app, err := New(t.Context(), conn, store)
	require.NoError(t, err)

	sess, err := app.Sessions.Create(t.Context(), "session memory cleanup")
	require.NoError(t, err)

	memorySvc, err := memory.NewService(store.Config().Options.DataDirectory)
	require.NoError(t, err)
	require.NoError(t, memorySvc.Store(t.Context(), memory.StoreParams{
		Key:   "session/" + sess.ID + "/current",
		Value: "# Current session state\n\nresume later",
		Scope: "session",
	}))

	require.NoError(t, app.Sessions.Delete(t.Context(), sess.ID))
	_, err = memorySvc.Get(t.Context(), "session/"+sess.ID+"/current")
	require.ErrorIs(t, err, memory.ErrNotFound)
}
