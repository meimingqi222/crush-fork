package httpext

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResponsesWebSocketTransportSessionState(t *testing.T) {
	t.Parallel()

	session := NewResponsesWebSocketTransportSession()
	id := "crush-session-a"
	require.False(t, session.ReplayNeeded(id))
	require.False(t, session.PreferHTTP(id))

	session.MarkReplayNeeded(id)
	session.MarkPreferHTTP(id)
	require.True(t, session.ReplayNeeded(id))
	require.True(t, session.PreferHTTP(id))

	session.ClearReplayNeeded(id)
	session.ClearPreferHTTP(id)
	require.False(t, session.ReplayNeeded(id))
	require.False(t, session.PreferHTTP(id))
}

func TestResponsesWebSocketTransportSessionClearAndClose(t *testing.T) {
	t.Parallel()

	session := NewResponsesWebSocketTransportSession()
	id := "crush-session-a"
	session.MarkReplayNeeded(id)
	session.MarkPreferHTTP(id)
	session.Clear()
	require.False(t, session.ReplayNeeded(id))
	require.False(t, session.PreferHTTP(id))

	session.MarkReplayNeeded(id)
	session.MarkPreferHTTP(id)
	session.Close()
	session.Close()
	require.True(t, session.Closed())
	require.False(t, session.ReplayNeeded(id))
	require.False(t, session.PreferHTTP(id))

	session.MarkReplayNeeded(id)
	session.MarkPreferHTTP(id)
	require.False(t, session.ReplayNeeded(id), "closed sessions must not accept replay state")
	require.False(t, session.PreferHTTP(id), "closed sessions must not accept fallback state")
}

func TestResponsesWebSocketTransportSessionStateIsIsolated(t *testing.T) {
	t.Parallel()

	session := NewResponsesWebSocketTransportSession()
	first := "crush-session-a"
	second := "crush-session-b"
	session.MarkReplayNeeded(first)
	session.MarkPreferHTTP(first)

	require.True(t, session.ReplayNeeded(first))
	require.True(t, session.PreferHTTP(first))
	require.False(t, session.ReplayNeeded(second))
	require.False(t, session.PreferHTTP(second))

	session.ClearSession(first)
	require.False(t, session.ReplayNeeded(first))
	require.False(t, session.PreferHTTP(first))
}
