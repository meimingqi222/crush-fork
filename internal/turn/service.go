// Package turn owns GUI turn lifecycle, queueing, waits, and retries.
package turn

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/sessionevent"
	"github.com/google/uuid"
)

var (
	ErrNotFound         = errors.New("turn not found")
	ErrNotQueued        = errors.New("turn is not queued")
	ErrQueueFull        = errors.New("turn queue is full")
	ErrInputTooLarge    = errors.New("turn input is too large")
	ErrRevisionConflict = errors.New("turn queue revision conflict")
	ErrInvalidOrder     = errors.New("invalid turn queue order")
	ErrRetrySource      = errors.New("turn cannot be retried")
	ErrClosed           = errors.New("turn service is closed")
)

const (
	defaultMaxQueue      = 128
	defaultMaxInputBytes = 1024 * 1024
	defaultRetention     = 10 * time.Minute
	defaultMaxTurns      = 4096
	previewBytes         = 256
)

type Status string

const (
	StatusQueued          Status = "queued"
	StatusRunning         Status = "running"
	StatusCancelRequested Status = "cancel_requested"
	StatusCompleted       Status = "completed"
	StatusFailed          Status = "failed"
	StatusCancelled       Status = "cancelled"
)

func (s Status) Terminal() bool {
	return s == StatusCompleted || s == StatusFailed || s == StatusCancelled
}

type Config struct {
	MaxQueue      int
	MaxInputBytes int
	Retention     time.Duration
	MaxTurns      int
	Clock         func() time.Time
}

type Input struct {
	Prompt               string
	Attachments          []message.Attachment
	AttachmentReferences []AttachmentReference
	Inference            session.InferenceOverrides
	// Scope installs connection/session capabilities at dispatch time. It is
	// deliberately in-memory and is preserved by queue/retry cloning.
	Scope func(context.Context) context.Context
}

type AttachmentReference struct {
	ID      string
	Resolve func(context.Context) (message.Attachment, error)
}

type Turn struct {
	ID               string `json:"turnId"`
	SessionID        string `json:"sessionId"`
	Status           Status `json:"status"`
	QueuePosition    int    `json:"queuePosition"`
	AcceptedSequence uint64 `json:"acceptedSequence"`
	MessageID        string `json:"messageId,omitempty"`
	Reason           string `json:"reason,omitempty"`
	CreatedAt        int64  `json:"createdAt"`
	StartedAt        int64  `json:"startedAt,omitempty"`
	CompletedAt      int64  `json:"completedAt,omitempty"`
}

type Queue struct {
	SessionID string                    `json:"sessionId"`
	Revision  uint64                    `json:"revision"`
	Paused    bool                      `json:"paused"`
	Turns     []sessionevent.QueuedTurn `json:"turns"`
}

type Runner interface {
	Run(context.Context, string, string, ...message.Attachment) error
	Cancel(string)
	RemoveQueuedTurn(string, string) bool
	Steer(string, string, ...message.Attachment) bool
}

type RetrySource interface {
	RetryInput(context.Context, string, string) (Input, error)
}

type Service struct {
	mu       sync.Mutex
	config   Config
	runner   Runner
	retry    RetrySource
	events   *sessionevent.Hub
	sessions map[string]*sessionRuntime
	turns    map[string]*turnState
	closed   bool
}

type sessionRuntime struct {
	active       string
	pending      []string
	revision     uint64
	paused       bool
	subscription *sessionevent.Subscription
	cancel       context.CancelFunc
	done         chan struct{}
}

type turnState struct {
	turn            Turn
	input           Input
	done            chan struct{}
	doneOnce        sync.Once
	dispatchCancel  context.CancelFunc
	cancelRequested bool
}

func New(events *sessionevent.Hub, runner Runner, retry RetrySource, config Config) *Service {
	if config.MaxQueue <= 0 {
		config.MaxQueue = defaultMaxQueue
	}
	if config.MaxInputBytes <= 0 {
		config.MaxInputBytes = defaultMaxInputBytes
	}
	if config.Retention <= 0 {
		config.Retention = defaultRetention
	}
	if config.MaxTurns <= 0 {
		config.MaxTurns = defaultMaxTurns
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	return &Service{
		config: config, runner: runner, retry: retry, events: events,
		sessions: make(map[string]*sessionRuntime), turns: make(map[string]*turnState),
	}
}

func (s *Service) Start(ctx context.Context, sessionID string, input Input) (Turn, error) {
	input.Prompt = strings.TrimSpace(input.Prompt)
	if input.Prompt == "" && len(input.Attachments) == 0 && len(input.AttachmentReferences) == 0 {
		return Turn{}, errors.New("turn prompt is empty")
	}
	if inputSize(input) > s.config.MaxInputBytes {
		return Turn{}, ErrInputTooLarge
	}
	runtime, createdRuntime, err := s.ensureRuntime(sessionID)
	if err != nil {
		return Turn{}, err
	}
	now := s.config.Clock().UnixMilli()
	state := &turnState{
		turn:  Turn{ID: uuid.NewString(), SessionID: sessionID, Status: StatusQueued, CreatedAt: now},
		input: cloneInput(input), done: make(chan struct{}),
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Turn{}, ErrClosed
	}
	s.pruneLocked(s.config.Clock())
	if len(runtime.pending)+boolInt(runtime.active != "") >= s.config.MaxQueue || len(s.turns) >= s.config.MaxTurns {
		cleanupRuntime := false
		if createdRuntime && runtime.active == "" && len(runtime.pending) == 0 && s.sessions[sessionID] == runtime {
			delete(s.sessions, sessionID)
			cleanupRuntime = true
		}
		s.mu.Unlock()
		if cleanupRuntime {
			runtime.cancel()
			runtime.subscription.Close()
		}
		return Turn{}, ErrQueueFull
	}
	s.turns[state.turn.ID] = state
	dispatch := runtime.active == ""
	if dispatch {
		runtime.active = state.turn.ID
		state.turn.QueuePosition = 0
	} else {
		runtime.pending = append(runtime.pending, state.turn.ID)
		state.turn.QueuePosition = len(runtime.pending)
	}
	runtime.revision++
	s.mu.Unlock()

	queueEvent, err := s.publishQueue(sessionID)
	if err == nil {
		s.mu.Lock()
		state.turn.AcceptedSequence = queueEvent.Sequence
		s.mu.Unlock()
	}
	if dispatch {
		s.dispatch(state.turn.ID)
	}
	return s.Get(state.turn.ID)
}

func (s *Service) Get(turnID string) (Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.turns[turnID]
	if state == nil {
		return Turn{}, ErrNotFound
	}
	return state.turn, nil
}

func (s *Service) Wait(ctx context.Context, turnID string) (Turn, error) {
	s.mu.Lock()
	state := s.turns[turnID]
	if state == nil {
		s.mu.Unlock()
		return Turn{}, ErrNotFound
	}
	done := state.done
	if state.turn.Status.Terminal() {
		turn := state.turn
		s.mu.Unlock()
		return turn, nil
	}
	s.mu.Unlock()
	select {
	case <-done:
		return s.Get(turnID)
	case <-ctx.Done():
		return Turn{}, ctx.Err()
	}
}

func (s *Service) Cancel(turnID string) (Turn, error) {
	s.mu.Lock()
	state := s.turns[turnID]
	if state == nil {
		s.mu.Unlock()
		return Turn{}, ErrNotFound
	}
	if state.turn.Status.Terminal() {
		turn := state.turn
		s.mu.Unlock()
		return turn, nil
	}
	runtime := s.sessions[state.turn.SessionID]
	if removeID(&runtime.pending, turnID) {
		s.finishLocked(state, StatusCancelled, "queue_removed", "")
		runtime.revision++
		turn := state.turn
		s.mu.Unlock()
		s.publishCancellation(state.turn.SessionID, turnID, "queue_removed")
		_, _ = s.publishQueue(state.turn.SessionID)
		return turn, nil
	}
	previousStatus := state.turn.Status
	state.cancelRequested = true
	state.turn.Status = StatusCancelRequested
	cancel := state.dispatchCancel
	sessionID := state.turn.SessionID
	s.mu.Unlock()
	s.publishCancelAck(sessionID, turnID, "user_requested")
	if cancel != nil {
		cancel()
	}
	if s.runner.RemoveQueuedTurn(sessionID, turnID) {
		s.finish(turnID, StatusCancelled, "queue_removed", "")
		s.publishTurnCancelled(sessionID, turnID, "queue_removed")
		s.advance(sessionID, turnID)
	} else if previousStatus == StatusRunning {
		s.runner.Cancel(sessionID)
	}
	return s.Get(turnID)
}

func (s *Service) Queue(sessionID string) Queue {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.queueLocked(sessionID)
}

func (s *Service) RemoveQueued(sessionID, turnID string) (Queue, error) {
	turn, err := s.Get(turnID)
	if err != nil || turn.SessionID != sessionID {
		return Queue{}, ErrNotFound
	}
	if turn.Status != StatusQueued && turn.Status != StatusCancelRequested {
		return Queue{}, ErrNotQueued
	}
	if _, err := s.Cancel(turnID); err != nil {
		return Queue{}, err
	}
	return s.Queue(sessionID), nil
}

func (s *Service) Reorder(sessionID string, expectedRevision uint64, orderedIDs []string) (Queue, error) {
	s.mu.Lock()
	runtime := s.sessions[sessionID]
	if runtime == nil {
		s.mu.Unlock()
		return Queue{SessionID: sessionID}, nil
	}
	if runtime.revision != expectedRevision {
		s.mu.Unlock()
		return Queue{}, ErrRevisionConflict
	}
	pendingSet := make(map[string]struct{}, len(runtime.pending))
	for _, id := range runtime.pending {
		pendingSet[id] = struct{}{}
	}
	seen := make(map[string]struct{}, len(orderedIDs))
	for _, id := range orderedIDs {
		if _, ok := pendingSet[id]; !ok {
			s.mu.Unlock()
			return Queue{}, ErrInvalidOrder
		}
		if _, duplicate := seen[id]; duplicate {
			s.mu.Unlock()
			return Queue{}, ErrInvalidOrder
		}
		seen[id] = struct{}{}
	}
	reordered := append([]string(nil), orderedIDs...)
	for _, id := range runtime.pending {
		if _, listed := seen[id]; !listed {
			reordered = append(reordered, id)
		}
	}
	runtime.pending = reordered
	runtime.revision++
	queue := s.queueLocked(sessionID)
	s.mu.Unlock()
	_, _ = s.publishQueue(sessionID)
	return queue, nil
}

func (s *Service) Steer(sessionID string, input Input) (string, *Turn, uint64, error) {
	if inputSize(input) > s.config.MaxInputBytes {
		return "", nil, 0, ErrInputTooLarge
	}
	attachments, err := resolveAttachments(context.Background(), input)
	if err != nil {
		return "", nil, 0, err
	}
	if s.runner.Steer(sessionID, input.Prompt, attachments...) {
		s.mu.Lock()
		activeID := ""
		if runtime := s.sessions[sessionID]; runtime != nil {
			activeID = runtime.active
		}
		s.mu.Unlock()
		event, err := s.events.Publish(sessionID, sessionevent.NewEvent{
			Kind: sessionevent.KindTurnSteered, Delivery: sessionevent.DeliveryReliable,
			Payload: sessionevent.TurnEvent{TurnID: activeID, Reason: "accepted"},
		})
		if err != nil {
			return "", nil, 0, err
		}
		return "steered", nil, event.Sequence, nil
	}
	turn, err := s.Start(context.Background(), sessionID, input)
	if err != nil {
		return "", nil, 0, err
	}
	return "queued", &turn, turn.AcceptedSequence, nil
}

func (s *Service) RetryTurn(ctx context.Context, turnID string) (Turn, error) {
	s.mu.Lock()
	state := s.turns[turnID]
	if state == nil || state.turn.Status != StatusFailed && state.turn.Status != StatusCancelled {
		s.mu.Unlock()
		return Turn{}, ErrRetrySource
	}
	input := cloneInput(state.input)
	sessionID := state.turn.SessionID
	s.mu.Unlock()
	return s.Start(ctx, sessionID, input)
}

func (s *Service) RetryMessage(ctx context.Context, sessionID, messageID string) (Turn, error) {
	if s.retry == nil {
		return Turn{}, ErrRetrySource
	}
	input, err := s.retry.RetryInput(ctx, sessionID, messageID)
	if err != nil {
		return Turn{}, fmt.Errorf("%w: %v", ErrRetrySource, err)
	}
	return s.Start(ctx, sessionID, input)
}

func (s *Service) CloseSession(sessionID string) {
	s.mu.Lock()
	runtime := s.sessions[sessionID]
	if runtime == nil {
		s.mu.Unlock()
		return
	}
	delete(s.sessions, sessionID)
	turnIDs := append([]string{runtime.active}, runtime.pending...)
	for _, id := range turnIDs {
		if state := s.turns[id]; state != nil && !state.turn.Status.Terminal() {
			s.finishLocked(state, StatusCancelled, "session_closed", "")
		}
	}
	s.mu.Unlock()
	if runtime.cancel != nil {
		runtime.cancel()
	}
	if runtime.subscription != nil {
		runtime.subscription.Close()
	}
	if runtime.active != "" {
		s.runner.Cancel(sessionID)
	}
}

func (s *Service) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	sessionIDs := make([]string, 0, len(s.sessions))
	for sessionID := range s.sessions {
		sessionIDs = append(sessionIDs, sessionID)
	}
	s.mu.Unlock()
	for _, sessionID := range sessionIDs {
		s.CloseSession(sessionID)
	}
}

func (s *Service) ensureRuntime(sessionID string) (*sessionRuntime, bool, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, false, ErrClosed
	}
	if runtime := s.sessions[sessionID]; runtime != nil {
		s.mu.Unlock()
		return runtime, false, nil
	}
	s.mu.Unlock()
	if s.events == nil {
		return nil, false, errors.New("turn event service is unavailable")
	}
	subscription, err := s.events.Subscribe(sessionID, s.events.LatestSequence(sessionID))
	if err != nil {
		return nil, false, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	runtime := &sessionRuntime{subscription: subscription, cancel: cancel, done: make(chan struct{})}
	s.mu.Lock()
	if existing := s.sessions[sessionID]; existing != nil {
		s.mu.Unlock()
		cancel()
		subscription.Close()
		return existing, false, nil
	}
	s.sessions[sessionID] = runtime
	s.mu.Unlock()
	go s.watch(ctx, sessionID, runtime)
	return runtime, true, nil
}

func (s *Service) watch(ctx context.Context, sessionID string, runtime *sessionRuntime) {
	defer close(runtime.done)
	for {
		event, err := runtime.subscription.Next(ctx)
		if err != nil {
			return
		}
		payload, ok := event.Payload.(sessionevent.TurnEvent)
		if !ok || payload.TurnID == "" {
			continue
		}
		s.handleTurnEvent(sessionID, event.Kind, payload)
	}
}

func (s *Service) handleTurnEvent(sessionID string, kind sessionevent.Kind, payload sessionevent.TurnEvent) {
	s.mu.Lock()
	state := s.turns[payload.TurnID]
	if state == nil || state.turn.SessionID != sessionID || state.turn.Status.Terminal() {
		s.mu.Unlock()
		return
	}
	now := s.config.Clock().UnixMilli()
	switch kind {
	case sessionevent.KindTurnStarted:
		cancelRequested := state.cancelRequested
		if cancelRequested {
			state.turn.Status = StatusCancelRequested
		} else {
			state.turn.Status = StatusRunning
		}
		state.turn.StartedAt = now
		s.mu.Unlock()
		if cancelRequested {
			s.runner.Cancel(sessionID)
		}
		return
	case sessionevent.KindCancelAcknowledged:
		state.turn.Status = StatusCancelRequested
		s.mu.Unlock()
		return
	case sessionevent.KindTurnCompleted:
		s.finishLocked(state, StatusCompleted, payload.Reason, payload.MessageID)
	case sessionevent.KindTurnCancelled:
		s.finishLocked(state, StatusCancelled, payload.Reason, payload.MessageID)
	case sessionevent.KindTurnFailed:
		s.finishLocked(state, StatusFailed, payload.Reason, payload.MessageID)
	default:
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.advance(sessionID, payload.TurnID)
}

func (s *Service) dispatch(turnID string) {
	s.mu.Lock()
	state := s.turns[turnID]
	if state == nil || state.turn.Status.Terminal() {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	state.dispatchCancel = cancel
	input := cloneInput(state.input)
	ctx = agent.WithTurnInferenceOverrides(ctx, input.Inference)
	if input.Scope != nil {
		ctx = input.Scope(ctx)
	}
	sessionID := state.turn.SessionID
	s.mu.Unlock()
	go func() {
		attachments, err := resolveAttachments(ctx, input)
		if err != nil {
			_, publishErr := s.events.Publish(sessionID, sessionevent.NewEvent{
				Kind: sessionevent.KindTurnFailed, Delivery: sessionevent.DeliveryReliable,
				Payload: sessionevent.TurnEvent{TurnID: turnID, Reason: "attachment_unavailable"},
			})
			if publishErr != nil {
				s.finish(turnID, StatusFailed, "attachment_unavailable", "")
				s.advance(sessionID, turnID)
			}
			return
		}
		err = s.runner.Run(agent.WithTurnID(ctx, turnID), sessionID, input.Prompt, attachments...)
		s.mu.Lock()
		state := s.turns[turnID]
		if state == nil || state.turn.Status.Terminal() {
			s.mu.Unlock()
			return
		}
		cancelRequested := state.cancelRequested
		s.mu.Unlock()
		if cancelRequested {
			if s.runner.RemoveQueuedTurn(sessionID, turnID) {
				s.finish(turnID, StatusCancelled, "queue_removed", "")
				s.publishTurnCancelled(sessionID, turnID, "queue_removed")
				s.advance(sessionID, turnID)
				return
			}
			s.runner.Cancel(sessionID)
			return
		}
		if err != nil {
			status := StatusFailed
			reason := "run_failed"
			if errors.Is(err, context.Canceled) {
				status, reason = StatusCancelled, "cancelled_before_start"
			}
			s.finish(turnID, status, reason, "")
			_, _ = s.events.Publish(sessionID, sessionevent.NewEvent{
				Kind: terminalKind(status), Delivery: sessionevent.DeliveryReliable,
				Payload: sessionevent.TurnEvent{TurnID: turnID, Reason: reason},
			})
			s.advance(sessionID, turnID)
		}
	}()
}

func (s *Service) advance(sessionID, completedID string) {
	s.mu.Lock()
	runtime := s.sessions[sessionID]
	if runtime == nil || runtime.active != completedID {
		s.mu.Unlock()
		return
	}
	runtime.active = ""
	var next string
	if !runtime.paused && len(runtime.pending) > 0 {
		next = runtime.pending[0]
		runtime.pending = runtime.pending[1:]
		runtime.active = next
	}
	runtime.revision++
	s.mu.Unlock()
	_, _ = s.publishQueue(sessionID)
	if next != "" {
		s.dispatch(next)
	}
}

func (s *Service) finish(turnID string, status Status, reason, messageID string) {
	s.mu.Lock()
	if state := s.turns[turnID]; state != nil && !state.turn.Status.Terminal() {
		s.finishLocked(state, status, reason, messageID)
	}
	s.mu.Unlock()
}

func (s *Service) finishLocked(state *turnState, status Status, reason, messageID string) {
	state.turn.Status = status
	state.turn.Reason = reason
	state.turn.MessageID = messageID
	state.turn.QueuePosition = 0
	state.turn.CompletedAt = s.config.Clock().UnixMilli()
	state.doneOnce.Do(func() { close(state.done) })
}

func (s *Service) publishCancellation(sessionID, turnID, reason string) {
	s.publishCancelAck(sessionID, turnID, reason)
	s.publishTurnCancelled(sessionID, turnID, reason)
}

func (s *Service) publishCancelAck(sessionID, turnID, reason string) {
	_, _ = s.events.Publish(sessionID, sessionevent.NewEvent{
		Kind: sessionevent.KindCancelAcknowledged, Delivery: sessionevent.DeliveryReliable,
		Payload: sessionevent.TurnEvent{TurnID: turnID, Reason: reason},
	})
}

func (s *Service) publishTurnCancelled(sessionID, turnID, reason string) {
	_, _ = s.events.Publish(sessionID, sessionevent.NewEvent{
		Kind: sessionevent.KindTurnCancelled, Delivery: sessionevent.DeliveryReliable,
		Payload: sessionevent.TurnEvent{TurnID: turnID, Reason: reason},
	})
}

func (s *Service) publishQueue(sessionID string) (sessionevent.Event, error) {
	queue := s.Queue(sessionID)
	return s.events.Publish(sessionID, sessionevent.NewEvent{
		Kind: sessionevent.KindQueueUpdated, Delivery: sessionevent.DeliveryLatest,
		CoalesceKey: "queue:" + sessionID,
		Payload:     sessionevent.QueueEvent{Revision: queue.Revision, Turns: queue.Turns},
	})
}

func (s *Service) queueLocked(sessionID string) Queue {
	runtime := s.sessions[sessionID]
	queue := Queue{SessionID: sessionID, Turns: []sessionevent.QueuedTurn{}}
	if runtime == nil {
		return queue
	}
	queue.Revision = runtime.revision
	queue.Paused = runtime.paused
	ids := append([]string(nil), runtime.pending...)
	if runtime.active != "" {
		if active := s.turns[runtime.active]; active != nil && active.turn.Status != StatusRunning {
			ids = append([]string{runtime.active}, ids...)
		}
	}
	for index, id := range ids {
		state := s.turns[id]
		if state == nil || state.turn.Status.Terminal() {
			continue
		}
		queue.Turns = append(queue.Turns, sessionevent.QueuedTurn{
			TurnID: id, Status: string(state.turn.Status), Position: index + 1,
			Preview: truncate(state.input.Prompt, previewBytes),
		})
	}
	return queue
}

func (s *Service) pruneLocked(now time.Time) {
	cutoff := now.Add(-s.config.Retention).UnixMilli()
	for id, state := range s.turns {
		if state.turn.Status.Terminal() && state.turn.CompletedAt < cutoff {
			delete(s.turns, id)
		}
	}
	for len(s.turns) >= s.config.MaxTurns {
		var oldestID string
		var oldest int64
		for id, state := range s.turns {
			if !state.turn.Status.Terminal() || oldestID != "" && state.turn.CompletedAt >= oldest {
				continue
			}
			oldestID, oldest = id, state.turn.CompletedAt
		}
		if oldestID == "" {
			break
		}
		delete(s.turns, oldestID)
	}
}

func inputSize(input Input) int {
	size := len(input.Prompt) + len(input.Inference.Model) + len(input.Inference.Provider)
	if input.Inference.MaxOutputTokens != nil || input.Inference.Temperature != nil || input.Inference.TopP != nil ||
		input.Inference.TopK != nil || input.Inference.FrequencyPenalty != nil || input.Inference.PresencePenalty != nil || input.Inference.Think != nil {
		size += 128
	}
	for _, attachment := range input.Attachments {
		size += len(attachment.Content) + len(attachment.FilePath) + len(attachment.MimeType)
	}
	for _, reference := range input.AttachmentReferences {
		size += len(reference.ID)
	}
	return size
}

func cloneInput(input Input) Input {
	result := Input{
		Prompt: input.Prompt, Attachments: make([]message.Attachment, len(input.Attachments)),
		AttachmentReferences: append([]AttachmentReference(nil), input.AttachmentReferences...),
		Inference:            cloneInferenceOverrides(input.Inference),
		Scope:                input.Scope,
	}
	copy(result.Attachments, input.Attachments)
	for index := range result.Attachments {
		result.Attachments[index].Content = append([]byte(nil), input.Attachments[index].Content...)
	}
	return result
}

func resolveAttachments(ctx context.Context, input Input) ([]message.Attachment, error) {
	attachments := append([]message.Attachment(nil), input.Attachments...)
	for _, reference := range input.AttachmentReferences {
		if reference.Resolve == nil {
			return nil, ErrRetrySource
		}
		attachment, err := reference.Resolve(ctx)
		if err != nil {
			return nil, err
		}
		attachments = append(attachments, attachment)
	}
	return attachments, nil
}

func cloneInferenceOverrides(value session.InferenceOverrides) session.InferenceOverrides {
	result := value
	result.MaxOutputTokens = clonePointer(value.MaxOutputTokens)
	result.Temperature = clonePointer(value.Temperature)
	result.TopP = clonePointer(value.TopP)
	result.TopK = clonePointer(value.TopK)
	result.FrequencyPenalty = clonePointer(value.FrequencyPenalty)
	result.PresencePenalty = clonePointer(value.PresencePenalty)
	result.Think = clonePointer(value.Think)
	return result
}

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func removeID(ids *[]string, target string) bool {
	for index, id := range *ids {
		if id == target {
			*ids = append((*ids)[:index:index], (*ids)[index+1:]...)
			return true
		}
	}
	return false
}

func terminalKind(status Status) sessionevent.Kind {
	switch status {
	case StatusCancelled:
		return sessionevent.KindTurnCancelled
	case StatusCompleted:
		return sessionevent.KindTurnCompleted
	default:
		return sessionevent.KindTurnFailed
	}
}

func truncate(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
