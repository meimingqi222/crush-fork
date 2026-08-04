package agent

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"charm.land/fantasy/providers/azure"
	"charm.land/fantasy/providers/openai"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/httpext"
)

// ResponsesWebSocketResetter resets an idle session's pooled Responses
// WebSocket when the session is resumed from storage.
type ResponsesWebSocketResetter interface {
	ResetResponsesWebSocket(sessionID string)
}

// ResetResponsesWebSocket closes only the session's pooled connection. It
// deliberately preserves transport fallback state and response chaining so a
// resumed session can continue normally over a fresh socket.
func (c *coordinator) ResetResponsesWebSocket(sessionID string) {
	if c == nil || c.responsesWSPool == nil || sessionID == "" || c.IsSessionBusy(sessionID) {
		return
	}
	if err := c.responsesWSPool.CloseSession(sessionID); err != nil {
		slog.Warn("Failed to reset Responses WebSocket for resumed session", "error", err, "session_id", sessionID)
	}
}

func (c *coordinator) beginResponsesWebSocketTurn(ctx context.Context, sessionID string) (context.Context, func()) {
	if c == nil || c.responsesWSPool == nil {
		return ctx, func() {}
	}
	model := c.Model()
	providerCfg, ok := c.cfg.Config().Providers.Get(model.ModelCfg.Provider)
	if !ok || !providerCfg.ResponsesWebSocket {
		return ctx, func() {}
	}
	if providerCfg.Type != openai.Name && providerCfg.Type != azure.Name {
		return ctx, func() {}
	}

	wsURL, headers, ok := responsesWebSocketDialTarget(providerCfg)
	if !ok {
		return ctx, func() {}
	}

	ctx = httpext.WithResponsesWebSocketSession(ctx, sessionID)
	turnCtx, cleanup := httpext.BeginResponsesWebSocketTurn(ctx, c.responsesWSPool, wsURL, headers, sessionID)

	return turnCtx, cleanup
}

func responsesWebSocketDialTarget(providerCfg config.ProviderConfig) (url.URL, http.Header, bool) {
	base := strings.TrimSpace(providerCfg.BaseURL)
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	base = strings.TrimRight(base, "/")
	u, err := url.Parse(base + "/responses")
	if err != nil {
		return url.URL{}, nil, false
	}
	headers := http.Header{}
	if key := strings.TrimSpace(providerCfg.APIKey); key != "" {
		headers.Set("Authorization", "Bearer "+key)
	}
	return *u, headers, true
}
