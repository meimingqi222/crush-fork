package agent

import (
	"context"
	"testing"
	"time"

	"charm.land/fantasy"
	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// newResumeTestCoordinator builds on newTestCoordinator (coordinator_test.go)
// with the registry/lifecycle wiring resumeSubagent, queueSubagentFollowUp,
// and backgroundAgentMessenger's AgentRegistry fallback all depend on.
// newTestCoordinator itself leaves agentRegistry/lifecycle/backgroundAgents
// nil, which is fine for the existing runSubAgentDirect-only tests but not
// for anything touching subagent addressing/revival.
func newResumeTestCoordinator(t *testing.T, env fakeEnv, providerID string, providerCfg config.ProviderConfig) *coordinator {
	t.Helper()
	coord := newTestCoordinator(t, env, providerID, providerCfg)
	coord.mainAgentID = "0-Main"
	coord.agentRegistry = &AgentRegistry{refs: make(map[string]*AgentRef)}
	coord.lifecycle = newSubagentLifecycleManager(coord.agentRegistry, &coord.childSessionAgents)
	coord.backgroundAgents = newBackgroundAgentRegistry()
	return coord
}

// yieldingRunFunc returns a mock SessionAgent runFunc that records every
// prompt it is called with, submits a completed yield tool result (so
// ensureSubagentYield sees a clean completion), and returns responseText as
// the run's final text.
func yieldingRunFunc(t *testing.T, env fakeEnv, responseText string, prompts *[]string) func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
	t.Helper()
	return func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		*prompts = append(*prompts, call.Prompt)
		_, err := env.messages.Create(ctx, call.SessionID, message.CreateMessageParams{
			Role: message.Tool,
			Parts: []message.ContentPart{
				message.ToolResult{Name: agenttools.YieldToolName}.WithYield(message.ToolResultYield{
					Status: string(message.ToolResultSubtaskStatusCompleted),
					Data:   responseText,
				}),
			},
		})
		require.NoError(t, err)
		return agentResultWithText(responseText), nil
	}
}

// TestResumeSubagent_WarmRevive is test 2 from
// docs/refactor-subagent-continuation.md §6: within the Adopt window,
// resumeSubagent must reuse the exact same live SessionAgent instance,
// thread ExistingSessionID through to runSubAgentDirect (proven here by the
// child session already existing and Get, not CreateTaskSession, being the
// only way that succeeds), and skip the handoff prefix (R4) -- proven by
// the mock observing the raw follow-up prompt with no prepended context.
func TestResumeSubagent_WarmRevive(t *testing.T) {
	t.Parallel()

	const providerID = "test-provider"
	env := testEnv(t)
	coord := newResumeTestCoordinator(t, env, providerID, config.ProviderConfig{ID: providerID})

	parentSession, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	childSession, err := env.sessions.CreateTaskSession(t.Context(), "child-warm", parentSession.ID, "Explore task")
	require.NoError(t, err)

	var prompts []string
	agent := newMockAgent(providerID, 4096, yieldingRunFunc(t, env, "warm response", &prompts))

	ref := coord.agentRegistry.Register(AgentRef{
		ID:              "0-Main::explore-1",
		DisplayName:     "Explore",
		Kind:            AgentKindSub,
		ParentID:        coord.mainAgentID,
		Status:          AgentStatusIdle,
		Agent:           agent,
		SessionID:       childSession.ID,
		ProfileName:     config.AgentGeneral,
		ParentSessionID: parentSession.ID,
	})
	coord.childSessionAgents.Store(childSession.ID, agent)
	coord.lifecycle.Adopt(childSession.ID, ref.ID, time.Hour)

	snap, ok := coord.agentRegistry.FullSnapshot(ref.ID)
	require.True(t, ok)
	require.Equal(t, AgentStatusIdle, snap.Status)

	resp, err := coord.resumeSubagent(t.Context(), snap, "follow-up prompt")
	require.NoError(t, err)
	require.Equal(t, "warm response", resp.Content)
	require.Equal(t, []string{"follow-up prompt"}, prompts,
		"the raw follow-up prompt must reach the agent with no handoff prefix (R4)")

	live, ok := coord.childSessionAgents.Load(childSession.ID)
	require.True(t, ok)
	require.Same(t, agent, live, "warm revive must keep reusing the same SessionAgent instance")

	updated, ok := coord.agentRegistry.Get(ref.ID)
	require.True(t, ok)
	require.Equal(t, AgentStatusIdle, updated.Status)
}

// TestResumeSubagent_ColdRevive is test 3 from
// docs/refactor-subagent-continuation.md §6: once parked, resumeSubagent
// must rebuild a fresh SessionAgent instance from the ref's saved profile
// (via buildSubAgentForType, the same entrypoint spawn uses -- proven here
// by asserting the exact requestedType/role fed into it), and the child
// session's history must be loaded from SQLite rather than replaced (the
// message count grows instead of resetting).
func TestResumeSubagent_ColdRevive(t *testing.T) {
	t.Parallel()

	const providerID = "test-provider"
	env := testEnv(t)
	coord := newResumeTestCoordinator(t, env, providerID, config.ProviderConfig{ID: providerID})

	parentSession, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	childSession, err := env.sessions.CreateTaskSession(t.Context(), "child-cold", parentSession.ID, "Explore task")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), childSession.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "original assignment"}},
	})
	require.NoError(t, err)

	var (
		buildRequestedTypes []string
		prompts             []string
	)
	coord.subAgentFactory = func(_ context.Context, requestedType string) (SessionAgent, config.Agent, error) {
		buildRequestedTypes = append(buildRequestedTypes, requestedType)
		agent := newMockAgent(providerID, 4096, yieldingRunFunc(t, env, "cold response", &prompts))
		return agent, config.Agent{ID: requestedType, Mode: config.AgentModeSubagent}, nil
	}

	ref := coord.agentRegistry.Register(AgentRef{
		ID:              "0-Main::explore-1",
		DisplayName:     "Explore",
		Kind:            AgentKindSub,
		ParentID:        coord.mainAgentID,
		Status:          AgentStatusIdle,
		SessionID:       childSession.ID,
		ProfileName:     "explore",
		ParentSessionID: parentSession.ID,
		Role:            "researcher",
	})
	// Simulate the TTL firing exactly like subagentLifecycleManager.Park:
	// the in-memory instance is released and the entry demoted.
	coord.agentRegistry.SetParked(ref.ID)

	before, err := env.messages.List(t.Context(), childSession.ID)
	require.NoError(t, err)

	snap, ok := coord.agentRegistry.FullSnapshot(ref.ID)
	require.True(t, ok)
	require.Equal(t, AgentStatusParked, snap.Status)
	require.Nil(t, snap.Agent)

	resp, err := coord.resumeSubagent(t.Context(), snap, "cold follow-up prompt")
	require.NoError(t, err)
	require.Equal(t, "cold response", resp.Content)
	require.Equal(t, []string{"explore"}, buildRequestedTypes,
		"cold revive must rebuild through buildSubAgentForType using the spawn-time profile identity")
	require.Equal(t, []string{"cold follow-up prompt"}, prompts, "handoff prefix must still be skipped (R4)")

	after, err := env.messages.List(t.Context(), childSession.ID)
	require.NoError(t, err)
	require.Greater(t, len(after), len(before),
		"cold revive must load the persisted child session history, not start a fresh one")

	updated, ok := coord.agentRegistry.Get(ref.ID)
	require.True(t, ok)
	require.Equal(t, AgentStatusIdle, updated.Status, "a successful cold revive re-adopts the entry as Idle")
}

// TestResumeSubagent_ColdRevive_DowngradesWorktreeIsolation covers R3: a
// parked subagent that originally ran with worktree isolation must not
// attempt to recreate the (already merged-back and removed) worktree on
// cold revive. It should downgrade to a shared-workspace isolation and say
// so in the response text.
func TestResumeSubagent_ColdRevive_DowngradesWorktreeIsolation(t *testing.T) {
	t.Parallel()

	const providerID = "test-provider"
	env := testEnv(t)
	coord := newResumeTestCoordinator(t, env, providerID, config.ProviderConfig{ID: providerID})

	parentSession, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	childSession, err := env.sessions.CreateTaskSession(t.Context(), "child-worktree", parentSession.ID, "Designer task")
	require.NoError(t, err)

	var prompts []string
	coord.subAgentFactory = func(_ context.Context, requestedType string) (SessionAgent, config.Agent, error) {
		agent := newMockAgent(providerID, 4096, yieldingRunFunc(t, env, "worktree-revived", &prompts))
		return agent, config.Agent{ID: requestedType, Mode: config.AgentModeSubagent}, nil
	}

	ref := coord.agentRegistry.Register(AgentRef{
		ID:              "0-Main::designer-1",
		DisplayName:     "Designer",
		Kind:            AgentKindSub,
		ParentID:        coord.mainAgentID,
		Status:          AgentStatusIdle,
		SessionID:       childSession.ID,
		ProfileName:     "designer",
		ParentSessionID: parentSession.ID,
		Isolation:       "worktree",
	})
	coord.agentRegistry.SetParked(ref.ID)

	snap, ok := coord.agentRegistry.FullSnapshot(ref.ID)
	require.True(t, ok)

	resp, err := coord.resumeSubagent(t.Context(), snap, "continue the design")
	require.NoError(t, err)
	require.Contains(t, resp.Content, "worktree-revived")
	require.Contains(t, resp.Content, "shared parent workspace",
		"the response must note the worktree isolation was downgraded")
}

// TestBackgroundAgentMessenger_AddressesAllTargetKinds is test 4 from
// docs/refactor-subagent-continuation.md §6: send_message's dispatch
// function (backgroundAgentMessenger) must correctly route to a background
// agent, a foreground idle subagent (warm revive), a foreground parked
// subagent (cold revive), report a hard failure for an aborted subagent,
// and report "not found" for an unknown address.
func TestBackgroundAgentMessenger_AddressesAllTargetKinds(t *testing.T) {
	t.Parallel()

	const providerID = "test-provider"
	env := testEnv(t)
	coord := newResumeTestCoordinator(t, env, providerID, config.ProviderConfig{ID: providerID})
	messenger := coord.backgroundAgentMessenger()

	parentSession, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)

	t.Run("background agent", func(t *testing.T) {
		agentID := coord.backgroundAgents.RegisterNamed("bg-explore", "general", "bg task",
			func(context.Context, backgroundAgentCommand) backgroundAgentRunResult {
				return backgroundAgentRunResult{Status: backgroundAgentStatusCompleted, Content: "bg done"}
			})

		disposition, found, err := messenger(t.Context(), agentID, "hello")
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, "queued", disposition)
	})

	t.Run("foreground idle subagent", func(t *testing.T) {
		childSession, err := env.sessions.CreateTaskSession(t.Context(), "child-idle", parentSession.ID, "Idle task")
		require.NoError(t, err)
		var prompts []string
		agent := newMockAgent(providerID, 4096, yieldingRunFunc(t, env, "idle-revived", &prompts))
		ref := coord.agentRegistry.Register(AgentRef{
			ID:              "0-Main::idle-1",
			DisplayName:     "IdleOne",
			Kind:            AgentKindSub,
			ParentID:        coord.mainAgentID,
			Status:          AgentStatusIdle,
			Agent:           agent,
			SessionID:       childSession.ID,
			ProfileName:     config.AgentGeneral,
			ParentSessionID: parentSession.ID,
		})
		coord.childSessionAgents.Store(childSession.ID, agent)
		coord.lifecycle.Adopt(childSession.ID, ref.ID, time.Hour)

		disposition, found, err := messenger(t.Context(), ref.ID, "hi idle")
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, "idle-revived", disposition)
	})

	t.Run("foreground parked subagent", func(t *testing.T) {
		childSession, err := env.sessions.CreateTaskSession(t.Context(), "child-parked", parentSession.ID, "Parked task")
		require.NoError(t, err)
		var prompts []string
		coord.subAgentFactory = func(_ context.Context, requestedType string) (SessionAgent, config.Agent, error) {
			agent := newMockAgent(providerID, 4096, yieldingRunFunc(t, env, "parked-revived", &prompts))
			return agent, config.Agent{ID: requestedType, Mode: config.AgentModeSubagent}, nil
		}
		ref := coord.agentRegistry.Register(AgentRef{
			ID:              "0-Main::parked-1",
			DisplayName:     "ParkedOne",
			Kind:            AgentKindSub,
			ParentID:        coord.mainAgentID,
			Status:          AgentStatusIdle,
			SessionID:       childSession.ID,
			ProfileName:     config.AgentGeneral,
			ParentSessionID: parentSession.ID,
		})
		coord.agentRegistry.SetParked(ref.ID)

		disposition, found, err := messenger(t.Context(), ref.ID, "hi parked")
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, "parked-revived", disposition)
		coord.subAgentFactory = nil
	})

	t.Run("aborted subagent cannot be resumed", func(t *testing.T) {
		ref := coord.agentRegistry.Register(AgentRef{
			ID:          "0-Main::aborted-1",
			DisplayName: "AbortedOne",
			Kind:        AgentKindSub,
			ParentID:    coord.mainAgentID,
			Status:      AgentStatusAborted,
		})

		disposition, found, err := messenger(t.Context(), ref.ID, "hi aborted")
		require.Error(t, err)
		require.Contains(t, err.Error(), "cannot be resumed")
		require.True(t, found)
		require.Empty(t, disposition)
	})

	t.Run("unknown address", func(t *testing.T) {
		disposition, found, err := messenger(t.Context(), "does-not-exist", "hi")
		require.NoError(t, err)
		require.False(t, found)
		require.Empty(t, disposition)
	})
}

// TestResolveSubagentRef_AmbiguousDisplayName is test 5 from
// docs/refactor-subagent-continuation.md §6 (R2): two subagents sharing a
// DisplayName must produce an addressing error listing both candidate IDs
// rather than silently resolving to either one.
func TestResolveSubagentRef_AmbiguousDisplayName(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	coord := newResumeTestCoordinator(t, env, "test-provider", config.ProviderConfig{ID: "test-provider"})

	coord.agentRegistry.Register(AgentRef{ID: "0-Main::explore-1", DisplayName: "explore", Kind: AgentKindSub, Status: AgentStatusParked})
	coord.agentRegistry.Register(AgentRef{ID: "0-Main::explore-2", DisplayName: "explore", Kind: AgentKindSub, Status: AgentStatusIdle})

	_, err := coord.resolveSubagentRef("explore")
	require.Error(t, err)
	require.Contains(t, err.Error(), "0-Main::explore-1")
	require.Contains(t, err.Error(), "0-Main::explore-2")

	// The messenger fallback must surface the same ambiguity as an error
	// (found=true, not a silent "not found").
	messenger := coord.backgroundAgentMessenger()
	disposition, found, msgErr := messenger(t.Context(), "explore", "hi")
	require.Error(t, msgErr)
	require.True(t, found)
	require.Empty(t, disposition)
}

// TestQueueSubagentFollowUp_RunningSubagent is test 6 from
// docs/refactor-subagent-continuation.md §6 (R1): a follow-up addressed to
// a running subagent must be queued (never executed as a second concurrent
// Run), and it is automatically consumed once the current turn ends. This
// uses the real *sessionAgent (not mockSessionAgent) because the
// auto-drain-at-end-of-turn behavior lives inside sessionAgent.Run itself.
func TestQueueSubagentFollowUp_RunningSubagent(t *testing.T) {
	t.Parallel()

	const providerID = "test-provider"
	env := testEnv(t)

	testAgent := &queuePrepareTestAgent{t: t}
	subAgent := newResumeQueueTestSessionAgent(env, providerID, testAgent)

	childSession, err := env.sessions.Create(t.Context(), "running subagent")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), childSession.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "seed"}},
	})
	require.NoError(t, err)

	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.Config().Providers.Set(providerID, config.ProviderConfig{ID: providerID})

	coord := &coordinator{cfg: cfg, sessions: env.sessions, messages: env.messages}
	coord.childSessionAgents.Store(childSession.ID, subAgent)

	ref := AgentRef{
		ID:          "sub-running-1",
		DisplayName: "Running",
		Kind:        AgentKindSub,
		SessionID:   childSession.ID,
		ProfileName: config.AgentGeneral,
		Status:      AgentStatusRunning,
	}

	unblock := make(chan struct{})
	testAgent.afterFirstPrepare = func() {
		<-unblock
	}
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		_, runErr := subAgent.Run(context.Background(), SessionAgentCall{
			SessionID:       childSession.ID,
			Prompt:          "run now",
			MaxOutputTokens: 1000,
		})
		require.NoError(t, runErr)
	}()

	require.Eventually(t, func() bool {
		return subAgent.IsSessionBusy(childSession.ID)
	}, time.Second, 5*time.Millisecond, "the first run must be observed as busy before queueing the follow-up")

	resp, err := coord.queueSubagentFollowUp(t.Context(), ref, "follow-up prompt")
	require.NoError(t, err)
	require.Contains(t, resp.Content, ref.ID)
	require.Equal(t, 1, subAgent.QueuedPrompts(childSession.ID),
		"the follow-up must be queued, not run as a second concurrent turn")

	close(unblock)
	<-firstDone

	require.Eventually(t, func() bool {
		if subAgent.QueuedPrompts(childSession.ID) != 0 {
			return false
		}
		msgs, listErr := env.messages.List(t.Context(), childSession.ID)
		if listErr != nil {
			return false
		}
		for _, msg := range msgs {
			if msg.Role == message.User && msg.Content().Text == "follow-up prompt" {
				return true
			}
		}
		return false
	}, time.Second, 10*time.Millisecond, "the queued follow-up must auto-run once the current turn ends")
}

// TestRemoveSubagentsForParentSession covers R5 from
// docs/refactor-subagent-continuation.md: deleting a parent session must
// fully unregister every subagent it spawned (idle, parked, and aborted
// alike), not just the ones that happened to still be in a terminal state
// that used to trigger Unregister. It must also drop any lingering
// childSessionAgents entry and pending lifecycle timer so nothing
// referencing the deleted parent survives.
func TestRemoveSubagentsForParentSession(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	coord := newResumeTestCoordinator(t, env, "test-provider", config.ProviderConfig{ID: "test-provider"})

	idleAgent := &mockSessionAgent{}
	idleRef := coord.agentRegistry.Register(AgentRef{
		ID:              "0-Main::idle-1",
		DisplayName:     "Idle",
		Kind:            AgentKindSub,
		Status:          AgentStatusIdle,
		Agent:           idleAgent,
		SessionID:       "child-idle",
		ParentSessionID: "parent-1",
	})
	coord.childSessionAgents.Store("child-idle", idleAgent)
	coord.lifecycle.Adopt("child-idle", idleRef.ID, time.Hour)

	parkedRef := coord.agentRegistry.Register(AgentRef{
		ID:              "0-Main::parked-1",
		DisplayName:     "Parked",
		Kind:            AgentKindSub,
		Status:          AgentStatusParked,
		SessionID:       "child-parked",
		ParentSessionID: "parent-1",
	})

	// A subagent belonging to a different parent session must survive.
	otherParentRef := coord.agentRegistry.Register(AgentRef{
		ID:              "0-Main::other-1",
		DisplayName:     "Other",
		Kind:            AgentKindSub,
		Status:          AgentStatusIdle,
		SessionID:       "child-other",
		ParentSessionID: "parent-2",
	})

	coord.removeSubagentsForParentSession("parent-1")

	_, ok := coord.agentRegistry.Get(idleRef.ID)
	require.False(t, ok, "idle subagent must be unregistered when its parent session is deleted")
	_, ok = coord.agentRegistry.Get(parkedRef.ID)
	require.False(t, ok, "parked subagent must be unregistered when its parent session is deleted")
	_, ok = coord.childSessionAgents.Load("child-idle")
	require.False(t, ok, "childSessionAgents entry must be cleared")
	require.False(t, coord.lifecycle.IsAdopted("child-idle"), "pending lifecycle timer must be revoked")

	_, ok = coord.agentRegistry.Get(otherParentRef.ID)
	require.True(t, ok, "subagents belonging to other parent sessions must not be touched")
}

// newResumeQueueTestSessionAgent mirrors queue_control_test.go's
// newQueuePrepareTestSessionAgent but sets ModelCfg.Provider so
// queueSubagentFollowUp's own provider lookup (Providers.Get) succeeds.
func newResumeQueueTestSessionAgent(env fakeEnv, providerID string, fakeAgent fantasy.Agent) *sessionAgent {
	model := Model{
		ModelCfg: config.SelectedModel{Provider: providerID},
	}
	return NewSessionAgent(SessionAgentOptions{
		LargeModel: model,
		SmallModel: model,
		WorkingDir: env.workingDir,
		IsYolo:     true,
		Sessions:   env.sessions,
		Messages:   env.messages,
		AgentFactory: func(fantasy.LanguageModel, ...fantasy.AgentOption) fantasy.Agent {
			return fakeAgent
		},
	}).(*sessionAgent)
}

// TestResumeSubagent_WarmRevive_DowngradesWorktreeIsolation is the warm-tier
// counterpart of TestResumeSubagent_ColdRevive_DowngradesWorktreeIsolation.
// The downgrade cannot be scoped to parked entries: runSubAgentDirect defers
// cleanupWorktreeIfNeeded on every worktree-isolated run, so the worktree is
// merged back and removed when the first run ends -- before the keep-alive
// window that makes a warm revive possible. An idle-revived subagent would
// otherwise silently get a second, unrelated worktree.
func TestResumeSubagent_WarmRevive_DowngradesWorktreeIsolation(t *testing.T) {
	t.Parallel()

	const providerID = "test-provider"
	env := testEnv(t)
	coord := newResumeTestCoordinator(t, env, providerID, config.ProviderConfig{ID: providerID})

	parentSession, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	childSession, err := env.sessions.CreateTaskSession(t.Context(), "child-warm-worktree", parentSession.ID, "Writer task")
	require.NoError(t, err)

	var prompts []string
	agent := newMockAgent(providerID, 4096, yieldingRunFunc(t, env, "warm worktree response", &prompts))

	ref := coord.agentRegistry.Register(AgentRef{
		ID:              "0-Main::writer-1",
		DisplayName:     "Writer",
		Kind:            AgentKindSub,
		ParentID:        coord.mainAgentID,
		Status:          AgentStatusIdle,
		Agent:           agent,
		SessionID:       childSession.ID,
		ProfileName:     config.AgentGeneral,
		ParentSessionID: parentSession.ID,
		Isolation:       "worktree",
	})
	coord.childSessionAgents.Store(childSession.ID, agent)
	coord.lifecycle.Adopt(childSession.ID, ref.ID, time.Hour)

	snap, ok := coord.agentRegistry.FullSnapshot(ref.ID)
	require.True(t, ok)
	require.Equal(t, AgentStatusIdle, snap.Status, "this must exercise the warm tier")

	resp, err := coord.resumeSubagent(t.Context(), snap, "continue writing")
	require.NoError(t, err)
	require.Contains(t, resp.Content, "warm worktree response")
	require.Contains(t, resp.Content, "shared parent workspace",
		"warm revive must downgrade worktree isolation too, not just cold revive")

	live, ok := coord.childSessionAgents.Load(childSession.ID)
	require.True(t, ok)
	require.Same(t, agent, live, "the downgrade must not disturb warm instance reuse")
}
