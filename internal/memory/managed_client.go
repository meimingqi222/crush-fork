package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// ManagedRuntimeClient is a MemoryClient implementation that manages a universal-memory runtime subprocess.
// It automatically starts the runtime, communicates via stdio JSON-RPC, and handles process lifecycle.
type ManagedRuntimeClient struct {
	process *RuntimeProcess
	rpc     *JSONRPCClient

	config RuntimeConfig

	mu          sync.Mutex
	initialized bool
}

// RuntimeConfig holds the configuration for the runtime.
type RuntimeConfig struct {
	Command string   // Command to start the runtime (e.g., "node")
	Args    []string // Arguments to the runtime (e.g., ["./node_modules/universal-memory/dist/bin/runtime.js"])
	Root    string   // Memory root directory
	Profile string   // Profile: "balanced" or "semantic"
	LLM     LLMConfig
	Embedding *EmbeddingConfig
}

// LLMConfig holds LLM configuration for extractor/consolidator.
type LLMConfig struct {
	BaseURL string
	Model   string
}

// EmbeddingConfig holds embedding configuration for semantic retrieval.
type EmbeddingConfig struct {
	BaseURL    string
	Model      string
	APIKeyEnv  string
}

// NewManagedRuntimeClient creates a new ManagedRuntimeClient.
func NewManagedRuntimeClient(config RuntimeConfig) *ManagedRuntimeClient {
	return &ManagedRuntimeClient{
		config: config,
	}
}

// Start initializes and starts the runtime process.
func (c *ManagedRuntimeClient) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.initialized {
		return nil
	}

	// Create runtime process
	c.process = NewRuntimeProcess(c.config.Command, c.config.Args)
	if err := c.process.Start(ctx); err != nil {
		return fmt.Errorf("start runtime process: %w", err)
	}

	// Create JSON-RPC client
	c.rpc = NewJSONRPCClient(c.process.Stdin(), c.process.Stdout(), c.process.Stderr())
	c.rpc.Start(ctx)

	// Initialize runtime
	initParams := map[string]interface{}{
		"root":    c.config.Root,
		"profile": c.config.Profile,
		"llm": map[string]interface{}{
			"baseUrl": c.config.LLM.BaseURL,
			"model":   c.config.LLM.Model,
		},
	}

	if c.config.Embedding != nil {
		initParams["embedding"] = map[string]interface{}{
			"baseUrl":    c.config.Embedding.BaseURL,
			"model":      c.config.Embedding.Model,
			"apiKeyEnv":  c.config.Embedding.APIKeyEnv,
		}
	}

	result, err := c.rpc.Call(ctx, "initialize", initParams)
	if err != nil {
		c.Stop()
		return fmt.Errorf("initialize runtime: %w", err)
	}

	var initResult struct {
		Success bool   `json:"success"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(result, &initResult); err != nil {
		c.Stop()
		return fmt.Errorf("parse initialize result: %w", err)
	}

	if !initResult.Success {
		c.Stop()
		return fmt.Errorf("runtime initialization failed")
	}

	c.initialized = true
	return nil
}

// Stop stops the runtime process.
func (c *ManagedRuntimeClient) Stop() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.initialized {
		return nil
	}

	if c.process != nil {
		c.process.Stop()
	}

	c.initialized = false
	return nil
}

// Shutdown gracefully shuts down the runtime.
func (c *ManagedRuntimeClient) Shutdown(ctx context.Context) error {
	c.mu.Lock()
	if !c.initialized {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	// Call shutdown method
	_, err := c.rpc.Call(ctx, "shutdown", map[string]interface{}{})
	if err != nil {
		// Force stop if shutdown fails
		return c.Stop()
	}

	return c.Stop()
}

// HealthCheck performs a health check on the runtime.
func (c *ManagedRuntimeClient) HealthCheck(ctx context.Context) error {
	c.mu.Lock()
	if !c.initialized {
		c.mu.Unlock()
		return fmt.Errorf("runtime not initialized")
	}
	c.mu.Unlock()

	_, err := c.rpc.Call(ctx, "health", map[string]interface{}{})
	return err
}

// EnsureStarted ensures the runtime is started and initialized.
func (c *ManagedRuntimeClient) EnsureStarted(ctx context.Context) error {
	c.mu.Lock()
	if !c.initialized {
		c.mu.Unlock()
		return c.Start(ctx)
	}
	c.mu.Unlock()
	return nil
}

// ---- MemoryClient implementation ----

// AppendMessages implements MemoryClient.AppendMessages.
func (c *ManagedRuntimeClient) AppendMessages(ctx context.Context, sessionID string, messages []AppendMessage) error {
	if err := c.EnsureStarted(ctx); err != nil {
		return err
	}

	// Convert messages to runtime format
	runtimeMessages := make([]map[string]interface{}, len(messages))
	for i, m := range messages {
		runtimeMessages[i] = map[string]interface{}{
			"role":      m.Role,
			"content":   m.Content,
			"timestamp": m.Timestamp,
		}
	}

	params := map[string]interface{}{
		"sessionId": sessionID,
		"messages":  runtimeMessages,
	}

	_, err := c.rpc.Call(ctx, "append_messages", params)
	if err != nil {
		return fmt.Errorf("append_messages: %w", err)
	}

	return nil
}

// Recall implements MemoryClient.Recall.
func (c *ManagedRuntimeClient) Recall(ctx context.Context, query string, scope string, sessionID string) ([]string, error) {
	if err := c.EnsureStarted(ctx); err != nil {
		return nil, err
	}

	params := map[string]interface{}{
		"query":     query,
		"scope":     scope,
		"sessionId": sessionID,
	}

	result, err := c.rpc.Call(ctx, "recall", params)
	if err != nil {
		return nil, fmt.Errorf("recall: %w", err)
	}

	var recallResult struct {
		Lines     []string `json:"lines"`
		Candidates int     `json:"candidates"`
		Warnings  []string `json:"warnings"`
	}
	if err := json.Unmarshal(result, &recallResult); err != nil {
		return nil, fmt.Errorf("parse recall result: %w", err)
	}

	return recallResult.Lines, nil
}

// Extract implements MemoryClient.Extract.
func (c *ManagedRuntimeClient) Extract(ctx context.Context, req ExtractRequest) error {
	if err := c.EnsureStarted(ctx); err != nil {
		return err
	}

	params := map[string]interface{}{
		"sessionId": req.SessionID,
		"force":     false,
	}

	_, err := c.rpc.Call(ctx, "extract", params)
	if err != nil {
		return fmt.Errorf("extract: %w", err)
	}

	return nil
}

// Consolidate implements MemoryClient.Consolidate.
func (c *ManagedRuntimeClient) Consolidate(ctx context.Context, req ConsolidateRequest) error {
	if err := c.EnsureStarted(ctx); err != nil {
		return err
	}

	params := map[string]interface{}{
		"force": req.Force,
	}

	_, err := c.rpc.Call(ctx, "consolidate", params)
	if err != nil {
		return fmt.Errorf("consolidate: %w", err)
	}

	return nil
}

// FreshnessStatus implements MemoryClient.FreshnessStatus.
func (c *ManagedRuntimeClient) FreshnessStatus(ctx context.Context) (FreshnessResult, error) {
	if err := c.EnsureStarted(ctx); err != nil {
		return FreshnessResult{HasMemories: false, Warning: err.Error()}, nil
	}

	// Check health
	healthResult, err := c.rpc.Call(ctx, "health", map[string]interface{}{})
	if err != nil {
		return FreshnessResult{HasMemories: false, Warning: err.Error()}, nil
	}

	var health struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(healthResult, &health); err != nil {
		return FreshnessResult{HasMemories: false, Warning: err.Error()}, nil
	}

	return FreshnessResult{
		HasMemories: true,
		Warning:     "",
	}, nil
}

// Store implements MemoryClient.Store.
func (c *ManagedRuntimeClient) Store(ctx context.Context, params StoreParams) error {
	if err := c.EnsureStarted(ctx); err != nil {
		return err
	}

	runtimeParams := map[string]interface{}{
		"record": map[string]interface{}{
			"title":   params.Key,
			"content": params.Value,
			"scope":   params.Scope,
			"tags":    params.Tags,
		},
	}

	_, err := c.rpc.Call(ctx, "store", runtimeParams)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}

	return nil
}

// Get implements MemoryClient.Get.
func (c *ManagedRuntimeClient) Get(ctx context.Context, key string) (Entry, error) {
	if err := c.EnsureStarted(ctx); err != nil {
		return Entry{}, err
	}

	params := map[string]interface{}{
		"id": key,
	}

	result, err := c.rpc.Call(ctx, "get", params)
	if err != nil {
		return Entry{}, fmt.Errorf("get: %w", err)
	}

	var getResult struct {
		Record *struct {
			ID        string   `json:"id"`
			Title     string   `json:"title"`
			Content   string   `json:"content"`
			Scope     string   `json:"scope"`
			Tags      []string `json:"tags"`
			CreatedAt string   `json:"createdAt"`
			UpdatedAt string   `json:"updatedAt"`
		} `json:"record"`
	}
	if err := json.Unmarshal(result, &getResult); err != nil {
		return Entry{}, fmt.Errorf("parse get result: %w", err)
	}

	if getResult.Record == nil {
		return Entry{}, ErrNotFound
	}

	return Entry{
		Key:       getResult.Record.ID,
		Value:     getResult.Record.Content,
		Scope:     getResult.Record.Scope,
		Tags:      getResult.Record.Tags,
		UpdatedAt: time.Now().Unix(),
	}, nil
}

// Delete implements MemoryClient.Delete.
func (c *ManagedRuntimeClient) Delete(ctx context.Context, key string) error {
	if err := c.EnsureStarted(ctx); err != nil {
		return err
	}

	params := map[string]interface{}{
		"id": key,
	}

	_, err := c.rpc.Call(ctx, "delete", params)
	if err != nil {
		return fmt.Errorf("delete: %w", err)
	}

	return nil
}

// Search implements MemoryClient.Search.
func (c *ManagedRuntimeClient) Search(ctx context.Context, params SearchParams) ([]Entry, error) {
	if err := c.EnsureStarted(ctx); err != nil {
		return nil, err
	}

	runtimeParams := map[string]interface{}{
		"query": params.Query,
		"scope": params.Scope,
		"limit": params.Limit,
	}

	result, err := c.rpc.Call(ctx, "search", runtimeParams)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}

	var searchResult struct {
		Records []struct {
			ID      string `json:"id"`
			Title   string `json:"title"`
			Content string `json:"content"`
			Scope   string `json:"scope"`
		} `json:"records"`
	}
	if err := json.Unmarshal(result, &searchResult); err != nil {
		return nil, fmt.Errorf("parse search result: %w", err)
	}

	entries := make([]Entry, len(searchResult.Records))
	for i, r := range searchResult.Records {
		entries[i] = Entry{
			Key:   r.ID,
			Value: r.Content,
			Scope: r.Scope,
		}
	}

	return entries, nil
}

// List implements MemoryClient.List.
func (c *ManagedRuntimeClient) List(ctx context.Context, params ListParams) ([]Entry, error) {
	if err := c.EnsureStarted(ctx); err != nil {
		return nil, err
	}

	runtimeParams := map[string]interface{}{
		"scope": params.Scope,
		"limit": params.Limit,
	}

	result, err := c.rpc.Call(ctx, "list", runtimeParams)
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}

	var listResult struct {
		Records []struct {
			ID        string `json:"id"`
			Title     string `json:"title"`
			Scope     string `json:"scope"`
			CreatedAt string `json:"createdAt"`
			UpdatedAt string `json:"updatedAt"`
		} `json:"records"`
	}
	if err := json.Unmarshal(result, &listResult); err != nil {
		return nil, fmt.Errorf("parse list result: %w", err)
	}

	entries := make([]Entry, len(listResult.Records))
	for i, r := range listResult.Records {
		entries[i] = Entry{
			Key:   r.ID,
			Value: r.Title, // Use title as value for list
			Scope: r.Scope,
		}
	}

	return entries, nil
}
