package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/charmbracelet/crush/internal/agent/mailbox"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
)

type subagentBridge struct {
	service   mailbox.Service
	session   session.Service
	mailbox   string
	sessionID string

	mu    sync.Mutex
	state map[string]*subagentNodeState
}

type subagentNodeState struct {
	AgentName   string
	Description string
	Status      message.ToolResultSubtaskStatus
	Content     string
	Messages    []string
	ToolUses    int
	LastTool    string
}

type subagentMailboxEffects struct {
	Messages []string
	Stop     bool
	Reason   string
}

func newSubagentBridge(service mailbox.Service, sessions session.Service, sessionID, mailboxID string, tasks []subagentTask) (*subagentBridge, error) {
	if service == nil {
		return nil, fmt.Errorf("mailbox service is not configured")
	}
	ids := make([]string, 0, len(tasks))
	state := make(map[string]*subagentNodeState, len(tasks))
	for _, task := range tasks {
		name := strings.TrimSpace(task.Name)
		if name == "" {
			continue
		}
		ids = append(ids, name)
		state[name] = &subagentNodeState{
			AgentName:   name,
			Description: strings.TrimSpace(task.Description),
			Status:      message.ToolResultSubtaskStatusPending,
			Messages:    []string{},
		}
	}
	if err := service.Open(mailboxID, ids); err != nil {
		return nil, err
	}
	bridge := &subagentBridge{
		service:   service,
		session:   sessions,
		mailbox:   strings.TrimSpace(mailboxID),
		sessionID: strings.TrimSpace(sessionID),
		state:     state,
	}
	bridge.syncTodos(context.Background())
	return bridge, nil
}

func (b *subagentBridge) Close() {
	if b == nil {
		return
	}
	b.service.Close(b.mailbox)
}

func (b *subagentBridge) UpdateProgress(agentName string, toolUses int, lastTool string) {
	if b == nil {
		return
	}
	name := strings.TrimSpace(agentName)
	b.mu.Lock()
	node, ok := b.state[name]
	if !ok {
		b.mu.Unlock()
		return
	}
	node.ToolUses = toolUses
	if lastTool != "" {
		node.LastTool = lastTool
	}
	b.mu.Unlock()
	b.syncTodos(context.Background())
}

func (b *subagentBridge) MarkPending(agentName string) {
	b.update(agentName, message.ToolResultSubtaskStatusPending, "")
}

func (b *subagentBridge) MarkInProgress(agentName string) {
	b.update(agentName, message.ToolResultSubtaskStatusInProgress, "")
}

func (b *subagentBridge) MarkResult(agentName string, status message.ToolResultSubtaskStatus, content string) {
	b.update(agentName, status, content)
}

func (b *subagentBridge) Consume(agentName string) (subagentMailboxEffects, error) {
	if b == nil {
		return subagentMailboxEffects{}, nil
	}
	envelopes, err := b.service.Consume(b.mailbox, agentName)
	if err != nil {
		return subagentMailboxEffects{}, err
	}
	if len(envelopes) == 0 {
		return subagentMailboxEffects{}, nil
	}

	effects := subagentMailboxEffects{}
	messages := make([]string, 0, len(envelopes))
	for _, envelope := range envelopes {
		switch envelope.Kind {
		case mailbox.EnvelopeKindMessage:
			if envelope.Message != "" {
				messages = append(messages, envelope.Message)
			}
		case mailbox.EnvelopeKindStop:
			effects.Stop = true
			effects.Reason = strings.TrimSpace(envelope.Reason)
		}
	}
	if len(messages) > 0 {
		effects.Messages = append(effects.Messages, messages...)
		b.mu.Lock()
		if node, ok := b.state[strings.TrimSpace(agentName)]; ok {
			node.Messages = append(node.Messages, messages...)
		}
		b.mu.Unlock()
		b.syncTodos(context.Background())
	}
	if effects.Stop {
		if effects.Reason == "" {
			effects.Reason = "Task stop requested via mailbox."
		}
		b.update(agentName, message.ToolResultSubtaskStatusCanceled, effects.Reason)
	}
	return effects, nil
}

func (b *subagentBridge) update(agentName string, status message.ToolResultSubtaskStatus, content string) {
	if b == nil {
		return
	}
	name := strings.TrimSpace(agentName)
	b.mu.Lock()
	node, ok := b.state[name]
	if !ok {
		b.mu.Unlock()
		return
	}
	node.Status = status
	trimmed := strings.TrimSpace(content)
	if trimmed != "" {
		node.Content = trimmed
	}
	b.mu.Unlock()
	b.syncTodos(context.Background())
}

func (b *subagentBridge) syncTodos(ctx context.Context) {
	if b == nil || b.session == nil || b.sessionID == "" {
		return
	}
	sess, err := b.session.Get(ctx, b.sessionID)
	if err != nil {
		return
	}

	b.mu.Lock()
	nodes := make([]*subagentNodeState, 0, len(b.state))
	for _, node := range b.state {
		nodes = append(nodes, &subagentNodeState{
			AgentName:   node.AgentName,
			Description: node.Description,
			Status:      node.Status,
			Content:     node.Content,
			Messages:    append([]string(nil), node.Messages...),
			ToolUses:    node.ToolUses,
			LastTool:    node.LastTool,
		})
	}
	b.mu.Unlock()

	todos := make([]session.Todo, 0, len(nodes))
	for _, node := range nodes {
		todos = append(todos, session.Todo{
			ID:         node.AgentName,
			Content:    subagentTodoContent(node),
			Status:     subagentTodoStatus(node.Status),
			Progress:   subagentTodoProgress(node.Status, node.ToolUses),
			ActiveForm: subagentTodoActiveForm(node),
		})
	}
	sess.Todos = todos
	_, _ = b.session.Save(ctx, sess)
}

func subagentTodoStatus(status message.ToolResultSubtaskStatus) session.TodoStatus {
	switch status {
	case message.ToolResultSubtaskStatusInProgress:
		return session.TodoStatusInProgress
	case message.ToolResultSubtaskStatusCompleted, message.ToolResultSubtaskStatusCompletedWithWarnings:
		return session.TodoStatusCompleted
	case message.ToolResultSubtaskStatusFailed, message.ToolResultSubtaskStatusBlocked:
		return session.TodoStatusFailed
	case message.ToolResultSubtaskStatusCanceled:
		return session.TodoStatusCanceled
	default:
		return session.TodoStatusPending
	}
}

func subagentTodoProgress(status message.ToolResultSubtaskStatus, toolUses int) int {
	switch status {
	case message.ToolResultSubtaskStatusCompleted, message.ToolResultSubtaskStatusCompletedWithWarnings, message.ToolResultSubtaskStatusFailed, message.ToolResultSubtaskStatusCanceled, message.ToolResultSubtaskStatusBlocked:
		return 100
	case message.ToolResultSubtaskStatusInProgress:
		return min(95, 10+toolUses*5)
	default:
		return 0
	}
}

func subagentTodoActiveForm(node *subagentNodeState) string {
	if node == nil {
		return ""
	}
	switch node.Status {
	case message.ToolResultSubtaskStatusInProgress:
		if node.LastTool != "" {
			return fmt.Sprintf("Running (%s)", node.LastTool)
		}
		return "Running"
	case message.ToolResultSubtaskStatusFailed:
		return "Failed"
	case message.ToolResultSubtaskStatusBlocked:
		return "Blocked"
	case message.ToolResultSubtaskStatusCanceled:
		return "Canceled"
	case message.ToolResultSubtaskStatusCompleted:
		return "Completed"
	case message.ToolResultSubtaskStatusCompletedWithWarnings:
		return "Completed with warnings"
	default:
		return "Pending"
	}
}

func subagentTodoContent(node *subagentNodeState) string {
	if node == nil {
		return ""
	}
	base := strings.TrimSpace(node.Description)
	if base == "" {
		base = node.AgentName
	}
	if content := compactText(node.Content); content != "" {
		content, truncated := ellipsizeText(content, subagentTodoNodeContentCharsLimit)
		if truncated {
			content += " [truncated]"
		}
		base = fmt.Sprintf("%s (%s)", base, content)
	}
	if len(node.Messages) > 0 {
		mailboxMessage := compactText(node.Messages[len(node.Messages)-1])
		if mailboxMessage != "" {
			mailboxMessage, _ = ellipsizeText(mailboxMessage, subagentTodoMailboxCharsLimit)
			base = fmt.Sprintf("%s [mailbox:%s]", base, mailboxMessage)
		}
	}
	trimmed, _ := ellipsizeText(base, subagentTodoContentCharsLimit)
	return trimmed
}
