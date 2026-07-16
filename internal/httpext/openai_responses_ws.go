package httpext

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gorilla/websocket"
)

// WrapOpenAIResponsesWebSocketHTTPClient wraps streaming POST /responses as WebSocket SSE.
func WrapOpenAIResponsesWebSocketHTTPClient(
	client *http.Client,
	pool *ResponsesWebSocketPool,
	opts ResponsesWebSocketOptions,
	session *ResponsesWebSocketTransportSession,
) *http.Client {
	if client == nil {
		client = &http.Client{Transport: http.DefaultTransport}
	}
	var preferHTTP *atomic.Bool
	if session != nil {
		preferHTTP = session.PreferHTTP()
	}
	clone := *client
	clone.Transport = openAIResponsesWebSocketTransport{
		base:       client.Transport,
		pool:       pool,
		opts:       opts,
		preferHTTP: preferHTTP,
	}
	return &clone
}

type openAIResponsesWebSocketTransport struct {
	base       http.RoundTripper
	pool       *ResponsesWebSocketPool
	opts       ResponsesWebSocketOptions
	preferHTTP *atomic.Bool
}

func (t openAIResponsesWebSocketTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}

	if req == nil || req.URL == nil || req.Method != http.MethodPost || !isResponsesPath(req.URL.Path) {
		return base.RoundTrip(req)
	}

	body, payload, stream, err := readStreamingRequestBody(req)
	if err != nil {
		return nil, err
	}
	if !stream {
		restoreRequestBody(req, body)
		return base.RoundTrip(req)
	}

	if !t.opts.Enabled {
		restoreRequestBody(req, body)
		return base.RoundTrip(req)
	}

	if t.preferHTTP != nil && t.preferHTTP.Load() {
		restoreRequestBody(req, body)
		return base.RoundTrip(req)
	}

	wsURL := toWebSocketURL(*req.URL)
	headers := req.Header.Clone()
	applyResponsesWebSocketBetaHeader(wsURL, headers)
	if turnState := pooledTurnState(t.pool, wsURL, headers, responsesWebSocketSessionID(req.Context())); turnState != "" {
		headers.Set(HeaderCodexTurnState, turnState)
	}

	requestPayload := buildWebSocketResponseCreate(payload)
	message, err := json.Marshal(requestPayload)
	if err != nil {
		return nil, fmt.Errorf("marshal websocket request: %w", err)
	}

	entry, reused, err := t.pool.acquireConn(req.Context(), wsURL, headers, t.preferHTTP)
	if err != nil {
		if errors.Is(err, errWebSocketDisabled) || t.shouldFallbackToHTTP() {
			if !errors.Is(err, errWebSocketDisabled) {
				t.markPreferHTTP()
			}
			restoreRequestBody(req, body)
			return base.RoundTrip(req)
		}
		t.markPreferHTTP()
		if t.shouldFallbackToHTTP() {
			restoreRequestBody(req, body)
			return base.RoundTrip(req)
		}
		return nil, err
	}

	sessionID := responsesWebSocketSessionID(req.Context())

	if reused {
		slog.Debug("Responses websocket connection reused", "url", wsURL.Redacted())
	}

	reader, writer := io.Pipe()
	ctx, cancel := context.WithCancel(req.Context())
	done := make(chan struct{})

	go func() {
		defer close(done)
		defer func() { _ = writer.Close() }()

		entry.streamMu.Lock()
		defer entry.streamMu.Unlock()

		if err := entry.conn.WriteMessage(websocket.TextMessage, message); err != nil {
			t.invalidateConn(wsURL, headers, sessionID)
			_ = writer.CloseWithError(fmt.Errorf("send websocket request: %w", err))
			return
		}

		for {
			_, data, err := entry.conn.ReadMessage()
			if err != nil {
				if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) || errors.Is(err, io.EOF) {
					return
				}
				t.invalidateConn(wsURL, headers, sessionID)
				_ = writer.CloseWithError(formatWebSocketReadError(err))
				return
			}

			if turnState := parseTurnStateFromEvent(data); turnState != "" {
				entry.setTurnState(turnState)
			}

			eventType := websocketEventType(data)
			if _, err := writer.Write([]byte("event: " + eventType + "\n")); err != nil {
				return
			}
			if _, err := writer.Write([]byte("data: ")); err != nil {
				return
			}
			if _, err := writer.Write(data); err != nil {
				return
			}
			if _, err := writer.Write([]byte("\n\n")); err != nil {
				return
			}
			if eventType == "response.completed" || eventType == "response.failed" || eventType == "response.incomplete" {
				return
			}
		}
	}()

	bodyCloser := &webSocketStreamBody{
		ReadCloser: reader,
		closeFn: func() error {
			cancel()
			<-done
			return reader.Close()
		},
	}

	go func() {
		<-ctx.Done()
		_ = bodyCloser.Close()
	}()

	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
		},
		Body:    bodyCloser,
		Request: req,
	}, nil
}

func (t openAIResponsesWebSocketTransport) shouldFallbackToHTTP() bool {
	return t.opts.fallbackEnabled()
}

func (t openAIResponsesWebSocketTransport) markPreferHTTP() {
	if t.preferHTTP != nil && t.opts.fallbackSessionScoped() {
		t.preferHTTP.Store(true)
	}
}

func (t openAIResponsesWebSocketTransport) invalidateConn(wsURL url.URL, headers http.Header, sessionID string) {
	if t.pool != nil {
		t.pool.invalidate(wsURL, headers, sessionID)
	}
	t.markPreferHTTP()
}

func pooledTurnState(pool *ResponsesWebSocketPool, wsURL url.URL, headers http.Header, sessionID string) string {
	if pool == nil {
		return ""
	}
	key := providerSessionKey(wsURL, headers, sessionID)
	pool.mu.Lock()
	defer pool.mu.Unlock()
	entry, ok := pool.conns[key]
	if !ok || entry == nil {
		return ""
	}
	return entry.turnStateHeader()
}

func buildWebSocketResponseCreate(payload map[string]any) map[string]any {
	requestPayload := map[string]any{"type": "response.create"}
	for k, v := range payload {
		requestPayload[k] = v
	}
	return requestPayload
}

func applyResponsesWebSocketBetaHeader(wsURL url.URL, headers http.Header) {
	if headers.Get("OpenAI-Beta") != "" {
		return
	}
	host := strings.ToLower(wsURL.Hostname())
	if host != "api.openai.com" && !strings.HasSuffix(host, ".api.openai.com") {
		return
	}
	headers.Set("OpenAI-Beta", OpenAIBetaResponsesWSV2)
}

func dialResponsesWebSocket(
	ctx context.Context,
	wsURL url.URL,
	headers http.Header,
	turnState string,
) (*websocket.Conn, bool, error) {
	h := headers.Clone()
	applyResponsesWebSocketBetaHeader(wsURL, h)
	if turnState != "" {
		h.Set(HeaderCodexTurnState, turnState)
	}
	dialer := websocket.Dialer{Proxy: http.ProxyFromEnvironment}
	conn, resp, err := dialer.DialContext(ctx, wsURL.String(), h)
	if err != nil {
		return nil, false, formatWebSocketDialError(err, resp)
	}
	return conn, false, nil
}

func parseTurnStateFromEvent(data []byte) string {
	var envelope struct {
		Response struct {
			Metadata map[string]string `json:"metadata"`
		} `json:"response"`
		TurnState string `json:"turn_state"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return ""
	}
	if envelope.TurnState != "" {
		return envelope.TurnState
	}
	if envelope.Response.Metadata != nil {
		if v := envelope.Response.Metadata[HeaderCodexTurnState]; v != "" {
			return v
		}
		if v := envelope.Response.Metadata["turn_state"]; v != "" {
			return v
		}
	}
	return ""
}

func formatWebSocketReadError(err error) error {
	if closeErr, ok := err.(*websocket.CloseError); ok {
		reason := strings.ToLower(closeErr.Text)
		if strings.Contains(reason, WebSocketConnectionLimitReached) {
			return fmt.Errorf("read websocket event: %w: create a new websocket connection to continue", err)
		}
	}
	return fmt.Errorf("read websocket event: %w", err)
}

type webSocketStreamBody struct {
	io.ReadCloser
	once    sync.Once
	closeFn func() error
}

func (b *webSocketStreamBody) Close() error {
	var err error
	b.once.Do(func() {
		if b.closeFn != nil {
			err = b.closeFn()
			return
		}
		err = b.ReadCloser.Close()
	})
	return err
}

func isResponsesPath(path string) bool {
	if path == "/responses" || path == "/responses/" {
		return true
	}
	return strings.HasSuffix(path, "/responses")
}

func readStreamingRequestBody(req *http.Request) ([]byte, map[string]any, bool, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, nil, false, nil
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, nil, false, fmt.Errorf("read request body: %w", err)
	}

	payload := make(map[string]any)
	if err := json.Unmarshal(body, &payload); err != nil {
		restoreRequestBody(req, body)
		return nil, nil, false, fmt.Errorf("decode request body: %w", err)
	}

	stream, _ := payload["stream"].(bool)
	return body, payload, stream, nil
}

func restoreRequestBody(req *http.Request, body []byte) {
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
}

func toWebSocketURL(httpURL url.URL) url.URL {
	httpURL.Scheme = mapToWebSocketScheme(httpURL.Scheme)
	return httpURL
}

func mapToWebSocketScheme(scheme string) string {
	switch strings.ToLower(scheme) {
	case "http":
		return "ws"
	case "https":
		return "wss"
	default:
		return scheme
	}
}

func websocketEventType(data []byte) string {
	var event struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &event); err != nil || event.Type == "" {
		return "message"
	}
	return event.Type
}

func formatWebSocketDialError(err error, resp *http.Response) error {
	if resp == nil || resp.Body == nil {
		return fmt.Errorf("dial websocket: %w", err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
	if readErr != nil || len(body) == 0 {
		return fmt.Errorf("dial websocket: %w", err)
	}
	return fmt.Errorf("dial websocket: %w: %s", err, strings.TrimSpace(string(body)))
}
