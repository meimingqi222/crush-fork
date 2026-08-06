package agent

import (
	"context"
	"testing"
	"time"

	"charm.land/fantasy"
	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/oauth/copilot"
	"github.com/stretchr/testify/require"
)

// TestIrcBus_Send_RunningPeer_InjectsWithoutSignalingSteering covers
// docs/refactor-irc.md §2.2(b) and §4.1(a): delivery to a running peer must
// ride the steering queue (EnqueueIRC), carry InitiatorType: InitiatorAgent
// (not the coordinator.Steer-style InitiatorUser -- see the billing
// rationale on deliverIRCViaSteering), and must not go through QueuePrompt.
func TestIrcBus_Send_RunningPeer_InjectsWithoutSignalingSteering(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	coord := newResumeTestCoordinator(t, env, "test-provider", config.ProviderConfig{ID: "test-provider"})
	bus := newIRCBus(coord)

	parentSession, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	childSession, err := env.sessions.CreateTaskSession(t.Context(), "child-running", parentSession.ID, "Running task")
	require.NoError(t, err)

	target := &mockSessionAgent{busy: true}
	coord.agentRegistry.Register(AgentRef{
		ID:              "0-Main::running-1",
		DisplayName:     "Running",
		Kind:            AgentKindSub,
		Status:          AgentStatusRunning,
		Agent:           target,
		SessionID:       childSession.ID,
		ParentSessionID: parentSession.ID,
	})
	coord.childSessionAgents.Store(childSession.ID, target)

	result := bus.Send(t.Context(), agenttools.IrcSendRequest{
		From: coord.mainAgentID,
		To:   "0-Main::running-1",
		Body: "how's it going?",
	})

	require.Equal(t, "injected", result.Outcome)
	require.Empty(t, result.Error)
	require.Len(t, target.ircQueued, 1, "delivery to a running peer must go through EnqueueIRC")
	require.Empty(t, target.queuedPrompts, "must not go through QueuePrompt")
	call := target.ircQueued[0]
	require.Equal(t, "how's it going?", call.Prompt)
	require.True(t, call.PeerSteering)
	require.Equal(t, coord.mainAgentID, call.PeerFrom)
	require.Equal(t, copilot.InitiatorAgent, call.InitiatorType,
		"IRC delivery must carry InitiatorAgent, never InitiatorUser (billing regression -- docs/refactor-irc.md §4.1(a))")
}

// TestIrcBus_Send_IdlePrimary_QueuesWithoutStartingTurn covers §4.1(b): a
// message to the idle primary agent must only queue -- never call Run or
// otherwise start a turn nobody asked for.
func TestIrcBus_Send_IdlePrimary_QueuesWithoutStartingTurn(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	coord := newResumeTestCoordinator(t, env, "test-provider", config.ProviderConfig{ID: "test-provider"})

	parentSession, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)

	realMain := &mockSessionAgent{
		runFunc: func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
			t.Fatal("delivering to an idle primary agent must not start a turn")
			return nil, nil
		},
	}
	coord.currentAgent = realMain
	coord.agentRegistry.Register(AgentRef{
		ID:          coord.mainAgentID,
		DisplayName: "Main",
		Kind:        AgentKindMain,
		Status:      AgentStatusIdle,
		Agent:       realMain,
	})

	// The sender is a subagent of the parent session; its ParentSessionID is
	// what resolveMainSessionID uses to pick which session's queue to use.
	coord.agentRegistry.Register(AgentRef{
		ID:              "0-Main::sender-1",
		DisplayName:     "Sender",
		Kind:            AgentKindSub,
		Status:          AgentStatusRunning,
		ParentSessionID: parentSession.ID,
	})

	bus := newIRCBus(coord)
	result := bus.Send(t.Context(), agenttools.IrcSendRequest{
		From:        "0-Main::sender-1",
		To:          coord.mainAgentID,
		Body:        "status update",
		ExpectReply: true,
	})

	require.Equal(t, "queued", result.Outcome)
	require.Empty(t, result.Reply, "an idle primary agent must not produce a synchronous reply")
	require.Len(t, realMain.ircQueued, 1)
	require.Equal(t, parentSession.ID, realMain.ircQueued[0].SessionID)
}

// TestIrcBus_Send_IdleSubagent_WakesWithRealReply covers docs/refactor-irc.md
// §4: an idle subagent is revived through the coordinator's existing
// resumeSubagent (its own inference model + history, a real turn), not a
// throwaway background generation. This mirrors
// subagent_resume_test.go's TestResumeSubagent_WarmRevive but drives it
// through the bus's Send path instead of calling resumeSubagent directly.
func TestIrcBus_Send_IdleSubagent_WakesWithRealReply(t *testing.T) {
	t.Parallel()

	const providerID = "test-provider"
	env := testEnv(t)
	coord := newResumeTestCoordinator(t, env, providerID, config.ProviderConfig{ID: providerID})
	bus := newIRCBus(coord)

	parentSession, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	childSession, err := env.sessions.CreateTaskSession(t.Context(), "child-idle", parentSession.ID, "Idle task")
	require.NoError(t, err)

	var prompts []string
	agent := newMockAgent(providerID, 4096, yieldingRunFunc(t, env, "idle woke up", &prompts))

	coord.agentRegistry.Register(AgentRef{
		ID:              "0-Main::idle-1",
		DisplayName:     "IdleOne",
		Kind:            AgentKindSub,
		Status:          AgentStatusIdle,
		Agent:           agent,
		SessionID:       childSession.ID,
		ProfileName:     config.AgentGeneral,
		ParentSessionID: parentSession.ID,
	})
	coord.childSessionAgents.Store(childSession.ID, agent)
	coord.lifecycle.Adopt(childSession.ID, "0-Main::idle-1", time.Hour)

	result := bus.Send(t.Context(), agenttools.IrcSendRequest{
		From:        coord.mainAgentID,
		To:          "0-Main::idle-1",
		Body:        "any update?",
		ExpectReply: true,
	})

	require.Equal(t, "woken", result.Outcome)
	require.Equal(t, "idle woke up", result.Reply)
	require.Equal(t, []string{"[IRC message from `0-Main`]\n\nany update?"}, prompts)
}

// TestIrcBus_Send_ParkedSubagent_ColdRevives covers the parked branch of the
// same dispatch: a cold revive still produces a real synchronous reply.
func TestIrcBus_Send_ParkedSubagent_ColdRevives(t *testing.T) {
	t.Parallel()

	const providerID = "test-provider"
	env := testEnv(t)
	coord := newResumeTestCoordinator(t, env, providerID, config.ProviderConfig{ID: providerID})
	bus := newIRCBus(coord)

	parentSession, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	childSession, err := env.sessions.CreateTaskSession(t.Context(), "child-parked", parentSession.ID, "Parked task")
	require.NoError(t, err)

	var prompts []string
	coord.subAgentFactory = func(_ context.Context, requestedType string) (SessionAgent, config.Agent, error) {
		agent := newMockAgent(providerID, 4096, yieldingRunFunc(t, env, "revived from parked", &prompts))
		return agent, config.Agent{ID: requestedType, Mode: config.AgentModeSubagent}, nil
	}

	coord.agentRegistry.Register(AgentRef{
		ID:              "0-Main::parked-1",
		DisplayName:     "ParkedOne",
		Kind:            AgentKindSub,
		Status:          AgentStatusIdle,
		SessionID:       childSession.ID,
		ProfileName:     config.AgentGeneral,
		ParentSessionID: parentSession.ID,
	})
	coord.agentRegistry.SetParked("0-Main::parked-1")

	result := bus.Send(t.Context(), agenttools.IrcSendRequest{
		From: coord.mainAgentID,
		To:   "0-Main::parked-1",
		Body: "wake up",
	})

	require.Equal(t, "revived", result.Outcome)
	require.Equal(t, "revived from parked", result.Reply)
}

// TestIrcBus_Send_AbortedPeer_FailsExplicitly covers docs/refactor-irc.md's
// verification requirement: an aborted peer must fail explicitly, never
// produce a fabricated reply.
func TestIrcBus_Send_AbortedPeer_FailsExplicitly(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	coord := newResumeTestCoordinator(t, env, "test-provider", config.ProviderConfig{ID: "test-provider"})
	bus := newIRCBus(coord)

	coord.agentRegistry.Register(AgentRef{
		ID:          "0-Main::dead-1",
		DisplayName: "Dead",
		Kind:        AgentKindSub,
		Status:      AgentStatusAborted,
	})

	result := bus.Send(t.Context(), agenttools.IrcSendRequest{
		From: coord.mainAgentID,
		To:   "0-Main::dead-1",
		Body: "hello?",
	})

	require.Equal(t, "failed", result.Outcome)
	require.Empty(t, result.Reply)
	require.Contains(t, result.Error, "cannot be resumed")
}

// TestIrcBus_Send_UnknownPeer_Fails covers the not-found path.
func TestIrcBus_Send_UnknownPeer_Fails(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	coord := newResumeTestCoordinator(t, env, "test-provider", config.ProviderConfig{ID: "test-provider"})
	bus := newIRCBus(coord)

	result := bus.Send(t.Context(), agenttools.IrcSendRequest{
		From: coord.mainAgentID,
		To:   "does-not-exist",
		Body: "hello?",
	})

	require.Equal(t, "failed", result.Outcome)
	require.Contains(t, result.Error, "not found")
}

// TestIrcBus_Broadcast_SkipsParkedAndReportsPerTargetOutcome covers
// docs/refactor-irc.md §4/§8: broadcast never revives parked peers (they
// must not even appear in the result set), and every attempted target gets
// its own independent outcome.
func TestIrcBus_Broadcast_SkipsParkedAndReportsPerTargetOutcome(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	coord := newResumeTestCoordinator(t, env, "test-provider", config.ProviderConfig{ID: "test-provider"})
	bus := newIRCBus(coord)

	parentSession, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	runningChild, err := env.sessions.CreateTaskSession(t.Context(), "child-running", parentSession.ID, "Running")
	require.NoError(t, err)

	runningAgent := &mockSessionAgent{busy: true}
	coord.agentRegistry.Register(AgentRef{
		ID:              "0-Main::running-1",
		DisplayName:     "Running",
		Kind:            AgentKindSub,
		Status:          AgentStatusRunning,
		Agent:           runningAgent,
		SessionID:       runningChild.ID,
		ParentSessionID: parentSession.ID,
	})
	coord.childSessionAgents.Store(runningChild.ID, runningAgent)

	coord.agentRegistry.Register(AgentRef{
		ID:          "0-Main::parked-1",
		DisplayName: "Parked",
		Kind:        AgentKindSub,
		Status:      AgentStatusParked,
	})

	coord.agentRegistry.Register(AgentRef{
		ID:          "0-Main::dead-1",
		DisplayName: "Dead",
		Kind:        AgentKindSub,
		Status:      AgentStatusAborted,
	})

	// A second, idle target so the broadcast exercises two independent
	// dispatch paths (steering injection vs a real revive) in one call,
	// proving outcomes are computed per target rather than shared.
	idleChild, err := env.sessions.CreateTaskSession(t.Context(), "child-idle", parentSession.ID, "Idle")
	require.NoError(t, err)
	var idlePrompts []string
	idleAgent := newMockAgent("test-provider", 4096, yieldingRunFunc(t, env, "idle broadcast reply", &idlePrompts))
	coord.agentRegistry.Register(AgentRef{
		ID:              "0-Main::idle-1",
		DisplayName:     "IdleOne",
		Kind:            AgentKindSub,
		Status:          AgentStatusIdle,
		Agent:           idleAgent,
		SessionID:       idleChild.ID,
		ProfileName:     config.AgentGeneral,
		ParentSessionID: parentSession.ID,
	})
	coord.childSessionAgents.Store(idleChild.ID, idleAgent)
	coord.lifecycle.Adopt(idleChild.ID, "0-Main::idle-1", time.Hour)

	results := bus.Broadcast(t.Context(), coord.mainAgentID, "heads up everyone", false, "")

	require.Len(t, results, 2, "running and idle are valid broadcast targets: parked is skipped, aborted is never visible")
	byTo := make(map[string]agenttools.IrcSendResult)
	for _, r := range results {
		byTo[r.To] = r
	}
	require.Equal(t, "injected", byTo["0-Main::running-1"].Outcome)
	require.Equal(t, "woken", byTo["0-Main::idle-1"].Outcome)
	require.Equal(t, "idle broadcast reply", byTo["0-Main::idle-1"].Reply)
	require.Len(t, runningAgent.ircQueued, 1, "must be delivered exactly once")
	require.Equal(t, []string{"[IRC message from `0-Main`]\n\nheads up everyone"}, idlePrompts)
}

// TestIrcBus_Send_BusyPeer_AwaitReply_GetsRealReplyViaWaiter covers
// docs/refactor-irc.md §5 phase 2: a send with await_reply=true to a busy
// peer registers a waiter and blocks; when the peer's own turn sends a reply
// with reply_to=<original message ID>, the waiter is notified and the reply
// is returned synchronously. This replaces the deleted legacyReply fallback
// (RespondAsBackground), so the reply is the peer's real turn output, not a
// no-context background generation.
func TestIrcBus_Send_BusyPeer_AwaitReply_GetsRealReplyViaWaiter(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	coord := newResumeTestCoordinator(t, env, "test-provider", config.ProviderConfig{ID: "test-provider"})
	bus := newIRCBus(coord)

	parentSession, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	childSession, err := env.sessions.CreateTaskSession(t.Context(), "child-busy", parentSession.ID, "Busy task")
	require.NoError(t, err)

	target := &mockSessionAgent{busy: true}
	coord.agentRegistry.Register(AgentRef{
		ID:              "0-Main::busy-1",
		DisplayName:     "Busy",
		Kind:            AgentKindSub,
		Status:          AgentStatusRunning,
		Agent:           target,
		SessionID:       childSession.ID,
		ParentSessionID: parentSession.ID,
	})
	coord.childSessionAgents.Store(childSession.ID, target)

	// Send with await_reply=true in a goroutine -- it should block on the
	// waiter until we deliver a reply.
	type sendResult struct {
		result agenttools.IrcSendResult
		ok     bool
	}
	done := make(chan sendResult, 1)
	go func() {
		r := bus.Send(t.Context(), agenttools.IrcSendRequest{
			From:        coord.mainAgentID,
			To:          "0-Main::busy-1",
			Body:        "what files are you editing?",
			ExpectReply: true,
		})
		done <- sendResult{result: r, ok: true}
	}()

	// Give the send time to register the waiter and block.
	// The message should have been injected into the target's steering queue.
	require.Eventually(t, func() bool {
		target.mu.Lock()
		defer target.mu.Unlock()
		return len(target.ircQueued) == 1
	}, time.Second, 10*time.Millisecond, "message must be injected before reply")

	target.mu.Lock()
	originalMsgID := target.ircQueued[0].PeerMessageID
	target.mu.Unlock()
	require.NotEmpty(t, originalMsgID, "injected message must carry PeerMessageID for reply correlation")

	// Simulate the peer's own turn replying: the peer calls irc send with
	// reply_to=originalMsgID. This goes through the bus's Send -> deliverOne
	// -> deliverReply, which should find the waiter and unblock the sender.
	replyResult := bus.Send(t.Context(), agenttools.IrcSendRequest{
		From:    "0-Main::busy-1",
		To:      coord.mainAgentID,
		Body:    "I'm editing coordinator.go and irc_bus.go",
		ReplyTo: originalMsgID,
	})

	// The reply delivery itself succeeds (it was correlated to the waiter).
	require.NotEqual(t, "failed", replyResult.Outcome)

	// The original send should now complete with the real reply.
	select {
	case r := <-done:
		require.True(t, r.ok)
		require.Equal(t, "injected", r.result.Outcome)
		require.Contains(t, r.result.Reply, "editing coordinator.go")
	case <-time.After(5 * time.Second):
		t.Fatal("send did not complete after reply was delivered")
	}
}

// TestIrcBus_Send_BusyPeer_AwaitReply_TimesOut covers the timeout path:
// when no reply arrives within the timeout, the send returns with the
// "injected" outcome and an empty reply -- the message was delivered, but
// no reply came back (docs/refactor-irc.md §5.2: "超时返回已投递但尚未收到回复").
func TestIrcBus_Send_BusyPeer_AwaitReply_TimesOut(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	coord := newResumeTestCoordinator(t, env, "test-provider", config.ProviderConfig{ID: "test-provider"})
	bus := newIRCBus(coord)

	parentSession, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	childSession, err := env.sessions.CreateTaskSession(t.Context(), "child-busy-to", parentSession.ID, "Busy task")
	require.NoError(t, err)

	target := &mockSessionAgent{busy: true}
	coord.agentRegistry.Register(AgentRef{
		ID:              "0-Main::busy-to-1",
		DisplayName:     "BusyTO",
		Kind:            AgentKindSub,
		Status:          AgentStatusRunning,
		Agent:           target,
		SessionID:       childSession.ID,
		ParentSessionID: parentSession.ID,
	})
	coord.childSessionAgents.Store(childSession.ID, target)

	// Use a short timeout so the test doesn't hang.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	result := bus.Send(ctx, agenttools.IrcSendRequest{
		From:        coord.mainAgentID,
		To:          "0-Main::busy-to-1",
		Body:        "anyone there?",
		ExpectReply: true,
	})

	require.Equal(t, "injected", result.Outcome, "message was delivered even though no reply came")
	require.Empty(t, result.Reply, "no reply should be available on timeout")
}

// TestIrcBus_Wait_ForSpecificMessageID covers op=wait: after a send with
// await_reply=false, the sender can use op=wait with message_id to block on
// a late reply.
func TestIrcBus_Wait_ForSpecificMessageID(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	coord := newResumeTestCoordinator(t, env, "test-provider", config.ProviderConfig{ID: "test-provider"})
	bus := newIRCBus(coord)

	parentSession, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	childSession, err := env.sessions.CreateTaskSession(t.Context(), "child-wait", parentSession.ID, "Wait task")
	require.NoError(t, err)

	target := &mockSessionAgent{busy: true}
	coord.agentRegistry.Register(AgentRef{
		ID:              "0-Main::busy-wait-1",
		DisplayName:     "BusyWait",
		Kind:            AgentKindSub,
		Status:          AgentStatusRunning,
		Agent:           target,
		SessionID:       childSession.ID,
		ParentSessionID: parentSession.ID,
	})
	coord.childSessionAgents.Store(childSession.ID, target)

	// Fire-and-forget send (await_reply=false).
	sendResult := bus.Send(t.Context(), agenttools.IrcSendRequest{
		From:        coord.mainAgentID,
		To:          "0-Main::busy-wait-1",
		Body:        "check this",
		ExpectReply: false,
	})
	require.Equal(t, "injected", sendResult.Outcome)
	msgID := sendResult.MessageID
	require.NotEmpty(t, msgID)

	// Wait for the reply in a goroutine.
	waitDone := make(chan agenttools.IrcWaitResult, 1)
	go func() {
		waitDone <- bus.Wait(t.Context(), agenttools.IrcWaitRequest{
			MessageID: msgID,
			Timeout:   5 * time.Second,
		})
	}()

	// Wait for the waiter to be registered before sending the reply.
	require.Eventually(t, func() bool {
		bus.waitersMu.Lock()
		defer bus.waitersMu.Unlock()
		_, ok := bus.waiters[msgID]
		return ok
	}, time.Second, 5*time.Millisecond, "waiter must be registered before reply")

	// Simulate the peer replying.
	target.mu.Lock()
	require.Equal(t, 1, len(target.ircQueued))
	originalMsgID := target.ircQueued[0].PeerMessageID
	target.mu.Unlock()
	require.Equal(t, msgID, originalMsgID, "PeerMessageID must match the send result's MessageID")
	bus.Send(t.Context(), agenttools.IrcSendRequest{
		From:    "0-Main::busy-wait-1",
		To:      coord.mainAgentID,
		Body:    "checked, looks good",
		ReplyTo: originalMsgID,
	})

	select {
	case r := <-waitDone:
		require.False(t, r.TimedOut)
		require.Contains(t, r.Reply, "looks good")
		require.Equal(t, "0-Main::busy-wait-1", r.From)
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not complete after reply was delivered")
	}
}

// TestIrcBus_ReplyCorrelation_NoWaiterRegistered covers the case where a
// reply arrives but no waiter is registered (the sender timed out or never
// asked for a reply). The reply should still be delivered as a regular
// message to the target.
func TestIrcBus_ReplyCorrelation_NoWaiterRegistered(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	coord := newResumeTestCoordinator(t, env, "test-provider", config.ProviderConfig{ID: "test-provider"})
	bus := newIRCBus(coord)

	parentSession, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)

	// Register the primary agent as idle with a live mock.
	realMain := &mockSessionAgent{busy: false}
	coord.currentAgent = realMain
	coord.agentRegistry.Register(AgentRef{
		ID:          coord.mainAgentID,
		DisplayName: "Main",
		Kind:        AgentKindMain,
		Status:      AgentStatusIdle,
		Agent:       realMain,
	})

	// Register the sender subagent.
	coord.agentRegistry.Register(AgentRef{
		ID:              "0-Main::sender-1",
		DisplayName:     "Sender",
		Kind:            AgentKindSub,
		Status:          AgentStatusRunning,
		ParentSessionID: parentSession.ID,
	})

	// Send a reply_to message with no waiter registered. It should be
	// delivered as a regular message to the main agent's steering queue.
	result := bus.Send(t.Context(), agenttools.IrcSendRequest{
		From:    "0-Main::sender-1",
		To:      coord.mainAgentID,
		Body:    "late reply",
		ReplyTo: "nonexistent-msg-id",
	})

	// The main agent is idle, so the outcome is "queued" (§4.1(b)).
	require.Equal(t, "queued", result.Outcome)
	require.Len(t, realMain.ircQueued, 1, "reply with no waiter should be delivered as a regular message")
}
