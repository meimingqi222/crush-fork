package memory

import "context"

// NullMemoryClient is a no-op MemoryClient used when memory is disabled
// (e.g. DisableAutoMemory=true or no backend configured).
// All write operations succeed silently; all read operations return empty results.
type NullMemoryClient struct{}

func (NullMemoryClient) AppendMessages(_ context.Context, _ string, _ []AppendMessage) error {
	return nil
}
func (NullMemoryClient) Recall(_ context.Context, _, _, _ string) ([]string, error) { return nil, nil }
func (NullMemoryClient) Extract(_ context.Context, _ ExtractRequest) error          { return nil }
func (NullMemoryClient) Consolidate(_ context.Context, _ ConsolidateRequest) error  { return nil }
func (NullMemoryClient) FreshnessStatus(_ context.Context) (FreshnessResult, error) {
	return FreshnessResult{}, nil
}
func (NullMemoryClient) Store(_ context.Context, _ StoreParams) error              { return nil }
func (NullMemoryClient) Get(_ context.Context, _ string) (Entry, error)            { return Entry{}, ErrNotFound }
func (NullMemoryClient) Delete(_ context.Context, _ string) error                  { return ErrNotFound }
func (NullMemoryClient) Search(_ context.Context, _ SearchParams) ([]Entry, error) { return nil, nil }
func (NullMemoryClient) List(_ context.Context, _ ListParams) ([]Entry, error)     { return nil, nil }
