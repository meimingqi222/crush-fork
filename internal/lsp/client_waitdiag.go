package lsp

import (
	"context"
	"time"
)

// WaitForDiagnostics waits until diagnostics change or the timeout is reached.
// Uses an event-driven channel from VersionedMap.WaitForChange instead of
// polling, so it returns as soon as diagnostics are published rather than
// waiting up to 200ms for the next poll tick.
func (c *Client) WaitForDiagnostics(ctx context.Context, d time.Duration) {
	if c == nil {
		return
	}
	pv := c.diagnostics.Version()
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		ch := c.diagnostics.WaitForChange(pv)
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			return
		case <-ch:
			pv = c.diagnostics.Version()
			return
		}
	}
}
