package agent

import (
	"context"
	"sync"
	"time"

	"github.com/charmbracelet/crush/internal/guimetrics"
)

const streamMessageFlushInterval = 150 * time.Millisecond

// streamMessageFlusher coalesces message persistence during streaming so each
// token delta does not trigger a separate DB write and pubsub event.
type streamMessageFlusher struct {
	ctx      context.Context
	flush    func() error
	mu       sync.Mutex
	dirty    bool
	stopCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once
}

func newStreamMessageFlusher(ctx context.Context, flush func() error) *streamMessageFlusher {
	f := &streamMessageFlusher{
		ctx:    ctx,
		flush:  flush,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
	go f.loop()
	return f
}

func (f *streamMessageFlusher) loop() {
	defer close(f.doneCh)
	ticker := time.NewTicker(streamMessageFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-f.ctx.Done():
			f.flushLocked()
			return
		case <-f.stopCh:
			f.flushLocked()
			return
		case <-ticker.C:
			f.flushLocked()
		}
	}
}

func (f *streamMessageFlusher) MarkDirty() {
	f.mu.Lock()
	f.dirty = true
	f.mu.Unlock()
}

func (f *streamMessageFlusher) FlushNow() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// FlushNow is an explicit flush request from the caller (e.g. OnToolCall,
	// OnReasoningEnd) — the caller has already mutated currentAssistant in
	// memory and needs it persisted immediately. Force dirty=true so the
	// flush is not skipped by the dirty-flag optimization, which would drop
	// tool-call updates when no text/reasoning delta preceded them.
	f.dirty = true
	return f.flushIfDirtyLocked()
}

func (f *streamMessageFlusher) Stop() {
	f.stopOnce.Do(func() {
		close(f.stopCh)
	})
	<-f.doneCh
}

func (f *streamMessageFlusher) flushLocked() {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = f.flushIfDirtyLocked()
}

func (f *streamMessageFlusher) flushIfDirtyLocked() error {
	if !f.dirty {
		return nil
	}
	if f.flush == nil {
		f.dirty = false
		return nil
	}
	started := time.Now()
	err := f.flush()
	outcome := "success"
	if err != nil {
		outcome = "error"
	}
	guimetrics.FromContext(f.ctx).ObserveDuration(guimetrics.SQLiteFlushDuration, time.Since(started), guimetrics.Labels{Outcome: outcome})
	if err != nil {
		// Keep dirty so the next tick or FlushNow retries the failed write.
		f.dirty = true
		return err
	}
	f.dirty = false
	return nil
}
