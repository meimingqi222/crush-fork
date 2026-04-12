package memory

import (
	"context"
	"sync"
	"time"
)

// InMemoryClient is a simple in-memory MemoryClient for testing.
type InMemoryClient struct {
	mu      sync.RWMutex
	entries map[string]Entry
}

// NewInMemoryClient creates a new InMemoryClient for testing.
func NewInMemoryClient() *InMemoryClient {
	return &InMemoryClient{
		entries: make(map[string]Entry),
	}
}

func (c *InMemoryClient) AppendMessages(_ context.Context, _ string, _ []AppendMessage) error {
	return nil
}

func (c *InMemoryClient) Recall(_ context.Context, query, _, _ string) ([]string, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var lines []string
	for _, e := range c.entries {
		if containsIgnoreCase(e.Value, query) {
			lines = append(lines, e.Value)
		}
	}
	return lines, nil
}

func (c *InMemoryClient) Extract(_ context.Context, _ ExtractRequest) error {
	return nil
}

func (c *InMemoryClient) Consolidate(_ context.Context, _ ConsolidateRequest) error {
	return nil
}

func (c *InMemoryClient) FreshnessStatus(_ context.Context) (FreshnessResult, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return FreshnessResult{HasMemories: len(c.entries) > 0}, nil
}

func (c *InMemoryClient) Store(_ context.Context, params StoreParams) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[params.Key] = Entry{
		Key:       params.Key,
		Value:     params.Value,
		Scope:     params.Scope,
		Category:  params.Category,
		Type:      params.Type,
		Tags:      params.Tags,
		UpdatedAt: time.Now().Unix(),
	}
	return nil
}

func (c *InMemoryClient) Get(_ context.Context, key string) (Entry, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[key]
	if !ok {
		return Entry{}, ErrNotFound
	}
	return e, nil
}

func (c *InMemoryClient) Delete(_ context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[key]; !ok {
		return ErrNotFound
	}
	delete(c.entries, key)
	return nil
}

func (c *InMemoryClient) Search(_ context.Context, params SearchParams) ([]Entry, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var result []Entry
	for _, e := range c.entries {
		if params.Scope != "" && e.Scope != params.Scope {
			continue
		}
		if params.Category != "" && e.Category != params.Category {
			continue
		}
		if params.Type != "" && e.Type != params.Type {
			continue
		}
		if params.Query != "" && !containsIgnoreCase(e.Value, params.Query) {
			continue
		}
		result = append(result, e)
		if params.Limit > 0 && len(result) >= params.Limit {
			break
		}
	}
	return result, nil
}

func (c *InMemoryClient) List(_ context.Context, params ListParams) ([]Entry, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var result []Entry
	for _, e := range c.entries {
		if params.Scope != "" && e.Scope != params.Scope {
			continue
		}
		if params.Category != "" && e.Category != params.Category {
			continue
		}
		if params.Type != "" && e.Type != params.Type {
			continue
		}
		result = append(result, e)
		if params.Limit > 0 && len(result) >= params.Limit {
			break
		}
	}
	return result, nil
}

func containsIgnoreCase(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && containsLower(toLower(s), toLower(substr))))
}

func containsLower(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func toLower(s string) string {
	b := make([]byte, len(s))
	for i := range s {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}
