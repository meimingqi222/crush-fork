package httpext

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/openai/openai-go/v2/packages/ssestream"
	"github.com/openai/openai-go/v2/responses"
	"github.com/stretchr/testify/require"
)

func wsUpgrader() websocket.Upgrader {
	return websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
}

func wsOpts(enabled bool) ResponsesWebSocketOptions {
	return ResponsesWebSocketOptions{
		Enabled:  enabled,
		Fallback: ResponsesWebSocketFallbackSession,
	}
}

func TestWrapOpenAIResponsesWebSocketHTTPClientStreamsResponseEvents(t *testing.T) {
	t.Parallel()

	upgrader := wsUpgrader()
	var requestPayload map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/responses", r.URL.Path)
		require.Equal(t, "responses-api=v1", r.Header.Get("OpenAI-Beta"))

		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()

		_, msg, err := conn.ReadMessage()
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(msg, &requestPayload))

		created := map[string]any{
			"type": "response.created",
			"response": map[string]any{
				"id": "resp_123",
			},
		}
		completed := map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id":     "resp_123",
				"output": []any{},
				"usage": map[string]any{
					"input_tokens":          1,
					"output_tokens":         1,
					"total_tokens":          2,
					"input_tokens_details":  map[string]any{},
					"output_tokens_details": map[string]any{},
				},
			},
		}

		createdJSON, err := json.Marshal(created)
		require.NoError(t, err)
		completedJSON, err := json.Marshal(completed)
		require.NoError(t, err)

		require.NoError(t, conn.WriteMessage(websocket.TextMessage, createdJSON))
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, completedJSON))
		require.NoError(t, conn.Close())
	}))
	defer srv.Close()

	wrapped := WrapOpenAIResponsesWebSocketHTTPClient(srv.Client(), NewResponsesWebSocketPool(), wsOpts(true), nil)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5","stream":true,"input":[]}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OpenAI-Beta", "responses-api=v1")

	resp, err := wrapped.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	stream := ssestream.NewStream[responses.ResponseStreamEventUnion](ssestream.NewDecoder(resp), nil)
	require.True(t, stream.Next())
	require.Equal(t, "response.created", stream.Current().Type)
	require.True(t, stream.Next())
	require.Equal(t, "response.completed", stream.Current().Type)
	require.NoError(t, stream.Err())

	require.Equal(t, "response.create", requestPayload["type"])
	require.Equal(t, true, requestPayload["stream"])
	require.Equal(t, "gpt-5", requestPayload["model"])
}

func TestWrapOpenAIResponsesWebSocketHTTPClientPassesThroughNonStreamingRequests(t *testing.T) {
	t.Parallel()

	sawRequest := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawRequest = true
		require.Equal(t, "/v1/responses", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	wrapped := WrapOpenAIResponsesWebSocketHTTPClient(srv.Client(), NewResponsesWebSocketPool(), wsOpts(true), nil)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5","stream":false,"input":[]}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := wrapped.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, `{"ok":true}`, string(body))
	require.True(t, sawRequest)
}

func TestApplyResponsesWebSocketBetaHeaderV2(t *testing.T) {
	t.Parallel()

	// api.openai.com always gets the v2 beta header when none is set.
	headers := http.Header{}
	applyResponsesWebSocketBetaHeader(url.URL{Host: "api.openai.com"}, headers)
	require.Equal(t, OpenAIBetaResponsesWSV2, headers.Get("OpenAI-Beta"))

	// An explicit beta header is preserved.
	headers = http.Header{}
	headers.Set("OpenAI-Beta", OpenAIBetaResponsesAPIV1)
	applyResponsesWebSocketBetaHeader(url.URL{Host: "api.openai.com"}, headers)
	require.Equal(t, OpenAIBetaResponsesAPIV1, headers.Get("OpenAI-Beta"))

	// Non-OpenAI hosts are left untouched.
	headers = http.Header{}
	applyResponsesWebSocketBetaHeader(url.URL{Host: "example.com"}, headers)
	require.Equal(t, "", headers.Get("OpenAI-Beta"))
}

func TestResponsesWebSocketPoolProviderSessionKeyStable(t *testing.T) {
	t.Parallel()

	u, err := url.Parse("wss://example.com/v1/responses")
	require.NoError(t, err)
	h := http.Header{"Authorization": []string{"Bearer a"}}
	k1 := providerSessionKey(*u, h, "")
	k2 := providerSessionKey(*u, h, "")
	require.Equal(t, k1, k2)
	h.Set("Authorization", "Bearer b")
	require.NotEqual(t, k1, providerSessionKey(*u, h, ""))
	k3 := providerSessionKey(*u, h, "turn-a")
	k4 := providerSessionKey(*u, h, "turn-a")
	require.Equal(t, k3, k4)
	require.NotEqual(t, k1, k3)
}

func TestOpenAIResponsesWebSocketFallbackAfterDialFailure(t *testing.T) {
	t.Parallel()

	httpHits := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "websocket" {
			http.Error(w, "websocket unavailable", http.StatusBadRequest)
			return
		}
		httpHits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"))
	}))
	defer srv.Close()

	opts := ResponsesWebSocketOptions{
		Enabled:  true,
		Fallback: ResponsesWebSocketFallbackSession,
	}
	client := WrapOpenAIResponsesWebSocketHTTPClient(srv.Client(), NewResponsesWebSocketPool(), opts, nil)

	for range 2 {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5","stream":true,"input":[]}`))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		require.NoError(t, err)
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	require.Equal(t, int32(2), httpHits.Load())
}

// A chained request that carries previous_response_id must not silently fall
// back to plain HTTP after the WebSocket transport has already failed: the
// server-side chain state is not reachable over a fresh HTTP request, so the
// request must be rejected with a full-replay error instead.
func TestOpenAIResponsesWebSocketRejectsChainedRequestOnHTTPFallback(t *testing.T) {
	t.Parallel()

	httpHits := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "websocket" {
			http.Error(w, "websocket unavailable", http.StatusBadRequest)
			return
		}
		httpHits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"))
	}))
	defer srv.Close()

	opts := ResponsesWebSocketOptions{
		Enabled:  true,
		Fallback: ResponsesWebSocketFallbackSession,
	}
	client := WrapOpenAIResponsesWebSocketHTTPClient(srv.Client(), NewResponsesWebSocketPool(), opts, nil)

	// First request without previous_response_id: WS fails, session marks
	// preferHTTP, and the request falls back to HTTP cleanly.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5","stream":true,"input":[]}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	require.NoError(t, err)
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, int32(1), httpHits.Load())

	// Chained request while preferHTTP is set: must be rejected, never sent to
	// the HTTP fallback with a dangling previous_response_id.
	chained := `{"model":"gpt-5","stream":true,"previous_response_id":"resp_123","input":[{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`
	req, err = http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/v1/responses", strings.NewReader(chained))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	_, err = client.Do(req)
	require.ErrorIs(t, err, ErrResponsesReplayRequired)
	require.Equal(t, int32(1), httpHits.Load(), "chained request must not reach the HTTP fallback")
}

// hasPreviousResponseID must only match a non-empty previous_response_id value.
func TestHasPreviousResponseID(t *testing.T) {
	t.Parallel()

	require.False(t, hasPreviousResponseID(nil))
	require.False(t, hasPreviousResponseID(map[string]any{}))
	require.False(t, hasPreviousResponseID(map[string]any{"previous_response_id": ""}))
	require.False(t, hasPreviousResponseID(map[string]any{"previous_response_id": "  "}))
	require.False(t, hasPreviousResponseID(map[string]any{"previous_response_id": 123}))
	require.True(t, hasPreviousResponseID(map[string]any{"previous_response_id": "resp_123"}))
}

func TestResponsesWebSocketPoolTurnState(t *testing.T) {
	t.Parallel()

	entry := &pooledWebSocketConn{}
	entry.setTurnState("turn-abc")
	require.Equal(t, "turn-abc", entry.turnStateHeader())
	require.Equal(t, "turn-xyz", parseTurnStateFromEvent([]byte(`{"type":"response.completed","turn_state":"turn-xyz"}`)))
}
