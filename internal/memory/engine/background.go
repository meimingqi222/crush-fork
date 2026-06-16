package engine

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// backgroundPassTimeout bounds a single background pass. A pass is also
// cancelled immediately when its stop signal fires (see backgroundLoop and
// consolidationLoop), so this timeout only matters when the pass itself is
// unresponsive to context cancellation.
const backgroundPassTimeout = 60 * time.Second

// backgroundStopTimeout bounds how long stopBackground/stopConsolidation wait
// for a loop to exit. Because the loops cancel in-flight passes on stop, this
// only needs to cover the brief window for a cancelled pass to unwind — not
// the full pass timeout.
const backgroundStopTimeout = 10 * time.Second

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
			// Derive the pass context from backgroundPassTimeout, but also
			// cancel it the moment bgStop fires. This keeps shutdown fast:
			// an in-flight pass gets interrupted immediately instead of
			// running to its full timeout.
			passCtx, cancel := context.WithTimeout(ctx, backgroundPassTimeout)
			stopDone := make(chan struct{})
			go func() {
				select {
				case <-e.bgStop:
					cancel()
				case <-stopDone:
				}
			}()
			if err := e.runBackgroundPass(passCtx); err != nil && !errors.Is(err, context.Canceled) {
				slog.Warn("Background materialization pass failed", "error", err)
			}
			close(stopDone)
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

// stopBackground is internal: signals the loop and waits for it to finish.
// Because the loop cancels its in-flight pass on stop, the wait is short.
// See backgroundStopTimeout.
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
	case <-time.After(backgroundStopTimeout):
	}
}

// StartBackgroundConsolidator launches a background goroutine that
// periodically triggers consolidation until Close() is called. Safe to call
// multiple times; subsequent calls become no-ops. Returns immediately.
//
// This complements deletion-time (OnSessionDeleted) and shutdown-time (Flush)
// consolidation: long-running sessions still merge episodic events into durable
// cross-session memory at the configured interval. The pass body delegates to
// TriggerConsolidation, which is already concurrency-safe (DB lease +
// pipelineMu + degraded check), so no additional locking is required here.
func (e *Engine) StartBackgroundConsolidator(ctx context.Context) {
	if e == nil {
		return
	}
	e.consBgMu.Lock()
	if e.consBgStarted || e.consBgInterval <= 0 {
		e.consBgMu.Unlock()
		return
	}
	e.consBgStarted = true
	e.consBgStop = make(chan struct{})
	e.consBgDone = make(chan struct{})
	interval := e.consBgInterval
	e.consBgMu.Unlock()

	go e.consolidationLoop(ctx, interval)
}

func (e *Engine) consolidationLoop(ctx context.Context, interval time.Duration) {
	defer close(e.consBgDone)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-e.consBgStop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Cancel the pass the moment consBgStop fires so shutdown is
			// fast even when a pass is mid-flight. See backgroundLoop.
			passCtx, cancel := context.WithTimeout(ctx, backgroundPassTimeout)
			stopDone := make(chan struct{})
			go func() {
				select {
				case <-e.consBgStop:
					cancel()
				case <-stopDone:
				}
			}()
			if err := e.runConsolidationPass(passCtx); err != nil && !errors.Is(err, context.Canceled) {
				slog.Warn("Background consolidation pass failed", "error", err)
			}
			close(stopDone)
			cancel()
		}
	}
}

// runConsolidationPass executes a single background consolidation. It is
// safe to invoke from goroutines. Mirrors runBackgroundPass but calls
// TriggerConsolidation instead of TriggerMaterialization.
func (e *Engine) runConsolidationPass(ctx context.Context) error {
	if e == nil || !e.enabled {
		return nil
	}
	if e.IsDegraded() {
		slog.Debug("Background consolidation skipped: engine degraded")
		return nil
	}
	if e.consolidator == nil {
		return nil
	}
	if err := e.TriggerConsolidation(ctx); err != nil {
		return err
	}
	now := time.Now()
	e.consBgMu.Lock()
	e.lastConsBgRun = &now
	e.consBgMu.Unlock()
	return nil
}

// LastBackgroundConsolidation reports the timestamp of the most recent
// successful background consolidation pass, or nil if none.
func (e *Engine) LastBackgroundConsolidation() *time.Time {
	if e == nil {
		return nil
	}
	e.consBgMu.Lock()
	defer e.consBgMu.Unlock()
	if e.lastConsBgRun == nil {
		return nil
	}
	t := *e.lastConsBgRun
	return &t
}

// stopConsolidation is internal: signals the loop and waits for it to finish.
// See stopBackground for the rationale on the wait duration.
func (e *Engine) stopConsolidation() {
	if e == nil {
		return
	}
	e.consBgMu.Lock()
	if !e.consBgStarted {
		e.consBgMu.Unlock()
		return
	}
	stop := e.consBgStop
	done := e.consBgDone
	e.consBgStarted = false
	e.consBgMu.Unlock()
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
	case <-time.After(backgroundStopTimeout):
	}
}
