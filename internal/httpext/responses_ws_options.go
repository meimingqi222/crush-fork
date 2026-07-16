package httpext

import "strings"

const (
	OpenAIBetaResponsesAPIV1 = "responses-api=v1"
	OpenAIBetaResponsesWSV2  = "responses_websockets=2026-02-06"

	HeaderCodexTurnState = "x-codex-turn-state"

	WebSocketConnectionLimitReached = "websocket_connection_limit_reached"
)

// ResponsesWebSocketFallback controls HTTP fallback after WebSocket failures.
type ResponsesWebSocketFallback string

const (
	ResponsesWebSocketFallbackSession ResponsesWebSocketFallback = "session"
	ResponsesWebSocketFallbackRequest ResponsesWebSocketFallback = "request"
	ResponsesWebSocketFallbackOff     ResponsesWebSocketFallback = "off"
)

// NormalizeResponsesWebSocketFallback returns a known fallback mode.
func NormalizeResponsesWebSocketFallback(s string) ResponsesWebSocketFallback {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "request":
		return ResponsesWebSocketFallbackRequest
	case "off", "none", "false":
		return ResponsesWebSocketFallbackOff
	default:
		return ResponsesWebSocketFallbackSession
	}
}

// ResponsesWebSocketOptions configures OpenAI Responses WebSocket transport.
//
// The transport is deliberately dumb: it dials the socket, reuses the pooled
// connection, and reframes events as SSE. previous_response_id chaining is
// handled once at the message layer (see internal/agent responses_chaining)
// and rides through here transparently, so no chaining state lives here.
type ResponsesWebSocketOptions struct {
	Enabled  bool
	Fallback ResponsesWebSocketFallback
}

func (o ResponsesWebSocketOptions) fallbackEnabled() bool {
	return o.Fallback != ResponsesWebSocketFallbackOff
}

func (o ResponsesWebSocketOptions) fallbackSessionScoped() bool {
	return o.Fallback == ResponsesWebSocketFallbackSession
}
