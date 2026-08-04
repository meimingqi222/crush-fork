package httpext

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResponsesWebSocketPoolCloseSessionIsolated(t *testing.T) {
	pool := NewResponsesWebSocketPool()
	pool.conns["first"] = &pooledWebSocketConn{sessionID: "session-a", turnState: "turn-a"}
	pool.conns["second"] = &pooledWebSocketConn{sessionID: "session-b", turnState: "turn-b"}

	require.NoError(t, pool.CloseSession("session-a"))
	require.NotContains(t, pool.conns, "first")
	require.Contains(t, pool.conns, "second")
	require.Equal(t, "turn-b", pool.conns["second"].turnStateHeader())

	require.NoError(t, pool.CloseSession("session-a"))
	pool.ClearSession("session-b")
	require.Empty(t, pool.conns)
}
