package agent

import (
	"context"
	"log/slog"
	"strings"

	"github.com/charmbracelet/crush/internal/agent/notify"
	"github.com/charmbracelet/crush/internal/memory"
	"github.com/charmbracelet/crush/internal/pubsub"
)

// MemoryFreshnessStatus holds the result of MemoryFreshness.
// Warning is non-empty when memories are stale; HasMemories reports if any exist.
type MemoryFreshnessStatus struct {
	Warning     string
	HasMemories bool
}

func (c *coordinator) MemoryFreshness(ctx context.Context) (MemoryFreshnessStatus, error) {
	if c.longTermMemory == nil {
		return MemoryFreshnessStatus{}, nil
	}
	fr, err := c.longTermMemory.FreshnessStatus(ctx)
	if err != nil {
		return MemoryFreshnessStatus{}, err
	}
	return MemoryFreshnessStatus{
		Warning:     fr.Warning,
		HasMemories: fr.HasMemories,
	}, nil
}

func (c *coordinator) Dream(ctx context.Context, sessionID string, force bool) error {
	if c.longTermMemory == nil {
		return nil
	}
	title := c.memoryDreamSessionTitle(ctx, sessionID)
	c.publishMemoryDreamNotification(sessionID, title, notify.TypeMemoryDreamStarted)
	go func() {
		consolidateErr := c.longTermMemory.Consolidate(context.Background(), memory.ConsolidateRequest{
			SessionID: sessionID,
			Force:     force,
		})
		if consolidateErr != nil {
			slog.Warn("Memory consolidation failed", "error", consolidateErr)
			c.publishMemoryDreamNotification(sessionID, title, notify.TypeMemoryDreamFailed)
		} else {
			c.publishMemoryDreamNotification(sessionID, title, notify.TypeMemoryDreamFinished)
		}
	}()
	return nil
}

func (c *coordinator) maybeStartMemoryDream(ctx context.Context, sessionID string) {
	if err := c.Dream(ctx, sessionID, false); err != nil {
		slog.Debug("Skipping memory dream", "error", err)
	}
}

func (c *coordinator) memoryDreamSessionTitle(ctx context.Context, sessionID string) string {
	if strings.TrimSpace(sessionID) == "" || c.sessions == nil {
		return ""
	}
	sess, err := c.sessions.Get(ctx, sessionID)
	if err != nil {
		return ""
	}
	return sess.Title
}

func (c *coordinator) publishMemoryDreamNotification(sessionID, sessionTitle string, typ notify.Type) {
	if c.notify == nil {
		return
	}
	c.notify.Publish(pubsub.CreatedEvent, notify.Notification{SessionID: sessionID, SessionTitle: sessionTitle, Type: typ})
}
