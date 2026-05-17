package engine

import (
	"context"
	"log/slog"
	"time"
)

// StartBackgroundMaterializer launches a background goroutine that
// periodically triggers materialization until Close() is called. Safe to
// call multiple times; subsequent calls become no-ops. Returns immediately.
//
// The loop runs at the configured interval (Engine.bgInterval). It skips
// passes when the engine is disabled, in degraded mode, or has no
// materializers configured.
func (e *Engine) StartBackgroundMaterializer(ctx context.Context) {
	if e == nil {
		return
	}
	e.bgMu.Lock()
	if e.bgStarted || e.bgInterval <= 0 {
		e.bgMu.Unlock()
		return
	}
	e.bgStarted = true
	e.bgStop = make(chan struct{})
	e.bgDone = make(chan struct{})
	interval := e.bgInterval
	e.bgMu.Unlock()

	go e.backgroundLoop(ctx, interval)
}

func (e *Engine) backgroundLoop(ctx context.Context, interval time.Duration) {
	defer close(e.bgDone)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-e.bgStop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			passCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			if err := e.runBackgroundPass(passCtx); err != nil {
				slog.Warn("Background materialization pass failed", "error", err)
			}
			cancel()
		}
	}
}

// runBackgroundPass executes a single background materialization. It is
// safe to invoke from goroutines or the turn-counter trigger.
func (e *Engine) runBackgroundPass(ctx context.Context) error {
	if e == nil || !e.enabled {
		return nil
	}
	if e.IsDegraded() {
		slog.Debug("Background materialization skipped: engine degraded")
		return nil
	}
	if len(e.materializers) == 0 {
		return nil
	}
	if err := e.TriggerMaterialization(ctx); err != nil {
		return err
	}
	now := time.Now()
	e.bgMu.Lock()
	e.lastBgRun = &now
	e.bgMu.Unlock()
	return nil
}

// LastBackgroundRun reports the timestamp of the most recent successful
// background materialization, or nil if none.
func (e *Engine) LastBackgroundRun() *time.Time {
	if e == nil {
		return nil
	}
	e.bgMu.Lock()
	defer e.bgMu.Unlock()
	if e.lastBgRun == nil {
		return nil
	}
	t := *e.lastBgRun
	return &t
}

// stopBackground is internal: signals the loop and waits up to 2s.
func (e *Engine) stopBackground() {
	if e == nil {
		return
	}
	e.bgMu.Lock()
	if !e.bgStarted {
		e.bgMu.Unlock()
		return
	}
	stop := e.bgStop
	done := e.bgDone
	e.bgStarted = false
	e.bgMu.Unlock()
	if stop == nil {
		return
	}
	// signal once
	select {
	case <-stop:
	default:
		close(stop)
	}
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
}
