package httpext

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/gorilla/websocket"
)

// ResponsesWebSocketPool reuses WebSocket connections per provider endpoint key.
type ResponsesWebSocketPool struct {
	mu    sync.Mutex
	conns map[string]*pooledWebSocketConn
}

// NewResponsesWebSocketPool creates an empty connection pool.
func NewResponsesWebSocketPool() *ResponsesWebSocketPool {
	return &ResponsesWebSocketPool{
		conns: make(map[string]*pooledWebSocketConn),
	}
}

type pooledWebSocketConn struct {
	conn      *websocket.Conn
	turnState string
	sessionID string
	turnMu    sync.Mutex
	streamMu  sync.Mutex
}

func sortedHeaderKeys(headers http.Header) []string {
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func providerSessionKey(wsURL url.URL, headers http.Header, sessionID string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(wsURL.String()))
	for _, k := range sortedHeaderKeys(headers) {
		_, _ = h.Write([]byte(k))
		_, _ = h.Write([]byte(headers.Get(k)))
	}
	if sessionID != "" {
		_, _ = h.Write([]byte("session"))
		_, _ = h.Write([]byte(sessionID))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// acquireConn returns a pooled connection or dials a new one.
func (p *ResponsesWebSocketPool) acquireConn(
	ctx context.Context,
	wsURL url.URL,
	headers http.Header,
	preferHTTP *atomic.Bool,
) (*pooledWebSocketConn, bool, error) {
	if p == nil {
		conn, _, err := dialResponsesWebSocket(ctx, wsURL, headers, "")
		if err != nil {
			return nil, false, err
		}
		return &pooledWebSocketConn{conn: conn}, false, nil
	}

	sessionID := responsesWebSocketSessionID(ctx)
	key := providerSessionKey(wsURL, headers, sessionID)

	p.mu.Lock()
	if preferHTTP != nil && preferHTTP.Load() {
		p.mu.Unlock()
		return nil, false, errWebSocketDisabled
	}
	entry, ok := p.conns[key]
	if ok && entry != nil && entry.conn != nil {
		p.mu.Unlock()
		return entry, true, nil
	}
	p.mu.Unlock()

	conn, _, err := dialResponsesWebSocket(ctx, wsURL, headers, "")
	if err != nil {
		return nil, false, err
	}

	entry = &pooledWebSocketConn{conn: conn, sessionID: sessionID}
	p.mu.Lock()
	if existing, exists := p.conns[key]; exists && existing != nil && existing.conn != nil {
		p.mu.Unlock()
		_ = conn.Close()
		return existing, true, nil
	}
	p.conns[key] = entry
	p.mu.Unlock()
	return entry, false, nil
}

func (p *ResponsesWebSocketPool) invalidate(wsURL url.URL, headers http.Header, sessionID string) {
	if p == nil {
		return
	}
	key := providerSessionKey(wsURL, headers, sessionID)
	p.mu.Lock()
	defer p.mu.Unlock()
	if entry, ok := p.conns[key]; ok {
		if entry.conn != nil {
			_ = entry.conn.Close()
		}
		delete(p.conns, key)
	}
}

func (entry *pooledWebSocketConn) setTurnState(value string) {
	if value == "" {
		return
	}
	entry.turnMu.Lock()
	entry.turnState = value
	entry.turnMu.Unlock()
}

func (entry *pooledWebSocketConn) turnStateHeader() string {
	entry.turnMu.Lock()
	defer entry.turnMu.Unlock()
	return entry.turnState
}

var errWebSocketDisabled = fmt.Errorf("responses websocket disabled for session (http fallback)")
