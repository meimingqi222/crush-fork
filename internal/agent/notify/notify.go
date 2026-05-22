// Package notify defines domain notification types for agent events.
// These types are decoupled from UI concerns so the agent can publish
// events without importing UI packages.
package notify

// Type identifies the kind of agent notification.
type Type string

const (
	// TypeAgentFinished indicates the agent has completed its turn.
	TypeAgentFinished Type = "agent_finished"
	// TypeSubagentFinished indicates a background subagent has completed.
	TypeSubagentFinished Type = "subagent_finished"
)

// Notification represents a domain event published by the agent.
type Notification struct {
	SessionID    string
	SessionTitle string
	Type         Type

	// Fields for background subagent completion
	SubagentID   string
	SubagentType string
	Status       string
	Summary      string
}
