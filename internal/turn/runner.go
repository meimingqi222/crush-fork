package turn

import (
	"context"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/message"
)

type CoordinatorRunner struct{ Coordinator agent.Coordinator }

func (r CoordinatorRunner) Run(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) error {
	_, err := r.Coordinator.Run(ctx, sessionID, prompt, attachments...)
	return err
}

func (r CoordinatorRunner) Cancel(sessionID string) { r.Coordinator.Cancel(sessionID) }

func (r CoordinatorRunner) RemoveQueuedTurn(sessionID, turnID string) bool {
	return r.Coordinator.RemoveQueuedTurn(sessionID, turnID)
}

func (r CoordinatorRunner) Steer(sessionID, prompt string, attachments ...message.Attachment) bool {
	return r.Coordinator.Steer(sessionID, prompt, attachments...)
}
