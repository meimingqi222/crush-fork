package agent

import (
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/oauth/copilot"
)

// Queue management methods on the coordinator. These are thin delegations to
// the current session agent's queue implementation.

func (c *coordinator) RemoveQueuedPrompt(sessionID string, index int) bool {
	return c.currentAgent.RemoveQueuedPrompt(sessionID, index)
}

func (c *coordinator) ClearQueue(sessionID string) {
	c.currentAgent.ClearQueue(sessionID)
}

func (c *coordinator) PauseQueue(sessionID string) {
	c.currentAgent.PauseQueue(sessionID)
}

func (c *coordinator) ResumeQueue(sessionID string) {
	c.currentAgent.ResumeQueue(sessionID)
}

func (c *coordinator) IsQueuePaused(sessionID string) bool {
	return c.currentAgent.IsQueuePaused(sessionID)
}

func (c *coordinator) PrioritizeQueuedPrompt(sessionID string, index int) bool {
	return c.currentAgent.PrioritizeQueuedPrompt(sessionID, index)
}

func (c *coordinator) QueuedPrompts(sessionID string) int {
	return c.currentAgent.QueuedPrompts(sessionID)
}

func (c *coordinator) RemoveQueuedTurn(sessionID, turnID string) bool {
	return c.currentAgent.RemoveQueuedTurn(sessionID, turnID)
}

func (c *coordinator) Steer(sessionID, prompt string, attachments ...message.Attachment) bool {
	return c.currentAgent.EnqueueSteer(sessionID, SessionAgentCall{
		SessionID:     sessionID,
		Prompt:        prompt,
		Attachments:   attachments,
		InitiatorType: copilot.InitiatorUser,
	})
}

func (c *coordinator) QueuedPromptsList(sessionID string) []string {
	return c.currentAgent.QueuedPromptsList(sessionID)
}
