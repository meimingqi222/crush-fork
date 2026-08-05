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
	// Steer messages are tied to the active run; a user cancel should
	// discard them so the next turn starts clean.
	a.clearSteeringQueue(sessionID)
	a.steeringSignals.Del(sessionID)
	a.pausedQueues.Del(sessionID)
}

func (a *sessionAgent) EnqueueSteer(sessionID string, call SessionAgentCall) bool {
	if !a.IsSessionBusy(sessionID) {
		return false
	}
	call.SessionID = sessionID
	call.Steering = true
	a.enqueueSteer(sessionID, call)
	// Notify the running tools that a mid-turn steering message arrived so
	// cooperative tools (e.g. foreground bash) can yield at a safe point.
	a.signalSteering(sessionID)
	return true
}

func (a *sessionAgent) RemoveQueuedTurn(sessionID, turnID string) bool {
	a.queueMu.Lock()
	defer a.queueMu.Unlock()
	removed := false
	if queuedCalls, ok := a.messageQueue.Get(sessionID); ok {
		for index := range queuedCalls {
			if queuedCalls[index].TurnID != turnID {
				continue
			}
			updated := append(queuedCalls[:index:index], queuedCalls[index+1:]...)
			a.setQueuedCallsLocked(sessionID, updated)
			removed = true
			break
		}
	}
	if steering, ok := a.steeringQueue.Get(sessionID); ok {
		for index := range steering {
			if steering[index].TurnID != turnID {
				continue
			}
			updated := append(steering[:index:index], steering[index+1:]...)
			a.setSteeringCallsLocked(sessionID, updated)
			removed = true
			break
		}
	}
	return removed
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
	// Steering messages are tied to the active run; clearing the queue
	// should discard them too so the next turn starts clean.
	a.clearSteeringQueue(sessionID)
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

// ActiveTurnID returns the id of the turn currently running for sessionID, or
// "" when the session is idle or its own request has no turn id (e.g. a
// background/system call). Backs the GUI snapshot's activeTurn.id — without
// it, a snapshot taken while busy reports state:"running" with an empty id,
// which the gui-acp client correctly rejects as invalid.
func (a *sessionAgent) ActiveTurnID(sessionID string) string {
	id, _ := a.activeTurnIDs.Get(sessionID)
	return id
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

// enqueueSteer appends a steering message to the session's dedicated
// steering queue. Unlike enqueueQueuedCall this never sets JoinActiveRun:
// steering messages are drained through popSteeringCalls at safe mid-turn
// points and formatted with a priority notice, so they must not share the
// join-active-run budget, dedupe, or queue-pause semantics.
func (a *sessionAgent) enqueueSteer(sessionID string, call SessionAgentCall) {
	a.queueMu.Lock()
	defer a.queueMu.Unlock()

	steering, _ := a.steeringQueue.Get(sessionID)
	steering = append(append([]SessionAgentCall(nil), steering...), call)
	a.setSteeringCallsLocked(sessionID, steering)
}

// popSteeringCalls drains and returns all pending steering messages for the
// session in FIFO order. It is the only consumer of the steering queue and
// is called exclusively at safe drain points (PrepareStep) or to flush
// stranded messages at the end of a run.
func (a *sessionAgent) popSteeringCalls(sessionID string) []SessionAgentCall {
	a.queueMu.Lock()
	defer a.queueMu.Unlock()

	steering, ok := a.steeringQueue.Get(sessionID)
	if !ok || len(steering) == 0 {
		return nil
	}
	a.steeringQueue.Del(sessionID)
	return steering
}

// clearSteeringQueue discards all pending steering messages for the session.
func (a *sessionAgent) clearSteeringQueue(sessionID string) {
	a.queueMu.Lock()
	defer a.queueMu.Unlock()

	a.steeringQueue.Del(sessionID)
}

// queuedSteeringSnapshot returns a copy of the session's pending steering
// messages without draining them.
func (a *sessionAgent) queuedSteeringSnapshot(sessionID string) []SessionAgentCall {
	a.queueMu.Lock()
	defer a.queueMu.Unlock()

	steering, ok := a.steeringQueue.Get(sessionID)
	if !ok || len(steering) == 0 {
		return nil
	}
	return append([]SessionAgentCall(nil), steering...)
}

func (a *sessionAgent) setSteeringCallsLocked(sessionID string, steering []SessionAgentCall) {
	if len(steering) == 0 {
		a.steeringQueue.Del(sessionID)
		return
	}
	a.steeringQueue.Set(sessionID, append([]SessionAgentCall(nil), steering...))
}

// signalSteering cancels the session's current cooperative steering signal
// so running tools can notice the mid-turn message and yield at a safe
// point. No-op when no signal is registered (e.g. the run just started and
// PrepareStep has not injected a signal yet).
func (a *sessionAgent) signalSteering(sessionID string) {
	if cancel, ok := a.steeringSignals.Get(sessionID); ok && cancel != nil {
		cancel()
	}
}

// flushStrandedSteeringMessages promotes steering messages that missed every
// mid-turn drain point (e.g. they arrived between the last PrepareStep and
// the end of the run) to the front of the regular queue, so the run's
// queued-message dispatch starts them as their own turn instead of silently
// dropping them.
func (a *sessionAgent) flushStrandedSteeringMessages(sessionID string) {
	stranded := a.popSteeringCalls(sessionID)
	if len(stranded) == 0 {
		return
	}

	a.queueMu.Lock()
	defer a.queueMu.Unlock()

	queuedCalls, _ := a.messageQueue.Get(sessionID)
	queuedCalls = append(append([]SessionAgentCall(nil), stranded...), queuedCalls...)
	a.setQueuedCallsLocked(sessionID, queuedCalls)
}

// formatSteeringPrompt wraps a mid-turn steering message so the model
// recognizes it as a high-priority instruction that supersedes earlier
// directions, rather than an ordinary user message. The prompt is capped at
// steeringMaxPromptChars runes to guard against pathological input.
func formatSteeringPrompt(prompt string) string {
	runes := []rune(prompt)
	if len(runes) > steeringMaxPromptChars {
		prompt = string(runes[:steeringMaxPromptChars-1]) + "…"
	}
	return "The user sent a message while you were working. Treat it as the active instruction; it supersedes earlier directions if they conflict.\n<user_query>\n" + prompt + "\n</user_query>"
}
