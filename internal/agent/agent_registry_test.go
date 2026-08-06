package agent

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAgentRegistry_RegisterAndGet(t *testing.T) {
	t.Parallel()

	r := &AgentRegistry{refs: make(map[string]*AgentRef)}

	ref := r.Register(AgentRef{
		ID:          "test-1",
		DisplayName: "Test Agent",
		Kind:        AgentKindSub,
		Status:      AgentStatusRunning,
	})

	require.Equal(t, "test-1", ref.ID)
	require.Equal(t, "Test Agent", ref.DisplayName)

	got, ok := r.Get("test-1")
	require.True(t, ok)
	require.Equal(t, "Test Agent", got.DisplayName)

	_, ok = r.Get("nonexistent")
	require.False(t, ok)
}

func TestAgentRegistry_Unregister(t *testing.T) {
	t.Parallel()

	r := &AgentRegistry{refs: make(map[string]*AgentRef)}

	r.Register(AgentRef{ID: "a", DisplayName: "A", Kind: AgentKindSub, Status: AgentStatusRunning})
	r.Register(AgentRef{ID: "b", DisplayName: "B", Kind: AgentKindSub, Status: AgentStatusRunning})

	r.Unregister("a")

	_, ok := r.Get("a")
	require.False(t, ok)

	_, ok = r.Get("b")
	require.True(t, ok)
}

func TestAgentRegistry_ListVisibleTo(t *testing.T) {
	t.Parallel()

	r := &AgentRegistry{refs: make(map[string]*AgentRef)}

	r.Register(AgentRef{ID: "main", DisplayName: "Main", Kind: AgentKindMain, Status: AgentStatusRunning})
	r.Register(AgentRef{ID: "task-1", DisplayName: "Task 1", Kind: AgentKindSub, Status: AgentStatusRunning})
	r.Register(AgentRef{ID: "task-2", DisplayName: "Task 2", Kind: AgentKindSub, Status: AgentStatusIdle})
	r.Register(AgentRef{ID: "task-3", DisplayName: "Task 3", Kind: AgentKindSub, Status: AgentStatusCompleted})
	r.Register(AgentRef{ID: "task-4", DisplayName: "Task 4", Kind: AgentKindSub, Status: AgentStatusAborted})

	visible := r.ListVisibleTo("main")
	require.Len(t, visible, 2)

	ids := make(map[string]bool)
	for _, ref := range visible {
		ids[ref.ID] = true
	}
	require.True(t, ids["task-1"])
	require.True(t, ids["task-2"])
}

func TestAgentRegistry_SetStatus(t *testing.T) {
	t.Parallel()

	r := &AgentRegistry{refs: make(map[string]*AgentRef)}

	r.Register(AgentRef{ID: "task-1", DisplayName: "Task 1", Kind: AgentKindSub, Status: AgentStatusRunning})

	r.SetStatus("task-1", AgentStatusCompleted)

	got, ok := r.Get("task-1")
	require.True(t, ok)
	require.Equal(t, AgentStatusCompleted, got.Status)
}

// TestAgentRegistry_EffectiveStatus_ReflectsBusyAgent guards against the
// bug described in docs/refactor-irc.md §2.1(c): the primary agent is
// registered once at startup as Idle and nothing ever calls SetStatus on it
// again, so peers querying the registry always saw it as idle regardless of
// whether a turn was actually running. EffectiveStatus (and, transitively,
// the IRC-facing adapter and peer roster) must derive Running from the
// attached SessionAgent's IsBusy() instead of trusting the stale stored
// status.
func TestAgentRegistry_EffectiveStatus_ReflectsBusyAgent(t *testing.T) {
	t.Parallel()

	r := &AgentRegistry{refs: make(map[string]*AgentRef)}
	mainAgent := &mockSessionAgent{}
	r.Register(AgentRef{ID: "0-Main", DisplayName: "Main", Kind: AgentKindMain, Status: AgentStatusIdle, Agent: mainAgent})

	status, ok := r.EffectiveStatus("0-Main")
	require.True(t, ok)
	require.Equal(t, AgentStatusIdle, status, "idle stored status with a non-busy agent stays idle")

	mainAgent.busy = true
	status, ok = r.EffectiveStatus("0-Main")
	require.True(t, ok)
	require.Equal(t, AgentStatusRunning, status, "a busy agent must be reported as running even though its stored status is idle")

	mainAgent.busy = false
	status, ok = r.EffectiveStatus("0-Main")
	require.True(t, ok)
	require.Equal(t, AgentStatusIdle, status, "status reverts to idle once the agent stops being busy")

	_, ok = r.EffectiveStatus("nonexistent")
	require.False(t, ok)
}

// TestAgentRegistry_EffectiveStatus_LeavesTerminalStatusesAlone ensures the
// derivation never resurrects a finished agent: Aborted/Completed must be
// returned as-is even if a stale Agent reference reports busy.
func TestAgentRegistry_EffectiveStatus_LeavesTerminalStatusesAlone(t *testing.T) {
	t.Parallel()

	r := &AgentRegistry{refs: make(map[string]*AgentRef)}
	r.Register(AgentRef{ID: "done", DisplayName: "Done", Kind: AgentKindSub, Status: AgentStatusCompleted, Agent: &mockSessionAgent{busy: true}})
	r.Register(AgentRef{ID: "dead", DisplayName: "Dead", Kind: AgentKindSub, Status: AgentStatusAborted, Agent: &mockSessionAgent{busy: true}})

	status, ok := r.EffectiveStatus("done")
	require.True(t, ok)
	require.Equal(t, AgentStatusCompleted, status)

	status, ok = r.EffectiveStatus("dead")
	require.True(t, ok)
	require.Equal(t, AgentStatusAborted, status)
}

// TestAgentRegistry_AsIrcRegistry_ReflectsBusyMainAgent is the IRC-facing
// regression counterpart of TestAgentRegistry_EffectiveStatus_ReflectsBusyAgent:
// `irc list` (and a DM's reachability check) must see the primary agent as
// running while it is busy, not permanently idle.
func TestAgentRegistry_AsIrcRegistry_ReflectsBusyMainAgent(t *testing.T) {
	t.Parallel()

	r := &AgentRegistry{refs: make(map[string]*AgentRef)}
	mainAgent := &mockSessionAgent{busy: true}
	r.Register(AgentRef{ID: "0-Main", DisplayName: "Main", Kind: AgentKindMain, Status: AgentStatusIdle, Agent: mainAgent})
	r.Register(AgentRef{ID: "0-Main::t1", DisplayName: "Explore", Kind: AgentKindSub, Status: AgentStatusRunning, ParentID: "0-Main"})

	irc := r.AsIrcRegistry()

	peer, ok := irc.Get("0-Main")
	require.True(t, ok)
	require.Equal(t, "running", peer.Status)

	visible := irc.ListVisibleTo("0-Main::t1")
	var found bool
	for _, p := range visible {
		if p.ID == "0-Main" {
			found = true
			require.Equal(t, "running", p.Status)
		}
	}
	require.True(t, found, "main agent should be visible to its subagent")
}

func TestAgentRegistry_AsIrcRegistry(t *testing.T) {
	t.Parallel()

	r := &AgentRegistry{refs: make(map[string]*AgentRef)}

	r.Register(AgentRef{ID: "0-Main", DisplayName: "Main", Kind: AgentKindMain, Status: AgentStatusRunning})
	r.Register(AgentRef{ID: "0-Main::t1", DisplayName: "Explore", Kind: AgentKindSub, Status: AgentStatusRunning, ParentID: "0-Main"})
	r.Register(AgentRef{ID: "0-Main::t2", DisplayName: "Fix", Kind: AgentKindSub, Status: AgentStatusCompleted, ParentID: "0-Main"})

	irc := r.AsIrcRegistry()

	peer, ok := irc.Get("0-Main::t1")
	require.True(t, ok)
	require.Equal(t, "0-Main::t1", peer.ID)
	require.Equal(t, "Explore", peer.DisplayName)
	require.Equal(t, "sub", peer.Kind)
	require.Equal(t, "running", peer.Status)
	require.Equal(t, "0-Main", peer.ParentID)

	_, ok = irc.Get("0-Main::t2")
	require.True(t, ok)

	_, ok = irc.Get("nonexistent")
	require.False(t, ok)

	visible := irc.ListVisibleTo("0-Main")
	require.Len(t, visible, 1)
	require.Equal(t, "0-Main::t1", visible[0].ID)
}

func TestAgentRegistry_OnChange(t *testing.T) {
	t.Parallel()

	t.Run("fires on register and set status", func(t *testing.T) {
		t.Parallel()
		r := &AgentRegistry{refs: make(map[string]*AgentRef)}

		var changes int32
		r.OnChange(func() { changes++ })

		r.Register(AgentRef{ID: "a", DisplayName: "A", Kind: AgentKindSub, Status: AgentStatusRunning})
		r.SetStatus("a", AgentStatusCompleted)

		require.GreaterOrEqual(t, changes, int32(2))
	})

	t.Run("unsubscribe stops notifications", func(t *testing.T) {
		t.Parallel()
		r := &AgentRegistry{refs: make(map[string]*AgentRef)}

		var changes int32
		unsub := r.OnChange(func() { changes++ })

		r.Register(AgentRef{ID: "a", DisplayName: "A", Kind: AgentKindSub, Status: AgentStatusRunning})
		beforeUnsub := changes

		unsub()

		r.Register(AgentRef{ID: "b", DisplayName: "B", Kind: AgentKindSub, Status: AgentStatusRunning})
		afterSecond := changes

		require.Equal(t, beforeUnsub, afterSecond)
	})
}

func TestAgentRegistry_Reset(t *testing.T) {
	t.Parallel()

	r := &AgentRegistry{refs: make(map[string]*AgentRef)}

	r.Register(AgentRef{ID: "a", DisplayName: "A", Kind: AgentKindSub, Status: AgentStatusRunning})
	r.Reset()

	_, ok := r.Get("a")
	require.False(t, ok)
}

func TestRenderIrcPeerRoster(t *testing.T) {
	t.Parallel()

	t.Run("returns empty when no peers visible", func(t *testing.T) {
		t.Parallel()
		r := &AgentRegistry{refs: make(map[string]*AgentRef)}
		r.Register(AgentRef{ID: "only", DisplayName: "Only", Kind: AgentKindMain, Status: AgentStatusRunning})
		result := renderIrcPeerRoster(r, "only")
		require.Empty(t, result)
	})

	t.Run("renders visible peers", func(t *testing.T) {
		t.Parallel()
		r := &AgentRegistry{refs: make(map[string]*AgentRef)}
		r.Register(AgentRef{ID: "main", DisplayName: "Main", Kind: AgentKindMain, Status: AgentStatusRunning})
		r.Register(AgentRef{ID: "sub-1", DisplayName: "Explore", Kind: AgentKindSub, Status: AgentStatusRunning, ParentID: "main"})
		r.Register(AgentRef{ID: "sub-2", DisplayName: "Fix", Kind: AgentKindSub, Status: AgentStatusIdle, ParentID: "main"})

		result := renderIrcPeerRoster(r, "sub-1")
		require.Contains(t, result, "<irc_peers>")
		require.Contains(t, result, "</irc_peers>")
		require.Contains(t, result, "`main`")
		require.Contains(t, result, "Main")
		require.Contains(t, result, "`sub-2`")
		require.Contains(t, result, "Fix")
		require.NotContains(t, result, "sub-1")
	})

	t.Run("excludes completed and aborted peers", func(t *testing.T) {
		t.Parallel()
		r := &AgentRegistry{refs: make(map[string]*AgentRef)}
		r.Register(AgentRef{ID: "main", DisplayName: "Main", Kind: AgentKindMain, Status: AgentStatusRunning})
		r.Register(AgentRef{ID: "done", DisplayName: "Done", Kind: AgentKindSub, Status: AgentStatusCompleted})
		r.Register(AgentRef{ID: "dead", DisplayName: "Dead", Kind: AgentKindSub, Status: AgentStatusAborted})

		result := renderIrcPeerRoster(r, "main")
		require.NotContains(t, result, "Done")
		require.NotContains(t, result, "Dead")
	})
}

// TestAgentRegistry_EffectiveStatus_ParkedStaysParked is test 7 from
// docs/refactor-subagent-continuation.md §6: a parked entry's effective
// status must be reported as Parked, never derived into Running/Idle, even
// though effectiveStatus's Idle/Running derivation switch would run for any
// ref that still carried a non-nil Agent. SetParked clearing Agent to nil
// takes the fast path (agent == nil -> return status unchanged); this test
// also covers the (currently unreachable in production, since SetParked
// always clears Agent) defensive case of a Parked status with a non-nil
// Agent, to guard the "default: return status" branch independently of
// that invariant.
func TestAgentRegistry_EffectiveStatus_ParkedStaysParked(t *testing.T) {
	t.Parallel()

	r := &AgentRegistry{refs: make(map[string]*AgentRef)}
	r.Register(AgentRef{ID: "parked-clean", DisplayName: "Parked", Kind: AgentKindSub, Status: AgentStatusIdle, Agent: &mockSessionAgent{busy: true}})
	r.SetParked("parked-clean")

	status, ok := r.EffectiveStatus("parked-clean")
	require.True(t, ok)
	require.Equal(t, AgentStatusParked, status)

	ref, ok := r.Get("parked-clean")
	require.True(t, ok)
	require.Nil(t, ref.Agent, "SetParked must clear the live SessionAgent reference")

	// Defensive case: even if a live Agent were still attached to a Parked
	// ref, effectiveStatus's default branch must return the stored status
	// unchanged rather than deriving Running/Idle from IsBusy().
	r.Register(AgentRef{ID: "parked-with-agent", DisplayName: "Parked2", Kind: AgentKindSub, Status: AgentStatusParked, Agent: &mockSessionAgent{busy: true}})
	status, ok = r.EffectiveStatus("parked-with-agent")
	require.True(t, ok)
	require.Equal(t, AgentStatusParked, status)
}

// TestAgentRegistry_ListVisibleTo_ExcludesParked is the second half of test
// 7: phase 1 keeps parked subagents out of the IRC-visible roster (phase 2
// is what makes them addressable there -- see
// docs/refactor-subagent-continuation.md §4 phase 2 item 1). This also
// guards against a panic: ListVisibleTo/snapshotVisibleTo must not try to
// call IsBusy() on a parked ref's (now nil) Agent.
func TestAgentRegistry_ListVisibleTo_ExcludesParked(t *testing.T) {
	t.Parallel()

	r := &AgentRegistry{refs: make(map[string]*AgentRef)}
	r.Register(AgentRef{ID: "main", DisplayName: "Main", Kind: AgentKindMain, Status: AgentStatusRunning})
	r.Register(AgentRef{ID: "sub-idle", DisplayName: "Idle", Kind: AgentKindSub, Status: AgentStatusIdle, Agent: &mockSessionAgent{}})
	r.Register(AgentRef{ID: "sub-parked", DisplayName: "Parked", Kind: AgentKindSub, Status: AgentStatusIdle, Agent: &mockSessionAgent{}})
	require.NotPanics(t, func() { r.SetParked("sub-parked") })

	visible := r.ListVisibleTo("main")
	ids := make(map[string]bool)
	for _, ref := range visible {
		ids[ref.ID] = true
	}
	require.True(t, ids["sub-idle"])
	require.False(t, ids["sub-parked"], "parked subagents must stay out of the phase-1 roster")

	snaps := r.snapshotVisibleTo("main")
	snapIDs := make(map[string]bool)
	for _, snap := range snaps {
		snapIDs[snap.ID] = true
	}
	require.True(t, snapIDs["sub-idle"])
	require.False(t, snapIDs["sub-parked"])
}

// TestAgentRegistry_IrcAdapterStatusIsRaceFree guards the reason the IRC
// adapter and roster read peers through snapshotVisibleTo/snapshot rather
// than the *AgentRef-returning Get/ListVisibleTo: refs are stored by
// pointer and SetStatus mutates Status in place, so reading Status off a
// returned ref races with any concurrent status write. Run under -race.
func TestAgentRegistry_IrcAdapterStatusIsRaceFree(t *testing.T) {
	t.Parallel()

	r := &AgentRegistry{refs: make(map[string]*AgentRef)}
	r.Register(AgentRef{
		ID:          "0-Main",
		DisplayName: "Main",
		Kind:        AgentKindMain,
		Status:      AgentStatusIdle,
		Agent:       &mockSessionAgent{},
	})
	adapter := r.AsIrcRegistry()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 2000 {
			r.SetStatus("0-Main", AgentStatusRunning)
			r.SetStatus("0-Main", AgentStatusIdle)
		}
	}()
	go func() {
		defer wg.Done()
		for range 2000 {
			adapter.Get("0-Main")
			adapter.ListVisibleTo("other")
			renderIrcPeerRoster(r, "other")
		}
	}()
	wg.Wait()
}
