package httpext

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/gorilla/websocket"
)

const responsesWebSocketPrewarmHeader = "X-Crush-Responses-WebSocket-Prewarm"

// BeginResponsesWebSocketTurn starts a new user-turn scope for sticky routing.
// Returns a context carrying the turn scope and a cleanup that resets turn-local
// chain state without closing the session-scoped WebSocket connection.
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

// PrewarmResponsesWebSocket performs a best-effort v2 warmup (generate=false) on the
// pooled connection before the first streamed model call in a turn.
func PrewarmResponsesWebSocket(
	ctx context.Context,
	client *http.Client,
	pool *ResponsesWebSocketPool,
	opts ResponsesWebSocketOptions,
	wsURL url.URL,
	headers http.Header,
	model string,
) error {
	if client == nil || !opts.Enabled || !opts.Prewarm || !opts.V2 {
		return nil
	}
	if model == "" {
		return nil
	}

	h := headers.Clone()
	applyResponsesWebSocketBetaHeader(wsURL, h, opts)
	if turnState := pooledTurnState(pool, wsURL, h, responsesWebSocketSessionID(ctx)); turnState != "" {
		h.Set(HeaderCodexTurnState, turnState)
	}
	h.Set(responsesWebSocketPrewarmHeader, "1")

	payload := map[string]any{
		"type":     "response.create",
		"model":    model,
		"stream":   true,
		"generate": false,
		"input":    []any{},
	}
	message, err := jsonMarshal(payload)
	if err != nil {
		return fmt.Errorf("marshal prewarm payload: %w", err)
	}

	entry, _, err := pool.acquireConn(ctx, wsURL, h, opts, nil)
	if err != nil {
		return err
	}

	entry.streamMu.Lock()
	defer entry.streamMu.Unlock()

	sessionID := responsesWebSocketSessionID(ctx)
	if err := entry.conn.WriteMessage(websocket.TextMessage, message); err != nil {
		pool.invalidate(wsURL, h, sessionID)
		return fmt.Errorf("send prewarm: %w", err)
	}

	for {
		_, data, err := entry.conn.ReadMessage()
		if err != nil {
			pool.invalidate(wsURL, h, sessionID)
			return fmt.Errorf("read prewarm: %w", err)
		}
		if turnState := parseTurnStateFromEvent(data); turnState != "" {
			entry.setTurnState(turnState)
		}
		eventType := websocketEventType(data)
		if eventType == "response.completed" || eventType == "response.failed" || eventType == "response.incomplete" {
			if id := parseResponseIDFromEvent(data); id != "" {
				entry.setLastResponseID(id)
			}
			return nil
		}
	}
}

func applyResponsesWebSocketChain(payload map[string]any, entry *pooledWebSocketConn, opts ResponsesWebSocketOptions) {
	if !opts.Chain || entry == nil {
		return
	}
	prev := entry.lastResponseID()
	if prev == "" {
		return
	}
	payload["store"] = true
	payload["previous_response_id"] = prev
	if input, ok := payload["input"]; ok {
		payload["input"] = trimChainedInput(input, entry.lastChainInputLen())
	}
}

func recordResponsesWebSocketChainState(entry *pooledWebSocketConn, data []byte, inputLen int) {
	if entry == nil {
		return
	}
	if id := parseResponseIDFromEvent(data); id != "" {
		entry.setLastResponseID(id)
	}
	if inputLen > 0 {
		entry.setLastChainInputLen(inputLen)
	}
}