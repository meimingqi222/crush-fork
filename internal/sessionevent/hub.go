package sessionevent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/crush/internal/guimetrics"
	"github.com/google/uuid"
)

var (
	// ErrSequenceExpired means replay no longer contains the requested range.
	ErrSequenceExpired = errors.New("session event sequence expired")
	// ErrSubscriptionClosed means the subscription has been closed.
	ErrSubscriptionClosed = errors.New("session event subscription closed")
	// ErrSnapshotRequired means a slow subscription must recover by snapshot.
	ErrSnapshotRequired = errors.New("session event snapshot required")
)

const (
	defaultJournalEvents = 4096
	defaultJournalAge    = 5 * time.Minute
	defaultQueueEvents   = 256
)

// Config controls bounded hub resources.
type Config struct {
	JournalEvents int
	JournalAge    time.Duration
	QueueEvents   int
	Clock         func() time.Time
	NewEventID    func() string
	Coalescer     Coalescer
	Metrics       guimetrics.Recorder
}

// Hub owns isolated session sequence spaces, journals, and subscriptions.
type Hub struct {
	mu       sync.Mutex
	sessions map[string]*sessionState
	config   Config
	closed   bool
}

type sessionState struct {
	mu            sync.Mutex
	latest        uint64
	revision      uint64
	journal       *Journal
	subscriptions map[string]*Subscription
	activeDraft   activeDraft
}

type activeDraft struct {
	MessageID string
	Text      string
	Truncated bool
	Available bool
}

// SnapshotCut is the sequence-consistent in-memory portion of a session
// snapshot. ActiveDraft is captured while the Hub holds the same lock that
// allocates and journals event sequences.
type SnapshotCut struct {
	LatestSequence  uint64
	SessionRevision uint64
	ActiveDraft     ActiveDraftCut
}

// ActiveDraftCut is the in-memory state needed by SnapshotService to project
// an active assistant draft at a precise sequence.
type ActiveDraftCut struct {
	MessageID string
	Text      string
	Truncated bool
	Available bool
}

// Subscription is a bounded pull queue. Next is the only blocking operation;
// publication never waits for a consumer.
type Subscription struct {
	id        string
	sessionID string
	maxEvents int
	coalescer Coalescer
	metrics   guimetrics.Recorder

	mu         sync.Mutex
	queue      []Event
	wake       chan struct{}
	closed     bool
	overflowed bool
	onClose    func()
	closeOnce  sync.Once
}

// NewHub creates an event hub with safe defaults for unset bounds.
func NewHub(config Config) *Hub {
	if config.JournalEvents <= 0 {
		config.JournalEvents = defaultJournalEvents
	}
	if config.JournalAge == 0 {
		config.JournalAge = defaultJournalAge
	}
	if config.QueueEvents <= 0 {
		config.QueueEvents = defaultQueueEvents
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	if config.NewEventID == nil {
		config.NewEventID = uuid.NewString
	}
	if config.Coalescer == nil {
		config.Coalescer = DefaultCoalescer{}
	}
	if config.Metrics == nil {
		config.Metrics = guimetrics.FromContext(context.Background())
	}
	return &Hub{sessions: make(map[string]*sessionState), config: config}
}

// Publish allocates, journals, and offers an event to all current subscribers.
// It never waits for a subscriber to consume an event.
func (h *Hub) Publish(sessionID string, input NewEvent) (Event, error) {
	state, err := h.getOrCreateSession(sessionID)
	if err != nil {
		return Event{}, err
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	now := h.config.Clock()
	state.latest++
	revision := input.SessionRevision
	if (input.Kind == KindSessionUpdated || input.AdvanceRevision) && revision == 0 {
		state.revision++
		revision = state.revision
	} else {
		state.revision = max(state.revision, revision)
	}
	event := Event{
		SessionID:       sessionID,
		FirstSequence:   state.latest,
		Sequence:        state.latest,
		SessionRevision: revision,
		EventID:         h.config.NewEventID(),
		Timestamp:       now,
		Kind:            input.Kind,
		Payload:         clonePayload(input.Payload),
		Delivery:        input.Delivery,
		CoalesceKey:     input.CoalesceKey,
		MergedCount:     1,
	}
	state.applyDraft(event)
	state.journal.Append(event, now)
	for _, subscription := range state.subscriptions {
		subscription.enqueue(event)
	}
	return event, nil
}

// SnapshotCut returns the latest sequence and active assistant draft from one
// Hub lock acquisition. A snapshot consumer may replay only after this cut.
func (h *Hub) SnapshotCut(sessionID string) SnapshotCut {
	state := h.getSession(sessionID)
	if state == nil {
		return SnapshotCut{}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return SnapshotCut{
		LatestSequence:  state.latest,
		SessionRevision: state.revision,
		ActiveDraft: ActiveDraftCut{
			MessageID: state.activeDraft.MessageID,
			Text:      state.activeDraft.Text,
			Truncated: state.activeDraft.Truncated,
			Available: state.activeDraft.Available,
		},
	}
}

// LatestRevision returns the revision attached to the newest session event.
func (h *Hub) LatestRevision(sessionID string) uint64 {
	state := h.getSession(sessionID)
	if state == nil {
		return 0
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.revision
}

// ReplayAfter returns retained events following sequence.
func (h *Hub) ReplayAfter(sessionID string, sequence uint64) ([]Event, error) {
	state := h.getSession(sessionID)
	if state == nil {
		if sequence == 0 {
			return []Event{}, nil
		}
		return nil, ErrSequenceExpired
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	events, available := state.journal.ReplayAfter(sequence, state.latest, h.config.Clock())
	if !available {
		return nil, ErrSequenceExpired
	}
	return events, nil
}

// LatestSequence returns the newest allocated sequence for a session, or zero
// when the session has no in-memory event state.
func (h *Hub) LatestSequence(sessionID string) uint64 {
	state := h.getSession(sessionID)
	if state == nil {
		return 0
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.latest
}

// Subscribe creates a subscription and atomically queues retained replay before
// making it visible to new publications.
func (h *Hub) Subscribe(sessionID string, afterSequence uint64) (*Subscription, error) {
	state, err := h.getOrCreateSession(sessionID)
	if err != nil {
		return nil, err
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	replay, available := state.journal.ReplayAfter(afterSequence, state.latest, h.config.Clock())
	if !available {
		return nil, ErrSequenceExpired
	}
	id := uuid.NewString()
	subscription := &Subscription{
		id:        id,
		sessionID: sessionID,
		maxEvents: h.config.QueueEvents,
		coalescer: h.config.Coalescer,
		metrics:   h.config.Metrics,
		wake:      make(chan struct{}, 1),
	}
	subscription.onClose = func() {
		state.mu.Lock()
		delete(state.subscriptions, id)
		state.mu.Unlock()
		h.config.Metrics.Add(guimetrics.ActiveSubscriptionCount, -1, guimetrics.Labels{})
	}
	for _, event := range replay {
		subscription.enqueue(event)
	}
	if subscription.overflowed {
		return nil, ErrSnapshotRequired
	}
	state.subscriptions[id] = subscription
	h.config.Metrics.Add(guimetrics.ActiveSubscriptionCount, 1, guimetrics.Labels{})
	return subscription, nil
}

// CloseSession closes subscriptions and removes the in-memory journal.
func (h *Hub) CloseSession(sessionID string) {
	h.mu.Lock()
	state := h.sessions[sessionID]
	delete(h.sessions, sessionID)
	h.mu.Unlock()
	if state == nil {
		return
	}
	state.mu.Lock()
	subscriptions := make([]*Subscription, 0, len(state.subscriptions))
	for _, subscription := range state.subscriptions {
		subscriptions = append(subscriptions, subscription)
	}
	state.subscriptions = make(map[string]*Subscription)
	state.mu.Unlock()
	for _, subscription := range subscriptions {
		closed := false
		subscription.closeOnce.Do(func() {
			subscription.closeWithoutCallback()
			closed = true
		})
		if closed {
			h.config.Metrics.Add(guimetrics.ActiveSubscriptionCount, -1, guimetrics.Labels{})
		}
	}
}

// Close releases all session state and rejects later publication/subscription.
func (h *Hub) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	sessionIDs := make([]string, 0, len(h.sessions))
	for sessionID := range h.sessions {
		sessionIDs = append(sessionIDs, sessionID)
	}
	h.mu.Unlock()
	for _, sessionID := range sessionIDs {
		h.CloseSession(sessionID)
	}
}

func (h *Hub) getOrCreateSession(sessionID string) (*sessionState, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, ErrSubscriptionClosed
	}
	state := h.sessions[sessionID]
	if state == nil {
		state = &sessionState{
			journal:       NewJournal(h.config.JournalEvents, h.config.JournalAge),
			subscriptions: make(map[string]*Subscription),
		}
		h.sessions[sessionID] = state
	}
	return state, nil
}

func (h *Hub) getSession(sessionID string) *sessionState {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessions[sessionID]
}

// ID returns the stable subscription identifier.
func (s *Subscription) ID() string { return s.id }

// Next waits for and removes the next event.
func (s *Subscription) Next(ctx context.Context) (Event, error) {
	for {
		s.mu.Lock()
		if len(s.queue) > 0 {
			event := s.queue[0]
			copy(s.queue, s.queue[1:])
			s.queue = s.queue[:len(s.queue)-1]
			remaining := len(s.queue)
			overflowComplete := s.overflowed && remaining == 0
			s.mu.Unlock()
			s.metrics.SetGauge(guimetrics.GUIEventQueueDepth, int64(remaining), guimetrics.Labels{Transport: "subscription"})
			if overflowComplete && event.Kind != KindSnapshotRequired {
				return Event{}, ErrSnapshotRequired
			}
			return event, nil
		}
		closed := s.closed
		overflowed := s.overflowed
		s.mu.Unlock()
		if overflowed {
			return Event{}, ErrSnapshotRequired
		}
		if closed {
			return Event{}, ErrSubscriptionClosed
		}
		select {
		case <-s.wake:
		case <-ctx.Done():
			return Event{}, ctx.Err()
		}
	}
}

// Close idempotently detaches the subscription.
func (s *Subscription) Close() {
	s.closeOnce.Do(func() {
		s.closeWithoutCallback()
		if s.onClose != nil {
			s.onClose()
		}
	})
}

func (s *Subscription) closeWithoutCallback() {
	s.mu.Lock()
	s.closed = true
	s.queue = nil
	s.mu.Unlock()
	s.signal()
}

func (s *Subscription) enqueue(event Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.overflowed {
		return
	}
	if size := len(s.queue); size > 0 {
		if merged, ok := s.coalescer.Merge(s.queue[size-1], event); ok {
			s.queue[size-1] = merged
			s.metrics.Add(guimetrics.GUIEventCoalescedTotal, 1, guimetrics.Labels{Kind: metricKind(event.Kind)})
			s.signal()
			return
		}
	}
	if len(s.queue) < s.maxEvents {
		s.queue = append(s.queue, event)
		s.metrics.SetGauge(guimetrics.GUIEventQueueDepth, int64(len(s.queue)), guimetrics.Labels{Transport: "subscription"})
		s.signal()
		return
	}
	// Preserve every reliable event already accepted. When the incoming event
	// is reliable, preserve it as well. Recoverable state is represented by the
	// snapshot marker. The queue can exceed maxEvents by at most two before the
	// subscription pauses.
	retained := s.queue[:0]
	for _, queued := range s.queue {
		if queued.Delivery == DeliveryReliable {
			retained = append(retained, queued)
		}
	}
	s.queue = retained
	if event.Delivery == DeliveryReliable {
		s.queue = append(s.queue, event)
	}
	s.queue = append(s.queue, Event{
		SessionID:       event.SessionID,
		FirstSequence:   event.Sequence,
		Sequence:        event.Sequence,
		SessionRevision: event.SessionRevision,
		EventID:         "snapshot-required:" + s.id,
		Timestamp:       event.Timestamp,
		Kind:            KindSnapshotRequired,
		Payload:         SnapshotRequired{Reason: "subscriber_overflow"},
		Delivery:        DeliveryReliable,
	})
	s.overflowed = true
	s.metrics.Add(guimetrics.GUISequenceGapTotal, 1, guimetrics.Labels{Outcome: "subscriber_overflow"})
	s.metrics.SetGauge(guimetrics.GUIEventQueueDepth, int64(len(s.queue)), guimetrics.Labels{Transport: "subscription"})
	s.signal()
}

func clonePayload(payload any) any {
	switch value := payload.(type) {
	case TerminalOutput:
		value.Data = append([]byte(nil), value.Data...)
		return value
	default:
		return payload
	}
}

func metricKind(kind Kind) string {
	switch kind {
	case KindSessionUpdated:
		return "session"
	case KindTurnStarted, KindTurnCompleted, KindTurnFailed, KindTurnCancelled, KindTurnSteered, KindTurnProgress, KindCancelAcknowledged:
		return "turn"
	case KindMessageDelta, KindMessageCreated, KindMessageCompleted, KindMessageReset:
		return "message"
	case KindReasoningDelta:
		return "reasoning"
	case KindToolProgress, KindToolCompleted:
		return "tool"
	case KindPermissionRequested:
		return "permission"
	case KindUsageUpdated:
		return "usage"
	case KindQueueUpdated:
		return "queue"
	case KindTerminalOutput:
		return "terminal"
	case KindSnapshotRequired:
		return "snapshot"
	default:
		return "other"
	}
}

func (s *Subscription) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *sessionState) applyDraft(event Event) {
	switch event.Kind {
	case KindMessageCreated:
		if value, ok := event.Payload.(MessageEvent); ok && value.MessageID != "" {
			s.activeDraft = activeDraft{MessageID: value.MessageID}
		}
	case KindMessageDelta:
		value, ok := event.Payload.(TextDelta)
		if !ok || value.MessageID == "" {
			return
		}
		if s.activeDraft.MessageID == "" {
			s.activeDraft.MessageID = value.MessageID
		}
		if s.activeDraft.MessageID != value.MessageID {
			return
		}
		s.activeDraft.append(value.Text)
	case KindMessageCompleted, KindMessageReset:
		if value, ok := event.Payload.(MessageEvent); ok && value.MessageID == s.activeDraft.MessageID {
			s.activeDraft = activeDraft{}
		}
	case KindTurnCompleted, KindTurnFailed, KindTurnCancelled:
		s.activeDraft = activeDraft{}
	}
}

func (d *activeDraft) append(text string) {
	if text == "" || d.Truncated {
		return
	}
	d.Available = true
	text = strings.ToValidUTF8(text, "�")
	remaining := SnapshotDraftBytes - len(d.Text)
	if remaining <= 0 {
		d.Truncated = true
		return
	}
	if len(text) <= remaining {
		d.Text += text
		return
	}
	prefix, _ := truncateUTF8(text, remaining)
	d.Text += prefix
	d.Truncated = true
}
