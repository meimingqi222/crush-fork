package engine

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

const (
	embeddingBatchSize    = 10
	embeddingBatchDelay   = 500 * time.Millisecond
	embeddingMaxQueueSize = 1000
)

type EmbeddingPipeline struct {
	store     EventStore
	embStore  *EmbeddingStore
	embedder  Embedder
	now       func() time.Time
	mu        sync.Mutex
	queue     []string
	stopped   bool
	stopCh    chan struct{}
	running   bool
}

func NewEmbeddingPipeline(store EventStore, embStore *EmbeddingStore, embedder Embedder) *EmbeddingPipeline {
	return &EmbeddingPipeline{
		store:    store,
		embStore: embStore,
		embedder: embedder,
		now:      time.Now,
		stopCh:   make(chan struct{}),
	}
}

func (p *EmbeddingPipeline) SetEmbedder(embedder Embedder) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.embedder = embedder
	p.mu.Unlock()
}

func (p *EmbeddingPipeline) Enqueue(eventIDs ...string) {
	if p == nil || p.embStore == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.stopped {
		return
	}

	for _, id := range eventIDs {
		if id == "" {
			continue
		}
		if p.embStore.IsPending(id) {
			continue
		}
		if len(p.queue) >= embeddingMaxQueueSize {
			slog.Debug("Embedding queue full, dropping event", "event_id", id)
			return
		}
		p.queue = append(p.queue, id)
		p.embStore.MarkPending(id)
	}

	if !p.running && len(p.queue) > 0 {
		p.running = true
		go p.run()
	}
}

func (p *EmbeddingPipeline) run() {
	defer func() {
		p.mu.Lock()
		p.running = false
		p.mu.Unlock()
	}()

	timer := time.NewTimer(embeddingBatchDelay)
	defer timer.Stop()

	for {
		select {
		case <-p.stopCh:
			p.processBatch(context.Background())
			return
		case <-timer.C:
			timer.Reset(embeddingBatchDelay)
		}

		p.processBatch(context.Background())

		p.mu.Lock()
		remaining := len(p.queue)
		p.mu.Unlock()
		if remaining == 0 {
			return
		}
	}
}

func (p *EmbeddingPipeline) processBatch(ctx context.Context) {
	p.mu.Lock()
	if p.embedder == nil {
		p.queue = nil
		p.mu.Unlock()
		return
	}

	batchSize := embeddingBatchSize
	if len(p.queue) < batchSize {
		batchSize = len(p.queue)
	}
	batch := make([]string, batchSize)
	copy(batch, p.queue[:batchSize])
	p.queue = p.queue[batchSize:]
	modelName := p.embedder.Name()
	p.mu.Unlock()

	if len(batch) == 0 {
		return
	}

	events, err := p.store.Query(ctx, EventFilter{Limit: 1000})
	if err != nil {
		slog.Debug("Embedding pipeline: query events failed", "error", err)
		return
	}

	eventMap := make(map[string]MemoryEvent, len(events))
	for _, evt := range events {
		eventMap[evt.ID] = evt
	}

	for _, eventID := range batch {
		evt, ok := eventMap[eventID]
		if !ok {
			slog.Debug("Embedding pipeline: event not found", "event_id", eventID)
			continue
		}

		text := embeddingEventText(evt)
		vec, err := p.embedder.Embed(ctx, text)
		if err != nil {
			slog.Debug("Embedding pipeline: embed failed", "error", err, "event_id", eventID)
			p.mu.Lock()
			delete(p.embStore.pending, eventID)
			p.mu.Unlock()
			continue
		}

		if err := p.embStore.Put(ctx, eventID, modelName, vec); err != nil {
			slog.Debug("Embedding pipeline: store failed", "error", err, "event_id", eventID)
		}
	}
}

func (p *EmbeddingPipeline) Start() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		p.stopped = false
		p.stopCh = make(chan struct{})
	}
}

func (p *EmbeddingPipeline) Stop() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	p.stopped = true
	close(p.stopCh)
	p.mu.Unlock()
}

func (p *EmbeddingPipeline) EmbedEvent(ctx context.Context, evt MemoryEvent) ([]float64, error) {
	if p == nil || p.embedder == nil {
		return nil, nil
	}

	modelName := p.embedder.Name()

	if p.embStore != nil {
		cached, err := p.embStore.Get(ctx, evt.ID, modelName)
		if err == nil && cached != nil {
			return cached, nil
		}
	}

	text := embeddingEventText(evt)
	vec, err := p.embedder.Embed(ctx, text)
	if err != nil {
		return nil, err
	}

	if p.embStore != nil && len(vec) > 0 {
		_ = p.embStore.Put(ctx, evt.ID, modelName, vec)
	}

	return vec, nil
}

func (p *EmbeddingPipeline) EmbedEvents(ctx context.Context, events []MemoryEvent) (map[string][]float64, error) {
	result := make(map[string][]float64, len(events))
	if p == nil || p.embedder == nil {
		return result, nil
	}

	modelName := p.embedder.Name()
	eventIDs := make([]string, len(events))
	for i, evt := range events {
		eventIDs[i] = evt.ID
	}

	var cached map[string][]float64
	if p.embStore != nil {
		var err error
		cached, err = p.embStore.GetBatch(ctx, eventIDs, modelName)
		if err != nil {
			slog.Debug("Batch get cached embeddings failed", "error", err)
			cached = make(map[string][]float64)
		}
	} else {
		cached = make(map[string][]float64)
	}

	for id, vec := range cached {
		result[id] = vec
	}

	missing := make([]MemoryEvent, 0)
	for _, evt := range events {
		if _, ok := cached[evt.ID]; !ok {
			missing = append(missing, evt)
		}
	}

	if len(missing) > 0 && p.embStore != nil {
		needEmbed, err := p.embStore.FindMissing(ctx, eventIDs, modelName)
		if err == nil {
			needEmbedSet := make(map[string]bool, len(needEmbed))
			for _, id := range needEmbed {
				needEmbedSet[id] = true
			}
			filtered := make([]MemoryEvent, 0, len(missing))
			for _, evt := range missing {
				if needEmbedSet[evt.ID] {
					filtered = append(filtered, evt)
				}
			}
			missing = filtered
		}
	}

	for _, evt := range missing {
		text := embeddingEventText(evt)
		vec, err := p.embedder.Embed(ctx, text)
		if err != nil {
			slog.Debug("Embed event failed", "error", err, "event_id", evt.ID)
			continue
		}
		result[evt.ID] = vec
		if p.embStore != nil {
			_ = p.embStore.Put(ctx, evt.ID, modelName, vec)
		}
	}

	return result, nil
}