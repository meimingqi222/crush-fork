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
