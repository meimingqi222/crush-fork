package httpext

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

type wsSessionKey struct{}

// ErrResponsesReplayRequired indicates that a Responses request depended on
// server-side chain state which is no longer available through the current
// transport. The agent must retry the logical step with a full replay.
var ErrResponsesReplayRequired = errors.New("responses request requires full replay")

// WithResponsesWebSocketSession tags requests with a Crush session ID for
// connection pooling and per-session transport state.
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

type responsesWebSocketSessionState struct {
	preferHTTP   bool
	replayNeeded bool
}

// ResponsesWebSocketTransportSession owns fallback state for one wrapped
// provider client, partitioned by Crush session ID.
type ResponsesWebSocketTransportSession struct {
	mu     sync.Mutex
	states map[string]responsesWebSocketSessionState
	closed bool
}

func NewResponsesWebSocketTransportSession() *ResponsesWebSocketTransportSession {
	return &ResponsesWebSocketTransportSession{states: make(map[string]responsesWebSocketSessionState)}
}

func (s *ResponsesWebSocketTransportSession) stateLocked(sessionID string) responsesWebSocketSessionState {
	if s.states == nil {
		s.states = make(map[string]responsesWebSocketSessionState)
	}
	return s.states[sessionID]
}

func (s *ResponsesWebSocketTransportSession) ReplayNeeded(sessionID string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stateLocked(strings.TrimSpace(sessionID)).replayNeeded
}

func (s *ResponsesWebSocketTransportSession) MarkReplayNeeded(sessionID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	state := s.stateLocked(strings.TrimSpace(sessionID))
	state.replayNeeded = true
	s.states[strings.TrimSpace(sessionID)] = state
}

func (s *ResponsesWebSocketTransportSession) ClearReplayNeeded(sessionID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.TrimSpace(sessionID)
	state := s.stateLocked(key)
	state.replayNeeded = false
	s.states[key] = state
}

func (s *ResponsesWebSocketTransportSession) PreferHTTP(sessionID string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stateLocked(strings.TrimSpace(sessionID)).preferHTTP
}

func (s *ResponsesWebSocketTransportSession) MarkPreferHTTP(sessionID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	key := strings.TrimSpace(sessionID)
	state := s.stateLocked(key)
	state.preferHTTP = true
	s.states[key] = state
}

func (s *ResponsesWebSocketTransportSession) ClearPreferHTTP(sessionID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.TrimSpace(sessionID)
	state := s.stateLocked(key)
	state.preferHTTP = false
	s.states[key] = state
}

func (s *ResponsesWebSocketTransportSession) ClearSession(sessionID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.states, strings.TrimSpace(sessionID))
	s.mu.Unlock()
}

// Close marks the transport session closed and clears all per-Crush-session
// state. It is idempotent.
func (s *ResponsesWebSocketTransportSession) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.closed = true
	s.states = make(map[string]responsesWebSocketSessionState)
	s.mu.Unlock()
}

func (s *ResponsesWebSocketTransportSession) Clear() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.closed {
		s.states = make(map[string]responsesWebSocketSessionState)
	}
	s.mu.Unlock()
}

func (s *ResponsesWebSocketTransportSession) Closed() bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// ResetTurnState clears sticky routing for a turn without closing the pooled
// WebSocket connection.
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
	entry.turnMu.Unlock()
}
