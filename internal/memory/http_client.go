package memory

// http_client.go — HTTPMemoryClient delegates all memory operations to a
// universal-memory HTTP sidecar running at a configurable base URL.
//
// DEPRECATED: Use ManagedRuntimeClient instead. HTTP sidecar mode is replaced by
// stdio JSON-RPC runtime. This client is kept for backward compatibility.
//
// API contract (implemented by universal-memory src/server/index.ts):
//
//   POST /recall        body: RecallReq       → RecallResp
//   POST /extract       body: ExtractReq      → 204
//   POST /consolidate   body: ConsolidateReq  → 204
//   GET  /freshness     → FreshnessResp
//   POST /store         body: StoreParams     → 204
//   GET  /get?key=K     → Entry (or 404)
//   DELETE /delete?key=K → 204 (or 404)
//   POST /search        body: SearchParams    → []Entry
//   POST /list          body: ListParams      → []Entry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// HTTPMemoryClientConfig holds configuration for the HTTP sidecar client.
type HTTPMemoryClientConfig struct {
	// BaseURL is the base URL of the universal-memory sidecar, e.g.
	// "http://127.0.0.1:7779".  No trailing slash.
	BaseURL string
	// Timeout for each HTTP request.  Defaults to 5 s.
	Timeout time.Duration
}

// HTTPMemoryClient implements MemoryClient by calling the universal-memory
// HTTP sidecar.
type HTTPMemoryClient struct {
	baseURL string
	http    *http.Client
}

// NewHTTPMemoryClient creates a new client pointed at the given sidecar.
func NewHTTPMemoryClient(cfg HTTPMemoryClientConfig) *HTTPMemoryClient {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &HTTPMemoryClient{
		baseURL: cfg.BaseURL,
		http:    &http.Client{Timeout: timeout},
	}
}

// ---- wire types ----

type recallReq struct {
	Query     string `json:"query"`
	Scope     string `json:"scope,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
}
type recallResp struct {
	Lines []string `json:"lines"`
}

type extractReq struct {
	SessionID  string   `json:"sessionId"`
	Scope      string   `json:"scope,omitempty"`
	Agent      string   `json:"agent,omitempty"`
	Prompt     string   `json:"prompt"`
	Transcript []string `json:"transcript"`
}

type consolidateReq struct {
	SessionID string `json:"sessionId"`
	Force     bool   `json:"force"`
}

type freshnessResp struct {
	HasMemories bool   `json:"hasMemories"`
	Warning     string `json:"warning"`
}

type appendMessagesReq struct {
	SessionID string              `json:"sessionId"`
	Messages  []appendMessageItem `json:"messages"`
}

type appendMessageItem struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp,omitempty"`
}

// ---- MemoryClient implementation ----

// AppendMessages implements MemoryClient.AppendMessages.
func (c *HTTPMemoryClient) AppendMessages(ctx context.Context, sessionID string, messages []AppendMessage) error {
	items := make([]appendMessageItem, len(messages))
	for i, m := range messages {
		items[i] = appendMessageItem{
			Role:      m.Role,
			Content:   m.Content,
			Timestamp: m.Timestamp,
		}
	}
	return c.postJSON(ctx, "/append_messages", appendMessagesReq{
		SessionID: sessionID,
		Messages:  items,
	}, nil)
}

func (c *HTTPMemoryClient) Recall(ctx context.Context, query, scope, sessionID string) ([]string, error) {
	var resp recallResp
	if err := c.postJSON(ctx, "/recall", recallReq{Query: query, Scope: scope, SessionID: sessionID}, &resp); err != nil {
		return nil, err
	}
	return resp.Lines, nil
}

func (c *HTTPMemoryClient) Extract(ctx context.Context, req ExtractRequest) error {
	return c.postJSON(ctx, "/extract", extractReq{
		SessionID:  req.SessionID,
		Scope:      req.Scope,
		Agent:      req.Agent,
		Prompt:     req.Prompt,
		Transcript: req.Transcript,
	}, nil)
}

func (c *HTTPMemoryClient) Consolidate(ctx context.Context, req ConsolidateRequest) error {
	return c.postJSON(ctx, "/consolidate", consolidateReq{
		SessionID: req.SessionID,
		Force:     req.Force,
	}, nil)
}

func (c *HTTPMemoryClient) FreshnessStatus(ctx context.Context) (FreshnessResult, error) {
	var resp freshnessResp
	if err := c.getJSON(ctx, "/freshness", &resp); err != nil {
		return FreshnessResult{}, err
	}
	return FreshnessResult{HasMemories: resp.HasMemories, Warning: resp.Warning}, nil
}

func (c *HTTPMemoryClient) Store(ctx context.Context, params StoreParams) error {
	return c.postJSON(ctx, "/store", params, nil)
}

func (c *HTTPMemoryClient) Get(ctx context.Context, key string) (Entry, error) {
	var entry Entry
	u := fmt.Sprintf("%s/get?key=%s", c.baseURL, url.QueryEscape(key))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return entry, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return entry, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return entry, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return entry, fmt.Errorf("memory sidecar GET /get: status %d", resp.StatusCode)
	}
	return entry, json.NewDecoder(resp.Body).Decode(&entry)
}

func (c *HTTPMemoryClient) Delete(ctx context.Context, key string) error {
	u := fmt.Sprintf("%s/delete?key=%s", c.baseURL, url.QueryEscape(key))
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("memory sidecar DELETE /delete: status %d", resp.StatusCode)
	}
	return nil
}

func (c *HTTPMemoryClient) Search(ctx context.Context, params SearchParams) ([]Entry, error) {
	var entries []Entry
	if err := c.postJSON(ctx, "/search", params, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

func (c *HTTPMemoryClient) List(ctx context.Context, params ListParams) ([]Entry, error) {
	var entries []Entry
	if err := c.postJSON(ctx, "/list", params, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// ---- helpers ----

func (c *HTTPMemoryClient) postJSON(ctx context.Context, path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("memory sidecar marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("memory sidecar POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("memory sidecar POST %s: status %d: %s", path, resp.StatusCode, body)
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *HTTPMemoryClient) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("memory sidecar GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("memory sidecar GET %s: status %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
