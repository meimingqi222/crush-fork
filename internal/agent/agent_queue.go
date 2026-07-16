package agent

import (
	"context"
	"log/slog"
	"time"

	"github.com/charmbracelet/crush/internal/sessionevent"
)

func (a *sessionAgent) Cancel(sessionID string) {
	// Cancel regular requests. Don't use Take() here - we need the entry to
	// remain in activeRequests so IsBusy() returns true until the goroutine
	// fully completes (including error handling that may access the DB).
	// The defer in processRequest will clean up the entry.
	if cancel, ok := a.activeRequests.Get(sessionID); ok && cancel != nil {
		turnID, _ := a.activeTurnIDs.Get(sessionID)
		slog.Debug("Request cancellation initiated", "session_id", sessionID)
		a.publishSessionEvent(context.Background(), sessionID, time.Now(), sessionevent.NewEvent{
			Kind:     sessionevent.KindCancelAcknowledged,
			Delivery: sessionevent.DeliveryReliable,
			Payload:  sessionevent.TurnEvent{TurnID: turnID, Reason: "user_requested"},
		})
		cancel()
	}

	// Also check for summarize requests.
	if cancel, ok := a.activeRequests.Get(sessionID + "-summarize"); ok && cancel != nil {
		slog.Debug("Summarize cancellation initiated", "session_id", sessionID)
		cancel()
	}

	if a.QueuedPrompts(sessionID) > 0 {
		slog.Debug("Clearing queued prompts", "session_id", sessionID)
		a.clearQueuedCalls(sessionID)
	}
	a.pausedQueues.Del(sessionID)
}

func (a *sessionAgent) EnqueueSteer(sessionID string, call SessionAgentCall) bool {
	if !a.IsSessionBusy(sessionID) {
		return false
	}
	call.SessionID = sessionID
	call.JoinActiveRun = true
	a.enqueueQueuedCall(sessionID, call)
	return true
}

func (a *sessionAgent) RemoveQueuedTurn(sessionID, turnID string) bool {
	a.queueMu.Lock()
	defer a.queueMu.Unlock()
	queuedCalls, ok := a.messageQueue.Get(sessionID)
	if !ok {
		return false
	}
	for index := range queuedCalls {
		if queuedCalls[index].TurnID != turnID {
			continue
		}
		updated := append(queuedCalls[:index:index], queuedCalls[index+1:]...)
		a.setQueuedCallsLocked(sessionID, updated)
		return true
	}
	return false
}

func (a *sessionAgent) RemoveQueuedPrompt(sessionID string, index int) bool {
	if !a.removeQueuedCall(sessionID, index) {
		return false
	}

	slog.Debug("Removing queued prompt", "session_id", sessionID, "index", index)
	if a.QueuedPrompts(sessionID) == 0 {
		a.pausedQueues.Del(sessionID)
	}
	return true
}

func (a *sessionAgent) ClearQueue(sessionID string) {
	if a.QueuedPrompts(sessionID) > 0 {
		slog.Debug("Clearing queued prompts", "session_id", sessionID)
		a.clearQueuedCalls(sessionID)
	}
	// Auto-unpause when the queue is cleared.
	a.pausedQueues.Del(sessionID)
}

func (a *sessionAgent) PrioritizeQueuedPrompt(sessionID string, index int) bool {
	if !a.prioritizeQueuedCall(sessionID, index) {
		return false
	}
	slog.Debug("Prioritizing queued prompt", "session_id", sessionID, "index", index)
	return true
}

// PauseQueue pauses automatic processing of queued prompts for the session.
// The current request (if any) continues, but the next queued prompt won't
// be automatically started. Use this to stop the queue without clearing it.
func (a *sessionAgent) PauseQueue(sessionID string) {
	a.pausedQueues.Set(sessionID, true)
	slog.Debug("Queue paused", "session_id", sessionID)
}

// ResumeQueue resumes automatic processing of queued prompts for the session.
// If there are queued prompts and no active request, it starts the next one.
func (a *sessionAgent) ResumeQueue(sessionID string) {
	a.pausedQueues.Del(sessionID)
	slog.Debug("Queue resumed", "session_id", sessionID)

	if a.IsSessionBusy(sessionID) {
		return
	}
	firstQueuedMessage, ok := a.popNextQueuedCall(sessionID)
	if !ok {
		return
	}
	go func(call SessionAgentCall) {
		if _, err := a.Run(context.Background(), call); err != nil {
			slog.Warn("Failed to resume queued prompt", "session_id", sessionID, "error", err)
		}
	}(firstQueuedMessage)
}

// IsQueuePaused reports whether the queue is paused for the session.
func (a *sessionAgent) IsQueuePaused(sessionID string) bool {
	paused, _ := a.pausedQueues.Get(sessionID)
	return paused
}

func (a *sessionAgent) CancelAll() {
	if !a.IsBusy() {
		return
	}
	for key := range a.activeRequests.Seq2() {
		a.Cancel(key) // key is sessionID
	}

	timeout := time.After(5 * time.Second)
	for a.IsBusy() {
		select {
		case <-timeout:
			return
		default:
			time.Sleep(200 * time.Millisecond)
		}
	}

	a.extractionMu.Lock()
	pending := len(a.pendingExtractions)
	a.extractionMu.Unlock()
	if pending > 0 {
		slog.Debug("Waiting for pending memory extractions", "count", pending)
		time.Sleep(2 * time.Second)
	}
}

func (a *sessionAgent) IsBusy() bool {
	var busy bool
	for cancelFunc := range a.activeRequests.Seq() {
		if cancelFunc != nil {
			busy = true
			break
		}
	}
	return busy
}

func (a *sessionAgent) IsSessionBusy(sessionID string) bool {
	_, busy := a.activeRequests.Get(sessionID)
	return busy
}

func (a *sessionAgent) QueuedPrompts(sessionID string) int {
	return len(a.queuedCallsSnapshot(sessionID))
}

func (a *sessionAgent) QueuedPromptsList(sessionID string) []string {
	l := a.queuedCallsSnapshot(sessionID)
	prompts := make([]string, len(l))
	for i, call := range l {
		prompts[i] = call.Prompt
	}
	return prompts
}

func (a *sessionAgent) enqueueQueuedCall(sessionID string, call SessionAgentCall) {
	a.queueMu.Lock()
	defer a.queueMu.Unlock()

	queuedCalls, _ := a.messageQueue.Get(sessionID)
	queuedCalls = append(append([]SessionAgentCall(nil), queuedCalls...), call)
	a.setQueuedCallsLocked(sessionID, queuedCalls)
}

func (a *sessionAgent) takeJoinActiveRunCalls(sessionID string) []SessionAgentCall {
	a.queueMu.Lock()
	defer a.queueMu.Unlock()

	queuedCalls, ok := a.messageQueue.Get(sessionID)
	if !ok || len(queuedCalls) == 0 {
		return nil
	}

	joinActiveRunCalls := make([]SessionAgentCall, 0, len(queuedCalls))
	remainingCalls := make([]SessionAgentCall, 0, len(queuedCalls))
	for _, queuedCall := range queuedCalls {
		if queuedCall.JoinActiveRun {
			joinActiveRunCalls = append(joinActiveRunCalls, queuedCall)
			continue
		}
		remainingCalls = append(remainingCalls, queuedCall)
	}
	a.setQueuedCallsLocked(sessionID, remainingCalls)
	return joinActiveRunCalls
}

func (a *sessionAgent) popNextQueuedCall(sessionID string) (SessionAgentCall, bool) {
	a.queueMu.Lock()
	defer a.queueMu.Unlock()

	queuedCalls, ok := a.messageQueue.Get(sessionID)
	if !ok || len(queuedCalls) == 0 {
		return SessionAgentCall{}, false
	}

	nextCall := queuedCalls[0]
	a.setQueuedCallsLocked(sessionID, queuedCalls[1:])
	return nextCall, true
}

func (a *sessionAgent) removeQueuedCall(sessionID string, index int) bool {
	a.queueMu.Lock()
	defer a.queueMu.Unlock()

	queuedCalls, ok := a.messageQueue.Get(sessionID)
	if !ok || index < 0 || index >= len(queuedCalls) {
		return false
	}

	updatedQueue := append(queuedCalls[:index:index], queuedCalls[index+1:]...)
	a.setQueuedCallsLocked(sessionID, updatedQueue)
	return true
}

func (a *sessionAgent) prioritizeQueuedCall(sessionID string, index int) bool {
	a.queueMu.Lock()
	defer a.queueMu.Unlock()

	queuedCalls, ok := a.messageQueue.Get(sessionID)
	if !ok || index < 0 || index >= len(queuedCalls) {
		return false
	}

	call := queuedCalls[index]
	call.JoinActiveRun = true

	newQueue := make([]SessionAgentCall, 0, len(queuedCalls))
	newQueue = append(newQueue, call)
	newQueue = append(newQueue, queuedCalls[:index]...)
	newQueue = append(newQueue, queuedCalls[index+1:]...)
	a.setQueuedCallsLocked(sessionID, newQueue)
	return true
}

func (a *sessionAgent) clearQueuedCalls(sessionID string) {
	a.queueMu.Lock()
	defer a.queueMu.Unlock()

	a.messageQueue.Del(sessionID)
}

func (a *sessionAgent) queuedCallsSnapshot(sessionID string) []SessionAgentCall {
	a.queueMu.Lock()
	defer a.queueMu.Unlock()

	queuedCalls, ok := a.messageQueue.Get(sessionID)
	if !ok || len(queuedCalls) == 0 {
		return nil
	}
	return append([]SessionAgentCall(nil), queuedCalls...)
}

func (a *sessionAgent) setQueuedCallsLocked(sessionID string, queuedCalls []SessionAgentCall) {
	if len(queuedCalls) == 0 {
		a.messageQueue.Del(sessionID)
		return
	}
	a.messageQueue.Set(sessionID, append([]SessionAgentCall(nil), queuedCalls...))
}
