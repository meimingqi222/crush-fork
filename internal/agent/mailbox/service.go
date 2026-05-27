package mailbox

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

type EnvelopeKind string

const (
	EnvelopeKindMessage EnvelopeKind = "message"
	EnvelopeKindStop    EnvelopeKind = "stop"
)

type Envelope struct {
	MailboxID     string       `json:"mailbox_id"`
	TargetAgentID string       `json:"target_agent_id,omitempty"`
	Kind          EnvelopeKind `json:"kind"`
	Message       string       `json:"message,omitempty"`
	Reason        string       `json:"reason,omitempty"`
	CreatedAt     int64        `json:"created_at"`
}

type Service interface {
	Open(mailboxID string, agentIDs []string) error
	Close(mailboxID string)
	Send(mailboxID, agentID, message string) (Envelope, error)
	Stop(mailboxID, agentID, reason string) (Envelope, error)
	Consume(mailboxID, agentID string) ([]Envelope, error)
	ActiveMailboxes() map[string][]string
}

type service struct {
	mu        sync.Mutex
	mailboxes map[string]*mailbox
}

type mailbox struct {
	agents map[string]struct{}
	queues map[string][]Envelope
}

func NewService() Service {
	return &service{mailboxes: map[string]*mailbox{}}
}

func (s *service) Open(mailboxID string, agentIDs []string) error {
	id := strings.TrimSpace(mailboxID)
	if id == "" {
		return fmt.Errorf("mailbox_id is required")
	}
	if len(agentIDs) == 0 {
		return fmt.Errorf("agent_ids is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	agents := make(map[string]struct{}, len(agentIDs))
	queues := make(map[string][]Envelope, len(agentIDs))
	for _, agentID := range agentIDs {
		trimmed := strings.TrimSpace(agentID)
		if trimmed == "" {
			continue
		}
		agents[trimmed] = struct{}{}
		queues[trimmed] = []Envelope{}
	}
	if len(agents) == 0 {
		return fmt.Errorf("agent_ids is required")
	}
	s.mailboxes[id] = &mailbox{agents: agents, queues: queues}
	return nil
}

func (s *service) Close(mailboxID string) {
	id := strings.TrimSpace(mailboxID)
	if id == "" {
		return
	}
	s.mu.Lock()
	delete(s.mailboxes, id)
	s.mu.Unlock()
}

func (s *service) Send(mailboxID, agentID, message string) (Envelope, error) {
	msg := strings.TrimSpace(message)
	if msg == "" {
		return Envelope{}, fmt.Errorf("message is required")
	}
	env := Envelope{
		MailboxID: strings.TrimSpace(mailboxID),
		Kind:      EnvelopeKindMessage,
		Message:   msg,
		CreatedAt: time.Now().UnixMilli(),
	}
	if err := s.enqueue(env, strings.TrimSpace(agentID)); err != nil {
		return Envelope{}, err
	}
	env.TargetAgentID = strings.TrimSpace(agentID)
	return env, nil
}

func (s *service) Stop(mailboxID, agentID, reason string) (Envelope, error) {
	env := Envelope{
		MailboxID: strings.TrimSpace(mailboxID),
		Kind:      EnvelopeKindStop,
		Reason:    strings.TrimSpace(reason),
		CreatedAt: time.Now().UnixMilli(),
	}
	if err := s.enqueue(env, strings.TrimSpace(agentID)); err != nil {
		return Envelope{}, err
	}
	env.TargetAgentID = strings.TrimSpace(agentID)
	return env, nil
}

func (s *service) Consume(mailboxID, agentID string) ([]Envelope, error) {
	id := strings.TrimSpace(mailboxID)
	agent := strings.TrimSpace(agentID)
	if id == "" {
		return nil, fmt.Errorf("mailbox_id is required")
	}
	if agent == "" {
		return nil, fmt.Errorf("agent_id is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	box, ok := s.mailboxes[id]
	if !ok {
		return nil, fmt.Errorf("mailbox %q not found", id)
	}
	if _, ok := box.agents[agent]; !ok {
		return nil, fmt.Errorf("agent %q not found in mailbox %q", agent, id)
	}
	queue := box.queues[agent]
	if len(queue) == 0 {
		return nil, nil
	}
	out := append([]Envelope(nil), queue...)
	box.queues[agent] = box.queues[agent][:0]
	return out, nil
}

func (s *service) enqueue(envelope Envelope, agentID string) error {
	mailboxID := strings.TrimSpace(envelope.MailboxID)
	if mailboxID == "" {
		return fmt.Errorf("mailbox_id is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	box, ok := s.mailboxes[mailboxID]
	if !ok {
		return fmt.Errorf("mailbox %q not found", mailboxID)
	}

	if agentID != "" {
		if _, ok := box.agents[agentID]; !ok {
			return fmt.Errorf("agent %q not found in mailbox %q", agentID, mailboxID)
		}
		envelope.TargetAgentID = agentID
		box.queues[agentID] = append(box.queues[agentID], envelope)
		return nil
	}

	envelope.TargetAgentID = ""
	for currentAgentID := range box.agents {
		box.queues[currentAgentID] = append(box.queues[currentAgentID], envelope)
	}
	return nil
}

func (s *service) ActiveMailboxes() map[string][]string {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make(map[string][]string, len(s.mailboxes))
	for id, box := range s.mailboxes {
		agents := make([]string, 0, len(box.agents))
		for agent := range box.agents {
			agents = append(agents, agent)
		}
		out[id] = agents
	}
	return out
}
