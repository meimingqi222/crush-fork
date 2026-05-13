// Package hindsight provides a client and engine adapters for the Hindsight
// remote memory service (https://github.com/vectorize-io/hindsight).
//
// Hindsight exposes a REST API under /v1/default/banks/{bank_id}/. This
// package implements:
//
//   - Client: thin HTTP client for retain, recall, reflect, and bank setup.
//   - Materializer: engine.Materializer that replicates local MemoryEvents to
//     Hindsight via the retain endpoint.
//   - Retriever: engine.Retriever that uses Hindsight as the only recall and
//     reflect source for the selected backend.
package hindsight

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const defaultBankID = "crush"

// Client is a thin HTTP client for the Hindsight REST API.
// It is safe for concurrent use.
type Client struct {
	baseURL string
	bankID  string
	http    *http.Client
	headers map[string]string
}

// NewClient creates a Client targeting baseURL with the given bank ID and
// optional Bearer token. When bankID is empty, "crush" is used.
func NewClient(baseURL, bankID, token string) *Client {
	if bankID == "" {
		bankID = defaultBankID
	}
	headers := map[string]string{
		"Content-Type": "application/json",
		"User-Agent":   "crush-memory-engine/1.0",
	}
	if token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		bankID:  bankID,
		http:    &http.Client{Timeout: 30 * time.Second},
		headers: headers,
	}
}

// BankID returns the configured bank ID.
func (c *Client) BankID() string { return c.bankID }

// RetainItem is a single item in a retain request.
type RetainItem struct {
	Content    string            `json:"content"`
	Context    string            `json:"context,omitempty"`
	DocumentID string            `json:"document_id,omitempty"`
	Tags       []string          `json:"tags,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	Async      bool              `json:"async"`
}

// Retain stores one or more memory items in the remote bank.
func (c *Client) Retain(ctx context.Context, items []RetainItem) error {
	if len(items) == 0 {
		return nil
	}
	body := map[string]any{
		"items": items,
		"async": true,
	}
	_, err := c.post(ctx, c.bankPath("memories"), body, nil)
	return err
}

// RecallRequest parameters for recall.
type RecallRequest struct {
	Query     string   `json:"query"`
	MaxTokens int      `json:"max_tokens,omitempty"`
	Budget    string   `json:"budget,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	TagsMatch string   `json:"tags_match,omitempty"`
	Types     []string `json:"types,omitempty"`
}

// RecallResult is a single recalled memory.
type RecallResult struct {
	ID          string    `json:"id,omitempty"`
	Text        string    `json:"text"`
	Type        string    `json:"type,omitempty"`
	MentionedAt time.Time `json:"mentioned_at,omitempty"`
}

// Recall queries the remote bank for memories relevant to query.
func (c *Client) Recall(ctx context.Context, req RecallRequest) ([]RecallResult, error) {
	var resp struct {
		Results []RecallResult `json:"results"`
	}
	if _, err := c.post(ctx, c.bankPath("memories/recall"), req, &resp); err != nil {
		return nil, err
	}
	return resp.Results, nil
}

// Reflect synthesizes across memories to answer query.
func (c *Client) Reflect(ctx context.Context, query, context_ string, budget string) (string, error) {
	if budget == "" {
		budget = "low"
	}
	body := map[string]any{
		"query":  query,
		"budget": budget,
	}
	if context_ != "" {
		body["context"] = context_
	}
	var resp struct {
		Text string `json:"text"`
	}
	if _, err := c.post(ctx, c.bankPath("reflect"), body, &resp); err != nil {
		return "", err
	}
	return resp.Text, nil
}

// EnsureBank creates or updates the bank with the given retain mission.
// This is idempotent and safe to call on every startup.
func (c *Client) EnsureBank(ctx context.Context, retainMission string) error {
	body := map[string]any{}
	if retainMission != "" {
		body["retain_mission"] = retainMission
	}
	_, err := c.put(ctx, "/v1/default/banks/"+c.bankID, body)
	return err
}

func (c *Client) bankPath(sub string) string {
	return fmt.Sprintf("/v1/default/banks/%s/%s", c.bankID, sub)
}

// post sends a JSON POST request and optionally decodes the response into dst.
func (c *Client) post(ctx context.Context, path string, body any, dst any) ([]byte, error) {
	return c.do(ctx, http.MethodPost, path, body, dst)
}

// put sends a JSON PUT request.
func (c *Client) put(ctx context.Context, path string, body any) ([]byte, error) {
	return c.do(ctx, http.MethodPut, path, body, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body any, dst any) ([]byte, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshaling request body: %w", err)
		}
		r = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, r)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hindsight request %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("hindsight %s %s: status %d: %s", method, path, resp.StatusCode, respBody)
	}

	if dst != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, dst); err != nil {
			return respBody, fmt.Errorf("decoding response: %w", err)
		}
	}
	return respBody, nil
}
