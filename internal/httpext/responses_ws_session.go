package httpext

import (
	"context"
	"net/http"
	"net/url"
	"sync/atomic"
)

type wsSessionKey struct{}

// WithResponsesWebSocketSession tags requests with a Crush session ID for
// connection pooling. Connections are reused for the lifetime of the session.
func WithResponsesWebSocketSession(ctx context.Context, sessionID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if sessionID == "" {
		return ctx
	}
	return context.WithValue(ctx, wsSessionKey{}, sessionID)
}

func responsesWebSocketSessionID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(wsSessionKey{}).(string)
	return v
}

// ResponsesWebSocketTransportSession holds session-scoped HTTP fallback state
// for a single provider HTTP client wrapper.
type ResponsesWebSocketTransportSession struct {
	preferHTTP atomic.Bool
}

// PreferHTTP returns whether WebSocket is disabled for the remainder of the session.
func (s *ResponsesWebSocketTransportSession) PreferHTTP() *atomic.Bool {
	if s == nil {
		return nil
	}
	return &s.preferHTTP
}

// ResetTurnState clears sticky routing and in-turn chain state without closing
// the pooled WebSocket connection.
func (p *ResponsesWebSocketPool) ResetTurnState(wsURL url.URL, headers http.Header, sessionID string) {
	if p == nil {
		return
	}
	key := providerSessionKey(wsURL, headers, sessionID)
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.conns[key]
	if !ok || entry == nil {
		return
	}
	entry.turnMu.Lock()
	entry.turnState = ""
	entry.lastResponseIDValue = ""
	entry.lastChainedInputCount = 0
	entry.turnMu.Unlock()
}