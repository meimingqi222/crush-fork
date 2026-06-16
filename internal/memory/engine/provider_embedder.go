package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ProviderEmbedder generates embeddings via an OpenAI-compatible /v1/embeddings
// API endpoint.  It supports any provider that follows the OpenAI embedding
// response format (OpenAI, Azure, Ollama, vLLM, etc.).
//
// Embeddings are cached in-memory by text hash to avoid redundant API calls
// within a single session.  The cache is bounded to cacheMaxEntries items
// with an LRU eviction policy.
type ProviderEmbedder struct {
	apiURL     string
	apiKey     string
	model      string
	dimensions int
	client     *http.Client

	mu    sync.RWMutex
	cache map[string][]float64
	keys  []string // LRU order
}

const (
	defaultEmbeddingModel = "text-embedding-3-small"
	providerCacheMax      = 4096
)

// ProviderEmbedderConfig holds the configuration for a ProviderEmbedder.
type ProviderEmbedderConfig struct {
	// APIURL is the base URL for the embedding API (e.g.
	// "https://api.openai.com/v1").  The embedder appends "/embeddings".
	APIURL string
	// APIKey is the bearer token for the API.
	APIKey string
	// Model is the embedding model name (default: text-embedding-3-small).
	Model string
	// Dimensions is the output vector dimensionality (0 = use model default).
	Dimensions int
}

// NewProviderEmbedder creates a ProviderEmbedder from the given config.
func NewProviderEmbedder(cfg ProviderEmbedderConfig) *ProviderEmbedder {
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		model = defaultEmbeddingModel
	}
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.APIURL), "/")
	return &ProviderEmbedder{
		apiURL:     baseURL,
		apiKey:     strings.TrimSpace(cfg.APIKey),
		model:      model,
		dimensions: cfg.Dimensions,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		cache: make(map[string][]float64, 64),
		keys:  make([]string, 0, 64),
	}
}

func (p *ProviderEmbedder) Name() string { return "provider:" + p.model }

// Embed returns the embedding vector for the given text, using the configured
// provider API.  Results are cached in memory.
func (p *ProviderEmbedder) Embed(ctx context.Context, text string) ([]float64, error) {
	key := cacheKey(text)

	// Check cache.
	p.mu.RLock()
	if vec, ok := p.cache[key]; ok {
		p.mu.RUnlock()
		return vec, nil
	}
	p.mu.RUnlock()

	// Call the API.
	vec, err := p.callAPI(ctx, text)
	if err != nil {
		// Fallback to hashing embedder on API failure so retrieval
		// still works, just with lower quality.
		slog.Warn("Provider embedding failed, falling back to hashing",
			"error", err, "model", p.model)
		fallback := NewHashingEmbedder(defaultHashingEmbeddingDimensions)
		return fallback.Embed(ctx, text)
	}

	// Store in cache.
	p.mu.Lock()
	if len(p.cache) >= providerCacheMax {
		// Evict oldest entry (simple LRU).
		evictKey := p.keys[0]
		delete(p.cache, evictKey)
		p.keys = p.keys[1:]
	}
	p.cache[key] = vec
	p.keys = append(p.keys, key)
	p.mu.Unlock()

	return vec, nil
}

func (p *ProviderEmbedder) callAPI(ctx context.Context, text string) ([]float64, error) {
	reqBody := embeddingRequest{
		Input: text,
		Model: p.model,
	}
	if p.dimensions > 0 {
		reqBody.Dimensions = p.dimensions
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshaling embedding request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.apiURL+"/embeddings", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("creating embedding request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling embedding API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("embedding API returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result embeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding embedding response: %w", err)
	}

	if len(result.Data) == 0 {
		return nil, fmt.Errorf("embedding API returned no data")
	}

	vec := result.Data[0].Embedding
	normalizeVector(vec)
	return vec, nil
}

// CosineSimilarity computes the cosine similarity between two vectors.
func CosineSimilarity(a, b []float64) float64 {
	return dotProduct(a, b) // Both vectors are already normalized.
}

// HybridScore blends vector similarity with a heuristic score.
// alpha controls the weight of semantic similarity (0 = pure heuristic,
// 1 = pure vector similarity).
func HybridScore(similarity, heuristicScore, alpha float64) float64 {
	return alpha*similarity + (1-alpha)*heuristicScore
}

// MMRSelect selects a diverse subset of candidates using Maximal Marginal
// Relevance.  It balances relevance to the query with diversity among
// selected items.  lambda=1.0 is pure relevance, lambda=0.0 is pure diversity.
func MMRSelect(
	ctx context.Context,
	queryVec []float64,
	candidates []MemoryEvent,
	embedder Embedder,
	lambda float64,
	maxResults int,
) ([]MemoryEvent, error) {
	if len(candidates) <= maxResults {
		return candidates, nil
	}
	if lambda <= 0 {
		lambda = 0.7
	}
	if maxResults <= 0 {
		maxResults = 10
	}

	// Pre-compute all candidate embeddings.
	type candidateInfo struct {
		evt   MemoryEvent
		vec   []float64
		score float64
	}
	infos := make([]candidateInfo, 0, len(candidates))
	for _, c := range candidates {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		vec, err := embedder.Embed(ctx, embeddingEventText(c))
		if err != nil {
			return nil, err
		}
		infos = append(infos, candidateInfo{
			evt:   c,
			vec:   vec,
			score: dotProduct(queryVec, vec),
		})
	}

	selected := make([]candidateInfo, 0, maxResults)
	remaining := make(map[int]bool, len(infos))
	for i := range infos {
		remaining[i] = true
	}

	for len(selected) < maxResults && len(remaining) > 0 {
		bestIdx := -1
		bestScore := math.Inf(-1)

		for idx := range remaining {
			relevance := infos[idx].score

			// Compute max similarity to already-selected items.
			maxSim := 0.0
			for _, s := range selected {
				sim := dotProduct(infos[idx].vec, s.vec)
				if sim > maxSim {
					maxSim = sim
				}
			}

			mmrScore := lambda*relevance - (1-lambda)*maxSim
			if mmrScore > bestScore {
				bestScore = mmrScore
				bestIdx = idx
			}
		}

		if bestIdx < 0 {
			break
		}
		selected = append(selected, infos[bestIdx])
		delete(remaining, bestIdx)
	}

	result := make([]MemoryEvent, 0, len(selected))
	for _, s := range selected {
		result = append(result, s.evt)
	}
	return result, nil
}

// cacheKey produces a deterministic cache key from text content.
func cacheKey(text string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(text))
	return fmt.Sprintf("%016x", h.Sum64())
}

// --- JSON request/response types for OpenAI-compatible embedding API ---

type embeddingRequest struct {
	Input      string `json:"input"`
	Model      string `json:"model"`
	Dimensions int    `json:"dimensions,omitempty"`
}

type embeddingResponse struct {
	Object string `json:"object"`
	Data   []struct {
		Object    string    `json:"object"`
		Embedding []float64 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
	Model string `json:"model"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}
