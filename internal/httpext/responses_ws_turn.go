package httpext

import (
	"context"
	"net/http"
	"net/url"
)

// BeginResponsesWebSocketTurn starts a new user-turn scope for sticky routing.
// Returns a context carrying the turn scope and a cleanup that resets turn-local
// routing state without closing the session-scoped WebSocket connection.
func BeginResponsesWebSocketTurn(
	ctx context.Context,
	pool *ResponsesWebSocketPool,
	wsURL url.URL,
	headers http.Header,
	sessionID string,
) (context.Context, func()) {
	scope := newTurnScopeID()
	ctx = WithResponsesWebSocketTurnScope(ctx, scope)
	cleanup := func() {
		if pool != nil {
			pool.ResetTurnState(wsURL, headers, sessionID)
		}
	}
	return ctx, cleanup
}
