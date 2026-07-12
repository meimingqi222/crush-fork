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
type ResponsesWebSocketOptions struct {
	Enabled  bool
	V2       bool
	Fallback ResponsesWebSocketFallback
	Prewarm  bool
	// Chain enables previous_response_id + store on the same WebSocket connection
	// within a user turn (tool-loop steps). Requires V2.
	Chain bool
}

func (o ResponsesWebSocketOptions) fallbackEnabled() bool {
	return o.Fallback != ResponsesWebSocketFallbackOff
}

func (o ResponsesWebSocketOptions) fallbackSessionScoped() bool {
	return o.Fallback == ResponsesWebSocketFallbackSession
}