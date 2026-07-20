package guiapi

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/crush/internal/acp"
	"github.com/charmbracelet/crush/internal/sessionevent"
)

const sessionEventNotification = "crush/session/event"

type subscribeParams struct {
	SessionID     string `json:"sessionId"`
	AfterSequence uint64 `json:"afterSequence,omitempty"`
}

type subscribeResult struct {
	SubscriptionID string `json:"subscriptionId"`
	LatestSequence uint64 `json:"latestSequence"`
}

type unsubscribeParams struct {
	SubscriptionID string `json:"subscriptionId"`
}

type unsubscribeResult struct {
	Unsubscribed bool `json:"unsubscribed"`
}

type syncParams struct {
	SessionID     string `json:"sessionId"`
	AfterSequence uint64 `json:"afterSequence"`
}

type syncResult struct {
	Mode           string                 `json:"mode"`
	LatestSequence uint64                 `json:"latestSequence"`
	Events         []eventEnvelope        `json:"events,omitempty"`
	Snapshot       *sessionevent.Snapshot `json:"snapshot,omitempty"`
}

type eventNotification struct {
	SubscriptionID string        `json:"subscriptionId"`
	Event          eventEnvelope `json:"event"`
}

type eventEnvelope struct {
	SessionID       string            `json:"sessionId"`
	FirstSequence   uint64            `json:"firstSequence"`
	Sequence        uint64            `json:"sequence"`
	SessionRevision uint64            `json:"sessionRevision"`
	EventID         string            `json:"eventId"`
	Timestamp       string            `json:"timestamp"`
	Kind            sessionevent.Kind `json:"kind"`
	Payload         any               `json:"payload"`
}

type responseLifecycle struct {
	result any
	after  func(error)
	once   sync.Once
}

func (r *responseLifecycle) ResponseResult() any { return r.result }

func (r *responseLifecycle) AfterResponse(_ context.Context, writeErr error) {
	r.once.Do(func() { r.after(writeErr) })
}

type managedSubscription struct {
	service      *Service
	subscription *sessionevent.Subscription
	writer       NotificationWriter
	ctx          context.Context
	cancel       context.CancelFunc
	done         chan struct{}
	started      atomic.Bool
	startOnce    sync.Once
	stopOnce     sync.Once
}

func (s *Service) registerSessionSyncHandlers() {
	s.routes["crush/session/subscribe"] = route{feature: FeatureSessionSync, handler: s.handleSubscribe}
	s.routes["crush/session/unsubscribe"] = route{feature: FeatureSessionSync, handler: s.handleUnsubscribe}
	s.routes["crush/session/sync"] = route{feature: FeatureSessionSync, handler: s.handleSync}
}

func (s *Service) handleSubscribe(_ context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params subscribeParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParams(err)
	}
	if params.SessionID == "" {
		return nil, invalidParams(errors.New("sessionId is required"))
	}

	s.mu.RLock()
	events := s.events
	writer := s.writer
	closed := s.closed
	s.mu.RUnlock()
	if closed {
		return nil, &acp.RPCError{Code: acp.CodeInternalError, Message: "crush extension service is closed"}
	}
	if events == nil || writer == nil {
		return nil, &acp.RPCError{Code: acp.CodeInternalError, Message: "session event service is unavailable"}
	}

	subscription, err := events.Subscribe(params.SessionID, params.AfterSequence)
	if err != nil {
		return nil, sessionReplayError(events, params.SessionID, params.AfterSequence, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	managed := &managedSubscription{
		service:      s,
		subscription: subscription,
		writer:       writer,
		ctx:          ctx,
		cancel:       cancel,
		done:         make(chan struct{}),
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		managed.stop()
		return nil, &acp.RPCError{Code: acp.CodeInternalError, Message: "crush extension service is closed"}
	}
	s.subscriptions[subscription.ID()] = managed
	s.mu.Unlock()

	result := subscribeResult{
		SubscriptionID: subscription.ID(),
		LatestSequence: events.LatestSequence(params.SessionID),
	}
	return &responseLifecycle{
		result: result,
		after: func(writeErr error) {
			if writeErr != nil {
				s.removeSubscription(subscription.ID(), managed)
				managed.stop()
				return
			}
			managed.start()
		},
	}, nil
}

func (s *Service) handleUnsubscribe(_ context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params unsubscribeParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParams(err)
	}
	if params.SubscriptionID == "" {
		return nil, invalidParams(errors.New("subscriptionId is required"))
	}

	s.mu.Lock()
	managed := s.subscriptions[params.SubscriptionID]
	delete(s.subscriptions, params.SubscriptionID)
	s.mu.Unlock()
	if managed != nil {
		managed.stop()
	}
	return unsubscribeResult{Unsubscribed: managed != nil}, nil
}

func (s *Service) handleSync(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params syncParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParams(err)
	}
	if params.SessionID == "" {
		return nil, invalidParams(errors.New("sessionId is required"))
	}

	s.mu.RLock()
	events := s.events
	s.mu.RUnlock()
	if events == nil {
		return nil, &acp.RPCError{Code: acp.CodeInternalError, Message: "session event service is unavailable"}
	}
	replay, err := events.ReplayAfter(params.SessionID, params.AfterSequence)
	if err != nil {
		if errors.Is(err, sessionevent.ErrSequenceExpired) || errors.Is(err, sessionevent.ErrSnapshotRequired) {
			s.mu.RLock()
			hasSnapshotSource := s.snapshots != nil
			s.mu.RUnlock()
			if !hasSnapshotSource {
				return nil, sessionReplayError(events, params.SessionID, params.AfterSequence, err)
			}
			snapshot, rpcErr := s.buildSnapshot(ctx, params.SessionID)
			if rpcErr == nil {
				return syncResult{
					Mode:           "snapshot",
					LatestSequence: snapshot.LatestSequence,
					Snapshot:       &snapshot,
				}, nil
			}
			return nil, rpcErr
		}
		return nil, sessionReplayError(events, params.SessionID, params.AfterSequence, err)
	}
	wireEvents := make([]eventEnvelope, len(replay))
	for index, event := range replay {
		wireEvents[index] = toEventEnvelope(event)
	}
	return syncResult{
		Mode:           "replay",
		LatestSequence: events.LatestSequence(params.SessionID),
		Events:         wireEvents,
	}, nil
}

func (m *managedSubscription) start() {
	m.startOnce.Do(func() {
		m.started.Store(true)
		go m.run()
	})
}

func (m *managedSubscription) run() {
	defer close(m.done)
	defer m.subscription.Close()
	defer m.service.removeSubscription(m.subscription.ID(), m)
	for {
		event, err := m.subscription.Next(m.ctx)
		if err != nil {
			return
		}
		if err := m.writer.NotifySync(m.ctx, sessionEventNotification, eventNotification{
			SubscriptionID: m.subscription.ID(),
			Event:          toEventEnvelope(event),
		}); err != nil {
			return
		}
	}
}

func (m *managedSubscription) stop() {
	m.stopOnce.Do(func() {
		m.cancel()
		m.subscription.Close()
	})
}

func (m *managedSubscription) wait(ctx context.Context) {
	if !m.started.Load() {
		return
	}
	select {
	case <-m.done:
	case <-ctx.Done():
	}
}

func (s *Service) removeSubscription(id string, expected *managedSubscription) {
	s.mu.Lock()
	if s.subscriptions[id] == expected {
		delete(s.subscriptions, id)
	}
	s.mu.Unlock()
}

func sessionReplayError(events *sessionevent.Hub, sessionID string, afterSequence uint64, err error) *acp.RPCError {
	if errors.Is(err, sessionevent.ErrSequenceExpired) || errors.Is(err, sessionevent.ErrSnapshotRequired) {
		return &acp.RPCError{
			Code:    -32020,
			Message: errorSequenceExpired,
			Data: ErrorData{
				Code:      errorSequenceExpired,
				Retryable: true,
				Details: map[string]any{
					"sessionId":        sessionID,
					"afterSequence":    afterSequence,
					"latestSequence":   events.LatestSequence(sessionID),
					"snapshotRequired": true,
				},
			},
		}
	}
	return &acp.RPCError{Code: acp.CodeInternalError, Message: err.Error()}
}

func invalidParams(err error) *acp.RPCError {
	return &acp.RPCError{Code: acp.CodeInvalidParams, Message: err.Error()}
}

func toEventEnvelope(event sessionevent.Event) eventEnvelope {
	return eventEnvelope{
		SessionID:       event.SessionID,
		FirstSequence:   event.FirstSequence,
		Sequence:        event.Sequence,
		SessionRevision: event.SessionRevision,
		EventID:         event.EventID,
		Timestamp:       event.Timestamp.UTC().Format(time.RFC3339Nano),
		Kind:            event.Kind,
		Payload:         toWirePayload(event.Payload),
	}
}

func toWirePayload(payload any) any {
	switch value := payload.(type) {
	case sessionevent.TextDelta:
		return struct {
			MessageID string `json:"messageId"`
			PartID    string `json:"partId"`
			Text      string `json:"text"`
		}{value.MessageID, value.PartID, value.Text}
	case sessionevent.MessageEvent:
		return struct {
			MessageID    string `json:"messageId"`
			FinishReason string `json:"finishReason,omitempty"`
		}{value.MessageID, value.FinishReason}
	case sessionevent.ToolEvent:
		return struct {
			MessageID  string               `json:"messageId"`
			ToolCallID string               `json:"toolCallId"`
			Name       string               `json:"name"`
			Status     string               `json:"status"`
			Input      string               `json:"input,omitempty"`
			Result     string               `json:"result,omitempty"`
			IsError    bool                 `json:"isError,omitempty"`
			Truncated  bool                 `json:"truncated,omitempty"`
			Files      []toolFileProjection `json:"files,omitempty"`
		}{
			MessageID: value.MessageID, ToolCallID: value.ToolCallID, Name: value.Name,
			Status: value.Status, Input: value.Input, Result: value.Result,
			IsError: value.IsError, Truncated: value.Truncated,
			Files: projectToolFiles(value.Files),
		}
	case sessionevent.TurnEvent:
		return struct {
			TurnID    string `json:"turnId,omitempty"`
			MessageID string `json:"messageId,omitempty"`
			Reason    string `json:"reason,omitempty"`
			Phase     string `json:"phase,omitempty"`
		}{value.TurnID, value.MessageID, value.Reason, value.Phase}
	case sessionevent.QueueEvent:
		turns := make([]struct {
			TurnID   string `json:"turnId"`
			Status   string `json:"status"`
			Position int    `json:"position"`
			Preview  string `json:"preview,omitempty"`
		}, len(value.Turns))
		for index, turn := range value.Turns {
			turns[index] = struct {
				TurnID   string `json:"turnId"`
				Status   string `json:"status"`
				Position int    `json:"position"`
				Preview  string `json:"preview,omitempty"`
			}{turn.TurnID, turn.Status, turn.Position, turn.Preview}
		}
		return struct {
			Revision uint64 `json:"revision"`
			Turns    any    `json:"turns"`
		}{value.Revision, turns}
	case sessionevent.UsageEvent:
		return struct {
			InputTokens      int64 `json:"inputTokens,omitempty"`
			OutputTokens     int64 `json:"outputTokens,omitempty"`
			ReasoningTokens  int64 `json:"reasoningTokens,omitempty"`
			CacheReadTokens  int64 `json:"cacheReadTokens,omitempty"`
			CacheWriteTokens int64 `json:"cacheWriteTokens,omitempty"`
		}{value.InputTokens, value.OutputTokens, value.ReasoningTokens, value.CacheReadTokens, value.CacheWriteTokens}
	case sessionevent.TerminalOutput:
		return struct {
			TerminalID string `json:"terminalId"`
			Offset     uint64 `json:"offset"`
			Data       []byte `json:"data"`
		}{value.TerminalID, value.Offset, value.Data}
	case sessionevent.TerminalExit:
		return struct {
			TerminalID string `json:"terminalId"`
			State      string `json:"state"`
			Code       int    `json:"code"`
			Signal     string `json:"signal,omitempty"`
			Timestamp  int64  `json:"timestamp"`
			Offset     uint64 `json:"offset"`
		}{value.TerminalID, value.State, value.Code, value.Signal, value.Timestamp, value.Offset}
	case sessionevent.MCPStatus:
		return struct {
			ServerID  string `json:"serverId"`
			Name      string `json:"name"`
			Scope     string `json:"scope"`
			Status    string `json:"status"`
			Tools     int    `json:"tools"`
			Prompts   int    `json:"prompts"`
			Resources int    `json:"resources"`
			Revision  uint64 `json:"revision"`
			ErrorCode string `json:"errorCode,omitempty"`
		}{
			value.ServerID, value.Name, value.Scope, value.Status, value.Tools,
			value.Prompts, value.Resources, value.Revision, value.ErrorCode,
		}
	case sessionevent.SnapshotRequired:
		return struct {
			Reason string `json:"reason"`
		}{value.Reason}
	default:
		return payload
	}
}

type toolFileProjection struct {
	Path      string `json:"path,omitempty"`
	SourceURI string `json:"sourceUri"`
	Revision  string `json:"revision"`
}

func projectToolFiles(files []sessionevent.ToolFile) []toolFileProjection {
	if len(files) == 0 {
		return nil
	}
	result := make([]toolFileProjection, len(files))
	for index, file := range files {
		result[index] = toolFileProjection{
			Path: file.Path, SourceURI: file.SourceURI, Revision: file.Revision,
		}
	}
	return result
}
