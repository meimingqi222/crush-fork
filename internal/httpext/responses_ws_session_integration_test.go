package httpext

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func wsOptsChainV2() ResponsesWebSocketOptions {
	return ResponsesWebSocketOptions{
		Enabled:  true,
		V2:       true,
		Chain:    true,
		Fallback: ResponsesWebSocketFallbackOff,
	}
}

func writeResponseCompleted(conn *websocket.Conn, id string) error {
	completed := map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id":     id,
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
	data, err := json.Marshal(completed)
	if err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, data)
}

func drainStreamingResponse(t *testing.T, client *http.Client, url string, body string, ctx context.Context) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test")
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
}

func testWSURLAndHeaders(srvURL string) (url.URL, http.Header) {
	u, err := url.Parse(srvURL + "/v1/responses")
	if err != nil {
		panic(err)
	}
	return toWebSocketURL(*u), http.Header{"Authorization": []string{"Bearer test"}}
}

// Two user turns with the same Crush session ID must dial WebSocket once and reuse
// the pooled connection; ResetTurnState between turns must not close the socket.
func TestResponsesWebSocketPoolReusesConnectionAcrossTwoTurns(t *testing.T) {
	t.Parallel()

	upgrader := wsUpgrader()
	var dialCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		dialCount.Add(1)

		go func() {
			defer conn.Close()
			for {
				_, _, err := conn.ReadMessage()
				if err != nil {
					return
				}
				_ = writeResponseCompleted(conn, "resp_turn")
			}
		}()
	}))
	defer srv.Close()

	pool := NewResponsesWebSocketPool()
	transportSession := &ResponsesWebSocketTransportSession{}
	client := WrapOpenAIResponsesWebSocketHTTPClient(srv.Client(), pool, wsOpts(true), transportSession)

	baseCtx := WithResponsesWebSocketSession(context.Background(), "crush-session-a")
	wsURL, headers := testWSURLAndHeaders(srv.URL)
	postURL := srv.URL + "/v1/responses"

	for turn := 0; turn < 2; turn++ {
		turnCtx, endTurn := BeginResponsesWebSocketTurn(baseCtx, pool, wsURL, headers, "crush-session-a")
		body := `{"model":"gpt-5","stream":true,"input":[]}`
		drainStreamingResponse(t, client, postURL, body, turnCtx)
		endTurn()
	}

	require.Equal(t, int32(1), dialCount.Load(), "expected one WebSocket dial for two turns in the same session")
}

// Chained tool-loop requests should send only appended input items over the socket.
func TestResponsesWebSocketChainTrimsIncrementalInputOnSameConnection(t *testing.T) {
	t.Parallel()

	upgrader := wsUpgrader()
	var mu sync.Mutex
	var payloads []map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)

		go func() {
			defer conn.Close()
			step := 0
			for {
				_, msg, err := conn.ReadMessage()
				if err != nil {
					return
				}
				var payload map[string]any
				require.NoError(t, json.Unmarshal(msg, &payload))
				mu.Lock()
				payloads = append(payloads, payload)
				mu.Unlock()
				step++
				id := fmt.Sprintf("resp_chain_%d", step)
				_ = writeResponseCompleted(conn, id)
			}
		}()
	}))
	defer srv.Close()

	pool := NewResponsesWebSocketPool()
	client := WrapOpenAIResponsesWebSocketHTTPClient(srv.Client(), pool, wsOptsChainV2(), nil)

	ctx := WithResponsesWebSocketSession(context.Background(), "chain-session")
	postURL := srv.URL + "/v1/responses"

	item0 := map[string]any{"type": "message", "role": "user", "content": "hi"}
	item1 := map[string]any{"type": "message", "role": "assistant", "content": "hello"}
	item2 := map[string]any{"type": "message", "role": "tool", "content": "result"}

	firstBody, err := json.Marshal(map[string]any{
		"model":  "gpt-5",
		"stream": true,
		"input":  []any{item0, item1},
	})
	require.NoError(t, err)
	drainStreamingResponse(t, client, postURL, string(firstBody), ctx)

	secondBody, err := json.Marshal(map[string]any{
		"model":  "gpt-5",
		"stream": true,
		"input":  []any{item0, item1, item2},
	})
	require.NoError(t, err)
	drainStreamingResponse(t, client, postURL, string(secondBody), ctx)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, payloads, 2)

	firstInput, ok := payloads[0]["input"].([]any)
	require.True(t, ok)
	require.Len(t, firstInput, 2)
	_, hasPrev := payloads[0]["previous_response_id"]
	require.False(t, hasPrev)

	require.Equal(t, "resp_chain_1", payloads[1]["previous_response_id"])
	require.Equal(t, true, payloads[1]["store"])
	secondInput, ok := payloads[1]["input"].([]any)
	require.True(t, ok)
	require.Len(t, secondInput, 1)
	require.Equal(t, item2, secondInput[0])
}

// Session-scoped preferHTTP must apply to every stream using the same transport session.
func TestResponsesWebSocketPreferHTTPSharedAcrossWrappedClients(t *testing.T) {
	t.Parallel()

	httpHits := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "websocket" {
			http.Error(w, "no websocket", http.StatusBadRequest)
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
	pool := NewResponsesWebSocketPool()
	session := &ResponsesWebSocketTransportSession{}
	client := WrapOpenAIResponsesWebSocketHTTPClient(srv.Client(), pool, opts, session)

	postURL := srv.URL + "/v1/responses"
	body := `{"model":"gpt-5","stream":true,"input":[]}`
	for range 2 {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, postURL, strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		require.NoError(t, err)
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	require.Equal(t, int32(2), httpHits.Load())
	require.True(t, session.PreferHTTP().Load())
}