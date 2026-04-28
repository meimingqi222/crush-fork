package httpext

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// WrapAnthropicDisableThinkingHTTPClient wraps an HTTP client so that any
// outgoing JSON request body to the Anthropic Messages API has its `thinking`
// field forced to {"type": "disabled"}.
//
// This is required for Anthropic-protocol providers whose default behavior is
// to enable thinking (e.g. DeepSeek's `/anthropic` endpoint with reasoning
// models such as deepseek-v4-flash / deepseek-v4-pro). The fantasy SDK has no
// way to express `thinking: {type: "disabled"}` in its typed options struct
// (its `ThinkingProviderOption` only carries a budget, which always emits
// `type: "enabled"`), and simply omitting the field is not enough on those
// providers because their default is ON.
//
// Sending `thinking: {type: "disabled"}` is also accepted by the upstream
// Anthropic API, where it is the documented way to explicitly disable
// extended thinking, so this transformation is safe across all
// Anthropic-protocol backends.
func WrapAnthropicDisableThinkingHTTPClient(client *http.Client) *http.Client {
	if client == nil {
		return &http.Client{
			Transport: &anthropicDisableThinkingTransport{base: http.DefaultTransport},
		}
	}
	clone := *client
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	clone.Transport = &anthropicDisableThinkingTransport{base: base}
	return &clone
}

type anthropicDisableThinkingTransport struct {
	base http.RoundTripper
}

func (t *anthropicDisableThinkingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !shouldRewriteAnthropicThinking(req) {
		return t.base.RoundTrip(req)
	}

	body, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return nil, err
	}

	rewritten, ok := rewriteAnthropicBodyDisableThinking(body)
	if !ok {
		// Restore original body and proceed.
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		return t.base.RoundTrip(req)
	}

	req.Body = io.NopCloser(bytes.NewReader(rewritten))
	req.ContentLength = int64(len(rewritten))
	if req.Header != nil {
		req.Header.Set("Content-Length", strconv.Itoa(len(rewritten)))
	}
	return t.base.RoundTrip(req)
}

func shouldRewriteAnthropicThinking(req *http.Request) bool {
	if req == nil || req.Body == nil {
		return false
	}
	if req.Method != http.MethodPost {
		return false
	}
	// Only rewrite the messages endpoint to avoid touching unrelated calls
	// (e.g. /v1/models, count_tokens helpers, etc).
	if !strings.Contains(req.URL.Path, "/messages") {
		return false
	}
	ct := req.Header.Get("Content-Type")
	if ct != "" && !strings.Contains(strings.ToLower(ct), "json") {
		return false
	}
	return true
}

// rewriteAnthropicBodyDisableThinking parses a JSON Anthropic Messages body
// and forces `thinking` to {"type": "disabled"}. It returns the rewritten
// bytes and true on success, or (nil, false) if the body is not a JSON object
// we can safely modify.
func rewriteAnthropicBodyDisableThinking(body []byte) ([]byte, bool) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, false
	}
	var payload map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&payload); err != nil {
		return nil, false
	}
	payload["thinking"] = map[string]any{"type": "disabled"}
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, false
	}
	return out, true
}
