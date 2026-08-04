package agent

import (
	"net/http"
	"strings"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/httpext"
)

type responsesWSTransportKey struct {
	providerID string
	baseURL    string
}

type responsesWSTransportEntry struct {
	client  *http.Client
	session *httpext.ResponsesWebSocketTransportSession
}

func (c *coordinator) responsesWebSocketWrappedClient(
	base *http.Client,
	providerCfg config.ProviderConfig,
) (*http.Client, *httpext.ResponsesWebSocketTransportSession) {
	if c == nil || c.responsesWSPool == nil || !providerCfg.ResponsesWebSocket {
		return base, nil
	}
	if base == nil {
		base = &http.Client{Transport: http.DefaultTransport}
	}

	key := responsesWSTransportKey{
		providerID: strings.TrimSpace(providerCfg.ID),
		baseURL:    strings.TrimRight(strings.TrimSpace(providerCfg.BaseURL), "/"),
	}

	c.responsesWSTransportMu.Lock()
	defer c.responsesWSTransportMu.Unlock()
	if c.responsesWSTransport == nil {
		c.responsesWSTransport = make(map[responsesWSTransportKey]responsesWSTransportEntry)
	}
	if entry, ok := c.responsesWSTransport[key]; ok && entry.client != nil {
		return entry.client, entry.session
	}

	session := httpext.NewResponsesWebSocketTransportSession()
	client := httpext.WrapOpenAIResponsesWebSocketHTTPClient(
		base,
		c.responsesWSPool,
		providerCfg.ResponsesWebSocketOptions(),
		session,
	)
	entry := responsesWSTransportEntry{client: client, session: session}
	c.responsesWSTransport[key] = entry
	return entry.client, entry.session
}
